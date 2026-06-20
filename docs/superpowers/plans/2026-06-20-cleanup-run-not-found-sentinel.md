# Run-not-found sentinel + deterministic Mock cancel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a typed `core.ErrRunNotFound` so `Store.GetRun`'s four callers return 404 only for a missing run and 500 for a storage error, and make `Mock.Run` honor an already-cancelled context deterministically.

**Architecture:** A sentinel error in `internal/core` wrapped (`%w`) by both `GetRun` implementations on the not-found path only; the four callers branch with `errors.Is`. Independently, hoist `Mock.Run`'s existing `ctx.Err()` guard above the `Delay` select so a cancelled context returns before the nondeterministic select.

**Tech Stack:** Go 1.22, standard library only. Packages `internal/core`, `internal/store`, `internal/api`, `internal/supervisor`, `internal/executor`.

## Global Constraints

- Go 1.22; **stdlib only, NO new dependency** (do not touch `go.mod`); no DB migration; no schema change.
- `core.ErrRunNotFound` lives in `internal/core`; `GetRun` wraps it with `%w` on the not-found path ONLY; the user-visible message stays `unknown run "<id>"`.
- Genuine unknown-run requests still return 404; only genuine storage errors change (404 → 500).
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`. `gofmt -l`, `go vet`, and the relevant `go test -race` clean before each commit.

## File Structure

- `internal/core/store.go` — add `ErrRunNotFound`. (Task 1)
- `internal/store/sqlite.go`, `internal/store/mem.go` — wrap the sentinel in `GetRun`. (Task 1)
- `internal/store/sqlite_test.go`, `internal/store/mem_test.go` — sentinel assertions. (Task 1)
- `internal/api/handlers.go`, `internal/api/sse.go` — 404-vs-500 branch. (Task 2)
- `internal/api/getrun_test.go` (new) — api 404 + 500 tests. (Task 2)
- `internal/supervisor/supervisor.go`, `internal/supervisor/pr.go` — 404-vs-500 branch; delete TODOs. (Task 3)
- `internal/supervisor/push_test.go`, `internal/supervisor/pr_test.go` — storage-error 500 tests. (Task 3)
- `internal/executor/mock.go` — hoist the context guard. (Task 4)

---

### Task 1: `core.ErrRunNotFound` sentinel + `GetRun` wrapping

**Files:**
- Modify: `internal/core/store.go`, `internal/store/sqlite.go`, `internal/store/mem.go`
- Test: `internal/store/sqlite_test.go`, `internal/store/mem_test.go`

**Interfaces:**
- Produces: `var core.ErrRunNotFound error` — returned wrapped by `Store.GetRun` on a missing run; consumed by Tasks 2 and 3 via `errors.Is(err, core.ErrRunNotFound)`.

- [ ] **Step 1: Write the failing tests**

In `internal/store/mem_test.go` (package `store`), add `"errors"` to the import block, then add:

```go
func TestMemGetRunUnknownIsSentinel(t *testing.T) {
	_, err := NewMem().GetRun(context.Background(), "nope")
	if !errors.Is(err, core.ErrRunNotFound) {
		t.Fatalf("GetRun unknown: want errors.Is ErrRunNotFound, got %v", err)
	}
}
```

In `internal/store/sqlite_test.go` (package `store`), add `"errors"` to the import block, then add:

```go
func TestSQLiteGetRunUnknownIsSentinel(t *testing.T) {
	s := tempDB(t)
	_, err := s.GetRun(context.Background(), "nope")
	if !errors.Is(err, core.ErrRunNotFound) {
		t.Fatalf("GetRun unknown: want errors.Is ErrRunNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'GetRunUnknownIsSentinel'`
Expected: compile failure — `undefined: core.ErrRunNotFound`.

- [ ] **Step 3: Add the sentinel to `core`**

In `internal/core/store.go`, add `"errors"` to the import block (currently `context`, `time`, `concentus/internal/event`), then add this top-level declaration immediately after the import block (above the `RunState` type):

```go
// ErrRunNotFound is returned (wrapped) by Store.GetRun when no run with the given
// id exists. Callers use errors.Is to distinguish a missing run (HTTP 404) from a
// storage failure (HTTP 500).
var ErrRunNotFound = errors.New("run not found")
```

- [ ] **Step 4: Wrap the sentinel in both `GetRun` implementations**

In `internal/store/sqlite.go`, in `GetRun`, the `sql.ErrNoRows` branch (currently `return core.RunState{}, fmt.Errorf("unknown run %q", id)`) becomes:

```go
		if errors.Is(err, sql.ErrNoRows) {
			return core.RunState{}, fmt.Errorf("unknown run %q: %w", id, core.ErrRunNotFound)
		}
```

In `internal/store/mem.go`, in `GetRun`, the map-miss branch (currently `return core.RunState{}, fmt.Errorf("unknown run %q", id)`) becomes:

```go
	if !ok {
		return core.RunState{}, fmt.Errorf("unknown run %q: %w", id, core.ErrRunNotFound)
	}
```

(Both files already import `fmt` and `concentus/internal/core`; `sqlite.go` already imports `errors`. No other change to either `GetRun`.)

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/store/ ./internal/core/`
Expected: PASS (the two new sentinel tests, plus all existing store tests — the `var _ core.Store` assertions still compile and the existing `GetRun of unknown id should error` assertion in sqlite_test.go still holds since the wrapped error is still non-nil).

- [ ] **Step 6: Verify formatting and vet**

Run: `gofmt -l internal/core/ internal/store/ && go vet ./internal/core/ ./internal/store/`
Expected: no `gofmt -l` output; `go vet` clean.

- [ ] **Step 7: Commit**

```bash
git add internal/core/store.go internal/store/sqlite.go internal/store/mem.go internal/store/sqlite_test.go internal/store/mem_test.go
git commit -m "feat(core): ErrRunNotFound sentinel for Store.GetRun"
```

---

### Task 2: api handlers distinguish unknown-run (404) from storage error (500)

**Files:**
- Modify: `internal/api/handlers.go` (`handleGetRun`), `internal/api/sse.go` (`handleEvents`)
- Test: `internal/api/getrun_test.go` (new)

**Interfaces:**
- Consumes: `core.ErrRunNotFound` (Task 1).

- [ ] **Step 1: Write the failing tests**

Create `internal/api/getrun_test.go` (package `api`):

```go
package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"concentus/internal/core"
	"concentus/internal/metrics"
	"concentus/internal/store"
)

// getErrStore is a core.Store whose GetRun fails with a non-sentinel (storage)
// error, to drive the 500 path. All other methods come from the embedded Mem.
type getErrStore struct{ *store.Mem }

func (getErrStore) GetRun(context.Context, core.RunID) (core.RunState, error) {
	return core.RunState{}, errors.New("boom")
}

func TestGetRunUnknownIs404(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Get(hs.URL + "/v1/runs/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown run: got %d, want 404", resp.StatusCode)
	}
}

func TestGetRunStorageErrorIs500(t *testing.T) {
	srv := &Server{
		Store:   getErrStore{store.NewMem()},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: metrics.New("test"),
	}
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/v1/runs/anything")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("storage error: got %d, want 500", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/api/ -run 'TestGetRun'`
Expected: `TestGetRunStorageErrorIs500` FAILS with `got 404, want 500` (today every GetRun error maps to 404). `TestGetRunUnknownIs404` passes already.

- [ ] **Step 3: Branch in `handleGetRun`**

In `internal/api/handlers.go`, `handleGetRun`'s error block (currently `if err != nil { writeError(w, http.StatusNotFound, "unknown run"); return }`) becomes (the file already imports `errors` and `concentus/internal/core`):

```go
	rs, err := s.Store.GetRun(r.Context(), core.RunID(r.PathValue("id")))
	if err != nil {
		if errors.Is(err, core.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "unknown run")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
```

- [ ] **Step 4: Branch in `handleEvents`**

In `internal/api/sse.go`, add `"errors"` to the import block, then change the `GetRun` check (currently `if _, err := s.Store.GetRun(r.Context(), id); err != nil { writeError(w, http.StatusNotFound, "unknown run"); return }`) to:

```go
	if _, err := s.Store.GetRun(r.Context(), id); err != nil {
		if errors.Is(err, core.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "unknown run")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/api/`
Expected: PASS — `TestGetRunUnknownIs404`, `TestGetRunStorageErrorIs500`, and all existing api tests (the `/healthz`, `/readyz`, run-lifecycle tests are unaffected).

- [ ] **Step 6: Verify formatting and vet**

Run: `gofmt -l internal/api/ && go vet ./internal/api/`
Expected: no `gofmt -l` output; `go vet` clean.

- [ ] **Step 7: Commit**

```bash
git add internal/api/handlers.go internal/api/sse.go internal/api/getrun_test.go
git commit -m "fix(api): distinguish unknown-run 404 from storage-error 500"
```

---

### Task 3: supervisor Push/PR distinguish unknown-run (404) from storage error (500)

**Files:**
- Modify: `internal/supervisor/supervisor.go` (`Push`), `internal/supervisor/pr.go` (`prCore`)
- Test: `internal/supervisor/push_test.go`, `internal/supervisor/pr_test.go`

**Interfaces:**
- Consumes: `core.ErrRunNotFound` (Task 1); existing `pushErr(status int, format string, a ...any) *PushError`, `prErr(status int, format string, a ...any) *PRError`, and the test helpers `pushErrStatus(t, err) int` / `prErrStatus(t, err) int` / `newPRSup(t, st core.Store) *Supervisor` / `testEngine(t, st core.Store, reg, ws) *engine.Engine`.

- [ ] **Step 1: Write the failing tests**

In `internal/supervisor/push_test.go` (package `supervisor`, already imports `errors`, `context`, `net/http`, `testing`, `core`, `store`, `workspace`), add the shared fake store and a push storage-error test:

```go
// getErrStore is a core.Store whose GetRun fails with a non-sentinel (storage)
// error, to drive the 500 path. Other methods come from the embedded Mem.
type getErrStore struct{ *store.Mem }

func (getErrStore) GetRun(context.Context, core.RunID) (core.RunState, error) {
	return core.RunState{}, errors.New("boom")
}

func TestPushStorageError500(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), getErrStore{st}, reg)
	_, err := sup.Push(context.Background(), "r1", PushOpts{})
	if got := pushErrStatus(t, err); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
}
```

In `internal/supervisor/pr_test.go` (package `supervisor`, already imports `errors`, `context`, `net/http`, `testing`, `store`), add (reusing `getErrStore` defined in push_test.go — same package):

```go
func TestPRStorageError500(t *testing.T) {
	sup := newPRSup(t, getErrStore{store.NewMem()})
	_, err := sup.PR(context.Background(), "r1", PROpts{})
	if got := prErrStatus(t, err); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/supervisor/ -run 'StorageError500'`
Expected: both FAIL with `status = 404, want 500` (today every GetRun error maps to 404).

- [ ] **Step 3: Branch in `Supervisor.Push`**

In `internal/supervisor/supervisor.go`, add `"errors"` to the import block. Replace the `GetRun` error block in `Push` (the current block is the three-line `// TODO: the store has no not-found sentinel …` comment plus `return PushResult{}, pushErr(http.StatusNotFound, "unknown run %q", runID)`) with:

```go
	rs, err := s.store.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, core.ErrRunNotFound) {
			return PushResult{}, pushErr(http.StatusNotFound, "unknown run %q", runID)
		}
		return PushResult{}, pushErr(http.StatusInternalServerError, "load run %q: %v", runID, err)
	}
```

- [ ] **Step 4: Branch in `Supervisor.prCore`**

In `internal/supervisor/pr.go`, add `"errors"` to the import block. Replace the `GetRun` error block in `prCore` (the current block is the one-line `// TODO: no store not-found sentinel …` comment plus `return PRResult{}, false, prErr(http.StatusNotFound, "unknown run %q", runID)`) with:

```go
	rs, err := s.store.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, core.ErrRunNotFound) {
			return PRResult{}, false, prErr(http.StatusNotFound, "unknown run %q", runID)
		}
		return PRResult{}, false, prErr(http.StatusInternalServerError, "load run %q: %v", runID, err)
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/supervisor/`
Expected: PASS — `TestPushStorageError500`, `TestPRStorageError500`, and the existing `TestPushUnknownRun` / `TestPRUnknownRun404` (still 404, since an empty Mem's `GetRun` now returns the sentinel-wrapped error and the new branch maps `errors.Is` → 404).

- [ ] **Step 6: Verify formatting and vet**

Run: `gofmt -l internal/supervisor/ && go vet ./internal/supervisor/`
Expected: no `gofmt -l` output; `go vet` clean.

- [ ] **Step 7: Commit**

```bash
git add internal/supervisor/supervisor.go internal/supervisor/pr.go internal/supervisor/push_test.go internal/supervisor/pr_test.go
git commit -m "fix(supervisor): distinguish unknown-run 404 from storage-error 500 in Push/PR"
```

---

### Task 4: deterministic `Mock.Run` cancellation

**Files:**
- Modify: `internal/executor/mock.go` (`Run`)
- Test: `internal/executor/mock_test.go` (existing `TestMockHonorsContextCancel` — no change; it becomes deterministic)

**Interfaces:**
- Produces: nothing new (terminal task).

- [ ] **Step 1: Characterize the current nondeterminism**

The existing `TestMockHonorsContextCancel` (mock_test.go) cancels the context, then calls `Mock.Run` with `Delay: 5` ns and expects a non-nil error. `Mock.Run` currently `select`s between `time.After(m.Delay)` and `ctx.Done()`; with an already-cancelled context and a 5 ns timer, both cases can be ready and Go picks one at random — a timer-win makes the mock proceed and return `nil`, failing the test.

Run: `go test ./internal/executor/ -run TestMockHonorsContextCancel -race -count=2000`
Expected: usually PASS (the race rarely surfaces locally), but the select-over-two-ready-channels is nondeterministic by construction. Note the count you ran; this is the characterization baseline, not a hard gate.

- [ ] **Step 2: Hoist the context guard above the `Delay` select**

In `internal/executor/mock.go`, the start of `Run` (currently the `if m.Delay > 0 { select { … } } else if err := ctx.Err(); err != nil { return core.Result{}, err }` block) becomes:

```go
func (m Mock) Run(ctx context.Context, t core.Task) (core.Result, error) {
	if err := ctx.Err(); err != nil {
		return core.Result{}, err
	}
	if m.Delay > 0 {
		select {
		case <-time.After(m.Delay):
		case <-ctx.Done():
			return core.Result{}, ctx.Err()
		}
	}

	body := fmt.Sprintf("# %s\nexecutor: %s\nrole: %s\ninputs: %d\n",
		t.StepID, m.Name, t.Role, len(t.Inputs))
```

The rest of `Run` (the `body`/`outPath`/`WriteFile`/return) is unchanged. The former `else if err := ctx.Err()` branch is removed — the top guard now covers the `Delay==0` case too. The in-flight `<-ctx.Done()` case inside the select is kept (it still handles cancellation that arrives during a real delay).

- [ ] **Step 3: Run the test to verify it passes deterministically**

Run: `go test ./internal/executor/ -race -count=2000`
Expected: PASS every iteration (`TestMockHonorsContextCancel` now returns the error before the select; `TestMockWritesArtifact` still green).

- [ ] **Step 4: Run the whole suite + verify formatting/vet**

Run: `go test -race ./... && gofmt -l internal cmd && go vet ./...`
Expected: ALL packages PASS (report the count). No `gofmt -l` output; `go vet` clean.

- [ ] **Step 5: Commit**

```bash
git add internal/executor/mock.go
git commit -m "fix(executor): Mock.Run checks context before delay (deterministic cancel)"
```

---

## Notes for the implementer

- The sentinel must be wrapped with `%w` on the not-found path ONLY. The non-`ErrNoRows` SQLite branch and the `loadSteps` error path must keep returning their raw errors so they surface as 500, not 404.
- `getErrStore` embeds `*store.Mem`, so it inherits every `core.Store` method (including `Ping`, `ReclaimableRuns`, etc.) and overrides only `GetRun`. Define it once per package: once in `internal/api/getrun_test.go`, once in `internal/supervisor/push_test.go` (pr_test.go reuses the supervisor-package copy).
- The existing `TestPushUnknownRun` / `TestPRUnknownRun404` and the sqlite `GetRun of unknown id should error` assertion must stay green — they exercise the not-found path, which still returns a (now-wrapped) non-nil error mapping to 404.
- The post-Edit hook emits a harmless path-doubling error on worktree edits; the edit still succeeds.
