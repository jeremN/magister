# Cross-repo / fork PRs (`cm pr --head-repo`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `cm pr` open a GitHub pull request from a fork's branch into the upstream repo by adding an optional `--head-repo` input that makes the PR head `forkowner:branch`.

**Architecture:** A fork PR keeps the **base** repo (where the PR opens) as the run's source origin (or `--remote`) and changes only the **head** from a bare `branch` to `forkowner:branch`. The fork owner is derived by resolving `--head-repo` through the existing `workspace.ResolveRemote` + `host.ParseRemote` (symmetric with `--remote`/`cm push`). `internal/host`'s `CreatePR` already passes the head string verbatim to `gh --head=`, so no host change is needed; the work is string composition in `Supervisor.prCore` plus a `BranchExists` fix to check the fork, threaded out through one DTO field, one handler mapping, and one `cm` flag.

**Tech Stack:** Go 1.22 stdlib only; `gh` CLI via ambient auth; existing `internal/host` (gh wrapper), `internal/workspace` (ResolveRemote), `internal/supervisor` (PR/Ship), `internal/api` (HTTP), `cmd/cm` (client). Tests use the env-driven `internal/host/testdata/fake-gh` stub.

## Global Constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no new package; no migration; no schema change; no new SSE event kind.
- `--head-repo` omitted ⇒ behavior is **byte-for-byte** today's same-repo PR.
- Read-only on the source repo (`ResolveRemote` only runs `git remote get-url`).
- Argv-safety: `forkOwner` is charset-guarded by `host.ParseRemote`/`safeSeg`; `branch` by `safePRRef`; the head is composed as a single `owner:branch` token. Auth stays ambient `gh` (zero token handling).
- `cm ship` is **out of scope** — it must keep its current same-repo-only behavior (never set `HeadRepo`).
- `internal/host` and `cm push` are **unchanged**.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.
- **Sandbox note for the implementer:** the real-git tests in `internal/supervisor`, `internal/api`, and `cmd/magisterd` require the Bash sandbox disabled. When running `go test` for `./internal/supervisor/...` or `./internal/api/...`, run with the sandbox disabled, or pre-existing real-git e2e tests will spuriously fail (they are unrelated to this change).

---

### Task 1: Server-side fork PR support (`prCore` + API surface)

**Files:**
- Modify: `internal/supervisor/pr.go` (PROpts struct ~19-22; prCore ~60-113)
- Modify: `internal/api/dto.go` (prRequest struct ~48-57)
- Modify: `internal/api/handlers.go` (handlePR PROpts literal ~172-175)
- Test: `internal/supervisor/pr_test.go` (add fork tests)
- Test: `internal/api/handlers_test.go` (add a fork endpoint test)

**Interfaces:**
- Consumes (existing, unchanged signatures):
  - `workspace.ResolveRemote(sourceRepo, remote string) (string, error)` — ""→origin, URL→passthrough, name→`git remote get-url`.
  - `host.ParseRemote(remoteURL string) (host, owner, repo string, err error)` — github.com only, else error.
  - `host.Runner.ExistingOpenPR(ctx, owner, repo, head string) (url string, exists bool, err error)` — `head` may be `owner:branch`.
  - `host.Runner.CreatePR(ctx, host.CreateOpts{Owner, Repo, Head, Base, Title, Body string; Draft bool}) (url string, err error)` — `Head` passed verbatim to `gh --head=`.
  - `host.Runner.BranchExists(ctx, owner, repo, branch string) bool` — checks `branch` on `owner/repo`.
  - `safePRRef(s string) bool` (in pr.go) — rejects `:`, leading `-`, `..`, etc.
- Produces (for Task 2 and the API):
  - `supervisor.PROpts` gains field `HeadRepo string` (the fork URL-or-remote-name; "" = same-repo PR).
  - `prRequest` (dto.go) gains field `HeadRepo string` with JSON tag `head_repo`.
  - On a fork PR, `PRResult.Head == "forkOwner:branch"`.

- [ ] **Step 1: Write the failing supervisor tests**

Append these three tests to `internal/supervisor/pr_test.go` (they reuse the file's existing helpers `newPRSup`, `seedExtRun`, `srcWithGHOrigin`, `prErrStatus`, `requireGitS`, and the env-driven fake-gh stub):

```go
func TestPRFromForkComposesCrossForkHead(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_GH_ARGV_FILE", argv)
	t.Setenv("FAKE_GH_PR_URL", "https://github.com/test-owner/test-repo/pull/9")
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))

	res, err := sup.PR(context.Background(), "r1", PROpts{HeadRepo: "https://github.com/fork-owner/test-repo.git"})
	if err != nil {
		t.Fatalf("pr: %v", err)
	}
	// Base repo stays the upstream; head is the cross-fork owner:branch form.
	if res.Repo != "test-owner/test-repo" {
		t.Errorf("repo = %q, want upstream test-owner/test-repo", res.Repo)
	}
	if res.Head != "fork-owner:magister/r1" {
		t.Errorf("head = %q, want fork-owner:magister/r1", res.Head)
	}
	got, _ := os.ReadFile(argv)
	for _, want := range []string{"create", "--repo=test-owner/test-repo", "--head=fork-owner:magister/r1"} {
		if !strings.Contains(string(got), want+"\n") {
			t.Errorf("argv missing %q; got:\n%s", want, got)
		}
	}
}

func TestPRFromForkChecksBranchOnFork(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_GH_ARGV_FILE", argv)
	t.Setenv("FAKE_GH_CREATE_FAIL", "GraphQL: Head sha can't be blank")
	t.Setenv("FAKE_GH_BRANCH_MISSING", "1")
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))

	_, err := sup.PR(context.Background(), "r1", PROpts{HeadRepo: "https://github.com/fork-owner/test-repo.git"})
	if got := prErrStatus(t, err); got != http.StatusConflict {
		t.Fatalf("status = %d, want 409", got)
	}
	var pe *PRError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PRError, got %T", err)
	}
	if !strings.Contains(pe.Msg, "cm push") {
		t.Errorf("message should tell the user to push first, got %q", pe.Msg)
	}
	// The branch-existence check must target the FORK owner, not the upstream.
	got, _ := os.ReadFile(argv)
	if !strings.Contains(string(got), "repos/fork-owner/test-repo/branches/magister/r1\n") {
		t.Errorf("BranchExists should query the fork; argv:\n%s", got)
	}
}

func TestPRFromForkRejectsNonGitHubHeadRepo(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))
	_, err := sup.PR(context.Background(), "r1", PROpts{HeadRepo: "https://gitlab.com/fork-owner/test-repo.git"})
	if got := prErrStatus(t, err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (non-github head-repo)", got)
	}
}
```

- [ ] **Step 2: Run the supervisor tests to verify they fail**

Run (sandbox disabled): `go test ./internal/supervisor/ -run 'TestPRFromFork'`
Expected: FAIL — `PROpts` has no field `HeadRepo` (compile error), once that is added the head/argv assertions fail because composition isn't implemented yet.

- [ ] **Step 3: Add `HeadRepo` to `PROpts`**

In `internal/supervisor/pr.go`, replace the `PROpts` struct and its doc comment (currently lines ~16-22):

```go
// PROpts configures PR. Zero values mean: origin remote, magister/<runID> head, the
// unique terminal step (for the body summary), the repo's default base branch,
// generated title/body, not a draft. HeadRepo empty means a same-repo PR (head is a
// bare branch); set to a fork URL-or-remote-name it makes a cross-fork PR whose head
// is forkowner:branch (the PR still opens on the base repo / origin / --remote).
type PROpts struct {
	Remote, As, Step, Base, Title, Body, HeadRepo string
	Draft                                         bool
}
```

- [ ] **Step 4: Compose the head (and track the head's owner) in `prCore`**

In `internal/supervisor/pr.go`, the block that currently reads (lines ~60-77):

```go
	head := opts.As
	if head == "" {
		head = "magister/" + string(runID)
	}
	if !safePRRef(head) {
		return PRResult{}, false, prErr(http.StatusBadRequest, "invalid head branch %q", head)
	}
	if opts.Base != "" && !safePRRef(opts.Base) {
		return PRResult{}, false, prErr(http.StatusBadRequest, "invalid base branch %q", opts.Base)
	}
	remoteURL, err := workspace.ResolveRemote(rs.Repo, opts.Remote)
	if err != nil {
		return PRResult{}, false, prErr(http.StatusBadRequest, "remote: %v", err)
	}
	_, owner, repo, err := host.ParseRemote(remoteURL)
	if err != nil {
		return PRResult{}, false, prErr(http.StatusBadRequest, "%v", err)
	}
```

becomes (rename `head`→`branch` for the bare ref, then compose `head` and `headOwner`):

```go
	branch := opts.As
	if branch == "" {
		branch = "magister/" + string(runID)
	}
	if !safePRRef(branch) {
		return PRResult{}, false, prErr(http.StatusBadRequest, "invalid head branch %q", branch)
	}
	if opts.Base != "" && !safePRRef(opts.Base) {
		return PRResult{}, false, prErr(http.StatusBadRequest, "invalid base branch %q", opts.Base)
	}
	remoteURL, err := workspace.ResolveRemote(rs.Repo, opts.Remote)
	if err != nil {
		return PRResult{}, false, prErr(http.StatusBadRequest, "remote: %v", err)
	}
	_, owner, repo, err := host.ParseRemote(remoteURL)
	if err != nil {
		return PRResult{}, false, prErr(http.StatusBadRequest, "%v", err)
	}
	// head defaults to the bare branch (same-repo PR). With --head-repo the branch
	// lives on a fork: resolve the fork owner and form the cross-fork head owner:branch.
	// headOwner is where the head branch actually lives (base owner unless --head-repo).
	head := branch
	headOwner := owner
	if opts.HeadRepo != "" {
		forkURL, ferr := workspace.ResolveRemote(rs.Repo, opts.HeadRepo)
		if ferr != nil {
			return PRResult{}, false, prErr(http.StatusBadRequest, "head-repo: %v", ferr)
		}
		_, forkOwner, _, ferr := host.ParseRemote(forkURL)
		if ferr != nil {
			return PRResult{}, false, prErr(http.StatusBadRequest, "head-repo: %v", ferr)
		}
		headOwner = forkOwner
		head = forkOwner + ":" + branch
	}
```

(The downstream `runner.ExistingOpenPR(ctx, owner, repo, head)`, `runner.CreatePR(... Head: head ...)`, and the two `PRResult{... Head: head ...}` returns are unchanged — `head` is now the possibly-composed value, and `owner`/`repo` remain the base repo.)

- [ ] **Step 5: Point `BranchExists` at the fork and give a fork-aware 409**

In `internal/supervisor/pr.go`, the create-failure block that currently reads (lines ~106-111):

```go
	if err != nil {
		if !runner.BranchExists(ctx, owner, repo, head) {
			return PRResult{}, false, prErr(http.StatusConflict, "branch %q not on remote; run `cm push %s` first", head, runID)
		}
		return PRResult{}, false, prErr(http.StatusBadGateway, "%v", err)
	}
```

becomes (check the bare `branch` on `headOwner`; keep the same-repo message byte-for-byte, add a fork-specific hint):

```go
	if err != nil {
		if !runner.BranchExists(ctx, headOwner, repo, branch) {
			if opts.HeadRepo != "" {
				return PRResult{}, false, prErr(http.StatusConflict, "branch %q not on fork %s/%s; run `cm push %s --remote <fork>` first", branch, headOwner, repo, runID)
			}
			return PRResult{}, false, prErr(http.StatusConflict, "branch %q not on remote; run `cm push %s` first", branch, runID)
		}
		return PRResult{}, false, prErr(http.StatusBadGateway, "%v", err)
	}
```

- [ ] **Step 6: Run the supervisor tests to verify they pass**

Run (sandbox disabled): `go test ./internal/supervisor/ -run 'TestPR'`
Expected: PASS — the three new `TestPRFromFork*` tests pass, and every existing `TestPR*` (including `TestPROpensPullRequest` asserting `--head=magister/r1`, and `TestPRUnpushedBranchSaysPushFirst` asserting `cm push`) still passes (same-repo path is byte-for-byte unchanged).

- [ ] **Step 7: Write the failing API endpoint test**

Append this test to `internal/api/handlers_test.go` (it mirrors `TestPREndpointOpensPR` at ~line 484; reuses `runGit`, `ghAPIStub`, `prResponse`, and the same Server/engine wiring):

```go
func TestPREndpointOpensCrossForkPR(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src := t.TempDir()
	runGit(t, src, "init")
	runGit(t, src, "remote", "add", "origin", "https://github.com/o/r.git")

	st := store.NewMem()
	reg := supervisor.NewApprovalRegistry()
	bus := event.NewBus()
	eng := &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: &supervisor.RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: core.SystemClock{},
	}
	sup := supervisor.New(eng, st, reg)
	sup.Host = &host.Runner{Bin: ghAPIStub(t)}
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	t.Setenv("FAKE_GH_PR_URL", "https://github.com/o/r/pull/9")
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Name: "demo", Repo: src, Status: core.RunSucceeded,
		FlowYAML: "name: demo\nsteps:\n  - id: integrate\n    agent: mock\n",
		Steps: []core.StepState{{
			RunID: "r1", StepID: "integrate", Status: core.StepSucceeded,
			Artifacts: []core.Artifact{{StepID: "integrate", Branch: "step/integrate", Commit: "abc"}},
		}},
	})
	srv := &Server{Sup: sup, Store: st, Bus: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	hs := httptest.NewServer(srv.Router(""))
	t.Cleanup(hs.Close)

	resp, err := http.Post(hs.URL+"/v1/runs/r1/pr", "application/json", strings.NewReader(`{"head_repo":"https://github.com/fork/r.git"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("pr = %d, want 200: %s", resp.StatusCode, b)
	}
	var pr prResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// head_repo threads through to a cross-fork head; base repo stays upstream.
	if pr.Repo != "o/r" || pr.Head != "fork:magister/r1" {
		t.Errorf("response = %+v, want Repo=o/r Head=fork:magister/r1", pr)
	}
}
```

- [ ] **Step 8: Run the API test to verify it fails**

Run (sandbox disabled): `go test ./internal/api/ -run TestPREndpointOpensCrossForkPR`
Expected: FAIL — `prRequest` has no `head_repo` field, so `head_repo` is dropped on decode and `pr.Head` comes back as the same-repo `magister/r1`.

- [ ] **Step 9: Add the `head_repo` DTO field and wire it through the handler**

In `internal/api/dto.go`, add the field to the `prRequest` struct (currently lines ~48-57), after `Body`:

```go
	Body     string `json:"body,omitempty"`
	HeadRepo string `json:"head_repo,omitempty"`
```

In `internal/api/handlers.go`, the `handlePR` call (lines ~172-175) currently reads:

```go
	res, err := s.Sup.PR(r.Context(), core.RunID(r.PathValue("id")), supervisor.PROpts{
		Remote: req.Remote, As: req.As, Step: req.Step, Base: req.Base,
		Title: req.Title, Body: req.Body, Draft: req.Draft,
	})
```

becomes (add `HeadRepo: req.HeadRepo` — do **not** touch `handleShip` below it, which keeps its same-repo `ShipOpts`):

```go
	res, err := s.Sup.PR(r.Context(), core.RunID(r.PathValue("id")), supervisor.PROpts{
		Remote: req.Remote, As: req.As, Step: req.Step, Base: req.Base,
		Title: req.Title, Body: req.Body, Draft: req.Draft, HeadRepo: req.HeadRepo,
	})
```

- [ ] **Step 10: Run the API test to verify it passes**

Run (sandbox disabled): `go test ./internal/api/ -run TestPREndpoint`
Expected: PASS — the new cross-fork test passes and the existing `TestPREndpointOpensPR` (same-repo, asserts `Head=="magister/r1"`) still passes.

- [ ] **Step 11: Vet, format, and run both packages**

Run (sandbox disabled):
```bash
gofmt -l internal/supervisor/pr.go internal/supervisor/pr_test.go internal/api/dto.go internal/api/handlers.go internal/api/handlers_test.go
go vet ./internal/supervisor/ ./internal/api/
go test ./internal/supervisor/ ./internal/api/
```
Expected: `gofmt -l` prints nothing; `go vet` clean; both packages PASS.

- [ ] **Step 12: Commit**

```bash
git add internal/supervisor/pr.go internal/supervisor/pr_test.go internal/api/dto.go internal/api/handlers.go internal/api/handlers_test.go
git commit -m "feat(pr): cross-fork PRs via PROpts.HeadRepo (head owner:branch)"
```

---

### Task 2: `cm pr --head-repo` client flag

**Files:**
- Modify: `cmd/cm/main.go` (the `pr` method ~306-348)
- Test: `cmd/cm/main_test.go` (add a flag test)

**Interfaces:**
- Consumes: the `POST /v1/runs/{id}/pr` JSON body field `head_repo` (added in Task 1).
- Produces: `cm pr <run> --head-repo <value>` sends `{"head_repo":"<value>"}` in the POST body.

- [ ] **Step 1: Write the failing client test**

Append this test to `cmd/cm/main_test.go`. It mirrors the existing `TestPRSendsJSONBody` **exactly** — the package's entrypoint is `dispatch(args []string, addr string, out io.Writer) int` (the server URL is the second positional arg, NOT an env var), and handler responses use the package's `writeBody(w, ...)` helper. The imports `bytes`, `io`, `net/http`, `net/http/httptest`, `encoding/json`, `strings` are already present in the file:

```go
func TestPRHeadRepoSendsHeadRepoJSONField(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeBody(w, `{"url":"https://github.com/o/r/pull/1"}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	code := dispatch([]string{"pr", "r1", "--head-repo", "https://github.com/fork/r.git"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if sent["head_repo"] != "https://github.com/fork/r.git" {
		t.Errorf("head_repo = %v, want the fork url; body=%s", sent["head_repo"], body)
	}
	if _, hyphen := sent["head-repo"]; hyphen {
		t.Errorf("body used the hyphenated key head-repo; want head_repo; body=%s", body)
	}
}
```

- [ ] **Step 2: Run the client test to verify it fails**

Run: `go test ./cmd/cm/ -run TestPRHeadRepoSendsHeadRepoJSONField`
Expected: FAIL — `--head-repo` is not parsed (it falls through to the `default` case and is mis-read as the run id), so `head_repo` is absent from the body.

- [ ] **Step 3: Parse `--head-repo` into the `head_repo` body key**

In `cmd/cm/main.go`, the `pr` method's argument loop currently reads (lines ~309-324):

```go
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--draft":
			body["draft"] = true
		case "--remote", "--as", "--step", "--base", "--title", "--body":
			flag := args[i]
			i++
			if i >= len(args) {
				fmt.Fprintf(out, "usage: %s requires a value\n", flag)
				return 2
			}
			body[flag[2:]] = args[i] // strip "--"
		default:
			run = args[i]
		}
	}
```

Add a dedicated `--head-repo` case (its JSON key is `head_repo`, not the hyphenated `flag[2:]`):

```go
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--draft":
			body["draft"] = true
		case "--head-repo":
			i++
			if i >= len(args) {
				fmt.Fprintln(out, "usage: --head-repo requires a value")
				return 2
			}
			body["head_repo"] = args[i]
		case "--remote", "--as", "--step", "--base", "--title", "--body":
			flag := args[i]
			i++
			if i >= len(args) {
				fmt.Fprintf(out, "usage: %s requires a value\n", flag)
				return 2
			}
			body[flag[2:]] = args[i] // strip "--"
		default:
			run = args[i]
		}
	}
```

- [ ] **Step 4: Update the `cm pr` usage string**

In `cmd/cm/main.go`, the `pr` method's usage line (line ~326) currently reads:

```go
		fmt.Fprintln(out, "usage: cm pr <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft]")
```

becomes:

```go
		fmt.Fprintln(out, "usage: cm pr <run> [--remote <url-or-name>] [--head-repo <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft]")
```

- [ ] **Step 5: Run the client test to verify it passes**

Run: `go test ./cmd/cm/ -run 'TestPR'`
Expected: PASS — the new `head_repo` test passes and the existing `TestPRSendsJSONBody` still passes.

- [ ] **Step 6: Vet, format, and run the package**

Run:
```bash
gofmt -l cmd/cm/main.go cmd/cm/main_test.go
go vet ./cmd/cm/
go test ./cmd/cm/
```
Expected: `gofmt -l` prints nothing; `go vet` clean; package PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/cm/main.go cmd/cm/main_test.go
git commit -m "feat(cm): cm pr --head-repo opens a cross-fork PR"
```

---

## Final verification (after both tasks)

Run (sandbox disabled — the supervisor/api/magisterd packages have real-git e2e tests):
```bash
gofmt -l .
go vet ./...
go test -race ./...
```
Expected: `gofmt -l` prints nothing; `go vet` clean; full suite green.

## Live smoke (manual, after merge — needs `gh` authed + a throwaway fork)

1. On GitHub, fork an upstream repo you don't own (or use two repos you own where one is a fork of the other).
2. Clone upstream locally as the source: `git clone <upstream-url> /tmp/upstream-src`.
3. `cm run flows/external-repo.yaml --repo /tmp/upstream-src --base main` → capture `$RID`.
4. `cm push "$RID" --remote <fork-url>` → delivers `magister/$RID` to the fork.
5. `cm pr "$RID" --head-repo <fork-url>` → opens a **real cross-fork PR** on upstream with head `forkowner:magister/$RID`, base upstream default. Confirm on GitHub the PR's "compare across forks" head is the fork.
6. `cm pr "$RID" --head-repo <fork-url>` again → `409` carrying the existing PR URL.
7. A `cm pr <fresh-run> --head-repo <fork-url>` whose branch was never pushed → `409` "branch … not on fork …; run `cm push … --remote <fork>` first".
