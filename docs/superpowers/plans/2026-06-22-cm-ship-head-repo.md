# cm ship --head-repo (fork ship) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `cm ship --head-repo <fork>` push a succeeded external-repo run's result branch to a fork and open the cross-fork PR into upstream in one command.

**Architecture:** A fork ship = push-to-fork + the cross-fork PR already built. `Supervisor.Ship` gains a `HeadRepo` field: when set, it routes the push to the fork (instead of `--remote`) and threads `HeadRepo` into the shared `prCore` (which already composes the `forkowner:branch` head and detects an existing cross-fork PR via the fixed `ExistingOpenPR`). Same-repo ship (`HeadRepo==""`) is byte-for-byte unchanged.

**Tech Stack:** Go 1.22 stdlib only; `gh` CLI (ambient auth) via `internal/host`; `git` via `internal/workspace`. No new dependency.

## Global Constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no new package; no migration; no schema change; no new SSE event kind.
- `--head-repo` omitted ⇒ behavior is **byte-for-byte** today's same-repo ship.
- Push always goes to `--head-repo` when set (never `--remote`); `--remote` feeds only the PR base in fork mode (its `cm pr` meaning).
- `--head-repo` is resolved/guarded by the existing machinery already inside `prCore` (`workspace.ResolveRemote` + `host.ParseRemote`); auth stays ambient `gh`/git (zero token handling); the source repo is never written.
- The CLI flag is `--head-repo` but the JSON field is `head_repo` (underscore). The generic `body[flag[2:]]` parser path would emit the hyphenated `head-repo`, so the client needs a **dedicated** `case "--head-repo"`.
- The fully-happy fork path (push to the fork **and** open the PR with one fork URL) is not offline-unit-testable — one URL cannot be both an offline-pushable bare repo and a github.com-parseable head owner. Offline tests prove push-routing and prCore-threading via the failure paths; the happy path is the live smoke (post-merge, manual).
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge. Real-git/`gh` e2e tests in `internal/supervisor`, `internal/api`, `cmd/magisterd` require the sandbox disabled.

---

## File Structure

- `internal/supervisor/ship.go` — `ShipOpts` gains `HeadRepo string`; `Ship` routes the push to the fork and threads `HeadRepo` into `prCore`; doc comment updated. (The core change.)
- `internal/supervisor/ship_test.go` — two new offline tests: push routes to the fork; `HeadRepo` threads into `prCore`.
- `internal/api/dto.go` — `shipRequest` gains `HeadRepo string \`json:"head_repo,omitempty"\``.
- `internal/api/handlers.go` — `handleShip` maps `HeadRepo: req.HeadRepo` into `ShipOpts`.
- `internal/api/handlers_test.go` — one new e2e test: a `/ship` body with `head_repo` routes the push to the fork.
- `cmd/cm/main.go` — `cm ship` parser gains a dedicated `case "--head-repo"`; usage string updated.
- `cmd/cm/main_test.go` — one new test: `cm ship --head-repo` sends the `head_repo` JSON field.

---

## Task 1: Server — `ShipOpts.HeadRepo`, push routing, `prCore` threading, DTO + handler

**Files:**
- Modify: `internal/supervisor/ship.go:12` (`ShipOpts` struct + doc comment), `internal/supervisor/ship.go:30-44` (`Ship` body)
- Modify: `internal/api/dto.go:79-88` (`shipRequest`), `internal/api/handlers.go:196` (`handleShip` `ShipOpts` literal)
- Test: `internal/supervisor/ship_test.go` (append two tests), `internal/api/handlers_test.go` (append one test)

**Interfaces:**
- Consumes: `Supervisor.Push(ctx, runID, PushOpts{Remote, As, Step string, Force bool})`; `Supervisor.prCore(ctx, runID, PROpts{...HeadRepo string})` — `prCore` already accepts `HeadRepo` (merged cross-fork PR slice) and returns `(PRResult, existed bool, error)`; on a non-github `HeadRepo` it returns a `*PRError` with `Status==400` and a message beginning `head-repo:`.
- Produces: `ShipOpts{Remote, As, Step, Base, Title, Body, HeadRepo string; Draft, Force bool}`; `shipRequest.HeadRepo` (JSON `head_repo`). Task 2 (the `cm` client) relies on the server accepting the `head_repo` JSON field.

- [ ] **Step 1: Write the failing supervisor tests**

Append to `internal/supervisor/ship_test.go` (the file already imports `context`, `errors`, `net/http`, `os`, `path/filepath`, `testing`, `time`, and `concentus/internal/{core,flow,host,store,workspace}`; all helpers below — `requireGitS`, `srcWithRemote`, `gitS`, `ghStub`, `prErrStatus`, `waitForStatus`, `testEngine`, `extRepoFlowYAML` — already exist in the package):

```go
// TestShipForkPushesToHeadRepo: a fork ship (HeadRepo set) pushes the result branch to
// the FORK remote, not to the source origin. Proven with the fork as a local bare repo:
// magister/<run> lands on the fork bare. (prCore then 400s on the non-github origin base
// — irrelevant here; this test asserts only the push destination.)
func TestShipForkPushesToHeadRepo(t *testing.T) {
	requireGitS(t)
	src, _, sha := srcWithRemote(t) // origin = a bare local repo (the "upstream")
	fork := t.TempDir()
	gitS(t, fork, "init", "--bare") // the "fork": a second bare local repo
	st := store.NewMem()
	reg := NewApprovalRegistry()
	gm := &workspace.GitManager{Root: t.TempDir()}
	sup := New(testEngine(t, st, reg, gm), st, reg)
	sup.Host = &host.Runner{Bin: ghStub(t)}
	t.Cleanup(func() { sup.Shutdown(time.Second) })

	f, err := flow.ParseBytes([]byte(extRepoFlowYAML))
	if err != nil {
		t.Fatal(err)
	}
	id, err := sup.Submit(context.Background(), f, extRepoFlowYAML, src, sha)
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, st, id, core.RunSucceeded)

	// Fork ship: the push must route to the fork. (The PR half then fails 400 on the
	// non-github origin base; we ignore that and assert the push landed on the fork.)
	_, _ = sup.Ship(context.Background(), id, ShipOpts{HeadRepo: fork})

	// gitS fatals if the ref is absent, so a successful rev-parse IS the assertion: the
	// branch is on the fork. If Ship had ignored HeadRepo and pushed to origin, the fork
	// bare would be empty and this would fail.
	gitS(t, fork, "rev-parse", "--verify", "magister/"+string(id))
}

// TestShipForkThreadsHeadRepoToPR: a fork ship threads HeadRepo into prCore. The source
// origin is a (never-contacted) github URL so the PR base parses; the fork is a local
// bare so the push succeeds. prCore then fails to parse the local-path HeadRepo as a
// github remote → a 400 whose message begins "head-repo:". That 400 can only arise if
// Ship forwarded HeadRepo to prCore (with HeadRepo unset, prCore would parse the github
// base and proceed to gh, not 400).
func TestShipForkThreadsHeadRepoToPR(t *testing.T) {
	requireGitS(t)
	// A committed source whose origin is a fake github URL (read by prCore, never fetched).
	src := t.TempDir()
	gitS(t, src, "init")
	gitS(t, src, "config", "user.name", "fix")
	gitS(t, src, "config", "user.email", "fix@example.com")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitS(t, src, "add", "-A")
	gitS(t, src, "commit", "-m", "base")
	gitS(t, src, "remote", "add", "origin", "https://github.com/test-owner/test-repo.git")
	sha := gitS(t, src, "rev-parse", "HEAD")

	fork := t.TempDir()
	gitS(t, fork, "init", "--bare")

	st := store.NewMem()
	reg := NewApprovalRegistry()
	gm := &workspace.GitManager{Root: t.TempDir()}
	sup := New(testEngine(t, st, reg, gm), st, reg)
	sup.Host = &host.Runner{Bin: ghStub(t)}
	t.Cleanup(func() { sup.Shutdown(time.Second) })

	f, err := flow.ParseBytes([]byte(extRepoFlowYAML))
	if err != nil {
		t.Fatal(err)
	}
	id, err := sup.Submit(context.Background(), f, extRepoFlowYAML, src, sha)
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, st, id, core.RunSucceeded)

	_, err = sup.Ship(context.Background(), id, ShipOpts{HeadRepo: fork})
	if got := prErrStatus(t, err); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (head-repo not a github remote)", got)
	}
	var pe *PRError
	errors.As(err, &pe)
	if !strings.HasPrefix(pe.Msg, "head-repo:") {
		t.Errorf("error = %q, want it to start with %q (proves HeadRepo threaded to prCore)", pe.Msg, "head-repo:")
	}
	// The push half delivered the branch to the fork before the PR step failed.
	gitS(t, fork, "rev-parse", "--verify", "magister/"+string(id))
}
```

This adds a `strings` use — add `"strings"` to the `internal/supervisor/ship_test.go` import block.

- [ ] **Step 2: Run the supervisor tests to verify they fail**

Run: `go test ./internal/supervisor/ -run 'TestShipFork' 2>&1 | head -30`
Expected: **build failure** — `unknown field 'HeadRepo' in struct literal of type ShipOpts` (the field does not exist yet).

- [ ] **Step 3: Write the failing API test**

Append to `internal/api/handlers_test.go` (mirrors `TestShipEndpointPropagatesPRErrorAfterPush` at line 580; helpers `setupAPISourceRepo`, `runGit`, `newGitServer`, `waitForStatus`, `extRepoFlowAPI`, `runResponse` already exist; the file already imports `bytes`, `context`, `encoding/json`, `io`, `net/http`, `net/url`, `os/exec`, `strings`, `testing`, and `concentus/internal/core`):

```go
// TestShipEndpointRoutesPushToHeadRepo: a /ship body with head_repo routes the push to
// the fork, proving the handler threads head_repo into ShipOpts.HeadRepo. origin is a
// local bare (the upstream), the fork is a second local bare; magister/<run> must land
// on the fork. (The PR half then 400s on the non-github origin base — ignored here.)
func TestShipEndpointRoutesPushToHeadRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src, _ := setupAPISourceRepo(t)
	origin := t.TempDir()
	runGit(t, origin, "init", "--bare")
	runGit(t, src, "remote", "add", "origin", origin)
	fork := t.TempDir()
	runGit(t, fork, "init", "--bare")

	hs, st := newGitServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs?repo="+url.QueryEscape(src)+"&base=HEAD",
		"application/x-yaml", bytes.NewBufferString(extRepoFlowAPI))
	if err != nil {
		t.Fatal(err)
	}
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()
	waitForStatus(t, st, rr.ID, core.RunSucceeded)

	payload, _ := json.Marshal(map[string]string{"head_repo": fork})
	sresp, err := http.Post(hs.URL+"/v1/runs/"+string(rr.ID)+"/ship", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	sresp.Body.Close()

	// runGit fatals if the ref is absent, so this rev-parse IS the assertion: the push
	// landed on the fork (not origin), which only happens if head_repo threaded through.
	runGit(t, fork, "rev-parse", "--verify", "magister/"+string(rr.ID))
}
```

This uses only already-imported packages (`encoding/json`, `bytes`, `net/http`, `net/url`, `os/exec`, `concentus/internal/core`) — no new import needed.

- [ ] **Step 4: Run the API test to verify it fails**

Run: `go test ./internal/api/ -run TestShipEndpointRoutesPushToHeadRepo 2>&1 | head -30` (sandbox disabled — real git)
Expected: FAIL at the final `runGit(fork, "rev-parse", …)`. The test compiles (it posts JSON, not a struct), but `shipRequest` has no `HeadRepo` field yet, so `encoding/json` drops the unknown `head_repo` key → `ShipOpts.HeadRepo==""` → the push routes to `origin` (default), leaving the fork bare empty → the rev-parse fatals "unknown revision".

- [ ] **Step 5: Implement the field, push routing, and threading in `ship.go`**

In `internal/supervisor/ship.go`, replace the `ShipOpts` doc comment + struct (lines ~9-15):

```go
// ShipOpts is the union of PushOpts and PROpts. Without HeadRepo this is a same-repo
// ship: Remote feeds both the push destination and the PR base. With HeadRepo set (a
// fork ship) the push goes to the fork (HeadRepo) and the PR opens on Remote/origin with
// a cross-fork head owned by the fork. Force is push-only; Base/Title/Body/Draft are
// pr-only; As/Step are shared.
type ShipOpts struct {
	Remote, As, Step, Base, Title, Body, HeadRepo string
	Draft, Force                                  bool
}
```

Then in the `Ship` body, route the push to the fork when `HeadRepo` is set and thread `HeadRepo` into `prCore`:

```go
func (s *Supervisor) Ship(ctx context.Context, runID core.RunID, opts ShipOpts) (ShipResult, error) {
	pushRemote := opts.Remote
	if opts.HeadRepo != "" {
		pushRemote = opts.HeadRepo // fork ship: the branch must land on the fork
	}
	pushRes, err := s.Push(ctx, runID, PushOpts{
		Remote: pushRemote, As: opts.As, Step: opts.Step, Force: opts.Force,
	})
	if err != nil {
		return ShipResult{}, err // *PushError; no PR attempted
	}
	prRes, existed, err := s.prCore(ctx, runID, PROpts{
		Remote: opts.Remote, As: opts.As, Step: opts.Step, Base: opts.Base,
		Title: opts.Title, Body: opts.Body, Draft: opts.Draft, HeadRepo: opts.HeadRepo,
	})
	if err != nil {
		return ShipResult{}, err // *PRError; the push already happened
	}
	return ShipResult{Push: pushRes, PR: prRes, PRExisted: existed}, nil
}
```

- [ ] **Step 6: Thread `head_repo` through the DTO + handler**

In `internal/api/dto.go`, add `HeadRepo` to `shipRequest` (after `Body`, before `Draft`):

```go
type shipRequest struct {
	Remote   string `json:"remote,omitempty"`
	As       string `json:"as,omitempty"`
	Step     string `json:"step,omitempty"`
	Base     string `json:"base,omitempty"`
	Title    string `json:"title,omitempty"`
	Body     string `json:"body,omitempty"`
	HeadRepo string `json:"head_repo,omitempty"`
	Draft    bool   `json:"draft,omitempty"`
	Force    bool   `json:"force,omitempty"`
}
```

In `internal/api/handlers.go`, add `HeadRepo: req.HeadRepo` to the `handleShip` `ShipOpts` literal (line ~196):

```go
	res, err := s.Sup.Ship(r.Context(), core.RunID(r.PathValue("id")), supervisor.ShipOpts{
		Remote: req.Remote, As: req.As, Step: req.Step, Base: req.Base,
		Title: req.Title, Body: req.Body, HeadRepo: req.HeadRepo, Draft: req.Draft, Force: req.Force,
	})
```

- [ ] **Step 7: Run the new tests + the existing ship regressions to verify they pass**

Run: `go test ./internal/supervisor/ -run 'TestShip' 2>&1 | tail -20` (sandbox disabled — real git)
Expected: PASS, including the pre-existing `TestShipPushFailsSkipsPR` and `TestShipPushesThenPropagatesPRError` (the same-repo regression — `HeadRepo==""` still pushes to origin then 400s on the local origin base).

Run: `go test ./internal/api/ -run 'TestShip' 2>&1 | tail -20` (sandbox disabled — real git)
Expected: PASS, including the pre-existing `TestShipEndpointPropagatesPRErrorAfterPush`.

- [ ] **Step 8: gofmt + vet, then commit**

Run: `gofmt -l internal/supervisor/ship.go internal/supervisor/ship_test.go internal/api/dto.go internal/api/handlers.go internal/api/handlers_test.go` (expect: no output)
Run: `go vet ./internal/supervisor/ ./internal/api/` (expect: no output)

```bash
git add internal/supervisor/ship.go internal/supervisor/ship_test.go internal/api/dto.go internal/api/handlers.go internal/api/handlers_test.go
git commit -m "feat(ship): cm ship --head-repo pushes to a fork and opens a cross-fork PR"
```

---

## Task 2: Client — `cm ship --head-repo`

**Files:**
- Modify: `cmd/cm/main.go:357-380` (the `ship` parser loop + usage string)
- Test: `cmd/cm/main_test.go` (append one test)

**Interfaces:**
- Consumes: the server's `/v1/runs/{id}/ship` endpoint now accepts a `head_repo` JSON field (Task 1).
- Produces: the `cm ship` CLI accepts `--head-repo <value>` and marshals it as `head_repo`.

- [ ] **Step 1: Write the failing client test**

Append to `cmd/cm/main_test.go` (mirrors `TestShipSendsJSONBodyAndPrintsOpened` at line 256; the file already imports `bytes`, `encoding/json`, `io`, `net/http`, `net/http/httptest`, `strings`, `testing`, and uses the `dispatch` + `writeBody` helpers):

```go
// TestShipHeadRepoSendsHeadRepoJSONField: `cm ship --head-repo <url>` marshals the value
// as the underscore JSON field head_repo (NOT the hyphenated head-repo the generic flag
// path would produce).
func TestShipHeadRepoSendsHeadRepoJSONField(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeBody(w, `{"pushed":{"remote":"r","branch":"magister/r1","source_branch":"step/integrate","commit":"abc"},"pr":{"url":"https://github.com/o/r/pull/4"},"pr_existed":false}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	code := dispatch([]string{"ship", "r1", "--head-repo", "https://github.com/me/fork.git"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if sent["head_repo"] != "https://github.com/me/fork.git" {
		t.Errorf("head_repo = %v, want the fork url", sent["head_repo"])
	}
	if _, hyphen := sent["head-repo"]; hyphen {
		t.Error("must send head_repo (underscore), not head-repo")
	}
}
```

- [ ] **Step 2: Run the client test to verify it fails**

Run: `go test ./cmd/cm/ -run TestShipHeadRepoSendsHeadRepoJSONField -v 2>&1 | tail -20`
Expected: FAIL — `--head-repo` falls through to the `default` case (treated as the run id), so `head_repo` is absent from the body and the run id is wrong; the assertion on `sent["head_repo"]` fails.

- [ ] **Step 3: Add the dedicated `--head-repo` case + update the usage string**

In `cmd/cm/main.go`, in the `ship` parser's `switch args[i]` (after the `case "--force":` block), add:

```go
		case "--head-repo":
			i++
			if i >= len(args) {
				fmt.Fprintln(out, "usage: --head-repo requires a value")
				return 2
			}
			body["head_repo"] = args[i]
```

And update the `ship` usage string to include `--head-repo`:

```go
		fmt.Fprintln(out, "usage: cm ship <run> [--remote <url-or-name>] [--head-repo <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft] [--force]")
```

- [ ] **Step 4: Run the client test to verify it passes**

Run: `go test ./cmd/cm/ -run 'TestShip' 2>&1 | tail -20`
Expected: PASS — the new test plus the pre-existing `TestShipSendsJSONBodyAndPrintsOpened`, `TestShipPrintsExistsWhenPRExisted`, `TestShipRequiresRun` (the parser change is additive; the generic flags still work).

- [ ] **Step 5: gofmt, then commit**

Run: `gofmt -l cmd/cm/main.go cmd/cm/main_test.go` (expect: no output)

```bash
git add cmd/cm/main.go cmd/cm/main_test.go
git commit -m "feat(cm): cm ship --head-repo sends the head_repo field"
```

---

## Final verification (after both tasks)

- [ ] Run the full suite with the sandbox disabled: `go test -race ./... 2>&1 | tail -30` — expect all packages green (the supervisor/api/magisterd real-git e2e tests need the sandbox off).
- [ ] `gofmt -l .` — expect no output.
- [ ] `go vet ./...` — expect no output.
- [ ] Update the running-the-orchestrator skill's `cm ship` surface line + the *External repo* section to mention `--head-repo` (the fork-ship one-command flow), mirroring the `cm pr --head-repo` note. (Doc-only; fold into the Task 2 commit or a follow-up doc commit.)

## Live smoke (post-merge, manual — not a task)

Against a real GitHub fork (`gh` authed; e.g. fork `octocat/Spoon-Knife`): clone upstream as the source; `cm run flows/external-repo.yaml --repo <clone> --base main`; then one `cm ship <run> --head-repo <fork-url>` → pushes `magister/<run>` to the fork **and** opens a real cross-fork PR into upstream. A second `cm ship <run> --head-repo <fork-url>` → idempotent success (`PRExisted=true`, the existing PR URL). Clean up (close PR, delete fork branch).
