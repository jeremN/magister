# Scratch GC + dead-field cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reclaim per-run scratch clones on a TTL-sweep-after-terminal policy so the daemon's disk doesn't grow unbounded, and remove two dead struct fields.

**Architecture:** A daemon janitor goroutine periodically asks the store for terminal runs older than a retention TTL and deletes each run's scratch directory. The mechanism stays inside the existing ports: a `Workspace.Reclaim` primitive, a `Store.ReclaimableRuns` query, an `Engine` delegator, a `Supervisor.SweepScratch` driver, and the daemon goroutine.

**Tech Stack:** Go 1.22 stdlib only; `database/sql` + checked-in sqlc-style code (sqlc binary not installed — hand-write the generated method); `os`/`path/filepath`; `time`.

## Global Constraints

- Go 1.22; **stdlib-only, no new dependency**; **no DB migration** (the `updated_at` column already exists and is bumped by `SetRunStatus`).
- Two new port methods, each implemented in every adapter: `core.Workspace.Reclaim(runID) error` and `core.Store.ReclaimableRuns(ctx, before time.Time) ([]RunID, error)` — same blessed pattern as the earlier `AppendEvents`/`Provision`/`BasePath` additions.
- Engine run lifecycle is otherwise untouched; GC is post-terminal, store-driven, background.
- API auth behavior is **unchanged** (this slice only removes a redundant struct field; enforcement stays in `Router(token)` → `authMiddleware`).
- Default retention TTL **24h**, default sweep interval **1h**, both configurable; `ScratchTTL <= 0` disables the janitor.
- `sqlc` is NOT installed — hand-write the generated query method in `internal/store/sqldb/query.sql.go` (the Slice-1 fallback) and keep `internal/store/query.sql` in sync as documentation.
- Commits: a single conventional-commit subject, **no body**, **no `Co-Authored-By`**, never `--no-verify`. `gofmt`/`go vet`/`go test -race ./...` clean before merge.

## File Structure

- `internal/core/ports.go` — **modify**: add `Reclaim` to the `Workspace` interface.
- `internal/core/store.go` — **modify**: add `ReclaimableRuns` to the `Store` interface (+ `time` import).
- `internal/workspace/gitmanager.go` — **modify**: `runDir` helper, `safeRunID`, `GitManager.Reclaim`.
- `internal/workspace/workspace.go` — **modify**: `Manager.Reclaim`.
- `internal/workspace/reclaim_test.go` — **create**: Reclaim tests (both managers).
- `internal/store/mem.go` — **modify**: `updatedAt` tracking + `Mem.ReclaimableRuns` + `isTerminal`.
- `internal/store/query.sql` — **modify**: add the `ReclaimableRuns` query (documentation).
- `internal/store/sqldb/query.sql.go` — **modify**: hand-write the generated `ReclaimableRuns` method.
- `internal/store/sqlite.go` — **modify**: `SQLite.ReclaimableRuns` wrapper.
- `internal/store/mem_test.go` + `internal/store/sqlite_test.go` — **modify**: `ReclaimableRuns` tests.
- `internal/engine/engine.go` — **modify**: `Engine.ReclaimScratch` delegator.
- `internal/supervisor/gc.go` — **create**: `Supervisor.SweepScratch`.
- `internal/supervisor/gc_test.go` — **create**: sweep test.
- `internal/config/config.go` — **modify**: `ScratchTTL` + `ScratchSweepInterval`.
- `internal/config/config_test.go` — **modify**: config tests.
- `cmd/magisterd/main.go` — **modify**: `runScratchJanitor` + wiring + remove dead `Server` fields.
- `cmd/magisterd/main_test.go` — **modify**: disabled-janitor test.
- `internal/api/handlers.go` — **modify**: remove dead `BearerToken`/`ShutdownTimeout` fields; gate `scratch` on `os.Stat`.
- `internal/api/handlers_test.go` — **modify**: scratch-existence-gate test.

---

### Task 1: `Workspace.Reclaim` primitive

**Files:**
- Modify: `internal/core/ports.go` (Workspace interface, after `TeardownRun` ~line 70)
- Modify: `internal/workspace/gitmanager.go` (add `runDir`, `safeRunID`, `Reclaim`)
- Modify: `internal/workspace/workspace.go` (add `Manager.Reclaim`)
- Test: `internal/workspace/reclaim_test.go` (create)

**Interfaces:**
- Produces: `Workspace.Reclaim(runID core.RunID) error` on the `core.Workspace` interface, implemented by `GitManager` and `Manager`. Removes `<Root>/<runID>/` (base + worktrees). Idempotent (missing dir → nil); refuses an unsafe runID. Consumed by Task 3's `Engine.ReclaimScratch`.

- [ ] **Step 1: Write the failing tests**

Create `internal/workspace/reclaim_test.go`:

```go
package workspace_test

import (
	"os"
	"path/filepath"
	"testing"

	"concentus/internal/core"
	"concentus/internal/workspace"
)

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func TestGitManagerReclaimRemovesRunScratch(t *testing.T) {
	root := t.TempDir()
	m := &workspace.GitManager{Root: root}
	runDir := filepath.Join(root, "run1")
	mkdirAll(t, filepath.Join(runDir, "base"))
	mkdirAll(t, filepath.Join(runDir, "wt", "stepA"))

	if err := m.Reclaim("run1"); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("run dir still present: %v", err)
	}
	// idempotent: a second reclaim of a missing dir is not an error
	if err := m.Reclaim("run1"); err != nil {
		t.Errorf("second Reclaim: %v", err)
	}
}

func TestGitManagerReclaimRejectsUnsafeID(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, "keep")
	mkdirAll(t, sentinel)
	m := &workspace.GitManager{Root: root}

	for _, id := range []core.RunID{"", ".", "..", "a/b", "../keep"} {
		if err := m.Reclaim(id); err == nil {
			t.Errorf("Reclaim(%q) = nil, want error", id)
		}
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("sentinel under root was disturbed: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("root removed: %v", err)
	}
}

func TestManagerReclaimRemovesRunDir(t *testing.T) {
	root := t.TempDir()
	m := &workspace.Manager{Root: root}
	mkdirAll(t, filepath.Join(root, "run9", "stepX"))
	if err := m.Reclaim("run9"); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "run9")); !os.IsNotExist(err) {
		t.Errorf("run dir still present")
	}
	if err := m.Reclaim(".."); err == nil {
		t.Errorf("Reclaim(\"..\") = nil, want error")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/workspace/ -run Reclaim`
Expected: FAIL — `m.Reclaim undefined` (compile error).

- [ ] **Step 3: Add `Reclaim` to the `Workspace` interface**

In `internal/core/ports.go`, immediately after the `TeardownRun(runID RunID) error` line inside the `Workspace` interface, add:

```go
	// Reclaim removes the run's entire scratch directory (base repo + worktrees).
	// Best-effort and idempotent: a missing directory is not an error. The scratch
	// janitor calls it once a run is terminal and past its retention TTL.
	Reclaim(runID RunID) error
```

- [ ] **Step 4: Implement `safeRunID`, `runDir`, and `GitManager.Reclaim`**

In `internal/workspace/gitmanager.go`, add `strings` to the import block (it currently imports `bytes`, `fmt`, `os`, `os/exec`, `path/filepath`, `sync`, plus the two project packages). Then add these functions (place them near `baseDir`/`wtDir`, e.g. after the `wtDir` helper):

```go
func (m *GitManager) runDir(id core.RunID) string { return filepath.Join(m.Root, string(id)) }

// safeRunID reports whether id is a clean, non-empty single path segment, so
// filepath.Join(Root, id) stays strictly under Root. It guards Reclaim's RemoveAll
// against an empty or ".."/separator id escaping to Root or a parent directory.
func safeRunID(id core.RunID) bool {
	s := string(id)
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsRune(s, '/') || strings.ContainsRune(s, filepath.Separator) {
		return false
	}
	return filepath.Base(s) == s
}

// Reclaim removes the run's entire scratch directory (base repo + worktrees). It
// takes the run lock (like TeardownRun) and is idempotent: a missing directory is
// not an error. Refuses an unsafe runID so an empty/".." id can never RemoveAll the
// runs root.
func (m *GitManager) Reclaim(runID core.RunID) error {
	if !safeRunID(runID) {
		return fmt.Errorf("refusing to reclaim unsafe run id %q", runID)
	}
	lock := m.runLock(runID)
	lock.Lock()
	defer lock.Unlock()
	return os.RemoveAll(m.runDir(runID))
}
```

- [ ] **Step 5: Implement `Manager.Reclaim`**

In `internal/workspace/workspace.go`, add `fmt` to the import block (it currently imports `os`, `path/filepath`, and the two project packages). Then add after `BasePath`:

```go
// Reclaim removes the run's scratch directory. Mirrors GitManager.Reclaim with the
// same safety guard; the plain Manager allocates plain dirs under Root.
func (m *Manager) Reclaim(runID core.RunID) error {
	if !safeRunID(runID) {
		return fmt.Errorf("refusing to reclaim unsafe run id %q", runID)
	}
	return os.RemoveAll(filepath.Join(m.Root, string(runID)))
}
```

(`safeRunID` lives in the same `workspace` package, so it is callable here.)

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/workspace/ -run Reclaim`
Expected: PASS (3 tests).

- [ ] **Step 7: Run the whole workspace package**

Run: `go test ./internal/workspace/`
Expected: PASS (Reclaim added a method to the interface; both impls satisfy it, so the package still builds and all tests pass).

- [ ] **Step 8: Commit**

```bash
git add internal/core/ports.go internal/workspace/gitmanager.go internal/workspace/workspace.go internal/workspace/reclaim_test.go
git commit -m "feat(workspace): Reclaim removes a run's scratch directory"
```

---

### Task 2: `Store.ReclaimableRuns` query

**Files:**
- Modify: `internal/core/store.go` (Store interface + `time` import)
- Modify: `internal/store/mem.go` (`updatedAt` tracking, `isTerminal`, `Mem.ReclaimableRuns`)
- Modify: `internal/store/query.sql` (documentation)
- Modify: `internal/store/sqldb/query.sql.go` (hand-written generated method)
- Modify: `internal/store/sqlite.go` (`SQLite.ReclaimableRuns` wrapper)
- Test: `internal/store/mem_test.go`, `internal/store/sqlite_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `Store.ReclaimableRuns(ctx context.Context, before time.Time) ([]core.RunID, error)` — IDs of runs in a terminal status (`succeeded`/`failed`/`canceled`) whose `updated_at` is strictly before `before`. Implemented by `Mem` and `SQLite`. Consumed by Task 3's `Supervisor.SweepScratch`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/store/mem_test.go` (use the file's existing package and imports; add `time` if absent):

```go
func sameRunIDSet(got, want []core.RunID) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[core.RunID]bool, len(got))
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			return false
		}
	}
	return true
}

func TestMemReclaimableRuns(t *testing.T) {
	st := store.NewMem()
	ctx := context.Background()
	mk := func(id core.RunID, status core.RunStatus) {
		if err := st.CreateRun(ctx, core.RunState{ID: id, Status: core.RunPending}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetRunStatus(ctx, id, status, ""); err != nil {
			t.Fatal(err)
		}
	}
	mk("done", core.RunSucceeded)
	mk("failed", core.RunFailed)
	mk("canceled", core.RunCanceled)
	mk("active", core.RunRunning)

	got, err := st.ReclaimableRuns(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !sameRunIDSet(got, []core.RunID{"done", "failed", "canceled"}) {
		t.Errorf("future cutoff = %v, want the 3 terminal runs", got)
	}

	got, err = st.ReclaimableRuns(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("past cutoff = %v, want none", got)
	}
}
```

Add to `internal/store/sqlite_test.go` (use the file's existing package/imports; add `time`, `path/filepath` if absent):

```go
func TestSQLiteReclaimableRuns(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.CreateRun(ctx, core.RunState{ID: "done", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunStatus(ctx, "done", core.RunSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(ctx, core.RunState{ID: "active", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunStatus(ctx, "active", core.RunRunning, ""); err != nil {
		t.Fatal(err)
	}

	got, err := st.ReclaimableRuns(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !sameRunIDSet(got, []core.RunID{"done"}) {
		t.Errorf("future cutoff = %v, want [done]", got)
	}
	got, err = st.ReclaimableRuns(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("past cutoff = %v, want none", got)
	}
}
```

(`sameRunIDSet` is defined once in `mem_test.go` and reused here — both files are the same `store_test` package. If they are not the same package, move `sameRunIDSet` to a shared `_test.go` helper file.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run ReclaimableRuns`
Expected: FAIL — `st.ReclaimableRuns undefined` (compile error).

- [ ] **Step 3: Add `ReclaimableRuns` to the `Store` interface**

In `internal/core/store.go`, add `"time"` to the import block, then add this method to the `Store` interface (after `EventsSince`):

```go
	// ReclaimableRuns returns the IDs of terminal runs (succeeded/failed/canceled)
	// whose last update is strictly before the cutoff. The scratch janitor uses it to
	// find runs whose scratch is past its retention TTL.
	ReclaimableRuns(ctx context.Context, before time.Time) ([]RunID, error)
```

- [ ] **Step 4: Implement `Mem.ReclaimableRuns` with `updatedAt` tracking**

In `internal/store/mem.go`:

1. Add `"time"` to the import block.
2. Add an `updatedAt` field to the `Mem` struct:

```go
type Mem struct {
	mu        sync.Mutex
	runs      map[core.RunID]*core.RunState
	events    map[core.RunID][]event.Event
	updatedAt map[core.RunID]time.Time
	seq       int64
}
```

3. Initialise it in `NewMem`:

```go
func NewMem() *Mem {
	return &Mem{
		runs:      make(map[core.RunID]*core.RunState),
		events:    make(map[core.RunID][]event.Event),
		updatedAt: make(map[core.RunID]time.Time),
	}
}
```

4. In `CreateRun`, after `m.runs[r.ID] = &cp`, set the timestamp:

```go
	m.runs[r.ID] = &cp
	m.updatedAt[r.ID] = time.Now()
	return nil
```

5. In `SetRunStatus`, after the status/error are applied to the stored run, set the timestamp. The existing body finds the run and sets its fields; add `m.updatedAt[id] = time.Now()` before the `return nil`. (Mirror exactly which SQL writes touch `runs.updated_at`: `CreateRun` and `SetRunStatus` only — NOT `SaveStepTransition` or `AppendEvents`.)

6. Add the helper and method (e.g. at the end of the file):

```go
func isTerminal(s core.RunStatus) bool {
	return s == core.RunSucceeded || s == core.RunFailed || s == core.RunCanceled
}

func (m *Mem) ReclaimableRuns(_ context.Context, before time.Time) ([]core.RunID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.RunID
	for id, r := range m.runs {
		if !isTerminal(r.Status) {
			continue
		}
		if u, ok := m.updatedAt[id]; ok && u.Before(before) {
			out = append(out, id)
		}
	}
	return out, nil
}
```

- [ ] **Step 5: Add the SQL query (documentation) and the hand-written generated method**

In `internal/store/query.sql`, append:

```sql
-- name: ReclaimableRuns :many
SELECT id FROM runs WHERE status IN ('succeeded', 'failed', 'canceled') AND updated_at < ? ORDER BY updated_at;
```

In `internal/store/sqldb/query.sql.go`, add (mirroring the existing `ListRuns` generated boilerplate — `q.db.QueryContext`, scan loop, `rows.Close`, `rows.Err`):

```go
const reclaimableRuns = `-- name: ReclaimableRuns :many
SELECT id FROM runs WHERE status IN ('succeeded', 'failed', 'canceled') AND updated_at < ? ORDER BY updated_at
`

func (q *Queries) ReclaimableRuns(ctx context.Context, updatedAt string) ([]string, error) {
	rows, err := q.db.QueryContext(ctx, reclaimableRuns, updatedAt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		items = append(items, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}
```

- [ ] **Step 6: Implement the `SQLite.ReclaimableRuns` wrapper**

In `internal/store/sqlite.go` (it already imports `time`), add:

```go
func (s *SQLite) ReclaimableRuns(ctx context.Context, before time.Time) ([]core.RunID, error) {
	ids, err := s.qr.ReclaimableRuns(ctx, before.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, err
	}
	out := make([]core.RunID, 0, len(ids))
	for _, id := range ids {
		out = append(out, core.RunID(id))
	}
	return out, nil
}
```

(The `"2006-01-02 15:04:05"` UTC layout matches the TEXT form SQLite's `datetime('now')` writes, so the `<` comparison is correct.)

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run ReclaimableRuns`
Expected: PASS (Mem + SQLite).

- [ ] **Step 8: Run the whole store package**

Run: `go test ./internal/store/`
Expected: PASS (the interface gained a method; both impls satisfy it).

- [ ] **Step 9: Commit**

```bash
git add internal/core/store.go internal/store/mem.go internal/store/query.sql internal/store/sqldb/query.sql.go internal/store/sqlite.go internal/store/mem_test.go internal/store/sqlite_test.go
git commit -m "feat(store): ReclaimableRuns lists terminal runs past a cutoff"
```

---

### Task 3: `Engine.ReclaimScratch` + `Supervisor.SweepScratch`

**Files:**
- Modify: `internal/engine/engine.go` (add `ReclaimScratch` delegator near `BasePath` ~line 53)
- Create: `internal/supervisor/gc.go`
- Test: `internal/supervisor/gc_test.go` (create)

**Interfaces:**
- Consumes: `Workspace.Reclaim` (Task 1) via `Engine.WS`; `Store.ReclaimableRuns` (Task 2) via `Supervisor.store`.
- Produces: `Engine.ReclaimScratch(runID core.RunID) error` (delegates to `WS.Reclaim`); `Supervisor.SweepScratch(ctx context.Context, olderThan time.Time) (int, error)` — reclaims every terminal run older than `olderThan`, best-effort, returns the count reclaimed. Consumed by Task 4's daemon janitor.

- [ ] **Step 1: Write the failing test**

Create `internal/supervisor/gc_test.go`:

```go
package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

func TestSweepScratchReclaimsTerminalAgedRuns(t *testing.T) {
	root := t.TempDir()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	gm := &workspace.GitManager{Root: root}
	sup := New(testEngine(t, st, reg, gm), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	ctx := context.Background()

	mkRun := func(id core.RunID, status core.RunStatus) {
		if err := st.CreateRun(ctx, core.RunState{ID: id, Status: core.RunPending}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetRunStatus(ctx, id, status, ""); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, string(id), "base"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkRun("done1", core.RunSucceeded)
	mkRun("done2", core.RunFailed)
	mkRun("active", core.RunRunning)

	n, err := sup.SweepScratch(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SweepScratch: %v", err)
	}
	if n != 2 {
		t.Errorf("reclaimed = %d, want 2", n)
	}
	for _, id := range []string{"done1", "done2"} {
		if _, err := os.Stat(filepath.Join(root, id)); !os.IsNotExist(err) {
			t.Errorf("%s scratch not reclaimed", id)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "active")); err != nil {
		t.Errorf("active scratch wrongly reclaimed: %v", err)
	}
}
```

(`testEngine(t, st, reg, gm)` and `New` already exist in the `supervisor` test package — used by `push_test.go`/`pr_test.go`.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/supervisor/ -run TestSweepScratch`
Expected: FAIL — `sup.SweepScratch undefined` (compile error).

- [ ] **Step 3: Add `Engine.ReclaimScratch`**

In `internal/engine/engine.go`, after the `BasePath` method (~line 55), add:

```go
// ReclaimScratch removes a run's scratch workspace. Delegates to the workspace; the
// scratch janitor calls it after a run is terminal and past its retention TTL.
func (e *Engine) ReclaimScratch(runID core.RunID) error {
	return e.WS.Reclaim(runID)
}
```

- [ ] **Step 4: Add `Supervisor.SweepScratch`**

Create `internal/supervisor/gc.go`:

```go
package supervisor

import (
	"context"
	"time"
)

// SweepScratch reclaims the scratch directory of every terminal run whose last
// update is before olderThan. It is best-effort: a single run's reclaim failure is
// logged and the sweep continues. Returns the number of runs reclaimed; a non-nil
// error means the store query failed (nothing was swept). The caller supplies the
// cutoff (e.g. time.Now().Add(-ttl)) so the sweep needs no clock of its own.
func (s *Supervisor) SweepScratch(ctx context.Context, olderThan time.Time) (int, error) {
	ids, err := s.store.ReclaimableRuns(ctx, olderThan)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if err := s.engine.ReclaimScratch(id); err != nil {
			s.logger().Error("scratch reclaim", "run", id, "err", err)
			continue
		}
		n++
	}
	return n, nil
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/supervisor/ -run TestSweepScratch`
Expected: PASS.

- [ ] **Step 6: Run engine + supervisor packages**

Run: `go test ./internal/engine/ ./internal/supervisor/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/engine/engine.go internal/supervisor/gc.go internal/supervisor/gc_test.go
git commit -m "feat(supervisor): SweepScratch reclaims terminal runs past a cutoff"
```

---

### Task 4: Config + daemon janitor

**Files:**
- Modify: `internal/config/config.go` (`ScratchTTL`, `ScratchSweepInterval`)
- Test: `internal/config/config_test.go`
- Modify: `cmd/magisterd/main.go` (`runScratchJanitor` + wiring)
- Test: `cmd/magisterd/main_test.go` (disabled-janitor test)

**Interfaces:**
- Consumes: `Supervisor.SweepScratch` (Task 3).
- Produces: config fields `ScratchTTL`/`ScratchSweepInterval`; a `runScratchJanitor(ctx, sup, ttl, interval, log)` goroutine started by the daemon.

- [ ] **Step 1: Write the failing config tests**

Add to `internal/config/config_test.go` (use the file's existing package/imports; add `time` if absent):

```go
func TestParseScratchDefaults(t *testing.T) {
	c := config.Parse(nil, func(string) string { return "" })
	if c.ScratchTTL != 24*time.Hour {
		t.Errorf("ScratchTTL = %v, want 24h", c.ScratchTTL)
	}
	if c.ScratchSweepInterval != time.Hour {
		t.Errorf("ScratchSweepInterval = %v, want 1h", c.ScratchSweepInterval)
	}
}

func TestParseScratchFlagsAndEnv(t *testing.T) {
	c := config.Parse([]string{"-scratch-ttl=1h", "-scratch-sweep-interval=5m"}, func(string) string { return "" })
	if c.ScratchTTL != time.Hour {
		t.Errorf("ScratchTTL flag = %v, want 1h", c.ScratchTTL)
	}
	if c.ScratchSweepInterval != 5*time.Minute {
		t.Errorf("ScratchSweepInterval flag = %v, want 5m", c.ScratchSweepInterval)
	}

	c = config.Parse(nil, func(k string) string {
		if k == "MAGISTER_SCRATCH_TTL" {
			return "2h"
		}
		return ""
	})
	if c.ScratchTTL != 2*time.Hour {
		t.Errorf("ScratchTTL env = %v, want 2h", c.ScratchTTL)
	}
}
```

- [ ] **Step 2: Run the config tests to verify they fail**

Run: `go test ./internal/config/ -run Scratch`
Expected: FAIL — `c.ScratchTTL undefined` (compile error).

- [ ] **Step 3: Add the config fields, flags, and env**

In `internal/config/config.go`, extend the `Config` struct:

```go
type Config struct {
	Addr                 string
	DBPath               string
	BearerToken          string
	ShutdownTimeout      time.Duration
	ScratchTTL           time.Duration
	ScratchSweepInterval time.Duration
}
```

In `Parse`, after the `shutdown-timeout` flag registration, add:

```go
	fs.DurationVar(&c.ScratchTTL, "scratch-ttl", 24*time.Hour, "reclaim a terminal run's scratch this long after it finishes (0 disables)")
	fs.DurationVar(&c.ScratchSweepInterval, "scratch-sweep-interval", time.Hour, "how often the scratch janitor sweeps")
```

After the existing env overrides (the `MAGISTER_DB` block), add:

```go
	if v := env("MAGISTER_SCRATCH_TTL"); v != "" && !flagSet(fs, "scratch-ttl") {
		if d, err := time.ParseDuration(v); err == nil {
			c.ScratchTTL = d
		}
	}
```

- [ ] **Step 4: Run the config tests to verify they pass**

Run: `go test ./internal/config/ -run Scratch`
Expected: PASS.

- [ ] **Step 5: Write the failing daemon test**

Add to `cmd/magisterd/main_test.go` (package `main`; add imports `context`, `io`, `log/slog`, `time` as needed):

```go
func TestRunScratchJanitorDisabledReturns(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan struct{})
	go func() {
		// ttl <= 0 disables the janitor; it must return without touching sup (nil here).
		runScratchJanitor(context.Background(), nil, 0, time.Hour, log)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("disabled janitor did not return")
	}
}
```

- [ ] **Step 6: Run the daemon test to verify it fails**

Run: `go test ./cmd/magisterd/ -run TestRunScratchJanitorDisabled`
Expected: FAIL — `runScratchJanitor undefined` (compile error).

- [ ] **Step 7: Add `runScratchJanitor` and wire it into `run`**

In `cmd/magisterd/main.go`, add the function (e.g. below `run`):

```go
// runScratchJanitor periodically reclaims the scratch of terminal runs past the
// retention TTL. A non-positive ttl disables it. It runs until ctx is canceled.
func runScratchJanitor(ctx context.Context, sup *supervisor.Supervisor, ttl, interval time.Duration, log *slog.Logger) {
	if ttl <= 0 {
		log.Info("scratch janitor disabled", "scratch_ttl", ttl)
		return
	}
	sweep := func() {
		n, err := sup.SweepScratch(ctx, time.Now().Add(-ttl))
		if err != nil {
			log.Error("scratch sweep", "err", err)
			return
		}
		if n > 0 {
			log.Info("scratch reclaimed", "runs", n)
		}
	}
	sweep() // immediate boot sweep (reclaims anything already past TTL from a prior process)
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			sweep()
		}
	}
}
```

Then wire it inside `run`, immediately after the `sup.ResumeAll(...)` block (after line ~85):

```go
	janitorCtx, stopJanitor := context.WithCancel(context.Background())
	go runScratchJanitor(janitorCtx, sup, cfg.ScratchTTL, cfg.ScratchSweepInterval, log)
```

And stop it on shutdown — change the post-`select` shutdown sequence so `stopJanitor()` runs before the store closes (`defer st.Close()` runs last):

```go
	select {
	case err := <-serveErr:
		stopJanitor()
		return err
	case <-stopCh:
	}

	stopJanitor()
	log.Info("shutting down")
	sup.Shutdown(cfg.ShutdownTimeout) // cancel active runs first
```

(`context`, `time`, `slog`, and `supervisor` are already imported by `main.go`.)

- [ ] **Step 8: Run the daemon test to verify it passes**

Run: `go test ./cmd/magisterd/ -run TestRunScratchJanitorDisabled`
Expected: PASS.

- [ ] **Step 9: Build the daemon and run its package**

Run: `go build ./cmd/magisterd/ && go test ./cmd/magisterd/ ./internal/config/`
Expected: build succeeds; tests PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go cmd/magisterd/main.go cmd/magisterd/main_test.go
git commit -m "feat(magisterd): scratch janitor sweeps terminal runs on a TTL"
```

---

### Task 5: Cleanups — dead fields + `cm get` scratch existence gate

**Files:**
- Modify: `internal/api/handlers.go` (remove `BearerToken`/`ShutdownTimeout` from `Server`; gate `scratch` on `os.Stat`)
- Modify: `cmd/magisterd/main.go` (drop the two dead field assignments)
- Test: `internal/api/handlers_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: a slimmer `api.Server` (no dead fields); `GET /v1/runs/{id}` reports `scratch` only when the directory exists.

- [ ] **Step 1: Write the failing test**

Add to `internal/api/handlers_test.go` (use the file's existing package/imports; add `os`, `path/filepath`, `net/http`, `net/http/httptest`, `encoding/json` as needed):

```go
func TestGetRunGatesScratchOnExistence(t *testing.T) {
	st := store.NewMem()
	ctx := context.Background()
	if err := st.CreateRun(ctx, core.RunState{ID: "r1", Status: core.RunSucceeded, Repo: "/some/src"}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	srv := &api.Server{Store: st, ScratchRoot: root}
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	getScratch := func() string {
		resp, err := http.Get(hs.URL + "/v1/runs/r1")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var snap struct {
			Scratch string `json:"scratch"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
			t.Fatal(err)
		}
		return snap.Scratch
	}

	// No scratch dir on disk yet → scratch omitted/empty.
	if s := getScratch(); s != "" {
		t.Errorf("scratch = %q, want empty when dir absent", s)
	}
	// Create the base dir → scratch now reported.
	if err := os.MkdirAll(filepath.Join(root, "r1", "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	if s := getScratch(); s == "" {
		t.Error("scratch empty, want the base path when dir exists")
	}
}
```

(If `handlers_test.go` is `package api` rather than `api_test`, drop the `api.` qualifier on `Server`. Match the file's existing package.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api/ -run TestGetRunGatesScratchOnExistence`
Expected: FAIL — `scratch` is reported even when the dir is absent (current code sets it unconditionally), so the first assertion fails.

- [ ] **Step 3: Gate `scratch` on `os.Stat` in `handleGetRun`**

In `internal/api/handlers.go`, ensure `"os"` is in the import block (add it if absent). Replace the scratch block in `handleGetRun`:

```go
	scratch := ""
	if rs.Repo != "" && s.ScratchRoot != "" {
		scratch = filepath.Join(s.ScratchRoot, string(rs.ID), "base")
	}
```

with:

```go
	scratch := ""
	if rs.Repo != "" && s.ScratchRoot != "" {
		p := filepath.Join(s.ScratchRoot, string(rs.ID), "base")
		if _, err := os.Stat(p); err == nil {
			scratch = p // omit a reclaimed run's dead path
		}
	}
```

- [ ] **Step 4: Remove the dead `Server` fields**

In `internal/api/handlers.go`, delete these two lines from the `Server` struct:

```go
	BearerToken     string
	ShutdownTimeout time.Duration
```

Then build the package: `go build ./internal/api/`. If it reports `"time" imported and not used`, remove the now-orphaned `"time"` import from `handlers.go`. (If `time` is still used elsewhere in the file, leave it.)

- [ ] **Step 5: Drop the dead assignments in `main.go`**

In `cmd/magisterd/main.go`, change the server construction from:

```go
	srv := &api.Server{Sup: sup, Store: st, Bus: bus, Log: log, BearerToken: cfg.BearerToken, ShutdownTimeout: cfg.ShutdownTimeout, ScratchRoot: runsRoot}
```

to:

```go
	srv := &api.Server{Sup: sup, Store: st, Bus: bus, Log: log, ScratchRoot: runsRoot}
```

(Auth and shutdown are unaffected: `srv.Router(cfg.BearerToken)` and `cfg.ShutdownTimeout` remain the live path.)

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./internal/api/ -run TestGetRunGatesScratchOnExistence`
Expected: PASS.

- [ ] **Step 7: Build + run the affected packages (auth/shutdown unchanged)**

Run: `go build ./... && go test ./internal/api/ ./cmd/magisterd/`
Expected: build succeeds; tests PASS (existing auth/middleware and daemon tests still green — behavior unchanged).

- [ ] **Step 8: Commit**

```bash
git add internal/api/handlers.go cmd/magisterd/main.go internal/api/handlers_test.go
git commit -m "refactor(api): drop dead Server fields; hide a reclaimed scratch path"
```

---

## Self-Review

**1. Spec coverage** (against `docs/superpowers/specs/2026-06-19-scratch-gc-design.md`):
- `Workspace.Reclaim` (GitManager + Manager + safety guard) → Task 1. ✓
- `Store.ReclaimableRuns` (interface + Mem `updatedAt` + SQLite hand-written query + UTC format) → Task 2. ✓
- `Engine.ReclaimScratch` delegator + `Supervisor.SweepScratch` (best-effort, count, caller-supplied cutoff) → Task 3. ✓
- Daemon janitor (boot sweep + ticker + ttl<=0 disable + stop-on-shutdown) + config (`ScratchTTL` 24h, `ScratchSweepInterval` 1h, env) → Task 4. ✓
- Cleanups: dead `BearerToken`/`ShutdownTimeout` fields removed; `cm get` `os.Stat` gate → Task 5. ✓
- No migration, no new dep, two blessed port additions → held across Tasks 1–2. ✓
- Out-of-scope (orphan dirs, `cm gc`, DB-row GC) → not implemented, as intended. ✓

**2. Placeholder scan:** No TBD/TODO; every code step carries complete code. The two conditional notes (drop `time` import if orphaned in Task 5; match the test file's package) are concrete, build-verified instructions, not placeholders. ✓

**3. Type consistency:** `Reclaim(runID core.RunID) error` identical in the interface (Task 1) and both impls; `ReclaimableRuns(ctx, before time.Time) ([]core.RunID, error)` identical in the interface (Task 2), Mem, SQLite wrapper, and its caller `SweepScratch` (Task 3); the generated `Queries.ReclaimableRuns(ctx, updatedAt string) ([]string, error)` is mapped to `[]core.RunID` in the wrapper. `Engine.ReclaimScratch(core.RunID) error` matches its call in `SweepScratch`. `runScratchJanitor(ctx, *supervisor.Supervisor, ttl, interval time.Duration, *slog.Logger)` matches both its definition and its call site (Task 4). `ScratchTTL`/`ScratchSweepInterval` names match across config, the janitor call, and the tests. ✓
