# Scratch-GC On-Demand Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `reclaimed_at` marker so reclaimed runs are never re-selected by the scratch sweep, plus on-demand `cm gc` (reclaim all terminal scratch now) and `cm rm <run>` (reclaim one run's scratch now).

**Architecture:** Stays store-driven, reusing the existing `SweepScratch → ReclaimableRuns → ReclaimScratch → Workspace.Reclaim` chain. A new shared `reclaimOne` helper does reclaim-then-mark-on-success; `SweepScratch` (bulk) and a new `ReclaimRun` (single) both call it. The marker is a nullable `reclaimed_at` column excluded by the `ReclaimableRuns` query. Two new auth-protected endpoints (`POST /v1/gc`, `DELETE /v1/runs/{id}/scratch`) surface the operations to the `cm` client.

**Tech Stack:** Go 1.22, stdlib `net/http` (ServeMux 1.22 patterns), modernc.org/sqlite (no cgo) via hand-edited sqlc-style generated code, goose migrations, `slog`.

## Global Constraints

- **No new dependencies.** Stdlib only. `ReclaimError` mirrors `PushError`/`RetryError`. No `go.mod` change.
- **`go 1.22`** unchanged. Pinned deps untouched: modernc.org/sqlite v1.36.1, pressly/goose/v3 v3.24.1, oklog/ulid/v2 v2.1.1, expr-lang/expr v1.17.8, OTel v1.32.0.
- **One DB migration** (`0004_run_reclaimed_at.sql`), additive + nullable, with a goose down. `sqlc` is NOT installed — hand-edit the generated `internal/store/sqldb/query.sql.go` (established pattern: migration 0003).
- Commit hygiene: single conventional-commit subject lines, no body, no `Co-Authored-By` trailer, never `--no-verify`. `gofmt -l` must be clean.
- `cm rm <run>` reclaims the **scratch workspace only, not the run record** — the run row, events, and artifacts persist.

---

### Task 1: Store layer — `reclaimed_at` marker

**Files:**
- Create: `internal/store/migrations/0004_run_reclaimed_at.sql`
- Modify: `internal/store/query.sql` (edit `ReclaimableRuns`; add `MarkReclaimed`)
- Modify: `internal/store/sqldb/query.sql.go` (hand-edit: edit `reclaimableRuns` const; add `markReclaimed` const + method)
- Modify: `internal/store/sqlite.go` (add `MarkReclaimed` wrapper)
- Modify: `internal/store/mem.go` (add `reclaimed` set; `MarkReclaimed`; exclude in `ReclaimableRuns`)
- Modify: `internal/core/store.go` (add `MarkReclaimed` to the `Store` interface)
- Test: `internal/store/mem_test.go`, `internal/store/sqlite_test.go`

**Interfaces:**
- Produces: `core.Store.MarkReclaimed(ctx context.Context, id core.RunID) error` — marks a run's scratch reclaimed so `ReclaimableRuns` never returns it again; idempotent; the run must exist (callers always hold a loaded run). `ReclaimableRuns` now additionally excludes any run whose `reclaimed_at` is set.
- Note: `MarkReclaimed` on a missing run — `Mem` returns an "unknown run" error (consistent with `SetRunStatus`/`SaveStepTransition` in that file); SQLite's `UPDATE ... WHERE id=?` is a benign no-op. This path is never hit in production (callers pass a loaded run); the interface contract is "the run must exist".

- [ ] **Step 1: Write the failing Mem test**

Add to `internal/store/mem_test.go`:

```go
func TestMemMarkReclaimedExcludesFromReclaimable(t *testing.T) {
	st := NewMem()
	ctx := context.Background()
	if err := st.CreateRun(ctx, core.RunState{ID: "done", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunStatus(ctx, "done", core.RunSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	got, err := st.ReclaimableRuns(ctx, future)
	if err != nil {
		t.Fatal(err)
	}
	if !sameRunIDSet(got, []core.RunID{"done"}) {
		t.Fatalf("before mark = %v, want [done]", got)
	}
	if err := st.MarkReclaimed(ctx, "done"); err != nil {
		t.Fatal(err)
	}
	got, err = st.ReclaimableRuns(ctx, future)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("after mark = %v, want none", got)
	}
	// Idempotent: a second mark is a no-op, not an error.
	if err := st.MarkReclaimed(ctx, "done"); err != nil {
		t.Errorf("second MarkReclaimed: %v", err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails (compile error — method missing)**

Run: `go test ./internal/store/ -run TestMemMarkReclaimedExcludesFromReclaimable`
Expected: FAIL — `st.MarkReclaimed undefined (type *Mem has no field or method MarkReclaimed)`.

- [ ] **Step 3: Add the `Store` interface method**

In `internal/core/store.go`, inside the `Store` interface, immediately after the `ReclaimableRuns` method (before `Ping`), add:

```go
	// MarkReclaimed records that a run's scratch has been reclaimed, so
	// ReclaimableRuns never selects it again. Idempotent. The run must exist.
	MarkReclaimed(ctx context.Context, id RunID) error
```

- [ ] **Step 4: Implement `Mem.MarkReclaimed` and exclude in `ReclaimableRuns`**

In `internal/store/mem.go`, add a field to the `Mem` struct (after `updatedAt`):

```go
	reclaimed map[core.RunID]bool
```

Initialise it in `NewMem` (add to the returned literal, after `updatedAt`):

```go
		reclaimed: make(map[core.RunID]bool),
```

Add the method (place it just above `ReclaimableRuns`):

```go
func (m *Mem) MarkReclaimed(_ context.Context, id core.RunID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[id]; !ok {
		return fmt.Errorf("unknown run %q", id)
	}
	m.reclaimed[id] = true
	return nil
}
```

Edit `ReclaimableRuns` to skip reclaimed runs — change its loop body's guard:

```go
	for id, r := range m.runs {
		if !isTerminal(r.Status) || m.reclaimed[id] {
			continue
		}
		if u, ok := m.updatedAt[id]; ok && u.Before(before) {
			out = append(out, id)
		}
	}
```

- [ ] **Step 5: Run the Mem test to verify it passes**

Run: `go test ./internal/store/ -run TestMemMarkReclaimedExcludesFromReclaimable`
Expected: PASS.

- [ ] **Step 6: Write the failing SQLite test**

Add to `internal/store/sqlite_test.go`:

```go
func TestSQLiteMarkReclaimedExcludesFromReclaimable(t *testing.T) {
	st := tempDB(t)
	ctx := context.Background()
	if err := st.CreateRun(ctx, core.RunState{ID: "done", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunStatus(ctx, "done", core.RunSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	got, err := st.ReclaimableRuns(ctx, future)
	if err != nil {
		t.Fatal(err)
	}
	if !sameRunIDSet(got, []core.RunID{"done"}) {
		t.Fatalf("before mark = %v, want [done]", got)
	}
	if err := st.MarkReclaimed(ctx, "done"); err != nil {
		t.Fatal(err)
	}
	got, err = st.ReclaimableRuns(ctx, future)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("after mark = %v, want none", got)
	}
	if err := st.MarkReclaimed(ctx, "done"); err != nil {
		t.Errorf("second MarkReclaimed: %v", err)
	}
}
```

- [ ] **Step 7: Run it to verify it fails**

Run: `go test ./internal/store/ -run TestSQLiteMarkReclaimedExcludesFromReclaimable`
Expected: FAIL — `st.MarkReclaimed undefined (type *SQLite has no field or method MarkReclaimed)`.

- [ ] **Step 8: Create the migration**

Create `internal/store/migrations/0004_run_reclaimed_at.sql` (same form as 0003):

```sql
-- +goose Up
-- +goose StatementBegin
ALTER TABLE runs ADD COLUMN reclaimed_at TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE runs DROP COLUMN reclaimed_at;
-- +goose StatementEnd
```

- [ ] **Step 9: Edit `query.sql` (source of truth for the generated code)**

In `internal/store/query.sql`, replace the `ReclaimableRuns` query:

```sql
-- name: ReclaimableRuns :many
SELECT id FROM runs WHERE status IN ('succeeded', 'failed', 'canceled') AND reclaimed_at IS NULL AND updated_at < ? ORDER BY updated_at;
```

And add, immediately after it:

```sql
-- name: MarkReclaimed :exec
UPDATE runs SET reclaimed_at = datetime('now') WHERE id = ?;
```

- [ ] **Step 10: Hand-edit the generated `query.sql.go`**

In `internal/store/sqldb/query.sql.go`:

(a) Edit the `reclaimableRuns` const to match — add `AND reclaimed_at IS NULL`:

```go
const reclaimableRuns = `-- name: ReclaimableRuns :many
SELECT id FROM runs WHERE status IN ('succeeded', 'failed', 'canceled') AND reclaimed_at IS NULL AND updated_at < ? ORDER BY updated_at
`
```

(b) Add the `markReclaimed` const + method (place after the `ReclaimableRuns` func), mirroring the `SetRunStatus` `:exec` shape:

```go
const markReclaimed = `-- name: MarkReclaimed :exec
UPDATE runs SET reclaimed_at = datetime('now') WHERE id = ?
`

func (q *Queries) MarkReclaimed(ctx context.Context, id string) error {
	_, err := q.db.ExecContext(ctx, markReclaimed, id)
	return err
}
```

- [ ] **Step 11: Add the `SQLite.MarkReclaimed` wrapper**

In `internal/store/sqlite.go`, add (place just after `ReclaimableRuns`, mirroring `SetRunStatus`'s use of `s.qw`):

```go
func (s *SQLite) MarkReclaimed(ctx context.Context, id core.RunID) error {
	return s.qw.MarkReclaimed(ctx, string(id))
}
```

- [ ] **Step 12: Run the store tests to verify they pass**

Run: `go test ./internal/store/ -run 'MarkReclaimed|ReclaimableRuns'`
Expected: PASS (`TestMemMarkReclaimedExcludesFromReclaimable`, `TestSQLiteMarkReclaimedExcludesFromReclaimable`, `TestMemReclaimableRuns`, `TestSQLiteReclaimableRuns`). The SQLite test passing also proves migration 0004 applied (the `reclaimed_at` column exists).

- [ ] **Step 13: Run the full store package under race + gofmt**

Run: `go test -race ./internal/store/ && gofmt -l internal/store/ internal/core/`
Expected: PASS, and `gofmt -l` prints nothing.

- [ ] **Step 14: Commit**

```bash
git add internal/store/migrations/0004_run_reclaimed_at.sql internal/store/query.sql internal/store/sqldb/query.sql.go internal/store/sqlite.go internal/store/mem.go internal/core/store.go internal/store/mem_test.go internal/store/sqlite_test.go
git commit -m "feat(store): add reclaimed_at marker + MarkReclaimed"
```

---

### Task 2: Supervisor reclaim flow — `reclaimOne` + `ReclaimRun`

**Files:**
- Modify: `internal/supervisor/gc.go` (refactor `SweepScratch`; add `reclaimOne`, `ReclaimRun`, `ReclaimError`, `reclaimErr`)
- Test: `internal/supervisor/gc_test.go`

**Interfaces:**
- Consumes: `core.Store.MarkReclaimed(ctx, id) error` (Task 1); existing `s.engine.ReclaimScratch(id) (bool, error)`, `s.store.GetRun`, `s.store.ReclaimableRuns`, `s.runs`/`s.mu`.
- Produces:
  - `Supervisor.ReclaimRun(ctx context.Context, runID core.RunID) (bool, error)` — reclaims one terminal run's scratch on demand; returns whether a dir was actually removed; errors are `*supervisor.ReclaimError{Status int, Msg string}`.
  - `ReclaimError struct { Status int; Msg string }` with `Error() string`.
  - `SweepScratch` keeps its signature `(ctx, olderThan time.Time) (int, error)` and now marks each reclaimed run.

- [ ] **Step 1: Write the failing `SweepScratch`-marks test**

Add to `internal/supervisor/gc_test.go`:

```go
func TestSweepScratchMarksReclaimed(t *testing.T) {
	root := t.TempDir()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	gm := &workspace.GitManager{Root: root}
	sup := New(testEngine(t, st, reg, gm), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	ctx := context.Background()

	if err := st.CreateRun(ctx, core.RunState{ID: "done", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunStatus(ctx, "done", core.RunSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "done", "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.SweepScratch(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The reclaimed run is now marked, so the store no longer selects it.
	left, err := st.ReclaimableRuns(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("after sweep, ReclaimableRuns = %v, want none (run marked)", left)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/supervisor/ -run TestSweepScratchMarksReclaimed`
Expected: FAIL — `after sweep, ReclaimableRuns = [done], want none` (SweepScratch does not yet mark).

- [ ] **Step 3: Rewrite `gc.go` with the shared `reclaimOne` + `ReclaimRun`**

Replace the entire contents of `internal/supervisor/gc.go` with:

```go
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"concentus/internal/core"
)

// ReclaimError carries an HTTP status so the API layer maps failures without
// string-matching (mirrors PushError/RetryError).
type ReclaimError struct {
	Status int
	Msg    string
}

func (e *ReclaimError) Error() string { return e.Msg }

func reclaimErr(status int, format string, a ...any) *ReclaimError {
	return &ReclaimError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// reclaimOne removes a single run's scratch and, on success, marks it reclaimed so
// the store never selects it again. "Success" means the workspace returned no error
// — whether it deleted the directory (removed==true) or found it already gone
// (removed==false); both mean the scratch is gone, so both should stop future
// selection. A reclaim error leaves the run unmarked so the next sweep retries. A
// MarkReclaimed error is non-fatal (the dir is already gone) — logged, not returned.
// Shared by SweepScratch and ReclaimRun so the mark-on-success rule lives in one
// place.
func (s *Supervisor) reclaimOne(ctx context.Context, id core.RunID) (bool, error) {
	removed, err := s.engine.ReclaimScratch(id)
	if err != nil {
		return false, err
	}
	if merr := s.store.MarkReclaimed(ctx, id); merr != nil {
		s.logger().Error("mark reclaimed", "run", id, "err", merr)
	}
	return removed, nil
}

// SweepScratch reclaims the scratch directory of every terminal, not-yet-reclaimed
// run whose last update is before olderThan. It is best-effort: a single run's
// reclaim failure is logged and the sweep continues. Returns the number of runs
// whose scratch was ACTUALLY removed. Each reclaimed run is marked, so subsequent
// sweeps no longer select it — steady state queries zero rows. A non-nil error means
// the store query failed (nothing was swept). The caller supplies the cutoff (e.g.
// time.Now().Add(-ttl)) so the sweep needs no clock of its own.
func (s *Supervisor) SweepScratch(ctx context.Context, olderThan time.Time) (int, error) {
	ids, err := s.store.ReclaimableRuns(ctx, olderThan)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		removed, err := s.reclaimOne(ctx, id)
		if err != nil {
			s.logger().Error("scratch reclaim", "run", id, "err", err)
			continue
		}
		if removed {
			n++
		}
	}
	return n, nil
}

// ReclaimRun reclaims a single run's scratch on demand (cm rm), independent of any
// TTL. Guards mirror Retry/Push: an active run (still in s.runs — running or being
// retried) → 409; an unknown run → 404; a non-terminal persisted run → 409. On
// success it returns whether a directory was actually removed (false when the
// scratch was already gone — idempotent). Errors are *ReclaimError with an HTTP
// status.
//
// The active-check is a point-in-time read of s.runs, not a reservation: a Retry
// that registers the run AFTER this check could have its fresh scratch removed
// mid-resume. That window is the same at-least-once edge the background janitor
// already carries (it selects a terminal run that is then retried); the reverse
// order is caught by Retry's own scratch pre-flight. Accepted and recoverable
// (resubmit).
func (s *Supervisor) ReclaimRun(ctx context.Context, runID core.RunID) (bool, error) {
	s.mu.Lock()
	_, active := s.runs[runID]
	s.mu.Unlock()
	if active {
		return false, reclaimErr(http.StatusConflict, "run %q in progress; cannot reclaim its scratch", runID)
	}
	rs, err := s.store.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, core.ErrRunNotFound) {
			return false, reclaimErr(http.StatusNotFound, "unknown run %q", runID)
		}
		return false, reclaimErr(http.StatusInternalServerError, "load run %q: %v", runID, err)
	}
	switch rs.Status {
	case core.RunSucceeded, core.RunFailed, core.RunCanceled:
		// terminal — reclaimable
	default:
		return false, reclaimErr(http.StatusConflict, "run %q is %s, not terminal", runID, rs.Status)
	}
	removed, err := s.reclaimOne(ctx, runID)
	if err != nil {
		return false, reclaimErr(http.StatusInternalServerError, "reclaim scratch: %v", err)
	}
	return removed, nil
}
```

- [ ] **Step 4: Run the marks test to verify it passes**

Run: `go test ./internal/supervisor/ -run TestSweepScratchMarksReclaimed`
Expected: PASS.

- [ ] **Step 5: Fix the now-stale comment in the existing sweep test**

In `internal/supervisor/gc_test.go`, `TestSweepScratchReclaimsTerminalAgedRuns`, the second-sweep block's comment describes the OLD (pre-marker) behavior. Replace the comment above the second `sup.SweepScratch` call so it reads:

```go
	// Each reclaimed run is now marked, so the store no longer selects it: the
	// second sweep queries zero rows and reports 0 reclaimed.
```

(The assertion `n != 0 → want 0` is unchanged and still correct.)

- [ ] **Step 6: Write the failing `ReclaimRun` tests**

Add to `internal/supervisor/gc_test.go` (the package already imports `context`, `os`, `path/filepath`, `testing`, `time`, `core`, `store`, `workspace`; add `"errors"` and `"net/http"` to the import block):

```go
func assertReclaimStatus(t *testing.T, err error, want int) {
	t.Helper()
	var re *ReclaimError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v, want *ReclaimError", err)
	}
	if re.Status != want {
		t.Errorf("status = %d, want %d", re.Status, want)
	}
}

func newReclaimSup(t *testing.T) (*Supervisor, *store.Mem, string) {
	t.Helper()
	root := t.TempDir()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.GitManager{Root: root}), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	return sup, st, root
}

func TestReclaimRunRemovesScratchAndIsIdempotent(t *testing.T) {
	sup, st, root := newReclaimSup(t)
	ctx := context.Background()
	if err := st.CreateRun(ctx, core.RunState{ID: "done", Status: core.RunSucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "done", "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	removed, err := sup.ReclaimRun(ctx, "done")
	if err != nil {
		t.Fatalf("ReclaimRun: %v", err)
	}
	if !removed {
		t.Error("removed = false, want true")
	}
	if _, err := os.Stat(filepath.Join(root, "done")); !os.IsNotExist(err) {
		t.Error("scratch not removed")
	}
	// Idempotent: the dir is gone, so the second call returns removed=false, no error.
	removed, err = sup.ReclaimRun(ctx, "done")
	if err != nil {
		t.Fatalf("second ReclaimRun: %v", err)
	}
	if removed {
		t.Error("second removed = true, want false (already gone)")
	}
}

func TestReclaimRunUnknownIs404(t *testing.T) {
	sup, _, _ := newReclaimSup(t)
	_, err := sup.ReclaimRun(context.Background(), "nope")
	assertReclaimStatus(t, err, http.StatusNotFound)
}

func TestReclaimRunActiveIs409(t *testing.T) {
	sup, st, _ := newReclaimSup(t)
	ctx := context.Background()
	if err := st.CreateRun(ctx, core.RunState{ID: "r", Status: core.RunFailed}); err != nil {
		t.Fatal(err)
	}
	sup.mu.Lock()
	sup.runs["r"] = func() {} // simulate an active run registered in the run map
	sup.mu.Unlock()
	_, err := sup.ReclaimRun(ctx, "r")
	assertReclaimStatus(t, err, http.StatusConflict)
}

func TestReclaimRunNonTerminalIs409(t *testing.T) {
	sup, st, _ := newReclaimSup(t)
	ctx := context.Background()
	if err := st.CreateRun(ctx, core.RunState{ID: "r", Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}
	_, err := sup.ReclaimRun(ctx, "r")
	assertReclaimStatus(t, err, http.StatusConflict)
}
```

- [ ] **Step 7: Run the `ReclaimRun` tests to verify they pass**

Run: `go test ./internal/supervisor/ -run 'ReclaimRun|SweepScratch'`
Expected: PASS (all `ReclaimRun*` + both `SweepScratch*` tests).

- [ ] **Step 8: Run the full supervisor package under race + gofmt**

Run: `go test -race ./internal/supervisor/ && gofmt -l internal/supervisor/`
Expected: PASS, `gofmt -l` prints nothing.

- [ ] **Step 9: Commit**

```bash
git add internal/supervisor/gc.go internal/supervisor/gc_test.go
git commit -m "feat(supervisor): mark-on-reclaim + ReclaimRun for on-demand GC"
```

---

### Task 3: HTTP API — `POST /v1/gc` + `DELETE /v1/runs/{id}/scratch`

**Files:**
- Modify: `internal/api/dto.go` (add `gcResponse`, `reclaimResponse`)
- Modify: `internal/api/handlers.go` (add `handleGC`, `handleReclaimScratch`)
- Modify: `internal/api/router.go` (register the two routes)
- Test: `internal/api/gc_test.go` (new file)

**Interfaces:**
- Consumes: `s.Sup.SweepScratch(ctx, time.Time) (int, error)`, `s.Sup.ReclaimRun(ctx, core.RunID) (bool, error)`, `*supervisor.ReclaimError` (Task 2); existing `writeJSON`, `writeError`, `core.RunID(r.PathValue("id"))`. (`handlers.go` already imports `time`, `errors`, `net/http`, `core`, `supervisor`.)
- Produces: `POST /v1/gc?older_than=<dur>` → `200 {"reclaimed": <int>}` (400 on unparseable `older_than`); `DELETE /v1/runs/{id}/scratch` → `200 {"removed": <bool>}` (404/409 per `ReclaimRun`). Both inside the auth-protected `v1` mux.

- [ ] **Step 1: Write the failing API tests**

Create `internal/api/gc_test.go`:

```go
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"concentus/internal/core"
)

func TestGCEndpointReturnsCount(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/gc", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Reclaimed int `json:"reclaimed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Reclaimed != 0 {
		t.Errorf("reclaimed = %d, want 0 (no scratch dirs)", body.Reclaimed)
	}
}

func TestGCEndpointBadOlderThanIs400(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/gc?older_than=notaduration", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestReclaimScratchEndpointUnknownIs404(t *testing.T) {
	hs, _, _ := testServer(t)
	req, _ := http.NewRequest(http.MethodDelete, hs.URL+"/v1/runs/nope/scratch", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestReclaimScratchEndpointNonTerminalIs409(t *testing.T) {
	hs, _, st := testServer(t)
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r", Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodDelete, hs.URL+"/v1/runs/r/scratch", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestReclaimScratchEndpointTerminalReturnsRemoved(t *testing.T) {
	hs, _, st := testServer(t)
	if err := st.CreateRun(context.Background(), core.RunState{ID: "done", Status: core.RunSucceeded}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodDelete, hs.URL+"/v1/runs/done/scratch", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Removed bool `json:"removed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Removed {
		t.Errorf("removed = true, want false (no scratch dir existed)")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/api/ -run 'GCEndpoint|ReclaimScratchEndpoint'`
Expected: FAIL — the routes return 404 (not registered) / compile is fine but assertions fail (e.g. GC returns 404, not 200).

- [ ] **Step 3: Add the response DTOs**

In `internal/api/dto.go`, after `pushResponse`, add:

```go
// gcResponse is returned from POST /v1/gc.
type gcResponse struct {
	Reclaimed int `json:"reclaimed"`
}

// reclaimResponse is returned from DELETE /v1/runs/{id}/scratch.
type reclaimResponse struct {
	Removed bool `json:"removed"`
}
```

- [ ] **Step 4: Add the handlers**

In `internal/api/handlers.go`, after `handleRetry`, add:

```go
func (s *Server) handleGC(w http.ResponseWriter, r *http.Request) {
	cutoff := time.Now()
	if v := r.URL.Query().Get("older_than"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid older_than: "+err.Error())
			return
		}
		cutoff = cutoff.Add(-d)
	}
	n, err := s.Sup.SweepScratch(r.Context(), cutoff)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, gcResponse{Reclaimed: n})
}

func (s *Server) handleReclaimScratch(w http.ResponseWriter, r *http.Request) {
	removed, err := s.Sup.ReclaimRun(r.Context(), core.RunID(r.PathValue("id")))
	if err != nil {
		var re *supervisor.ReclaimError
		if errors.As(err, &re) {
			writeError(w, re.Status, re.Msg)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, reclaimResponse{Removed: removed})
}
```

- [ ] **Step 5: Register the routes**

In `internal/api/router.go`, inside the `v1` block (after the `POST /v1/runs/{id}/retry` line), add:

```go
	v1.HandleFunc("POST /v1/gc", s.handleGC)
	v1.HandleFunc("DELETE /v1/runs/{id}/scratch", s.handleReclaimScratch)
```

- [ ] **Step 6: Run the API tests to verify they pass**

Run: `go test ./internal/api/ -run 'GCEndpoint|ReclaimScratchEndpoint'`
Expected: PASS (all five).

- [ ] **Step 7: Run the full api package under race + gofmt**

Run: `go test -race ./internal/api/ && gofmt -l internal/api/`
Expected: PASS, `gofmt -l` prints nothing.

- [ ] **Step 8: Commit**

```bash
git add internal/api/dto.go internal/api/handlers.go internal/api/router.go internal/api/gc_test.go
git commit -m "feat(api): POST /v1/gc + DELETE /v1/runs/{id}/scratch"
```

---

### Task 4: CLI + docs — `cm gc` / `cm rm`

**Files:**
- Modify: `cmd/cm/main.go` (dispatch cases; `gc`/`rm` methods; usage line)
- Modify: `.claude/skills/running-the-orchestrator/SKILL.md` (document `cm gc`/`cm rm`)
- Test: `cmd/cm/gc_test.go` (new file)

**Interfaces:**
- Consumes: `POST /v1/gc?older_than=<dur>` → `{"reclaimed":N}`, `DELETE /v1/runs/{id}/scratch` → `{"removed":bool}` (Task 3); existing `client{base, http}`, `printErr(resp, out) int`. (`main.go` already imports `encoding/json`, `net/url`, `net/http`, `io`.)
- Produces: `cm gc [--older-than <dur>]` and `cm rm <run>` subcommands.

- [ ] **Step 1: Write the failing CLI tests**

Create `cmd/cm/gc_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGCSubcommandPostsGC(t *testing.T) {
	var gotMethod, gotPath, gotOlderThan string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotOlderThan = r.URL.Query().Get("older_than")
		json.NewEncoder(w).Encode(map[string]int{"reclaimed": 3})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"gc"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, want 0; out=%s", code, out.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/gc" {
		t.Errorf("request = %s %s, want POST /v1/gc", gotMethod, gotPath)
	}
	if gotOlderThan != "" {
		t.Errorf("older_than = %q, want empty", gotOlderThan)
	}
	if !strings.Contains(out.String(), "reclaimed 3") {
		t.Errorf("output = %q, want it to contain 'reclaimed 3'", out.String())
	}
}

func TestGCSubcommandForwardsOlderThan(t *testing.T) {
	var gotOlderThan string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOlderThan = r.URL.Query().Get("older_than")
		json.NewEncoder(w).Encode(map[string]int{"reclaimed": 0})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"gc", "--older-than", "1h"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, want 0; out=%s", code, out.String())
	}
	if gotOlderThan != "1h" {
		t.Errorf("older_than = %q, want %q", gotOlderThan, "1h")
	}
}

func TestRmSubcommandDeletesScratch(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		json.NewEncoder(w).Encode(map[string]bool{"removed": true})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"rm", "01ABC"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, want 0; out=%s", code, out.String())
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/runs/01ABC/scratch" {
		t.Errorf("request = %s %s, want DELETE /v1/runs/01ABC/scratch", gotMethod, gotPath)
	}
	if !strings.Contains(out.String(), "removed") {
		t.Errorf("output = %q, want it to contain 'removed'", out.String())
	}
}

func TestRmSubcommandRequiresRun(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"rm"}, "http://unused", &out); code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./cmd/cm/ -run 'GCSubcommand|RmSubcommand'`
Expected: FAIL — `cm gc`/`cm rm` are unknown commands (dispatch hits the default usage branch, exit 2 / wrong output).

- [ ] **Step 3: Add the dispatch cases + usage line**

In `cmd/cm/main.go`, in `dispatch`'s `switch`, after the `case "retry":` block, add:

```go
	case "gc":
		return c.gc(args[1:], out)
	case "rm":
		if len(args) < 2 {
			fmt.Fprintln(out, "usage: cm rm <run>")
			return 2
		}
		return c.rm(args[1], out)
```

Update the top-of-`dispatch` usage line to include `gc|rm`:

```go
		fmt.Fprintln(out, "usage: cm <run|ls|get|watch|approve|reject|cancel|retry|push|pr|ship|gc|rm|loglevel> ...")
```

- [ ] **Step 4: Add the `gc` and `rm` client methods**

In `cmd/cm/main.go`, after the `retry` method (or near the other `client` methods), add:

```go
func (c *client) gc(args []string, out io.Writer) int {
	path := "/v1/gc"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--older-than":
			if i+1 >= len(args) {
				fmt.Fprintln(out, "usage: --older-than requires a value")
				return 2
			}
			i++
			path = "/v1/gc?older_than=" + url.QueryEscape(args[i])
		default:
			fmt.Fprintln(out, "usage: cm gc [--older-than <dur>]")
			return 2
		}
	}
	resp, err := c.http.Post(c.base+path, "application/json", nil)
	if err != nil {
		fmt.Fprintln(out, "gc:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	var body struct {
		Reclaimed int `json:"reclaimed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		fmt.Fprintln(out, "gc: decode:", err)
		return 1
	}
	fmt.Fprintf(out, "reclaimed %d\n", body.Reclaimed)
	return 0
}

func (c *client) rm(run string, out io.Writer) int {
	req, _ := http.NewRequest(http.MethodDelete, c.base+"/v1/runs/"+run+"/scratch", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Fprintln(out, "rm:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	var body struct {
		Removed bool `json:"removed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		fmt.Fprintln(out, "rm: decode:", err)
		return 1
	}
	if body.Removed {
		fmt.Fprintln(out, "removed")
	} else {
		fmt.Fprintln(out, "already gone")
	}
	return 0
}
```

- [ ] **Step 5: Run the CLI tests to verify they pass**

Run: `go test ./cmd/cm/ -run 'GCSubcommand|RmSubcommand'`
Expected: PASS (all four).

- [ ] **Step 6: Document the commands in the skill**

In `.claude/skills/running-the-orchestrator/SKILL.md`, find the command-surface reference (the line listing `cm run|ls|get|...|ship|...`). Update that command list to include `gc` and `rm`, and add a short explanatory block near the other delivery commands:

```markdown
- `cm gc [--older-than <dur>]` — reclaim scratch workspaces for all terminal runs now (omit `--older-than` to reclaim everything terminal; pass e.g. `--older-than 24h` to keep recent runs). Frees disk without waiting for the background janitor.
- `cm rm <run>` — reclaim ONE run's scratch workspace immediately. Removes only the on-disk clone; the run record, its events, and artifacts are kept. 404 if unknown, 409 if the run is still in progress.
```

(Preserve any pre-existing flags on the command-surface line, exactly as Task 4 of the retry slice had to preserve `--head-repo` and `cm ship`. Read the current line first and extend it; do not replace it from memory.)

- [ ] **Step 7: Verify the build, the full CLI package under race, and gofmt**

Run: `go build ./... && go test -race ./cmd/cm/ && gofmt -l cmd/cm/`
Expected: build clean, tests PASS, `gofmt -l` prints nothing.

- [ ] **Step 8: Commit**

```bash
git add cmd/cm/main.go cmd/cm/gc_test.go .claude/skills/running-the-orchestrator/SKILL.md
git commit -m "feat(cm): add gc and rm subcommands"
```

---

## Final verification (after all tasks)

- [ ] **Whole-suite race run:** `go test -race ./...` — all packages green.
- [ ] **Format:** `gofmt -l .` prints nothing.
- [ ] **Dependency check:** `git diff main -- go.mod go.sum` is empty (no new deps; no version bumps).

## Self-Review (author)

**Spec coverage:** every spec section maps to a task —
- §1 data model (`reclaimed_at`, migration 0004, `ReclaimableRuns` exclusion, `MarkReclaimed`, Mem set) → **Task 1**.
- §2 reclaim flow (`reclaimOne` mark-on-success, `SweepScratch` refactor) → **Task 2**.
- §3 surface (`cm gc`/`cm rm`, `POST /v1/gc`, `DELETE /v1/runs/{id}/scratch`, `older_than` parse, auth mux) → **Tasks 3 + 4**.
- §4 guards (`ReclaimRun` 404/409/idempotent; `cm gc` query-guaranteed) → **Tasks 2 + 3**.
- Testing list → per-task test steps + final whole-suite race run.

**Deviations from the spec (flagged for the reviewer / human):**
1. **Mem marker type.** Spec wording said `reclaimedAt map[core.RunID]time.Time`; the plan uses `reclaimed map[core.RunID]bool` (a set). The timestamp is never read (`GetRun`/`RunState` do not surface `reclaimed_at`, per spec out-of-scope), so a bool set is the YAGNI form with identical exclusion behavior.
2. **No real-git in supervisor reclaim tests.** Spec's testing note said "Real-git via `requireGitS`". `Workspace.Reclaim` is a stat-then-`RemoveAll` of `<root>/<id>` and does not require a `.git` (the existing `TestSweepScratchReclaimsTerminalAgedRuns` already reclaims a plain `MkdirAll` dir). So the `ReclaimRun` tests use a plain temp dir — simpler and with no sandbox-git dependency.
3. **No explicit goose-`down` test.** Spec's testing list said "down reverses; goose round-trips." The codebase has no precedent for down-migration tests; the SQLite `MarkReclaimed` test already proves the `up` migration applied (the column exists). The `down` statement is provided in the migration for symmetry but is not separately tested (YAGNI). 

These three are judgment calls a reviewer may overturn; none changes the shipped behavior.
