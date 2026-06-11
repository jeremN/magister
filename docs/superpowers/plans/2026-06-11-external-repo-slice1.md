# External-Repo Slice 1 (Provision from a Real Repo) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a run provision its per-run scratch repo as a read-only clone of a real, pre-existing git repo, with `step/<id>` branches forking from a pinned base commit, so the existing git-native join machinery produces real mergeable history over real code.

**Architecture:** Repo + base ref enter at submit (`?repo=&base=`); the API validates and pins `base` to a concrete SHA against the read-only source; `RunState` carries `Repo`/`Base` (persisted via goose migration `0003` for resume). A new `Workspace.Provision(runID, repo, base)` seam records the per-run spec; `GitManager.ensureRepo` branches on it — `git clone` the source when set, today's `init` + empty commit otherwise. All worktree/commit/merge/teardown machinery is unchanged; it just forks off a real base. No `repo` ⇒ today's behavior exactly.

**Tech Stack:** Go 1.22, stdlib `net/http`, `os/exec` git, SQLite (`modernc.org/sqlite`) + goose migrations + hand-edited sqlc output (sqlc not installed), `expr-lang` (unrelated, untouched).

**Spec:** `docs/superpowers/specs/2026-06-11-external-repo-design.md`

**Worktree:** Execute in an isolated worktree created via `superpowers:using-git-worktrees` (native `EnterWorktree` is broken in this repo; use `git worktree add .worktrees/external-repo -b external-repo`).

**Conventions:** single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. `rtk` NOT installed — run `go`/`git` directly. Run the WHOLE suite (`go test ./...`) between tasks, not just the touched package. `gofmt` is NOT hook-enforced — run `gofmt -l .` yourself each task and fix anything it lists.

---

## File Structure

**Created:**
- `internal/store/migrations/0003_run_repo.sql` — add `repo`/`base` columns to `runs`.
- `internal/workspace/provision.go` — `ResolveBase` (read-only repo/base validation + SHA pinning).
- `flows/external-repo.yaml` — zero-cost mock demo of an external-repo run.

**Modified:**
- `internal/core/store.go` — `RunState` += `Repo`, `Base`.
- `internal/core/ports.go` — `Workspace` interface += `Provision`.
- `internal/store/query.sql`, `internal/store/sqldb/query.sql.go`, `internal/store/sqldb/models.go`, `internal/store/sqlite.go` — carry `repo`/`base` through `CreateRun`/`GetRun`/`ListIncompleteRuns`.
- `internal/workspace/workspace.go` — `Manager.Provision` no-op.
- `internal/workspace/gitmanager.go` — per-run spec map, `Provision`, clone-or-init `ensureRepo`, `cloneBase`.
- `internal/engine/engine.go` — `Engine.Provision` delegator.
- `internal/supervisor/supervisor.go` — `Submit` += `repo,base`; provision in `Submit` and `ResumeAll`.
- `internal/api/handlers.go` — parse + validate `?repo=&base=`, thread to `Submit`.
- `internal/api/dto.go` — `runSnapshot` += `Scratch`.
- `cmd/cm/main.go` — `run --repo/--base` flags.
- `cmd/magisterd/main.go` — compute the runs root once; wire it to `GitManager.Root` and `Server.ScratchRoot`.

The in-memory store (`internal/store/mem.go`) needs **no change**: it stores `RunState` by whole-struct copy (`cp := r`; `cloneRun` does `out := *r`), so new scalar fields carry automatically.

---

## Task 1: Store — persist repo + base

**Files:**
- Create: `internal/store/migrations/0003_run_repo.sql`
- Modify: `internal/core/store.go` (RunState struct)
- Modify: `internal/store/query.sql` (CreateRun, GetRun, ListIncompleteRuns)
- Modify: `internal/store/sqldb/models.go` (Run struct)
- Modify: `internal/store/sqldb/query.sql.go` (hand-edit sqlc output)
- Modify: `internal/store/sqlite.go` (CreateRun, GetRun, LoadIncompleteRuns)
- Test: `internal/store/sqlite_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/sqlite_test.go`:

```go
func TestRunRepoBaseRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	want := core.RunState{
		ID: "r1", Name: "f", FlowYAML: "name: f\n", Status: core.RunPending,
		Concurrency: 1, Repo: "/abs/path/proj", Base: "abc123def",
	}
	if err := st.CreateRun(ctx, want); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.GetRun(ctx, "r1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Repo != want.Repo || got.Base != want.Base {
		t.Errorf("GetRun repo/base = %q/%q, want %q/%q", got.Repo, got.Base, want.Repo, want.Base)
	}

	inc, err := st.LoadIncompleteRuns(ctx)
	if err != nil {
		t.Fatalf("load incomplete: %v", err)
	}
	if len(inc) != 1 || inc[0].Repo != want.Repo || inc[0].Base != want.Base {
		t.Errorf("LoadIncompleteRuns repo/base = %+v, want repo/base %q/%q", inc, want.Repo, want.Base)
	}
}
```

Ensure the test file imports `context`, `path/filepath`, `testing`, and `concentus/internal/core` (add any missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestRunRepoBaseRoundTrip`
Expected: FAIL — compile error (`unknown field Repo in struct literal`) or, once the struct field is added, mismatch because the columns aren't persisted.

- [ ] **Step 3: Add the RunState fields**

In `internal/core/store.go`, extend `RunState`:

```go
type RunState struct {
	ID          RunID
	Name        string
	FlowYAML    string
	Status      RunStatus
	Concurrency int
	Err         string
	Repo        string // source repo for external-repo runs; empty = synthetic empty base
	Base        string // pinned base commit SHA; empty when Repo is empty
	Steps       []StepState
}
```

- [ ] **Step 4: Add the migration**

Create `internal/store/migrations/0003_run_repo.sql` (mirrors `0002`'s shape exactly):

```sql
-- +goose Up
-- +goose StatementBegin
ALTER TABLE runs ADD COLUMN repo TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN base TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE runs DROP COLUMN base;
ALTER TABLE runs DROP COLUMN repo;
-- +goose StatementEnd
```

- [ ] **Step 5: Update the SQL source (documentation of record)**

In `internal/store/query.sql`, update three queries to add the columns (append `repo, base` last):

```sql
-- name: CreateRun :exec
INSERT INTO runs (id, name, flow_yaml, status, concurrency, error, repo, base)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);
```
```sql
-- name: GetRun :one
SELECT id, name, flow_yaml, status, concurrency, error, repo, base FROM runs WHERE id = ?;
```
```sql
-- name: ListIncompleteRuns :many
SELECT id, name, flow_yaml, status, concurrency, error, repo, base
FROM runs WHERE status IN ('pending', 'running') ORDER BY created_at, id;
```

- [ ] **Step 6: Hand-edit the sqlc output**

In `internal/store/sqldb/models.go`, add the two fields to `Run` (append last):

```go
type Run struct {
	ID          string
	Name        string
	FlowYaml    string
	Status      string
	Concurrency int64
	Error       string
	CreatedAt   string
	UpdatedAt   string
	Repo        string
	Base        string
}
```

In `internal/store/sqldb/query.sql.go`, update `createRun`, `getRun`, and `listIncompleteRuns`.

`createRun`:
```go
const createRun = `-- name: CreateRun :exec
INSERT INTO runs (id, name, flow_yaml, status, concurrency, error, repo, base)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`

type CreateRunParams struct {
	ID          string
	Name        string
	FlowYaml    string
	Status      string
	Concurrency int64
	Error       string
	Repo        string
	Base        string
}

func (q *Queries) CreateRun(ctx context.Context, arg CreateRunParams) error {
	_, err := q.db.ExecContext(ctx, createRun,
		arg.ID,
		arg.Name,
		arg.FlowYaml,
		arg.Status,
		arg.Concurrency,
		arg.Error,
		arg.Repo,
		arg.Base,
	)
	return err
}
```

`getRun`:
```go
const getRun = `-- name: GetRun :one
SELECT id, name, flow_yaml, status, concurrency, error, repo, base FROM runs WHERE id = ?
`

type GetRunRow struct {
	ID          string
	Name        string
	FlowYaml    string
	Status      string
	Concurrency int64
	Error       string
	Repo        string
	Base        string
}

func (q *Queries) GetRun(ctx context.Context, id string) (GetRunRow, error) {
	row := q.db.QueryRowContext(ctx, getRun, id)
	var i GetRunRow
	err := row.Scan(
		&i.ID,
		&i.Name,
		&i.FlowYaml,
		&i.Status,
		&i.Concurrency,
		&i.Error,
		&i.Repo,
		&i.Base,
	)
	return i, err
}
```

`listIncompleteRuns`:
```go
const listIncompleteRuns = `-- name: ListIncompleteRuns :many
SELECT id, name, flow_yaml, status, concurrency, error, repo, base
FROM runs WHERE status IN ('pending', 'running') ORDER BY created_at, id
`

type ListIncompleteRunsRow struct {
	ID          string
	Name        string
	FlowYaml    string
	Status      string
	Concurrency int64
	Error       string
	Repo        string
	Base        string
}

func (q *Queries) ListIncompleteRuns(ctx context.Context) ([]ListIncompleteRunsRow, error) {
	rows, err := q.db.QueryContext(ctx, listIncompleteRuns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []ListIncompleteRunsRow
	for rows.Next() {
		var i ListIncompleteRunsRow
		if err := rows.Scan(
			&i.ID,
			&i.Name,
			&i.FlowYaml,
			&i.Status,
			&i.Concurrency,
			&i.Error,
			&i.Repo,
			&i.Base,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
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

- [ ] **Step 7: Wire the columns through the SQLite store**

In `internal/store/sqlite.go`, `CreateRun` passes the fields:
```go
func (s *SQLite) CreateRun(ctx context.Context, r core.RunState) error {
	return s.qw.CreateRun(ctx, sqldb.CreateRunParams{
		ID:          string(r.ID),
		Name:        r.Name,
		FlowYaml:    r.FlowYAML,
		Status:      string(r.Status),
		Concurrency: int64(r.Concurrency),
		Error:       r.Err,
		Repo:        r.Repo,
		Base:        r.Base,
	})
}
```

In `GetRun`, set the fields on the returned `RunState` (add the two lines):
```go
	return core.RunState{
		ID:          core.RunID(row.ID),
		Name:        row.Name,
		FlowYAML:    row.FlowYaml,
		Status:      core.RunStatus(row.Status),
		Concurrency: int(row.Concurrency),
		Err:         row.Error,
		Repo:        row.Repo,
		Base:        row.Base,
		Steps:       steps,
	}, nil
```

In `LoadIncompleteRuns`, likewise (add the two lines inside the loop's `append`):
```go
		out = append(out, core.RunState{
			ID:          core.RunID(r.ID),
			Name:        r.Name,
			FlowYAML:    r.FlowYaml,
			Status:      core.RunStatus(r.Status),
			Concurrency: int(r.Concurrency),
			Err:         r.Error,
			Repo:        r.Repo,
			Base:        r.Base,
			Steps:       steps,
		})
```

- [ ] **Step 8: Run the test to verify it passes**

Run: `go test ./internal/store/ -run TestRunRepoBaseRoundTrip -v`
Expected: PASS.

- [ ] **Step 9: Run the whole suite + gofmt**

Run: `go build ./... && gofmt -l . && go test ./...`
Expected: build clean, `gofmt -l` prints nothing, all packages PASS (the new column is additive; existing rows default to `''`).

- [ ] **Step 10: Commit**

```bash
git add internal/store internal/core/store.go
git commit -m "feat(store): persist run repo+base (migration 0003)"
```

---

## Task 2: Workspace — Provision seam + clone-or-init

**Files:**
- Modify: `internal/core/ports.go` (Workspace interface)
- Modify: `internal/workspace/workspace.go` (Manager.Provision no-op)
- Modify: `internal/workspace/gitmanager.go` (spec map, Provision, ensureRepo, cloneBase)
- Test: `internal/workspace/gitmanager_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/workspace/gitmanager_test.go`. The helper builds a throwaway source repo:

```go
// setupSourceRepo builds a committed fixture repo and returns its dir + HEAD sha.
func setupSourceRepo(t *testing.T) (string, string) {
	t.Helper()
	src := t.TempDir()
	gitOut(t, src, "init")
	gitOut(t, src, "config", "user.name", "fix")
	gitOut(t, src, "config", "user.email", "fix@example.com")
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("base content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, src, "add", "-A")
	gitOut(t, src, "commit", "-m", "base")
	return src, gitOut(t, src, "rev-parse", "HEAD")
}

func TestGitManagerProvisionClonesRealRepo(t *testing.T) {
	requireGit(t)
	src, sha := setupSourceRepo(t)

	m := &GitManager{Root: t.TempDir()}
	if err := m.Provision("r1", src, sha); err != nil {
		t.Fatalf("provision: %v", err)
	}
	wt, _, err := m.For("r1", &flow.Step{ID: "build", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "hello.txt"))
	if err != nil {
		t.Fatalf("base file missing in worktree (clone did not fork from base): %v", err)
	}
	if string(got) != "base content\n" {
		t.Errorf("base content = %q, want %q", got, "base content\n")
	}
	// The step branch forks from the cloned base, so its parent is the base sha.
	if parent := gitOut(t, wt, "rev-parse", "HEAD"); parent != sha {
		t.Errorf("worktree HEAD = %q, want pinned base %q", parent, sha)
	}
}

func TestGitManagerNoRepoUsesEmptyBase(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	// No Provision at all => synthetic empty base (today's behavior).
	wt, _, err := m.For("r1", &flow.Step{ID: "build", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected empty base, found a stray file (stat err=%v)", err)
	}
}

func TestGitManagerProvisionEmptyRepoUsesEmptyBase(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	if err := m.Provision("r1", "", ""); err != nil {
		t.Fatalf("provision empty: %v", err)
	}
	wt, _, err := m.For("r1", &flow.Step{ID: "build", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("empty repo should select the empty base, found a stray file (stat err=%v)", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/workspace/ -run TestGitManagerProvision`
Expected: FAIL — compile error (`m.Provision undefined`).

- [ ] **Step 3: Add Provision to the Workspace interface**

In `internal/core/ports.go`, add to the `Workspace` interface (after `Commit`, before `TeardownRun`):

```go
	// Provision records the run's source repo + pinned base commit SHA before any
	// step runs. An empty repo selects the synthetic empty-base scratch repo
	// (default; today's behavior). A no-op for the plain Manager (no git backing).
	Provision(runID RunID, repo, base string) error
```

- [ ] **Step 4: Add the Manager no-op**

In `internal/workspace/workspace.go`, add:

```go
// Provision is a no-op: the plain Manager has no git backing, so there is no repo
// to clone. External-repo runs require the GitManager.
func (m *Manager) Provision(core.RunID, string, string) error { return nil }
```

- [ ] **Step 5: Add the spec map, Provision, and clone-or-init to GitManager**

In `internal/workspace/gitmanager.go`:

Add the spec type and a field on `GitManager` (alongside `locks`):
```go
// repoSpec is a run's external-repo provisioning request: the source repo to
// clone and the pinned base commit SHA to check out. The zero value (empty repo)
// selects the synthetic empty-base scratch repo.
type repoSpec struct {
	repo string
	base string
}
```
Add to the `GitManager` struct, under `locks`:
```go
	// specs holds each run's provisioning request (set by Provision). Guarded by
	// mu, like locks. Absent/zero spec ⇒ synthetic empty base.
	specs map[core.RunID]repoSpec
```

Add the two methods:
```go
// Provision records a run's source repo + pinned base SHA before its first step.
// Empty repo ⇒ synthetic empty base. Idempotent; safe to call once per run.
func (m *GitManager) Provision(runID core.RunID, repo, base string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.specs == nil {
		m.specs = make(map[core.RunID]repoSpec)
	}
	m.specs[runID] = repoSpec{repo: repo, base: base}
	return nil
}

func (m *GitManager) specFor(runID core.RunID) repoSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.specs[runID] // zero value (empty repo) if never provisioned
}
```

Change `For` to look up the spec and pass it to `ensureRepo`:
```go
	base := m.baseDir(runID)
	if err := m.ensureRepo(base, m.specFor(runID)); err != nil {
		return "", nil, err
	}
```

Replace `ensureRepo` with the clone-or-init version (note the param rename: the dir is `baseDir`, the spec carries the base *ref*):
```go
// ensureRepo lazily provisions the per-run base repo. With a spec.repo set it
// clones the source at the pinned base SHA; otherwise it inits an empty base
// with one empty commit. Idempotent and self-healing: a present .git is reused
// (resume), and a crash between `git init` and the first commit (unborn HEAD) is
// healed by re-issuing the empty commit. A clone is born with a real HEAD, so the
// heal applies only to the empty-base path.
func (m *GitManager) ensureRepo(baseDir string, spec repoSpec) error {
	if _, err := os.Stat(filepath.Join(baseDir, ".git")); err != nil {
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			return err
		}
		if spec.repo != "" {
			return m.cloneBase(baseDir, spec)
		}
		if _, err := m.run(baseDir, "init"); err != nil {
			return err
		}
		if _, err := m.run(baseDir, "config", "user.name", m.name()); err != nil {
			return err
		}
		if _, err := m.run(baseDir, "config", "user.email", m.email()); err != nil {
			return err
		}
	}
	if spec.repo != "" {
		return nil // a clone is born with a real HEAD; nothing to heal
	}
	if _, err := m.run(baseDir, "rev-parse", "--verify", "-q", "HEAD"); err == nil {
		return nil // base commit already present
	}
	if _, err := m.run(baseDir,
		"-c", "user.name="+m.name(), "-c", "user.email="+m.email(),
		"commit", "--allow-empty", "-m", "base"); err != nil {
		return err
	}
	return nil
}

// cloneBase clones the source repo into baseDir and detaches HEAD at the pinned
// base SHA, so step worktrees fork from that exact commit. The source is read
// only (clone only reads it). Identity is set after clone — a clone does not copy
// the source's local user.name/user.email, which merge commits need.
func (m *GitManager) cloneBase(baseDir string, spec repoSpec) error {
	if _, err := m.run("", "clone", spec.repo, baseDir); err != nil {
		return err
	}
	if _, err := m.run(baseDir, "checkout", "--detach", spec.base); err != nil {
		return err
	}
	if _, err := m.run(baseDir, "config", "user.name", m.name()); err != nil {
		return err
	}
	if _, err := m.run(baseDir, "config", "user.email", m.email()); err != nil {
		return err
	}
	return nil
}
```

Note: `m.run("", "clone", ...)` runs git with the process CWD; `spec.repo` and `baseDir` are absolute, so CWD is irrelevant. `MkdirAll(baseDir)` then `git clone` into that empty dir is allowed.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/workspace/ -run 'TestGitManager(Provision|NoRepo)' -v`
Expected: PASS (all three new tests).

- [ ] **Step 7: Run the whole suite -race + gofmt**

Run: `gofmt -l . && go vet ./... && go test -race ./...`
Expected: `gofmt -l` prints nothing, vet clean, all packages PASS — including the existing `gitmanager_test.go` tests (the `var _ core.Workspace = (*GitManager)(nil)` assertion now also forces `Provision` to exist) and all engine/supervisor tests (their `Manager`/`teardownSpy` inherit the no-op `Provision`).

- [ ] **Step 8: Commit**

```bash
git add internal/core/ports.go internal/workspace
git commit -m "feat(workspace): clone a real repo as the per-run base (Provision seam)"
```

---

## Task 3: Engine + Supervisor — thread repo/base and provision

**Files:**
- Modify: `internal/engine/engine.go` (Engine.Provision delegator)
- Modify: `internal/supervisor/supervisor.go` (Submit signature; provision in Submit + ResumeAll)
- Modify: `internal/api/handlers.go` (update the Submit call site to compile — placeholder args, enriched in Task 5)
- Modify: `internal/supervisor/supervisor_test.go`, `cmd/magisterd/e2e_test.go` (update Submit call sites)
- Test: `internal/supervisor/supervisor_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/supervisor/supervisor_test.go`. A spy workspace records `Provision` calls so we can assert the wiring without real git. The file already imports `context`, `testing`, `time`, `core`, `engine`, `event`, `executor`, `flow`, `gate`, `join`, `store`, `workspace` and defines `testEngine`/`RegistryApprover`/`waitForStatus` — **add `"sync"` to the imports** for the spy mutex. The engine is built inline here (not `testEngine`) because we need a custom `WS`:

```go
// provisionSpy records Provision calls while delegating the rest to a real Manager.
type provisionSpy struct {
	*workspace.Manager
	mu  sync.Mutex
	got []string // "repo|base" per call
}

func (p *provisionSpy) Provision(id core.RunID, repo, base string) error {
	p.mu.Lock()
	p.got = append(p.got, repo+"|"+base)
	p.mu.Unlock()
	return nil
}

func spyEngine(t *testing.T, st core.Store, reg *ApprovalRegistry, spy *provisionSpy) *engine.Engine {
	t.Helper()
	return &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    spy,
		Gate:  &gate.Evaluator{Approver: &RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: core.SystemClock{},
	}
}

// autoStepFlow is a one-step flow with an auto gate, so the run completes without approval.
const autoStepYAML = "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"true\" } }\n"

func TestSubmitProvisionsAndPersistsRepoBase(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	spy := &provisionSpy{Manager: &workspace.Manager{Root: t.TempDir()}}
	sup := New(spyEngine(t, st, reg, spy), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })

	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
	}}
	id, err := sup.Submit(context.Background(), f, autoStepYAML, "/abs/proj", "deadbeef")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitForStatus(t, st, id, core.RunSucceeded)

	rs, err := st.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Repo != "/abs/proj" || rs.Base != "deadbeef" {
		t.Errorf("persisted repo/base = %q/%q, want /abs/proj/deadbeef", rs.Repo, rs.Base)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.got) != 1 || spy.got[0] != "/abs/proj|deadbeef" {
		t.Errorf("Provision calls = %v, want [/abs/proj|deadbeef]", spy.got)
	}
}

func TestResumeAllProvisions(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	spy := &provisionSpy{Manager: &workspace.Manager{Root: t.TempDir()}}
	sup := New(spyEngine(t, st, reg, spy), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })

	// Seed an incomplete run carrying repo/base, as if persisted before a crash.
	if err := st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Name: "f", FlowYAML: autoStepYAML, Status: core.RunRunning,
		Repo: "/abs/proj", Base: "deadbeef",
	}); err != nil {
		t.Fatal(err)
	}
	if err := sup.ResumeAll(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.got) != 1 || spy.got[0] != "/abs/proj|deadbeef" {
		t.Errorf("Provision calls on resume = %v, want [/abs/proj|deadbeef]", spy.got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/supervisor/ -run TestSubmitProvisions`
Expected: FAIL — compile error (`too many arguments to sup.Submit` / `eng has no field Provision` via the spy path).

- [ ] **Step 3: Add the Engine.Provision delegator**

In `internal/engine/engine.go`, near `Run`/`Resume`:

```go
// Provision records a run's source repo + pinned base SHA with the workspace
// before the run starts (see core.Workspace.Provision). Empty repo selects the
// synthetic empty-base scratch repo (default; today's behavior).
func (e *Engine) Provision(runID core.RunID, repo, base string) error {
	return e.WS.Provision(runID, repo, base)
}
```

- [ ] **Step 4: Thread repo/base through Submit**

In `internal/supervisor/supervisor.go`, change `Submit`:

```go
// Submit persists a pending run, provisions its workspace (repo+base; empty repo
// = synthetic base), and starts it. Validating the flow and the repo/base is the
// caller's job (the API handler does it before calling Submit).
func (s *Supervisor) Submit(ctx context.Context, f *flow.Flow, flowYAML, repo, base string) (core.RunID, error) {
	id := NewRunID()
	if err := s.store.CreateRun(ctx, core.RunState{
		ID: id, Name: f.Name, FlowYAML: flowYAML, Status: core.RunPending,
		Concurrency: f.Concurrency, Repo: repo, Base: base,
	}); err != nil {
		return "", fmt.Errorf("create run: %w", err)
	}
	if err := s.engine.Provision(id, repo, base); err != nil {
		return "", fmt.Errorf("provision run: %w", err)
	}
	s.start(id, func(runCtx context.Context) error { return s.engine.Run(runCtx, id, f) })
	return id, nil
}
```

- [ ] **Step 5: Provision on resume**

In `internal/supervisor/supervisor.go` `ResumeAll`, provision before starting (replace the `s.resetIncompleteSteps(...)` + `s.start(...)` tail of the loop):

```go
		s.resetIncompleteSteps(ctx, rs)
		rs := rs
		if err := s.engine.Provision(rs.ID, rs.Repo, rs.Base); err != nil {
			s.logger().Error("resume: provision run", "run", rs.ID, "err", err)
			continue
		}
		s.start(rs.ID, func(runCtx context.Context) error { return s.engine.Resume(runCtx, rs, f) })
```

- [ ] **Step 6: Update the other Submit call sites to compile**

In `internal/api/handlers.go`, line 44, temporarily pass empty repo/base (Task 5 replaces this):
```go
	id, err := s.Sup.Submit(r.Context(), f, string(body), "", "")
```

In `cmd/magisterd/e2e_test.go` (~line 204), update the call:
```go
	id, err := sup.Submit(context.Background(), f, flowYAML, "", "")
```

In `internal/supervisor/supervisor_test.go`, update the two existing calls at the old lines (38, 59) to add `"", ""`:
```go
	id, err := sup.Submit(context.Background(), f, "name: f\n", "", "")
	...
	id, _ := sup.Submit(context.Background(), f, "x", "", "")
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/supervisor/ -run 'TestSubmitProvisions|TestResumeAllProvisions' -v`
Expected: PASS (both — Submit and ResumeAll provision).

- [ ] **Step 8: Run the whole suite -race + gofmt**

Run: `gofmt -l . && go vet ./... && go test -race ./...`
Expected: `gofmt -l` clean, vet clean, all packages PASS (every `Submit` caller now compiles; behavior unchanged for empty repo/base).

- [ ] **Step 9: Commit**

```bash
git add internal/engine/engine.go internal/supervisor internal/api/handlers.go cmd/magisterd/e2e_test.go
git commit -m "feat(engine,supervisor): provision run workspace from repo+base"
```

---

## Task 4: workspace.ResolveBase — read-only repo/base validation + SHA pinning

**Files:**
- Create: `internal/workspace/provision.go`
- Test: `internal/workspace/provision_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/workspace/provision_test.go`. It reuses `requireGit` and `setupSourceRepo` from `gitmanager_test.go` (same `workspace` package), so it only needs `testing`:

```go
package workspace

import "testing"

func TestResolveBaseDefaultsToHEAD(t *testing.T) {
	requireGit(t)
	src, sha := setupSourceRepo(t)
	got, err := ResolveBase(src, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != sha {
		t.Errorf("ResolveBase(HEAD) = %q, want %q", got, sha)
	}
}

func TestResolveBasePinsRef(t *testing.T) {
	requireGit(t)
	src, sha := setupSourceRepo(t)
	got, err := ResolveBase(src, "HEAD")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != sha {
		t.Errorf("ResolveBase = %q, want pinned %q", got, sha)
	}
}

func TestResolveBaseRejectsNonRepo(t *testing.T) {
	requireGit(t)
	if _, err := ResolveBase(t.TempDir(), ""); err == nil {
		t.Error("expected error for a non-git directory")
	}
}

func TestResolveBaseRejectsUnknownRef(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t)
	if _, err := ResolveBase(src, "no-such-branch"); err == nil {
		t.Error("expected error for an unresolvable ref")
	}
}

func TestResolveBaseRejectsRelativePath(t *testing.T) {
	requireGit(t)
	if _, err := ResolveBase("relative/path", ""); err == nil {
		t.Error("expected error for a relative repo path")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/workspace/ -run TestResolveBase`
Expected: FAIL — `undefined: ResolveBase`.

- [ ] **Step 3: Implement ResolveBase**

Create `internal/workspace/provision.go`:

```go
package workspace

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// ResolveBase validates that repoDir is a git repo and resolves ref (default
// "HEAD") to a concrete commit SHA, reading the source repo only — it never
// writes. Used at submit time so a bad repo/base fails the request, not a running
// step. The returned SHA is what gets pinned and cloned, so a later resume forks
// from the same commit even if the source branch advanced.
func ResolveBase(repoDir, ref string) (string, error) {
	if repoDir == "" {
		return "", fmt.Errorf("empty repo path")
	}
	if !filepath.IsAbs(repoDir) {
		return "", fmt.Errorf("repo path must be absolute: %q", repoDir)
	}
	if _, err := gitRead(repoDir, "rev-parse", "--git-dir"); err != nil {
		return "", fmt.Errorf("not a git repo: %q", repoDir)
	}
	if ref == "" {
		ref = "HEAD"
	}
	out, err := gitRead(repoDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("base %q does not resolve in %q", ref, repoDir)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitRead runs a read-only git command in dir. Args are orchestrator-controlled
// fixed subcommands plus a caller-supplied ref/path; no shell is involved.
func gitRead(dir string, args ...string) ([]byte, error) {
	// #nosec G204 -- read-only git on a caller-named local repo, invoked without a shell.
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/workspace/ -run TestResolveBase -v`
Expected: PASS (all five).

- [ ] **Step 5: Run the whole suite + gofmt**

Run: `gofmt -l . && go test ./internal/workspace/`
Expected: clean + PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/workspace/provision.go internal/workspace/provision_test.go
git commit -m "feat(workspace): ResolveBase validates repo + pins base to a SHA"
```

---

## Task 5: API — accept and validate ?repo=&base=

**Files:**
- Modify: `internal/api/handlers.go` (handleCreateRun)
- Test: `internal/api/handlers_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/api/handlers_test.go`. This file already defines `testServer(t) → (*httptest.Server, *supervisor.Supervisor, core.Store)`, the const `oneStepFlow` (a one-step mock flow with an auto gate), and `waitForStatus`. **Add imports** `"context"`, `"net/url"`, `"os"`, `"os/exec"`, `"path/filepath"`, `"strings"` (the file already has `bytes`, `encoding/json`, `io`, `net/http`, `net/http/httptest`, `testing`, `time`, and the concentus packages). Add a local git-fixture helper plus two tests. Note the engine here uses the plain `workspace.Manager`, so `Provision` is a harmless no-op — this task verifies the API *plumbing* (validate + pin + persist), while the clone behavior is covered by Task 2.

```go
// setupAPISourceRepo builds a committed fixture repo (skips if git absent).
func setupAPISourceRepo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init")
	run("config", "user.name", "fix")
	run("config", "user.email", "fix@example.com")
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "base")
	return src, run("rev-parse", "HEAD")
}

func TestCreateRunWithRepoPinsBase(t *testing.T) {
	src, sha := setupAPISourceRepo(t)
	hs, _, st := testServer(t)

	resp, err := http.Post(
		hs.URL+"/v1/runs?repo="+url.QueryEscape(src)+"&base=HEAD",
		"application/x-yaml", bytes.NewBufferString(oneStepFlow))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, b)
	}
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)

	waitForStatus(t, st, rr.ID, core.RunSucceeded)
	rs, err := st.GetRun(context.Background(), rr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Repo != src || rs.Base != sha {
		t.Errorf("persisted repo/base = %q/%q, want %q/%q", rs.Repo, rs.Base, src, sha)
	}
}

func TestCreateRunRejectsBadRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	hs, _, _ := testServer(t)
	resp, err := http.Post(
		hs.URL+"/v1/runs?repo=/no/such/repo",
		"application/x-yaml", bytes.NewBufferString(oneStepFlow))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, b)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestCreateRun`
Expected: FAIL — `repo` ignored, so `rs.Repo` is empty (first test), and a bad repo is currently accepted as a normal run → 201 not 400 (second test).

- [ ] **Step 3: Parse + validate the query params**

In `internal/api/handlers.go`, add the import `"concentus/internal/workspace"`, then update `handleCreateRun` (replace the `Submit` call and add the block above it):

```go
	repo := r.URL.Query().Get("repo")
	base := ""
	if repo != "" {
		sha, err := workspace.ResolveBase(repo, r.URL.Query().Get("base"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "repo: "+err.Error())
			return
		}
		base = sha
	}
	id, err := s.Sup.Submit(r.Context(), f, string(body), repo, base)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, runResponse{ID: id})
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/api/ -run TestCreateRun -v`
Expected: PASS (both).

- [ ] **Step 5: Run the whole suite + gofmt**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: clean + all PASS. Existing `POST /v1/runs` tests (no `repo`) still 201 — `repo == ""` skips validation entirely.

- [ ] **Step 6: Commit**

```bash
git add internal/api/handlers.go internal/api/handlers_test.go
git commit -m "feat(api): accept ?repo=&base=, validate + pin at submit"
```

---

## Task 6: cm — run --repo / --base flags

**Files:**
- Modify: `cmd/cm/main.go` (run command: arg parse + URL query)
- Test: `cmd/cm/main_test.go`

- [ ] **Step 1: Write the failing test**

Add to `cmd/cm/main_test.go`. This file already defines `fakeAPI(t, status, body, record *http.Request)` (records the request into `*record`) and imports `bytes`, `net/http`, `os`, `strings`, `testing`. The test reads back `record.URL.Query()`:

```go
func TestRunPassesRepoBaseAsQuery(t *testing.T) {
	var got http.Request
	srv := fakeAPI(t, http.StatusCreated, `{"id":"r1"}`, &got)
	defer srv.Close()

	flowPath := t.TempDir() + "/f.yaml"
	if err := os.WriteFile(flowPath, []byte("name: f\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code := dispatch([]string{"run", flowPath, "--repo", "/abs/proj", "--base", "main"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if got.URL.Query().Get("repo") != "/abs/proj" || got.URL.Query().Get("base") != "main" {
		t.Errorf("query repo/base = %q/%q, want /abs/proj/main",
			got.URL.Query().Get("repo"), got.URL.Query().Get("base"))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/cm/ -run TestRunPassesRepoBase`
Expected: FAIL — `--repo`/`--base` are swallowed as `path` (the run command's arg loop treats any non-`--watch` arg as the flow path), so the query is absent and the wrong path is read.

- [ ] **Step 3: Parse the flags and build the query**

In `cmd/cm/main.go`, add `"net/url"` to the imports. Replace the arg-parse loop in `run` (the `for _, a := range args` block) with:

```go
	watch := false
	var path, repo, base string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--watch":
			watch = true
		case "--repo":
			i++
			if i < len(args) {
				repo = args[i]
			}
		case "--base":
			i++
			if i < len(args) {
				base = args[i]
			}
		default:
			path = args[i]
		}
	}
	if path == "" {
		fmt.Fprintln(out, "usage: cm run <flow.yaml> [--repo <path>] [--base <ref>] [--watch]")
		return 2
	}
```

Then build the endpoint with the query (replace the `c.http.Post(c.base+"/v1/runs", ...)` line):
```go
	endpoint := c.base + "/v1/runs"
	if repo != "" {
		q := url.Values{}
		q.Set("repo", repo)
		if base != "" {
			q.Set("base", base)
		}
		endpoint += "?" + q.Encode()
	}
	resp, err := c.http.Post(endpoint, "application/x-yaml", bytes.NewReader(body))
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/cm/ -run TestRunPassesRepoBase -v`
Expected: PASS.

- [ ] **Step 5: Run the whole suite + gofmt**

Run: `gofmt -l . && go test ./...`
Expected: clean + all PASS (existing `cm run` tests unaffected — no `--repo` ⇒ no query).

- [ ] **Step 6: Commit**

```bash
git add cmd/cm/main.go cmd/cm/main_test.go
git commit -m "feat(cm): run --repo/--base flags"
```

---

## Task 7: Surface the scratch base path in run GET

**Files:**
- Modify: `internal/api/dto.go` (runSnapshot += Scratch; snapshotFromState signature)
- Modify: `internal/api/handlers.go` (Server.ScratchRoot field; handleGetRun)
- Modify: `cmd/magisterd/main.go` (compute runs root once; wire to both GitManager and Server)
- Test: `internal/api/handlers_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/api/handlers_test.go`. Use `newServerOnly` (returns the `*Server`, so we can set `ScratchRoot` before wrapping it in an `httptest.Server`). The `context`/`filepath`/`strings` imports were added in Task 5.

```go
func TestGetRunSurfacesScratchPathForExternalRepo(t *testing.T) {
	srv, _, st := newServerOnly(t)
	srv.ScratchRoot = "/var/runs"
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	if err := st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Name: "f", Status: core.RunSucceeded, Repo: "/abs/proj", Base: "abc",
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(hs.URL + "/v1/runs/r1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var snap runSnapshot
	json.NewDecoder(resp.Body).Decode(&snap)
	if snap.Scratch != filepath.Join("/var/runs", "r1", "base") {
		t.Errorf("scratch = %q, want /var/runs/r1/base", snap.Scratch)
	}
}

func TestGetRunOmitsScratchForSyntheticRun(t *testing.T) {
	srv, _, st := newServerOnly(t)
	srv.ScratchRoot = "/var/runs"
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	if err := st.CreateRun(context.Background(), core.RunState{
		ID: "r2", Name: "f", Status: core.RunSucceeded, // no Repo
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(hs.URL + "/v1/runs/r2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "scratch") {
		t.Errorf("synthetic run should omit scratch, body=%s", body)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestGetRun`
Expected: FAIL — `srv.ScratchRoot` undefined; `scratch` never emitted.

- [ ] **Step 3: Add the DTO field and pass the scratch path**

In `internal/api/dto.go`, add to `runSnapshot`:
```go
	Scratch     string     `json:"scratch,omitempty"`
```
Change `snapshotFromState` to accept the scratch path and set it:
```go
func snapshotFromState(rs core.RunState, scratch string) runSnapshot {
	out := runSnapshot{
		ID: rs.ID, Name: rs.Name, Status: string(rs.Status),
		Concurrency: rs.Concurrency, Error: rs.Err, Scratch: scratch,
		Steps: make([]stepDTO, 0, len(rs.Steps)),
	}
	// ... rest unchanged ...
}
```

- [ ] **Step 4: Add the Server field and compute the path in handleGetRun**

In `internal/api/handlers.go`, add to `Server` (after `ShutdownTimeout`):
```go
	// ScratchRoot is the per-run scratch repo root (= GitManager.Root). When set and
	// a run targets a real repo, GET /v1/runs/{id} surfaces <root>/<id>/base so the
	// caller can find the result history. Empty disables the field.
	ScratchRoot string
```
Add `"path/filepath"` to the imports. Update `handleGetRun`:
```go
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	rs, err := s.Store.GetRun(r.Context(), core.RunID(r.PathValue("id")))
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown run")
		return
	}
	scratch := ""
	if rs.Repo != "" && s.ScratchRoot != "" {
		scratch = filepath.Join(s.ScratchRoot, string(rs.ID), "base")
	}
	writeJSON(w, http.StatusOK, snapshotFromState(rs, scratch))
}
```

- [ ] **Step 5: Wire the runs root once in main**

In `cmd/magisterd/main.go`, compute the root once and pass to both. Replace the inline `WS: &workspace.GitManager{...}` and the `srv := &api.Server{...}` construction:
```go
	runsRoot := filepath.Join(filepath.Dir(cfg.DBPath), "runs")
	eng := &engine.Engine{
		Execs: agents(),
		WS:    &workspace.GitManager{Root: runsRoot},
		Gate:  &gate.Evaluator{Approver: &supervisor.RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: core.SystemClock{}, Log: log,
	}
```
and:
```go
	srv := &api.Server{Sup: sup, Store: st, Bus: bus, Log: log, BearerToken: cfg.BearerToken, ShutdownTimeout: cfg.ShutdownTimeout, ScratchRoot: runsRoot}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/api/ -run TestGetRun -v`
Expected: PASS. (Fix any other in-package callers of `snapshotFromState` to pass `""` — grep `snapshotFromState(` to be sure.)

- [ ] **Step 7: Run the whole suite -race + gofmt + vet**

Run: `gofmt -l . && go vet ./... && go test -race ./...`
Expected: clean + all PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/api cmd/magisterd/main.go
git commit -m "feat(api): surface scratch base path in run GET for external-repo runs"
```

---

## Task 8: Demo flow + manual proof + docs + final suite

**Files:**
- Create: `flows/external-repo.yaml`
- Modify: `.claude/skills/running-the-orchestrator/SKILL.md` (note the external-repo run)

- [ ] **Step 1: Add the demo flow**

Create `flows/external-repo.yaml` (same shape as `git-native-merge.yaml`; provisioning comes from `--repo`/`--base`, not the YAML — the flow stays repo-agnostic):

```yaml
name: external-repo
concurrency: 2

# External-repo demo: run with `cm run flows/external-repo.yaml --repo <abs-repo> --base <ref>`.
# The per-run scratch repo is a read-only CLONE of <repo> at <ref>; the two isolated
# upstreams commit branches that fork from that real base, and the `merge` join does a
# real git merge over it. Runs zero-cost with `mock` (writes a unique <id>.out.md per step).
# Join + all its upstreams must be workspace: isolated; auto gates keep it non-blocking.
steps:
  - id: build-api
    agent: mock
    role: implementer
    workspace: isolated
    prompt: "build the api"
    gate: { policy: auto, verifier: { command: "true" } }

  - id: build-ui
    agent: mock
    role: implementer
    workspace: isolated
    prompt: "build the ui"
    gate: { policy: auto, verifier: { command: "true" } }

  - id: integrate
    needs: [build-api, build-ui]
    workspace: isolated
    join:
      strategy: merge
    gate: { policy: auto, verifier: { command: "true" } }
```

- [ ] **Step 2: Manual SSE proof (the slice convention)**

Follow `.claude/skills/running-the-orchestrator/SKILL.md` to build and start `magisterd`, then:

```bash
# A throwaway source repo with real committed history:
SRC=$(mktemp -d)
git -C "$SRC" init -q
git -C "$SRC" config user.name demo && git -C "$SRC" config user.email demo@x
echo "base readme" > "$SRC/README.md"
git -C "$SRC" add -A && git -C "$SRC" commit -qm "base"

# Submit against the real repo; watch live SSE:
cm run flows/external-repo.yaml --repo "$SRC" --base HEAD --watch
```

Then confirm the run produced real history forked off the real base. Get the scratch path from the run GET (`cm get <id>` → `scratch`), and:

```bash
SCRATCH=<scratch path from cm get>
git -C "$SCRATCH" log --oneline --graph step/integrate
git -C "$SCRATCH" show step/integrate:README.md   # the cloned base file is present
git -C "$SCRATCH" cat-file -p step/integrate | grep -c parent  # 2 parents = real merge commit
```

Expected: `step/integrate` is a 2-parent merge commit whose tree contains `README.md` (from the cloned base) **and** `build-api.out.md` + `build-ui.out.md` (from the two upstreams). Capture the output in the handoff.

- [ ] **Step 3: Document the external-repo run in the run skill**

In `.claude/skills/running-the-orchestrator/SKILL.md`, add a short subsection under the existing git-native section: how to run a flow against a real repo (`cm run <flow> --repo <abs> --base <ref>`), that the source is cloned read-only, and how to find the result via the `scratch` field of `cm get <id>`.

- [ ] **Step 4: Final whole-suite gate**

Run: `gofmt -l . && go vet ./... && go test -race ./...`
Expected: `gofmt -l` prints nothing, vet clean, all 13 test-bearing packages PASS.

- [ ] **Step 5: Commit**

```bash
git add flows/external-repo.yaml .claude/skills/running-the-orchestrator/SKILL.md
git commit -m "docs(external-repo): demo flow + run-skill note"
```

---

## Done criteria

- A run submitted with `--repo <abs> --base <ref>` provisions a read-only clone of the real repo; `step/<id>` branches fork from the pinned base SHA; the `merge` join produces a 2-parent merge commit containing the base's files plus the steps' work.
- A run with no `--repo` behaves exactly as today (synthetic empty base); all pre-existing tests stay green.
- `repo`/`base` are persisted (migration `0003`) and a resumed run re-provisions from them.
- A bad repo path or unresolvable base fails submission with `400`.
- `gofmt -l` clean, `go vet ./...` clean, `go test -race ./...` green across all packages.

## Carried follow-ups (out of scope; note in the handoff)

- Slice 2 (push result back) and Slice 3 (open PR) — the north star, separate cycles.
- A `repo` pointing at a subdir of a git repo: `ResolveBase` accepts it (rev-parse finds the parent), but `git clone <subdir>` would fail at run time. Document "pass the repo root"; resolve `--show-toplevel` in a later hardening pass if it bites.
- `select`-on-resume stale paths (pre-existing M5b); `merge`+`escalate` non-`ConflictError` re-run path untested (pre-existing) — unchanged here.
