# Retry / Resume a Failed Run — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `cm retry <run>` + `POST /v1/runs/{id}/retry` that resumes a `failed` or `canceled` run in place — same run id, reusing its preserved scratch, skipping already-succeeded steps.

**Architecture:** A new `Supervisor.Retry` reuses the engine's existing seed-based resume (`engine.Resume` → `runDAG`, which already skips `StepSucceeded` steps). The reset→provision→start(Resume) sequence currently inline in `ResumeAll` is extracted into a shared `resumeRun` helper used by both. Guards (reject-if-active, terminal-status, pre-flight scratch presence) wrap the resume; a status-flip-to-`pending` precedes the scratch check so the GC janitor can't reclaim mid-retry. Surface mirrors the existing `Push`/`Cancel` patterns (`RetryError` like `PushError`, a `cm` subcommand like `cm push`).

**Tech Stack:** Go 1.22 stdlib (`net/http`, `os`, `errors`); existing `internal/{supervisor,engine,api,core,flow,workspace,store,event}`; `cmd/cm`. No new dependencies.

## Global Constraints

- **No new dependencies.** Stdlib only. `RetryError` mirrors the existing `PushError`/`PRError`; no new modules, no `go.mod` change.
- **`go 1.22`** unchanged. Pinned deps untouched: modernc.org/sqlite v1.36.1, pressly/goose/v3 v3.24.1, oklog/ulid/v2 v2.1.1, expr-lang/expr v1.17.8, OTel v1.32.0.
- **Persist-then-publish** preserved — retry mutates persisted run/step status through existing store methods (`SetRunStatus`, `resetIncompleteSteps`) before `runDAG` publishes events.
- **Same run id** for an in-place resume (no new ULID). The run keeps its own id; new events append to its stream.
- **`409`** (not `410`) for a reclaimed scratch, matching the existing handlers' status vocabulary.
- Commit hygiene: single conventional-commit subject lines, **no body**, **no `Co-Authored-By` trailer**, never `--no-verify`. Run `gofmt -w` on touched files (not hook-enforced; verify with `gofmt -l`).
- Real-git tests guard with `requireGitS(t)` / `exec.LookPath("git")` and run sandbox-disabled.

---

## File Structure

- `internal/supervisor/supervisor.go` (modify) — extract the `resumeRun` helper; rewire `ResumeAll` to call it.
- `internal/supervisor/retry.go` (create) — `RetryError` + `retryErr` helper + `Supervisor.Retry`.
- `internal/supervisor/retry_test.go` (create) — supervisor-layer tests (reject paths, scratch-reclaimed revert, fail-then-pass skip-succeeded integration, canceled resume).
- `internal/api/handlers.go` (modify) — add `handleRetry`.
- `internal/api/router.go` (modify) — register `POST /v1/runs/{id}/retry`.
- `internal/api/retry_test.go` (create) — endpoint wiring tests (404, 409, 202 happy path).
- `cmd/cm/main.go` (modify) — `retry` dispatch case + `c.retry` method + usage line.
- `cmd/cm/retry_test.go` (create) — client subcommand tests (POST shape, `--watch`, usage).
- `.claude/skills/running-the-orchestrator/SKILL.md` (modify) — document `cm retry` + add it to the command surface.

---

## Task 1: Extract the shared `resumeRun` helper (refactor)

Behavior-preserving refactor: pull the reset → provision → start(Resume) sequence out of `ResumeAll` into a private `resumeRun` method, so Task 2's `Retry` can reuse the exact same resume path. No new behavior, so the gate is the existing supervisor suite staying green (TDD's red step is N/A for a pure refactor).

**Files:**
- Modify: `internal/supervisor/supervisor.go` (the `ResumeAll` method at lines 157-183)

**Interfaces:**
- Consumes: `s.resetIncompleteSteps(ctx, rs)` (existing, supervisor.go:143), `s.engine.Provision(id, repo, base) error` (existing), `s.start(ctx, id, func(context.Context) error)` (existing), `s.engine.Resume(ctx, rs, f) error` (existing, engine.go:86).
- Produces: `func (s *Supervisor) resumeRun(ctx context.Context, rs core.RunState, f *flow.Flow) error` — resets non-succeeded steps, provisions, and starts `engine.Resume` under `rs.ID`; returns a non-nil error only when provisioning fails. Task 2 calls this.

- [ ] **Step 1: Add the `resumeRun` helper**

In `internal/supervisor/supervisor.go`, immediately after the `ResumeAll` method (after line 183), add:

```go
// resumeRun resets the run's non-succeeded steps to pending, re-provisions its
// scratch spec, and starts engine.Resume under the run's own id. Shared by
// ResumeAll (startup reconciliation) and Retry (on-demand), so the two resume
// paths cannot drift. Returns a non-nil error only when provisioning fails; the
// caller decides whether that is fatal.
func (s *Supervisor) resumeRun(ctx context.Context, rs core.RunState, f *flow.Flow) error {
	s.resetIncompleteSteps(ctx, rs)
	if err := s.engine.Provision(rs.ID, rs.Repo, rs.Base); err != nil {
		return fmt.Errorf("provision run: %w", err)
	}
	s.start(ctx, rs.ID, func(runCtx context.Context) error { return s.engine.Resume(runCtx, rs, f) })
	return nil
}
```

- [ ] **Step 2: Rewire `ResumeAll` to use the helper**

Replace the body of the `for _, rs := range runs` loop in `ResumeAll` (currently supervisor.go:164-181). The full method becomes:

```go
// ResumeAll loads incomplete runs from the store and resumes each (startup). A run
// with an unparseable/invalid flow is skipped (logged), not fatal to the others.
func (s *Supervisor) ResumeAll(ctx context.Context) error {
	runs, err := s.store.LoadIncompleteRuns(ctx)
	if err != nil {
		return fmt.Errorf("load incomplete runs: %w", err)
	}
	for _, rs := range runs {
		f, err := flow.ParseBytes([]byte(rs.FlowYAML))
		if err != nil {
			s.logger().Error("resume: skip run with unparseable flow", "run", rs.ID, "err", err)
			continue
		}
		if err := flow.Validate(f); err != nil {
			s.logger().Error("resume: skip run with invalid flow", "run", rs.ID, "err", err)
			continue
		}
		if err := s.resumeRun(context.Background(), rs, f); err != nil {
			s.logger().Error("resume: provision run", "run", rs.ID, "err", err)
			continue
		}
	}
	return nil
}
```

(Note: `ResumeAll` keeps passing `context.Background()` so a resumed run outlives any request — unchanged from today. The `rs := rs` line is gone: `rs` is passed by value into `resumeRun`, whose closure captures its own parameter copy, so there is no loop-aliasing bug.)

- [ ] **Step 3: Run the existing supervisor suite (expect PASS — refactor preserves behavior)**

Run: `go test ./internal/supervisor/ -run 'TestResumeAll|TestResetIncompleteSteps|TestSupervisor' -count=1`
Expected: PASS (TestResumeAllProvisions, TestResumeAllContinuesPastCorruptFlow, TestResetIncompleteStepsToPending, TestSupervisorSubmitRunsToCompletion all green — they exercise `resumeRun` via `ResumeAll`).

- [ ] **Step 4: gofmt + commit**

```bash
gofmt -w internal/supervisor/supervisor.go
git add internal/supervisor/supervisor.go
git commit -m "refactor(supervisor): extract resumeRun shared by ResumeAll"
```

---

## Task 2: `Supervisor.Retry` + `RetryError`

The core feature: a supervisor method that resumes a terminal run in place, behind the ordered guards from the spec.

**Files:**
- Create: `internal/supervisor/retry.go`
- Create: `internal/supervisor/retry_test.go`

**Interfaces:**
- Consumes: `s.runs` map + `s.mu` (Supervisor fields, supervisor.go:44-45), `s.store.GetRun(ctx, id) (core.RunState, error)`, `core.ErrRunNotFound` (core/store.go:14), `s.store.SetRunStatus(ctx, id, status, errMsg) error`, `flow.ParseBytes`/`flow.Validate`, `s.engine.BasePath(id) string` (engine.go:64), `dirHasGit(dir string) bool` (supervisor.go:330), `s.resumeRun(ctx, rs, f) error` (Task 1), `s.logger()`. Run statuses `core.RunFailed/RunCanceled/RunSucceeded/RunPending` (core/state.go).
- Produces: `type RetryError struct { Status int; Msg string }` with `Error()`; `func (s *Supervisor) Retry(ctx context.Context, runID core.RunID) (core.RunID, error)` returning the same id on success and a `*RetryError` on failure. Tasks 3 use these.

- [ ] **Step 1: Write the failing tests**

Create `internal/supervisor/retry_test.go`. (`requireGitS`, `gitS`, `autoStepYAML`, `waitForStatus`, `waitFor`, `testEngine` are in-package from `push_test.go`/`gc_test.go`/`supervisor_test.go`.)

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
	"concentus/internal/event"
	"concentus/internal/flow"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

func retryErrStatus(t *testing.T, err error) int {
	t.Helper()
	var re *RetryError
	if !errors.As(err, &re) {
		t.Fatalf("error %v is not a *RetryError", err)
	}
	return re.Status
}

func mustFlow(t *testing.T, yaml string) *flow.Flow {
	t.Helper()
	f, err := flow.ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("parse flow: %v", err)
	}
	return f
}

func stepStatus(rs core.RunState, id string) core.StepStatus {
	for _, s := range rs.Steps {
		if s.StepID == id {
			return s.Status
		}
	}
	return ""
}

// countStarts returns how many step.started events each of steps a and b recorded.
func countStarts(t *testing.T, st core.Store, id core.RunID) (a, b int) {
	t.Helper()
	evs, err := st.EventsSince(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind != event.StepStarted {
			continue
		}
		switch e.StepID {
		case "a":
			a++
		case "b":
			b++
		}
	}
	return a, b
}

func TestRetryUnknownRun404(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	_, err := sup.Retry(context.Background(), "nope")
	if got := retryErrStatus(t, err); got != http.StatusNotFound {
		t.Errorf("status = %d, want 404", got)
	}
}

func TestRetryRejectsSucceeded(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	if err := st.CreateRun(ctx, core.RunState{ID: "r1", Name: "f", FlowYAML: autoStepYAML, Status: core.RunSucceeded}); err != nil {
		t.Fatal(err)
	}
	_, err := sup.Retry(ctx, "r1")
	if got := retryErrStatus(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409", got)
	}
}

func TestRetryRejectsActiveRun(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	// A manual gate blocks, so the run stays active (registered in s.runs).
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	id, err := sup.Submit(context.Background(), f, "name: f\n", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = sup.Retry(context.Background(), id)
	if got := retryErrStatus(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409 (active)", got)
	}
}

func TestRetryScratchReclaimedReverts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	gm := &workspace.GitManager{Root: root}
	sup := New(testEngine(t, st, reg, gm), st, reg)
	// Seed a terminal run whose scratch was never created (as if GC-reclaimed):
	// BasePath(root/r1/base) does not exist, so dirHasGit is false.
	if err := st.CreateRun(ctx, core.RunState{ID: "r1", Name: "f", FlowYAML: autoStepYAML, Status: core.RunFailed, Err: "boom"}); err != nil {
		t.Fatal(err)
	}
	_, err := sup.Retry(ctx, "r1")
	if got := retryErrStatus(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409 (reclaimed)", got)
	}
	rs, _ := st.GetRun(ctx, "r1")
	if rs.Status != core.RunFailed || rs.Err != "boom" {
		t.Errorf("status/err = %s/%q, want failed/boom (fully reverted)", rs.Status, rs.Err)
	}
}

func TestRetryResumesSkippingSucceeded(t *testing.T) {
	requireGitS(t)
	ctx := context.Background()
	root := t.TempDir()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	gm := &workspace.GitManager{Root: root}
	sup := New(testEngine(t, st, reg, gm), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })

	// Step a always passes; step b's gate passes only once `flag` exists.
	flag := filepath.Join(t.TempDir(), "ok")
	yaml := "name: f\nsteps:\n" +
		"  - id: a\n    agent: mock\n    workspace: isolated\n    gate: { policy: auto, verifier: { command: \"true\" } }\n" +
		"  - id: b\n    agent: mock\n    workspace: isolated\n    gate: { policy: auto, verifier: { command: \"test -f " + flag + "\" } }\n"

	id, err := sup.Submit(ctx, mustFlow(t, yaml), yaml, "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, st, id, core.RunFailed) // b's gate fails (flag absent) → run fails

	rs, _ := st.GetRun(ctx, id)
	if stepStatus(rs, "a") != core.StepSucceeded {
		t.Fatalf("step a = %s, want succeeded", stepStatus(rs, "a"))
	}
	if stepStatus(rs, "b") != core.StepFailed {
		t.Fatalf("step b = %s, want failed", stepStatus(rs, "b"))
	}

	// Fix the condition and retry in place.
	if err := os.WriteFile(flag, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sup.Retry(ctx, id)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if got != id {
		t.Errorf("Retry returned id %q, want the same id %q", got, id)
	}
	waitForStatus(t, st, id, core.RunSucceeded)

	// Proof of skip: a started once (seeded on retry); b started twice (orig + retry).
	a, b := countStarts(t, st, id)
	if a != 1 {
		t.Errorf("step a started %d times, want 1 (skipped on retry)", a)
	}
	if b != 2 {
		t.Errorf("step b started %d times, want 2 (original + retry)", b)
	}
}

func TestRetryResumesCanceledRun(t *testing.T) {
	requireGitS(t)
	ctx := context.Background()
	root := t.TempDir()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	gm := &workspace.GitManager{Root: root}
	sup := New(testEngine(t, st, reg, gm), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })

	yaml := "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: manual }\n"
	id, err := sup.Submit(ctx, mustFlow(t, yaml), yaml, "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return sup.Cancel(id) })
	waitForStatus(t, st, id, core.RunCanceled)

	got, err := sup.Retry(ctx, id)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if got != id {
		t.Errorf("Retry returned id %q, want %q", got, id)
	}
	// The manual gate blocks again on resume; approve to finish.
	waitFor(t, func() bool { return sup.Approve(id, "a", true, "") })
	waitForStatus(t, st, id, core.RunSucceeded)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/supervisor/ -run TestRetry -count=1`
Expected: FAIL — `sup.Retry` undefined / `RetryError` undefined (compile error).

- [ ] **Step 3: Implement `Retry` + `RetryError`**

Create `internal/supervisor/retry.go`:

```go
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// RetryError carries an HTTP status so the API layer maps failures without
// string-matching (mirrors PushError/PRError).
type RetryError struct {
	Status int
	Msg    string
}

func (e *RetryError) Error() string { return e.Msg }

func retryErr(status int, format string, a ...any) *RetryError {
	return &RetryError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// Retry resumes a failed or canceled run in place: it keeps the run's own id and
// reuses its preserved scratch, skipping already-succeeded steps and re-running
// from the failed step onward (engine.Resume). The guard ordering is load-bearing:
// the run is flipped out of its terminal status (step 5) before the scratch is
// checked (step 6) so the scratch GC — which only selects terminal runs — cannot
// reclaim it mid-retry. Errors are *RetryError with an HTTP status.
func (s *Supervisor) Retry(ctx context.Context, runID core.RunID) (core.RunID, error) {
	// 1. reject if the run is still active (running or unwinding).
	s.mu.Lock()
	_, active := s.runs[runID]
	s.mu.Unlock()
	if active {
		return "", retryErr(http.StatusConflict, "run %q still in progress", runID)
	}
	// 2. load.
	rs, err := s.store.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, core.ErrRunNotFound) {
			return "", retryErr(http.StatusNotFound, "unknown run %q", runID)
		}
		return "", retryErr(http.StatusInternalServerError, "load run %q: %v", runID, err)
	}
	// 3. only failed/canceled runs are resumable.
	switch rs.Status {
	case core.RunFailed, core.RunCanceled:
		// resumable
	case core.RunSucceeded:
		return "", retryErr(http.StatusConflict, "run %q succeeded; nothing to retry", runID)
	default: // pending/running persisted but not in the active map
		return "", retryErr(http.StatusConflict, "run %q still in progress", runID)
	}
	// 4. re-parse + validate the stored flow (it validated at submit, so a failure
	//    here is corrupt persisted state).
	f, err := flow.ParseBytes([]byte(rs.FlowYAML))
	if err != nil {
		return "", retryErr(http.StatusInternalServerError, "stored flow no longer parses: %v", err)
	}
	if err := flow.Validate(f); err != nil {
		return "", retryErr(http.StatusInternalServerError, "stored flow no longer valid: %v", err)
	}
	// 5. flip out of the terminal state first so the scratch GC can't reclaim it
	//    between the check below and the resume.
	if err := s.store.SetRunStatus(ctx, runID, core.RunPending, ""); err != nil {
		return "", retryErr(http.StatusInternalServerError, "reset run status: %v", err)
	}
	// 6. pre-flight scratch presence (same check Push uses). If gone, fully revert
	//    the run to its original terminal status + error and reject.
	base := s.engine.BasePath(runID)
	if base == "" || !dirHasGit(base) {
		if err := s.store.SetRunStatus(ctx, runID, rs.Status, rs.Err); err != nil {
			s.logger().Error("retry: revert status after reclaimed scratch", "run", runID, "err", err)
		}
		return "", retryErr(http.StatusConflict, "scratch for run %q reclaimed; resubmit the flow", runID)
	}
	// 7. resume in place (reset non-succeeded steps, re-provision, start Resume).
	if err := s.resumeRun(ctx, rs, f); err != nil {
		if rerr := s.store.SetRunStatus(ctx, runID, rs.Status, rs.Err); rerr != nil {
			s.logger().Error("retry: revert status after provision failure", "run", runID, "err", rerr)
		}
		return "", retryErr(http.StatusInternalServerError, "%v", err)
	}
	return runID, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/supervisor/ -run TestRetry -count=1` (with git on PATH so the git-backed tests don't skip)
Expected: PASS (all six TestRetry* tests).

- [ ] **Step 5: Run the full supervisor package with the race detector**

Run: `go test -race ./internal/supervisor/ -count=1`
Expected: PASS (the refactor + new code race-clean; existing tests still green).

- [ ] **Step 6: gofmt + commit**

```bash
gofmt -w internal/supervisor/retry.go internal/supervisor/retry_test.go
git add internal/supervisor/retry.go internal/supervisor/retry_test.go
git commit -m "feat(supervisor): Retry resumes a failed/canceled run in place"
```

---

## Task 3: `POST /v1/runs/{id}/retry` endpoint

Expose `Retry` over HTTP, mapping `RetryError` to its status exactly like `handlePush`.

**Files:**
- Modify: `internal/api/handlers.go` (add `handleRetry` after `handleCancelRun`, ~line 123)
- Modify: `internal/api/router.go` (add a route in the `v1` block, ~line 23)
- Create: `internal/api/retry_test.go`

**Interfaces:**
- Consumes: `s.Sup.Retry(ctx, id) (core.RunID, error)` (Task 2), `*supervisor.RetryError` (Task 2), `writeError`/`writeJSON` (existing), `runResponse{ID core.RunID }` (dto.go:8), `testServer(t) (*httptest.Server, *supervisor.Supervisor, core.Store)` + `newGitServer(t) (*httptest.Server, core.Store)` + `waitForStatus` (handlers_test.go).
- Produces: `POST /v1/runs/{id}/retry` → `202` + `{"id":"<runID>"}`, or `RetryError.Status` + message.

- [ ] **Step 1: Write the failing tests**

Create `internal/api/retry_test.go`:

```go
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"concentus/internal/core"
)

func TestRetryEndpointUnknownRun404(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs/nope/retry", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRetryEndpointSucceeded409(t *testing.T) {
	hs, _, st := testServer(t)
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Status: core.RunSucceeded,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n",
	})
	resp, err := http.Post(hs.URL+"/v1/runs/r1/retry", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestRetryEndpointResumesFailedRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	hs, st := newGitServer(t)
	flag := filepath.Join(t.TempDir(), "ok")
	yaml := "name: f\nsteps:\n  - id: a\n    agent: mock\n    workspace: isolated\n    gate: { policy: auto, verifier: { command: \"test -f " + flag + "\" } }\n"

	resp, err := http.Post(hs.URL+"/v1/runs", "application/x-yaml", strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()
	waitForStatus(t, st, rr.ID, core.RunFailed)

	if err := os.WriteFile(flag, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rresp, err := http.Post(hs.URL+"/v1/runs/"+string(rr.ID)+"/retry", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rresp.Body.Close()
	if rresp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(rresp.Body)
		t.Fatalf("retry = %d, want 202: %s", rresp.StatusCode, b)
	}
	var got runResponse
	json.NewDecoder(rresp.Body).Decode(&got)
	if got.ID != rr.ID {
		t.Errorf("retry id = %q, want the same id %q", got.ID, rr.ID)
	}
	waitForStatus(t, st, rr.ID, core.RunSucceeded)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/api/ -run TestRetryEndpoint -count=1`
Expected: FAIL — the route is unregistered, so `POST /v1/runs/.../retry` returns `405`/`404` (not the expected statuses) and the handler is undefined.

- [ ] **Step 3: Add the handler**

In `internal/api/handlers.go`, after `handleCancelRun` (line 123), add:

```go
func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	id, err := s.Sup.Retry(r.Context(), core.RunID(r.PathValue("id")))
	if err != nil {
		var re *supervisor.RetryError
		if errors.As(err, &re) {
			writeError(w, re.Status, re.Msg)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, runResponse{ID: id})
}
```

(`errors`, `supervisor`, and `core` are already imported in handlers.go.)

- [ ] **Step 4: Register the route**

In `internal/api/router.go`, in the `v1` mux block (after line 23, `DELETE /v1/runs/{id}`), add:

```go
	v1.HandleFunc("POST /v1/runs/{id}/retry", s.handleRetry)
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/api/ -run TestRetryEndpoint -count=1`
Expected: PASS (404, 409, and the git-backed 202 happy path).

- [ ] **Step 6: gofmt + commit**

```bash
gofmt -w internal/api/handlers.go internal/api/router.go internal/api/retry_test.go
git add internal/api/handlers.go internal/api/router.go internal/api/retry_test.go
git commit -m "feat(api): POST /v1/runs/{id}/retry resumes a terminal run"
```

---

## Task 4: `cm retry [--watch]` client + docs

Add the client subcommand and document it.

**Files:**
- Modify: `cmd/cm/main.go` (dispatch switch ~line 37; usage line ~line 33; add `c.retry` method)
- Create: `cmd/cm/retry_test.go`
- Modify: `.claude/skills/running-the-orchestrator/SKILL.md`

**Interfaces:**
- Consumes: `dispatch(args, base, out)`, `client{base, http}`, `c.watch(id, out)` (main.go:146), `printErr` (main.go:451).
- Produces: `cm retry <run> [--watch]` → POSTs `/v1/runs/{run}/retry`, prints `resuming <id>`, and streams events when `--watch`.

- [ ] **Step 1: Write the failing tests**

Create `cmd/cm/retry_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRetrySubcommandPostsRetry(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"id": "01ABC"})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"retry", "01ABC"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, want 0; out=%s", code, out.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/runs/01ABC/retry" {
		t.Errorf("request = %s %s, want POST /v1/runs/01ABC/retry", gotMethod, gotPath)
	}
	if !strings.Contains(out.String(), "resuming 01ABC") {
		t.Errorf("output = %q, want it to contain 'resuming 01ABC'", out.String())
	}
}

func TestRetrySubcommandWatchStreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/01ABC/retry":
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"id": "01ABC"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/01ABC/events":
			io.WriteString(w, "event: run.done\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"retry", "01ABC", "--watch"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, want 0; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "run.done") {
		t.Errorf("watch output = %q, want streamed events", out.String())
	}
}

func TestRetrySubcommandUsage(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"retry"}, "http://x", &out); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestRetrySubcommandServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"error":"run \"x\" succeeded; nothing to retry"}`)
	}))
	defer srv.Close()
	var out bytes.Buffer
	if code := dispatch([]string{"retry", "x"}, srv.URL, &out); code != 1 {
		t.Errorf("exit = %d, want 1 (server 409)", code)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/cm/ -run TestRetry -count=1`
Expected: FAIL — `retry` is an unknown command (`dispatch` returns 2 with "unknown command", so the POST assertions fail).

- [ ] **Step 3: Add the dispatch case + usage line**

In `cmd/cm/main.go`, update the no-args usage line (line 33) to include `retry`:

```go
		fmt.Fprintln(out, "usage: cm <run|ls|get|watch|approve|reject|cancel|retry|push|pr|ship|loglevel> ...")
```

Add a case in the `switch args[0]` block (after the `cancel` case, line 63):

```go
	case "retry":
		return c.retry(args[1:], out)
```

- [ ] **Step 4: Implement `c.retry`**

In `cmd/cm/main.go`, add the method (e.g. after `c.delete`, line 229):

```go
func (c *client) retry(args []string, out io.Writer) int {
	watch := false
	var run string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--watch":
			watch = true
		default:
			run = args[i]
		}
	}
	if run == "" {
		fmt.Fprintln(out, "usage: cm retry <run> [--watch]")
		return 2
	}
	resp, err := c.http.Post(c.base+"/v1/runs/"+run+"/retry", "application/json", nil)
	if err != nil {
		fmt.Fprintln(out, "retry:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return printErr(resp, out)
	}
	var rr struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&rr)
	fmt.Fprintln(out, "resuming", rr.ID)
	if watch {
		return c.watch(rr.ID, out)
	}
	return 0
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./cmd/cm/ -run TestRetry -count=1`
Expected: PASS (POST shape, `--watch` streaming, usage, server-error paths).

- [ ] **Step 6: Document `cm retry` in the skill**

In `.claude/skills/running-the-orchestrator/SKILL.md`:

1. Add a subsection after the *External repo* section's `cm pr` block:

```markdown
### Retry / resume a failed run (`cm retry`)

A `failed` or `canceled` run can be resumed **in place** — same run id, reusing
its preserved scratch — so already-succeeded steps are not re-run:

```
cm retry <run> [--watch]
```

`cm retry` re-runs the run from its failed step onward (succeeded steps are
seeded from their persisted artifacts and skipped) via `POST /v1/runs/{id}/retry`.
It rejects a `succeeded` run (`409`, nothing to retry), an in-progress run (`409`),
an unknown run (`404`), and a run whose scratch was already reclaimed by the GC
janitor (`409` — resubmit the flow). `--watch` streams the resumed run's events
until `run.done`, like `cm run --watch`.
```

2. Update the **cm command surface** line at the end of the file to include `retry`:

```markdown
`cm run <flow.yaml> [--repo <abs-path>] [--base <ref>] [--watch]` · `cm ls` · `cm get <run>` · `cm watch <run>` · `cm approve|reject <run> <step> [reason]` · `cm cancel <run>` · `cm retry <run> [--watch]` · `cm push <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--force]` · `cm pr <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft]`. All target `$MAGISTER_ADDR`.
```

- [ ] **Step 7: gofmt + full suite + commit**

```bash
gofmt -w cmd/cm/main.go cmd/cm/retry_test.go
go test -race ./... 2>&1 | tail -20   # expect ok across all packages
git add cmd/cm/main.go cmd/cm/retry_test.go .claude/skills/running-the-orchestrator/SKILL.md
git commit -m "feat(cm): cm retry resumes a failed run, with --watch"
```

---

## Self-Review

**1. Spec coverage:**
- In-place resume, same id, skip succeeded → Task 2 `Retry` + `TestRetryResumesSkippingSucceeded`. ✓
- `failed` and `canceled` accepted → Task 2 status guard + `TestRetryResumesCanceledRun`. ✓
- Reject succeeded / active / unknown → Task 2 tests. ✓
- Status-flip-first then scratch check; reclaimed → revert to original terminal status + `409` → Task 2 step 3 (steps 5-6) + `TestRetryScratchReclaimedReverts`. ✓
- Shared `resumeRun` (no drift) → Task 1. ✓
- `POST /v1/runs/{id}/retry`, `RetryError` like `PushError`, `202` + `{"id"}` → Task 3. ✓
- `cm retry <run> [--watch]`, docs → Task 4. ✓
- Pre-flight scratch guard via the existing `BasePath`+`dirHasGit` (the spec's `engine.ScratchExists` is realized through the established Push pattern instead of a new method — same guard, less surface, noted here as the one deliberate refinement). ✓

**2. Placeholder scan:** No TBD/TODO; every code step shows complete code; every test step shows real assertions and exact run commands. ✓

**3. Type consistency:** `RetryError{Status int; Msg string}` + `retryErr(...)` defined in Task 2 and consumed in Task 3 (`*supervisor.RetryError`). `Retry(ctx, runID) (core.RunID, error)` signature consistent across Tasks 2-3. `resumeRun(ctx, rs, f) error` defined in Task 1, consumed in Task 2. `runResponse{ID core.RunID}` (dto.go) reused by the handler. Client decodes `{"id"}` matching `runResponse`'s `json:"id"` tag. Status consts (`RunFailed/RunCanceled/RunSucceeded/RunPending`, `StepSucceeded/StepFailed`) and `event.StepStarted` match the source. ✓
