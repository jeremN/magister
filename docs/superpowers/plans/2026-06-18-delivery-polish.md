# Delivery Polish (cm ship + cleanups) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `cm ship <run>` — one command that pushes a succeeded external-repo run's result and opens its PR (idempotent) — plus three carried cleanups.

**Architecture:** Server-side composition. A new `POST /v1/runs/{id}/ship` → `Supervisor.Ship` calls the existing `Push` then a refactored `prCore` (the body of `PR`, reporting created-vs-existing). `cm` stays a thin HTTP client. Idempotency lives in the split: the thin `PR` wrapper converts "already exists" → 409 (so `cm pr` is unchanged), while `Ship` treats it as success.

**Tech Stack:** Go 1.22 stdlib only; `os/exec` (ambient `git`/`gh`); `net/http` (stdlib mux, Go 1.22 method+wildcard routes); `encoding/json`.

## Global Constraints

- Go 1.22; **stdlib-only, no new dependencies** (`go.mod` require block must not grow).
- Zero token handling — credentials are ambient `git`/`gh`; the source repo is read-only.
- GitHub-only host; engine lifecycle untouched (ship is post-run, store-driven).
- `cm` is a thin HTTP client: no orchestration logic in `cmd/cm`.
- `cm pr` behavior is unchanged by this slice (still 409 on an existing PR).
- Commits: a single conventional-commit subject line, **no body**, **no `Co-Authored-By` trailer**, never `--no-verify`.
- `gofmt`, `go vet ./...`, and `go test -race ./...` must be clean before merge.
- Run the WHOLE suite (`go test ./...`) between tasks; `gofmt` is not hook-enforced — run `gofmt -l` yourself.

## File Structure

- `internal/supervisor/pr.go` — **modify**: split `PR` into `prCore` (created-vs-existing) + a thin `PR` wrapper.
- `internal/supervisor/pr_test.go` — **modify**: add a `prCore`-reports-existing test.
- `internal/supervisor/ship.go` — **create**: `ShipOpts`, `ShipResult`, `Ship`.
- `internal/supervisor/ship_test.go` — **create**: composition + error-propagation tests.
- `internal/api/dto.go` — **modify**: `shipRequest`, `shipResponse`.
- `internal/api/handlers.go` — **modify**: `handleShip`.
- `internal/api/router.go` — **modify**: route `POST /v1/runs/{id}/ship`.
- `internal/api/handlers_test.go` — **modify**: ship endpoint error-mapping tests.
- `cmd/cm/main.go` — **modify**: `ship` dispatch + `c.ship`.
- `cmd/cm/main_test.go` — **modify**: `cm ship` body/output tests.
- `internal/host/gh.go` — **modify**: strip `:port` in `ParseRemote`.
- `internal/host/gh_test.go` — **modify**: `:port` table cases.
- `internal/api/middleware.go` — **modify**: 120s bound for delivery routes.
- `internal/api/middleware_test.go` — **modify**: deadline test.
- `internal/executor/gemini.go` — **modify**: `gofmt -w` only.

**Testing note (applies to Tasks 2 & 3):** ship's *fully-happy* path (push AND PR both succeed) is **not unit-testable offline** — `Push` needs a remote that an offline `git push` can reach (a local bare repo), while `prCore` needs a remote that `ParseRemote` accepts (a `github.com` URL), and `Ship` uses ONE shared remote for both. The same impossibility is why push's and pr's network-happy paths were each manual proofs. Offline tests therefore cover: push-fails-skips-pr, push-succeeds-then-pr-error-propagates, the `prCore` idempotency mechanism, the unchanged `PR` 409 wrapper, and `cm ship`'s output formatting. The full happy + idempotent re-run is a **manual proof** (Task 8), exactly like the slice proofs before it.

---

### Task 1: Split `PR` into `prCore` + thin wrapper

**Files:**
- Modify: `internal/supervisor/pr.go` (the `PR` function, currently the method at the top of the file)
- Test: `internal/supervisor/pr_test.go`

**Interfaces:**
- Consumes: existing `PROpts`, `PRResult`, `PRError`, `prErr`, `pickResultStep`, `defaultPRTitle`, `generatePRBody`, `safePRRef`, `s.hostRunner()`, `workspace.ResolveRemote`, `host.ParseRemote`, `host.CreateOpts`.
- Produces: `func (s *Supervisor) prCore(ctx context.Context, runID core.RunID, opts PROpts) (PRResult, bool, error)` — `(res, existed, err)`; the existing-PR case is `(PRResult{URL:…}, true, nil)`. `PR` keeps its exact public signature and behavior (409 on existing). Task 2 (`Ship`) consumes `prCore`.

- [ ] **Step 1: Write the failing test**

Add to `internal/supervisor/pr_test.go`:

```go
func TestPRCoreReportsExistingPR(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	t.Setenv("FAKE_GH_EXISTING_PR", "https://github.com/test-owner/test-repo/pull/8")
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))

	res, existed, err := sup.prCore(context.Background(), "r1", PROpts{})
	if err != nil {
		t.Fatalf("prCore: %v", err)
	}
	if !existed {
		t.Fatal("existed = false, want true")
	}
	if res.URL != "https://github.com/test-owner/test-repo/pull/8" {
		t.Errorf("url = %q, want the existing PR url", res.URL)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/supervisor/ -run TestPRCoreReportsExistingPR`
Expected: FAIL — `sup.prCore undefined` (compile error).

- [ ] **Step 3: Refactor `PR` into `prCore` + wrapper**

In `internal/supervisor/pr.go`, replace the entire `func (s *Supervisor) PR(...)` body with these two functions (keep the existing doc comment above `prCore`, and give `PR` a short wrapper comment):

```go
// prCore does the PR work and reports whether an open PR already existed. On an
// already-existing PR it returns (PRResult{URL:…}, true, nil); on a newly created
// PR (PRResult{URL:…}, false, nil); on failure (PRResult{}, false, *PRError). It is
// the shared core of PR (strict: existing→409) and Ship (idempotent: existing→ok).
func (s *Supervisor) prCore(ctx context.Context, runID core.RunID, opts PROpts) (PRResult, bool, error) {
	rs, err := s.store.GetRun(ctx, runID)
	if err != nil {
		// TODO: no store not-found sentinel; a genuine storage error reads as 404 (as in Push).
		return PRResult{}, false, prErr(http.StatusNotFound, "unknown run %q", runID)
	}
	if rs.Repo == "" {
		return PRResult{}, false, prErr(http.StatusBadRequest, "run %q is not an external-repo run (no --repo)", runID)
	}
	if rs.Status != core.RunSucceeded {
		return PRResult{}, false, prErr(http.StatusConflict, "run %q is %s, not succeeded", runID, rs.Status)
	}
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
	f, err := flow.ParseBytes([]byte(rs.FlowYAML))
	if err != nil {
		return PRResult{}, false, prErr(http.StatusInternalServerError, "parse stored flow: %v", err)
	}
	term, perr := pickResultStep(f, opts.Step)
	if perr != nil {
		return PRResult{}, false, prErr(perr.Status, "%s", perr.Msg)
	}
	title := opts.Title
	if title == "" {
		title = defaultPRTitle(rs)
	}
	body := opts.Body
	if body == "" {
		body = generatePRBody(rs, term)
	}

	runner := s.hostRunner()
	if url, exists, err := runner.ExistingOpenPR(ctx, owner, repo, head); err != nil {
		return PRResult{}, false, prErr(http.StatusBadGateway, "%v", err)
	} else if exists {
		return PRResult{URL: url, Repo: owner + "/" + repo, Head: head, Base: opts.Base, Draft: opts.Draft}, true, nil
	}

	url, err := runner.CreatePR(ctx, host.CreateOpts{
		Owner: owner, Repo: repo, Head: head, Base: opts.Base,
		Title: title, Body: body, Draft: opts.Draft,
	})
	if err != nil {
		if !runner.BranchExists(ctx, owner, repo, head) {
			return PRResult{}, false, prErr(http.StatusConflict, "branch %q not on remote; run `cm push %s` first", head, runID)
		}
		return PRResult{}, false, prErr(http.StatusBadGateway, "%v", err)
	}
	return PRResult{URL: url, Repo: owner + "/" + repo, Head: head, Base: opts.Base, Draft: opts.Draft}, false, nil
}

// PR opens a pull request on the host repo for a succeeded external-repo run. It is
// a post-run, store-driven operation (engine untouched, no scratch clone). An
// already-open PR for the head branch is a 409 carrying its URL. See the slice-3
// spec; Ship reuses prCore for the idempotent variant.
func (s *Supervisor) PR(ctx context.Context, runID core.RunID, opts PROpts) (PRResult, error) {
	res, existed, err := s.prCore(ctx, runID, opts)
	if err != nil {
		return PRResult{}, err
	}
	if existed {
		return PRResult{}, prErr(http.StatusConflict, "PR already exists for %s: %s", res.Head, res.URL)
	}
	return res, nil
}
```

Leave everything else in `pr.go` (the `PROpts`/`PRResult`/`PRError` types, `prErr`, `safePRRef`, `defaultPRTitle`, `generatePRBody`, `shortSHA`, `statusMark`) unchanged.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/supervisor/ -run 'TestPRCoreReportsExistingPR|TestPROpensPullRequest|TestPRExistingOpenPRReturns409|TestPRUnpushedBranchSaysPushFirst|TestPRCreateFailureWithExistingBranchIs502'`
Expected: PASS — the new `prCore` test passes, and the existing `PR` tests still pass (the wrapper preserves the 409 message `PR already exists for magister/r1: …/pull/2`, so `TestPRExistingOpenPRReturns409`'s `pull/2` substring check still holds).

- [ ] **Step 5: Commit**

```bash
git add internal/supervisor/pr.go internal/supervisor/pr_test.go
git commit -m "refactor(supervisor): split PR into prCore reporting created-vs-existing"
```

---

### Task 2: `Supervisor.Ship`

**Files:**
- Create: `internal/supervisor/ship.go`
- Test: `internal/supervisor/ship_test.go`

**Interfaces:**
- Consumes: `Push` (Task-unchanged), `prCore` (Task 1), `PushOpts`, `PROpts`, `PushResult`, `PRResult`.
- Produces:
  - `type ShipOpts struct { Remote, As, Step, Base, Title, Body string; Draft, Force bool }`
  - `type ShipResult struct { Push PushResult; PR PRResult; PRExisted bool }`
  - `func (s *Supervisor) Ship(ctx context.Context, runID core.RunID, opts ShipOpts) (ShipResult, error)` — returns the underlying `*PushError` or `*PRError` on failure. Task 3 (`handleShip`) consumes these.

- [ ] **Step 1: Write the failing tests**

Create `internal/supervisor/ship_test.go`:

```go
package supervisor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/flow"
	"concentus/internal/host"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

// TestShipPushFailsSkipsPR: when Push fails, Ship returns the *PushError and never
// invokes gh (no PR attempted).
func TestShipPushFailsSkipsPR(t *testing.T) {
	st := store.NewMem()
	sup := newPRSup(t, st) // Host = fake-gh stub, plain Manager
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_GH_ARGV_FILE", argv)
	// A running (not succeeded) external-repo run → Push 409 before any PR work.
	if err := st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Repo: "/abs/proj", Status: core.RunRunning,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := sup.Ship(context.Background(), "r1", ShipOpts{})
	var pe *PushError
	if !errors.As(err, &pe) || pe.Status != http.StatusConflict {
		t.Fatalf("want *PushError 409, got %v", err)
	}
	if _, statErr := os.Stat(argv); statErr == nil {
		t.Error("gh must not be invoked when push fails")
	}
}

// TestShipPushesThenPropagatesPRError: a real run + local bare origin → push
// succeeds (the branch lands on the bare), then prCore can't parse the local origin
// as a github remote → Ship returns the *PRError, after the push side-effect.
func TestShipPushesThenPropagatesPRError(t *testing.T) {
	requireGitS(t)
	src, bare, sha := srcWithRemote(t) // origin = bare local repo (not github)
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

	_, err = sup.Ship(context.Background(), id, ShipOpts{})
	if got := prErrStatus(t, err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (local origin not a github remote)", got)
	}
	// The push half must have delivered the branch before the PR step failed.
	if sha := gitS(t, bare, "rev-parse", "--verify", "magister/"+string(id)); sha == "" {
		t.Error("push should have delivered magister/<run> before pr failed")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/supervisor/ -run 'TestShip'`
Expected: FAIL — `Ship`/`ShipOpts` undefined (compile error).

- [ ] **Step 3: Write `ship.go`**

Create `internal/supervisor/ship.go`:

```go
package supervisor

import (
	"context"

	"concentus/internal/core"
)

// ShipOpts is the union of PushOpts and PROpts. Shared fields (Remote/As/Step) feed
// both operations, so the push destination and the PR head branch can never disagree.
// Force is push-only; Base/Title/Body/Draft are pr-only.
type ShipOpts struct {
	Remote, As, Step, Base, Title, Body string
	Draft, Force                        bool
}

// ShipResult bundles the push outcome, the PR outcome, and whether the PR already
// existed (idempotent re-run).
type ShipResult struct {
	Push      PushResult
	PR        PRResult
	PRExisted bool
}

// Ship pushes a succeeded external-repo run's result branch, then ensures a PR exists
// for it. Push runs first (it needs the scratch clone); an already-open PR is success
// (PRExisted=true), so ship is safe to re-run and converges. On failure it returns the
// underlying *PushError (push half) or *PRError (pr half), which the API maps via
// errors.As. Post-run and store-driven; the engine is untouched.
func (s *Supervisor) Ship(ctx context.Context, runID core.RunID, opts ShipOpts) (ShipResult, error) {
	pushRes, err := s.Push(ctx, runID, PushOpts{
		Remote: opts.Remote, As: opts.As, Step: opts.Step, Force: opts.Force,
	})
	if err != nil {
		return ShipResult{}, err // *PushError; no PR attempted
	}
	prRes, existed, err := s.prCore(ctx, runID, PROpts{
		Remote: opts.Remote, As: opts.As, Step: opts.Step, Base: opts.Base,
		Title: opts.Title, Body: opts.Body, Draft: opts.Draft,
	})
	if err != nil {
		return ShipResult{}, err // *PRError; the push already happened
	}
	return ShipResult{Push: pushRes, PR: prRes, PRExisted: existed}, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/supervisor/ -run 'TestShip'`
Expected: PASS — both ship tests pass.

- [ ] **Step 5: Run the supervisor package to confirm nothing regressed**

Run: `go test ./internal/supervisor/`
Expected: PASS (all tests).

- [ ] **Step 6: Commit**

```bash
git add internal/supervisor/ship.go internal/supervisor/ship_test.go
git commit -m "feat(supervisor): Ship pushes then opens a PR (idempotent)"
```

---

### Task 3: `POST /v1/runs/{id}/ship` endpoint

**Files:**
- Modify: `internal/api/dto.go` (add `shipRequest`, `shipResponse` after `pushResponse`, ~line 74)
- Modify: `internal/api/handlers.go` (add `handleShip`; reuse `decodeJSON`)
- Modify: `internal/api/router.go` (register the route after the `/pr` line)
- Test: `internal/api/handlers_test.go`

**Interfaces:**
- Consumes: `supervisor.ShipOpts`, `supervisor.ShipResult`, `supervisor.PushError`, `supervisor.PRError` (Task 2), `s.Sup.Ship`, `decodeJSON`, `writeJSON`, `writeError`, existing `pushResponse`/`prResponse`.
- Produces: route `POST /v1/runs/{id}/ship`; `shipResponse{pushed, pr, pr_existed}`. Task 4 (`cm ship`) consumes the response shape.

- [ ] **Step 1: Write the failing tests**

Add to `internal/api/handlers_test.go`:

```go
func TestShipEndpointUnknownRun404(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs/nope/ship", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestShipEndpointNonExternal400(t *testing.T) {
	hs, _, st := testServer(t)
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Status: core.RunSucceeded,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n",
	})
	resp, err := http.Post(hs.URL+"/v1/runs/r1/ship", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestShipEndpointPropagatesPRErrorAfterPush: a real external-repo run with a local
// bare origin → /ship pushes (branch lands on the bare) then the PR step fails to
// parse the local origin as github → 400 from the *PRError, proving both the push
// side-effect and the *PRError mapping path.
func TestShipEndpointPropagatesPRErrorAfterPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src, _ := setupAPISourceRepo(t)
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")
	runGit(t, src, "remote", "add", "origin", bare)

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

	sresp, err := http.Post(hs.URL+"/v1/runs/"+string(rr.ID)+"/ship", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	if sresp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(sresp.Body)
		t.Fatalf("ship = %d, want 400 (pr parse of local origin): %s", sresp.StatusCode, b)
	}
	if sha := runGit(t, bare, "rev-parse", "--verify", "magister/"+string(rr.ID)); sha == "" {
		t.Error("push should have delivered magister/<run> before the pr step failed")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/api/ -run 'TestShipEndpoint'`
Expected: FAIL — route not registered → 404 for the non-external case / handler `handleShip` undefined (compile error first).

- [ ] **Step 3: Add the DTOs**

In `internal/api/dto.go`, after the `pushResponse` struct (ends ~line 74), add:

```go
// shipRequest is the JSON body of POST /v1/runs/{id}/ship: the union of pr's and
// push's options. All fields optional.
type shipRequest struct {
	Remote string `json:"remote,omitempty"`
	As     string `json:"as,omitempty"`
	Step   string `json:"step,omitempty"`
	Base   string `json:"base,omitempty"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
	Force  bool   `json:"force,omitempty"`
}

// shipResponse is returned from POST /v1/runs/{id}/ship.
type shipResponse struct {
	Pushed    pushResponse `json:"pushed"`
	PR        prResponse   `json:"pr"`
	PRExisted bool         `json:"pr_existed"`
}
```

- [ ] **Step 4: Add the handler**

In `internal/api/handlers.go`, after `handlePR` (the function ends ~line 170, just before `decodeJSON` at line 173), add:

```go
func (s *Server) handleShip(w http.ResponseWriter, r *http.Request) {
	var req shipRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.Sup.Ship(r.Context(), core.RunID(r.PathValue("id")), supervisor.ShipOpts{
		Remote: req.Remote, As: req.As, Step: req.Step, Base: req.Base,
		Title: req.Title, Body: req.Body, Draft: req.Draft, Force: req.Force,
	})
	if err != nil {
		var pushE *supervisor.PushError
		if errors.As(err, &pushE) {
			writeError(w, pushE.Status, pushE.Msg)
			return
		}
		var prE *supervisor.PRError
		if errors.As(err, &prE) {
			writeError(w, prE.Status, prE.Msg)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, shipResponse{
		Pushed: pushResponse{
			Remote:       res.Push.Remote,
			Branch:       res.Push.Branch,
			SourceBranch: res.Push.SourceBranch,
			Commit:       res.Push.Commit,
		},
		PR:        prResponse{URL: res.PR.URL, Repo: res.PR.Repo, Head: res.PR.Head, Base: res.PR.Base, Draft: res.PR.Draft},
		PRExisted: res.PRExisted,
	})
}
```

(The file already imports `errors`, `net/http`, `concentus/internal/core`, and `concentus/internal/supervisor` — handlePush/handlePR use all four.)

- [ ] **Step 5: Register the route**

In `internal/api/router.go`, after the `/pr` line (line 25), add:

```go
	v1.HandleFunc("POST /v1/runs/{id}/ship", s.handleShip)
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/api/ -run 'TestShipEndpoint'`
Expected: PASS — all three ship endpoint tests.

- [ ] **Step 7: Run the api package to confirm nothing regressed**

Run: `go test ./internal/api/`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/api/dto.go internal/api/handlers.go internal/api/router.go internal/api/handlers_test.go
git commit -m "feat(api): POST /v1/runs/{id}/ship pushes and opens a PR"
```

---

### Task 4: `cm ship` CLI

**Files:**
- Modify: `cmd/cm/main.go` (add `ship` to `dispatch` + the usage line; add `c.ship`)
- Test: `cmd/cm/main_test.go`

**Interfaces:**
- Consumes: the `/v1/runs/{id}/ship` endpoint and `shipResponse` shape (Task 3); existing `client`, `printErr`.
- Produces: `cm ship <run> [--remote --as --step --base --title --body --draft --force]`.

- [ ] **Step 1: Write the failing tests**

Add to `cmd/cm/main_test.go`:

```go
func TestShipSendsJSONBodyAndPrintsOpened(t *testing.T) {
	var got http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeBody(w, `{"pushed":{"remote":"git@h:o/r.git","branch":"magister/r1","source_branch":"step/integrate","commit":"abc"},"pr":{"url":"https://github.com/o/r/pull/4"},"pr_existed":false}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	code := dispatch([]string{"ship", "r1", "--as", "feature/x", "--force", "--title", "Hi"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v1/runs/r1/ship" {
		t.Errorf("request = %s %s, want POST /v1/runs/r1/ship", got.Method, got.URL.Path)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if sent["as"] != "feature/x" || sent["force"] != true || sent["title"] != "Hi" {
		t.Errorf("body = %v, want as/force/title set", sent)
	}
	s := out.String()
	if !strings.Contains(s, "step/integrate") || !strings.Contains(s, "magister/r1") {
		t.Errorf("output missing pushed line: %q", s)
	}
	if !strings.Contains(s, "opened https://github.com/o/r/pull/4") {
		t.Errorf("output missing opened line: %q", s)
	}
}

func TestShipPrintsExistsWhenPRExisted(t *testing.T) {
	var got http.Request
	srv := fakeAPI(t, http.StatusOK,
		`{"pushed":{"remote":"r","branch":"magister/r1","source_branch":"step/integrate","commit":"abc"},"pr":{"url":"https://github.com/o/r/pull/9"},"pr_existed":true}`, &got)
	defer srv.Close()
	var out bytes.Buffer
	if code := dispatch([]string{"ship", "r1"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "exists https://github.com/o/r/pull/9") {
		t.Errorf("output should say 'exists' when pr_existed, got %q", out.String())
	}
}

func TestShipRequiresRun(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"ship"}, "http://x", &out); code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/cm/ -run 'TestShip'`
Expected: FAIL — `ship` falls through to the `unknown command` default (exit 2 for the first two; the require-run test would coincidentally pass, but the happy tests fail).

- [ ] **Step 3: Add the dispatch case + usage**

In `cmd/cm/main.go`, change the usage line (line 33) to include `ship`:

```go
		fmt.Fprintln(out, "usage: cm <run|ls|get|watch|approve|reject|cancel|push|pr|ship> ...")
```

And add a case after the `"pr"` case (line 67):

```go
	case "ship":
		return c.ship(args[1:], out)
```

- [ ] **Step 4: Add `c.ship`**

In `cmd/cm/main.go`, after `c.pr` (ends ~line 344), add:

```go
func (c *client) ship(args []string, out io.Writer) int {
	var run string
	body := map[string]any{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--draft":
			body["draft"] = true
		case "--force":
			body["force"] = true
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
	if run == "" {
		fmt.Fprintln(out, "usage: cm ship <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft] [--force]")
		return 2
	}
	payload, _ := json.Marshal(body)
	resp, err := c.http.Post(c.base+"/v1/runs/"+run+"/ship", "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintln(out, "ship:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	var sr struct {
		Pushed struct {
			Remote       string `json:"remote"`
			Branch       string `json:"branch"`
			SourceBranch string `json:"source_branch"`
		} `json:"pushed"`
		PR struct {
			URL string `json:"url"`
		} `json:"pr"`
		PRExisted bool `json:"pr_existed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		fmt.Fprintln(out, "ship: decode response:", err)
		return 1
	}
	fmt.Fprintf(out, "pushed %s → %s on %s\n", sr.Pushed.SourceBranch, sr.Pushed.Branch, sr.Pushed.Remote)
	verb := "opened"
	if sr.PRExisted {
		verb = "exists"
	}
	fmt.Fprintln(out, verb, sr.PR.URL)
	return 0
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./cmd/cm/ -run 'TestShip'`
Expected: PASS — all three ship CLI tests.

- [ ] **Step 6: Commit**

```bash
git add cmd/cm/main.go cmd/cm/main_test.go
git commit -m "feat(cm): ship <run> pushes and opens a PR in one command"
```

---

### Task 5: `ParseRemote` strips an explicit `:port`

**Files:**
- Modify: `internal/host/gh.go` (the `://` branch of `ParseRemote`, ~line 32)
- Test: `internal/host/gh_test.go`

**Interfaces:**
- Consumes/Produces: `ParseRemote(remoteURL) (host, owner, repo string, err error)` — unchanged signature; a github URL with an explicit `:port` now resolves instead of failing closed.

- [ ] **Step 1: Write the failing test**

Add to `internal/host/gh_test.go`:

```go
func TestParseRemoteStripsPort(t *testing.T) {
	cases := []struct {
		url, owner, repo string
		ok               bool
	}{
		{"ssh://git@github.com:22/test-owner/test-repo.git", "test-owner", "test-repo", true},
		{"https://github.com:443/o/r.git", "o", "r", true},
		{"ssh://git@gitlab.com:22/o/r.git", "", "", false}, // other host + port still rejected
	}
	for _, c := range cases {
		_, owner, repo, err := ParseRemote(c.url)
		if c.ok {
			if err != nil || owner != c.owner || repo != c.repo {
				t.Errorf("ParseRemote(%q) = %q/%q/%v, want %q/%q/nil", c.url, owner, repo, err, c.owner, c.repo)
			}
		} else if err == nil {
			t.Errorf("ParseRemote(%q) = nil error, want unsupported-host error", c.url)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/host/ -run TestParseRemoteStripsPort`
Expected: FAIL — `ssh://git@github.com:22/...` currently yields host `github.com:22` → "unsupported host" error, so the first case fails.

- [ ] **Step 3: Strip the port in the `://` branch**

In `internal/host/gh.go`, in the `case strings.Contains(s, "://")` branch, replace:

```go
		host = rest[:slash]
		owner, repo, err = splitOwnerRepo(rest[slash+1:])
```

with:

```go
		host = rest[:slash]
		if c := strings.IndexByte(host, ':'); c >= 0 {
			host = host[:c] // drop an explicit :port (e.g. ssh://git@github.com:22/o/r)
		}
		owner, repo, err = splitOwnerRepo(rest[slash+1:])
```

(Do not touch the scp-like branch — its colon separates host from path, not a port.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/host/`
Expected: PASS — the new test passes and all existing `ParseRemote` cases still pass.

- [ ] **Step 5: Commit**

```bash
git add internal/host/gh.go internal/host/gh_test.go
git commit -m "fix(host): ParseRemote strips an explicit :port from the github host"
```

---

### Task 6: 120s request-timeout bound for delivery routes

**Files:**
- Modify: `internal/api/middleware.go` (`timeoutMiddleware`, ~line 95)
- Test: `internal/api/middleware_test.go`

**Interfaces:**
- Consumes/Produces: `timeoutMiddleware(d time.Duration)` — same signature; `/push`, `/pr`, `/ship` now get a 120s bound instead of `d`; `/events` stays exempt; everything else keeps `d`.

- [ ] **Step 1: Write the failing test**

Add to `internal/api/middleware_test.go`:

```go
func TestTimeoutMiddlewareDeliveryRoutesGetLongerBound(t *testing.T) {
	var remaining time.Duration
	h := timeoutMiddleware(30*time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dl, ok := r.Context().Deadline(); ok {
			remaining = time.Until(dl)
		} else {
			remaining = -1 // no deadline (exempt)
		}
	}))
	check := func(method, path string) time.Duration {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
		return remaining
	}

	if d := check(http.MethodPost, "/v1/runs/r1/ship"); d < 60*time.Second {
		t.Errorf("ship deadline = %v, want ~120s", d)
	}
	if d := check(http.MethodPost, "/v1/runs/r1/push"); d < 60*time.Second {
		t.Errorf("push deadline = %v, want ~120s", d)
	}
	if d := check(http.MethodGet, "/v1/runs/r1"); d > 31*time.Second {
		t.Errorf("normal deadline = %v, want ~30s", d)
	}
	if d := check(http.MethodGet, "/v1/runs/r1/events"); d != -1 {
		t.Errorf("events should be exempt (no deadline), got %v", d)
	}
}
```

(`internal/api/middleware_test.go` is package `api`; ensure its imports include `net/http`, `net/http/httptest`, and `time` — add any that are missing.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api/ -run TestTimeoutMiddlewareDeliveryRoutesGetLongerBound`
Expected: FAIL — `/ship` currently gets the 30s bound, so `d < 60s` fails.

- [ ] **Step 3: Add the delivery-route bound**

In `internal/api/middleware.go`, replace the body of `timeoutMiddleware`'s inner handler (the part after the `/events` exemption) so it reads:

```go
func timeoutMiddleware(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// SSE streams must not be force-timed-out; they manage their own lifetime.
			if strings.HasSuffix(r.URL.Path, "/events") {
				next.ServeHTTP(w, r)
				return
			}
			timeout := d
			// Delivery operations shell out to git/gh over the network; give them a
			// longer bound than ordinary requests, but still bound them.
			if p := r.URL.Path; strings.HasSuffix(p, "/push") || strings.HasSuffix(p, "/pr") || strings.HasSuffix(p, "/ship") {
				timeout = 120 * time.Second
			}
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/api/ -run 'TestTimeout'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/middleware.go internal/api/middleware_test.go
git commit -m "fix(api): give push/pr/ship a 120s timeout bound, not the 30s default"
```

---

### Task 7: gofmt `gemini.go`

**Files:**
- Modify: `internal/executor/gemini.go` (formatting only — no behavior change)

**Interfaces:** none.

- [ ] **Step 1: Confirm the file is currently unformatted**

Run: `gofmt -l internal/executor/gemini.go`
Expected: prints `internal/executor/gemini.go` (it is dirty on main).

- [ ] **Step 2: Format it**

Run: `gofmt -w internal/executor/gemini.go`

- [ ] **Step 3: Verify it is now clean and the package still builds**

Run: `gofmt -l internal/executor/gemini.go && go build ./internal/executor/`
Expected: empty `gofmt -l` output; build succeeds.

- [ ] **Step 4: Commit**

```bash
git add internal/executor/gemini.go
git commit -m "style(executor): gofmt gemini.go"
```

---

### Task 8: Manual proof (human-run; not a subagent task)

The ship fully-happy + idempotent re-run is not unit-testable offline (shared-remote constraint). Verify it live against the throwaway repo `jeremN/site_test` (`gh` authed), driven by the `running-the-orchestrator` skill — daemon launched **sandbox-disabled**:

1. Clone `site_test`, build `magisterd`+`cm`, start the daemon on a throwaway db/port.
2. `cm run flows/external-repo.yaml --repo <clone> --base HEAD` → `cm get` shows `succeeded`.
3. `cm ship <run> --title "ship smoke"` → prints `pushed step/integrate → magister/<run> on …` then `opened https://github.com/jeremN/site_test/pull/N`.
4. `cm ship <run>` **again** → prints `pushed …` then `exists https://…/pull/N`, exit 0 (idempotent).
5. Confirm on GitHub the PR exists once; clean up (close PR, delete branch) afterward.

This task has no commit; it gates "done" for the slice.

---

## Self-Review

**1. Spec coverage** (against `docs/superpowers/specs/2026-06-18-delivery-polish-design.md`):
- `cm ship` server endpoint composing Push + prCore → Tasks 1–4. ✓
- Idempotent ship (existing PR → success) → Task 1 (`prCore` existed) + Task 2 (`Ship` returns `PRExisted`) + Task 4 (`exists` output); full live path → Task 8. ✓
- `cm pr` stays strict (409) → Task 1 wrapper + its preserved tests. ✓
- Flag union, shared remote/as/step feed both → `ShipOpts`→`PushOpts`/`PROpts` mapping in Task 2. ✓
- `ParseRemote` `:port` → Task 5. ✓
- 120s delivery-route timeout (not blanket exempt) → Task 6. ✓
- `gemini.go` gofmt → Task 7. ✓
- Error/status mapping (both `*PushError` and `*PRError`) → Task 3 handler + tests. ✓

**2. Placeholder scan:** No TBD/TODO except the one pre-existing `// TODO: no store not-found sentinel` carried verbatim from the current `PR`/`Push` (out of scope). Every code step has complete code. ✓

**3. Type consistency:** `prCore` returns `(PRResult, bool, error)` and is called identically in the `PR` wrapper (Task 1) and `Ship` (Task 2). `ShipOpts` fields map to `PushOpts{Remote,As,Step,Force}` and `PROpts{Remote,As,Step,Base,Title,Body,Draft}` — names match the existing structs (verified against `supervisor.go`/`pr.go`). `shipResponse` field names (`pushed`/`pr`/`pr_existed`) match what `cm ship` decodes (Task 4). `pushResponse`/`prResponse` reused verbatim. ✓
