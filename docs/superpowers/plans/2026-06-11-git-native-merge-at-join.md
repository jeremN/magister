# git-native merge-at-join Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the fan-in handoff git-native — isolated steps commit their worktree to a branch, downstream steps receive branch refs, and joins perform a real `git merge` where `on_conflict` is a true merge-conflict policy.

**Architecture:** `core.Artifact` gains `Branch`/`Commit` (superset; `Path` always set). The engine commits each successful isolated non-join step's worktree via a new `Workspace.Commit`; joins self-commit. `merge` does sequential `git merge`; `synthesize` merges then has its arbiter resolve only conflicts; `select` forwards the winner's branch by ref. A `merge` conflict with `on_conflict: escalate` runs an arbiter-resolves → human-approves ladder in the engine. Path-based joins (`.candidates/` staging, manifest `Merge`) are removed.

**Tech Stack:** Go 1.22, `git` (shelled out, no shell), goose migrations + hand-edited sqlc output (`sqlc` is not installed), `modernc.org/sqlite`.

**Spec:** `docs/superpowers/specs/2026-06-11-git-native-merge-at-join-design.md`

---

## File Structure

| File | Responsibility | Action |
|------|----------------|--------|
| `internal/core/ports.go` | `Artifact` fields; `Workspace.Commit` | Modify |
| `internal/store/migrations/0002_artifact_refs.sql` | add `branch`/`commit_sha` columns | Create |
| `internal/store/query.sql` | insert/list artifacts with new cols | Modify |
| `internal/store/sqldb/models.go` | `Artifact` struct (+2 fields) | Modify (hand-edit) |
| `internal/store/sqldb/query.sql.go` | InsertArtifact / ListArtifactsForRun | Modify (hand-edit) |
| `internal/store/sqlite.go` | load/save carry Branch/Commit | Modify |
| `internal/workspace/gitmanager.go` | persistent git identity; `Commit` | Modify |
| `internal/workspace/workspace.go` | `Manager.Commit` no-op | Modify |
| `internal/engine/engine.go` | commit-on-success; escalate ladder | Modify |
| `internal/join/git.go` | gitCmd, branches, conflicts, CommittedResult, ConflictError, ResolveConflictPrompt | Create |
| `internal/join/join.go` | git-native `Merge`; drop manifest/stageCandidates | Modify |
| `internal/join/synthesize.go` | git-native `Synthesize` | Modify |
| `internal/join/select.go` | by-ref `Select`; no staging | Modify |
| `internal/flow/validate.go` | isolated-join / merge+escalate / retry rules | Modify |
| demo flow + `.claude/skills/running-the-orchestrator/` | conflict demo + manual proof | Modify |

---

## Task 1: Artifact superset + store persistence

**Files:**
- Modify: `internal/core/ports.go` (Artifact struct)
- Create: `internal/store/migrations/0002_artifact_refs.sql`
- Modify: `internal/store/query.sql`, `internal/store/sqldb/models.go`, `internal/store/sqldb/query.sql.go`, `internal/store/sqlite.go`
- Test: `internal/store/sqlite_test.go`

- [ ] **Step 1: Add the fields to `core.Artifact`**

In `internal/core/ports.go`, replace the `Artifact` struct:

```go
// Artifact points at something a step produced on disk. The filesystem is the
// source of truth for handoffs; artifacts are just pointers. For a committed
// isolated step, Branch/Commit also name the git ref that carries the work, so
// fan-in joins can `git merge` it. Branch/Commit are empty for shared steps and
// the mock executor (path-only).
type Artifact struct {
	StepID string
	Path   string
	Branch string
	Commit string
}
```

- [ ] **Step 2: Write the failing store round-trip test**

In `internal/store/sqlite_test.go`, add:

```go
func TestSQLiteArtifactRefsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	if err := s.CreateRun(ctx, core.RunState{ID: "r1", Name: "f", FlowYAML: "x", Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveStepTransition(ctx,
		core.StepState{RunID: "r1", StepID: "a", Status: core.StepSucceeded, Attempt: 1,
			Artifacts: []core.Artifact{{StepID: "a", Path: "/w/a.md", Branch: "step/a", Commit: "deadbeef"}}},
		nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	arts := got.Steps[0].Artifacts
	if len(arts) != 1 || arts[0].Branch != "step/a" || arts[0].Commit != "deadbeef" {
		t.Fatalf("artifact refs did not round-trip: %+v", arts)
	}
}
```

- [ ] **Step 3: Run it — verify it fails**

Run: `go test ./internal/store/ -run TestSQLiteArtifactRefsRoundTrip`
Expected: FAIL — Branch/Commit come back empty (columns and mapping don't exist yet).

- [ ] **Step 4: Add the migration**

Create `internal/store/migrations/0002_artifact_refs.sql`:

```sql
-- +goose Up
-- +goose StatementBegin
ALTER TABLE artifacts ADD COLUMN branch TEXT NOT NULL DEFAULT '';
ALTER TABLE artifacts ADD COLUMN commit_sha TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE artifacts DROP COLUMN commit_sha;
ALTER TABLE artifacts DROP COLUMN branch;
-- +goose StatementEnd
```

- [ ] **Step 5: Update the query source (`internal/store/query.sql`)**

Replace the `InsertArtifact` and `ListArtifactsForRun` queries:

```sql
-- name: InsertArtifact :exec
INSERT INTO artifacts (run_id, step_id, path, branch, commit_sha) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (run_id, step_id, path) DO NOTHING;

-- name: ListArtifactsForRun :many
SELECT run_id, step_id, path, branch, commit_sha FROM artifacts WHERE run_id = ? ORDER BY step_id, path;
```

- [ ] **Step 6: Hand-edit the generated sqlc output**

`sqlc` is not installed, so edit the generated files directly to match the queries.

In `internal/store/sqldb/models.go`, the `Artifact` struct:

```go
type Artifact struct {
	RunID     string
	StepID    string
	Path      string
	Branch    string
	CommitSha string
}
```

In `internal/store/sqldb/query.sql.go`, replace the `insertArtifact` block:

```go
const insertArtifact = `-- name: InsertArtifact :exec
INSERT INTO artifacts (run_id, step_id, path, branch, commit_sha) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (run_id, step_id, path) DO NOTHING
`

type InsertArtifactParams struct {
	RunID     string
	StepID    string
	Path      string
	Branch    string
	CommitSha string
}

func (q *Queries) InsertArtifact(ctx context.Context, arg InsertArtifactParams) error {
	_, err := q.db.ExecContext(ctx, insertArtifact, arg.RunID, arg.StepID, arg.Path, arg.Branch, arg.CommitSha)
	return err
}
```

and the `listArtifactsForRun` block (const + scan):

```go
const listArtifactsForRun = `-- name: ListArtifactsForRun :many
SELECT run_id, step_id, path, branch, commit_sha FROM artifacts WHERE run_id = ? ORDER BY step_id, path
`
```

In its scan loop, change `rows.Scan(&i.RunID, &i.StepID, &i.Path)` to:

```go
		if err := rows.Scan(&i.RunID, &i.StepID, &i.Path, &i.Branch, &i.CommitSha); err != nil {
```

- [ ] **Step 7: Map the fields in `internal/store/sqlite.go`**

In `loadSteps`, the artifact build (currently line ~179):

```go
	for _, a := range arts {
		byStep[a.StepID] = append(byStep[a.StepID], core.Artifact{StepID: a.StepID, Path: a.Path, Branch: a.Branch, Commit: a.CommitSha})
	}
```

In `SaveStepTransition`, the insert loop (currently line ~228):

```go
	for _, a := range st.Artifacts {
		if err := q.InsertArtifact(ctx, sqldb.InsertArtifactParams{
			RunID: string(st.RunID), StepID: st.StepID, Path: a.Path, Branch: a.Branch, CommitSha: a.Commit,
		}); err != nil {
			return err
		}
	}
```

- [ ] **Step 8: Run the test — verify it passes**

Run: `go test ./internal/store/ -run TestSQLiteArtifactRefsRoundTrip -v`
Expected: PASS.

- [ ] **Step 9: Full store + core build check**

Run: `go test ./internal/store/... ./internal/core/...`
Expected: PASS (existing store tests still green — additive change).

- [ ] **Step 10: Commit**

```bash
git add internal/core/ports.go internal/store/
git commit -m "feat(store): artifacts carry branch/commit refs"
```

---

## Task 2: Workspace.Commit + persistent git identity

**Files:**
- Modify: `internal/core/ports.go` (Workspace interface)
- Modify: `internal/workspace/gitmanager.go`, `internal/workspace/workspace.go`
- Test: `internal/workspace/gitmanager_test.go`, `internal/workspace/workspace_test.go`

- [ ] **Step 1: Add `Commit` to the `Workspace` interface**

In `internal/core/ports.go`, inside `type Workspace interface`, add after `For`:

```go
	// Commit records the step's worktree as a commit on its branch and returns the
	// branch name and commit sha. A no-op (returns "", "", nil) for workspaces with
	// no git backing (the plain Manager) and acceptable to call for any step; the
	// engine only calls it for committed isolated steps.
	Commit(runID RunID, s *flow.Step, workDir string) (branch, commit string, err error)
```

- [ ] **Step 2: Write the failing GitManager.Commit tests**

In `internal/workspace/gitmanager_test.go`, add:

```go
func TestGitManagerCommitRecordsWork(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	dir, _, err := m.For("r1", &flow.Step{ID: "a", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	branch, commit, err := m.Commit("r1", &flow.Step{ID: "a", Workspace: flow.WSIsolated}, dir)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if branch != "step/a" {
		t.Errorf("branch = %q, want step/a", branch)
	}
	if commit == "" || commit != gitOut(t, dir, "rev-parse", "HEAD") {
		t.Errorf("commit sha = %q, want HEAD", commit)
	}
	if status := gitOut(t, dir, "status", "--porcelain"); status != "" {
		t.Errorf("worktree should be clean after commit, got %q", status)
	}
}

func TestGitManagerCommitAllowsEmpty(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	dir, _, err := m.For("r1", &flow.Step{ID: "a", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatal(err)
	}
	if _, commit, err := m.Commit("r1", &flow.Step{ID: "a", Workspace: flow.WSIsolated}, dir); err != nil || commit == "" {
		t.Fatalf("commit of a no-file step should still produce a commit, got commit=%q err=%v", commit, err)
	}
}
```

- [ ] **Step 3: Run — verify it fails**

Run: `go test ./internal/workspace/ -run TestGitManagerCommit`
Expected: FAIL — `m.Commit` undefined.

- [ ] **Step 4: Persist git identity in `ensureRepo`**

In `internal/workspace/gitmanager.go`, in `ensureRepo`, right after the `git init` block (so merges in linked worktrees, which share the base config, have a committer identity), add the config calls. Replace the init block:

```go
	if _, err := os.Stat(filepath.Join(base, ".git")); err != nil {
		if err := os.MkdirAll(base, 0o755); err != nil {
			return err
		}
		if _, err := m.run(base, "init"); err != nil {
			return err
		}
		if _, err := m.run(base, "config", "user.name", m.name()); err != nil {
			return err
		}
		if _, err := m.run(base, "config", "user.email", m.email()); err != nil {
			return err
		}
	}
```

- [ ] **Step 5: Implement `GitManager.Commit`**

In `internal/workspace/gitmanager.go`, add (after `For`):

```go
// Commit stages and commits everything in the step's worktree onto its branch,
// returning the branch name and the new commit sha. --allow-empty so a step that
// wrote nothing still advances its branch deterministically. Serialised by the
// run lock, like For/TeardownRun. Identity comes from the repo config ensureRepo set.
func (m *GitManager) Commit(runID core.RunID, s *flow.Step, workDir string) (string, string, error) {
	lock := m.runLock(runID)
	lock.Lock()
	defer lock.Unlock()

	if _, err := m.run(workDir, "add", "-A"); err != nil {
		return "", "", err
	}
	if _, err := m.run(workDir, "commit", "--allow-empty", "-m", "step/"+s.ID); err != nil {
		return "", "", err
	}
	out, err := m.run(workDir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	return "step/" + s.ID, string(bytes.TrimSpace(out)), nil
}
```

(`bytes` is already imported in this file.)

- [ ] **Step 6: Implement the no-op `Manager.Commit`**

In `internal/workspace/workspace.go`, add:

```go
// Commit is a no-op: the plain Manager has no git backing, so steps stay path-only.
func (m *Manager) Commit(core.RunID, *flow.Step, string) (string, string, error) {
	return "", "", nil
}
```

- [ ] **Step 7: Run the tests — verify they pass**

Run: `go test ./internal/workspace/ -v`
Expected: PASS (including the existing GitManager/Manager tests; the `var _ core.Workspace` assertions still hold with the new method).

- [ ] **Step 8: Commit**

```bash
git add internal/core/ports.go internal/workspace/
git commit -m "feat(workspace): Commit records an isolated step's worktree to its branch"
```

---

## Task 3: Engine commits successful isolated steps

**Files:**
- Modify: `internal/engine/engine.go` (`runStep` + new `commitIsolated`)
- Test: `internal/engine/engine_test.go`

- [ ] **Step 1: Write a failing test (engine stamps branch/commit)**

In `internal/engine/engine_test.go`, add a GitManager-backed engine helper and a test:

```go
// newGitEngine wires an engine whose workspace is a real GitManager, so isolated
// steps commit and joins can git-merge. Returns the engine and its store.
func newGitEngine(t *testing.T, exec map[string]core.Executor) (*Engine, *store.Mem) {
	t.Helper()
	if _, err := execLookGit(); err != nil {
		t.Skip("git not on PATH")
	}
	st := store.NewMem()
	bus := event.NewBus()
	return &Engine{
		Execs: exec,
		WS:    &workspace.GitManager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st,
		Bus:   bus,
		Clock: core.SystemClock{},
	}, st
}

func execLookGit() (string, error) { return exec.LookPath("git") }

func TestIsolatedStepCommitsAndStampsRefs(t *testing.T) {
	eng, st := newGitEngine(t, mocks())
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Workspace: flow.WSIsolated,
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
	}}
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r1", Name: "f", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	arts := got.Steps[0].Artifacts
	if len(arts) == 0 || arts[0].Branch != "step/a" || arts[0].Commit == "" {
		t.Fatalf("isolated step artifacts not stamped with refs: %+v", arts)
	}
}
```

Add `"os/exec"` to the test imports if not present.

- [ ] **Step 2: Run — verify it fails**

Run: `go test ./internal/engine/ -run TestIsolatedStepCommitsAndStampsRefs`
Expected: FAIL — `Branch` is empty (the engine never commits).

- [ ] **Step 3: Add `commitIsolated` and call it on success**

In `internal/engine/engine.go`, add the helper (near `execute`):

```go
// commitIsolated records a successful isolated NON-join step's worktree on its
// branch and stamps the result's artifacts with the branch/commit. Joins
// self-commit (the strategy does the git work) and shared steps have no branch,
// so both are skipped. A commit failure is surfaced as a step failure.
func (e *Engine) commitIsolated(runID core.RunID, s *flow.Step, workDir string, res *core.Result) error {
	if s.Workspace != flow.WSIsolated || s.Join != nil {
		return nil
	}
	br, sha, err := e.WS.Commit(runID, s, workDir)
	if err != nil {
		return fmt.Errorf("commit step %q: %w", s.ID, err)
	}
	for i := range res.Artifacts {
		res.Artifacts[i].Branch = br
		res.Artifacts[i].Commit = sha
	}
	return nil
}
```

In `runStep`, replace the success block (currently `engine.go:250-254`):

```go
		res, gateFailed, execErr := e.attempt(ctx, runID, s, inputs, attempt, workDir)
		if execErr == nil {
			if cerr := e.commitIsolated(runID, s, workDir, &res); cerr != nil {
				execErr = cerr // a failed commit is a step failure → normal disposition
			} else {
				e.transition(ctx, runID, stepState(runID, s.ID, core.StepSucceeded, attempt, workDir, res, nil),
					event.Event{StepID: s.ID, Kind: event.StepDone, Summary: res.Summary, CostUSD: res.CostUSD, Attempt: attempt})
				return res, nil
			}
		}
		lastErr = execErr
```

- [ ] **Step 4: Run — verify it passes**

Run: `go test ./internal/engine/ -run TestIsolatedStepCommitsAndStampsRefs -v`
Expected: PASS.

- [ ] **Step 5: Run the whole engine suite (no regressions)**

Run: `go test ./internal/engine/...`
Expected: PASS — existing tests use the plain `Manager` (whose `Commit` is a no-op), so shared/isolated steps there keep empty refs.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go
git commit -m "feat(engine): commit isolated steps and stamp branch/commit refs"
```

---

## Task 4: Join git foundation (helpers, ConflictError, prompts)

**Files:**
- Create: `internal/join/git.go`
- Test: `internal/join/git_test.go`, `internal/join/gitfixture_test.go`

- [ ] **Step 1: Create the shared git test fixture**

Create `internal/join/gitfixture_test.go`:

```go
package join

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func gitX(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setupJoinRepo builds a base repo with two committed branches (step/a, step/b)
// and a join worktree on step/integrate off the empty base. With conflict=true
// both branches write shared.txt differently (a merge conflict); otherwise they
// touch disjoint files (a clean merge). Returns the join worktree dir and the
// branch-backed inputs a join would receive.
func setupJoinRepo(t *testing.T, conflict bool) (string, []core.Artifact) {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	base := filepath.Join(root, "base")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	gitX(t, base, "init")
	gitX(t, base, "config", "user.name", "test")
	gitX(t, base, "config", "user.email", "test@test")
	gitX(t, base, "commit", "--allow-empty", "-m", "base")

	wtA := filepath.Join(root, "wt-a")
	gitX(t, base, "worktree", "add", wtA, "-b", "step/a", "HEAD")
	writeFile(t, wtA, "a.txt", "from A")
	if conflict {
		writeFile(t, wtA, "shared.txt", "A version")
	}
	gitX(t, wtA, "add", "-A")
	gitX(t, wtA, "commit", "-m", "a")

	wtB := filepath.Join(root, "wt-b")
	gitX(t, base, "worktree", "add", wtB, "-b", "step/b", "HEAD")
	writeFile(t, wtB, "b.txt", "from B")
	if conflict {
		writeFile(t, wtB, "shared.txt", "B version")
	}
	gitX(t, wtB, "add", "-A")
	gitX(t, wtB, "commit", "-m", "b")

	joinDir := filepath.Join(root, "wt-join")
	gitX(t, base, "worktree", "add", joinDir, "-b", "step/integrate", "HEAD")

	inputs := []core.Artifact{
		{StepID: "a", Branch: "step/a", Commit: gitX(t, base, "rev-parse", "step/a"), Path: filepath.Join(wtA, "a.txt")},
		{StepID: "b", Branch: "step/b", Commit: gitX(t, base, "rev-parse", "step/b"), Path: filepath.Join(wtB, "b.txt")},
	}
	return joinDir, inputs
}

func joinStep(strategy flow.JoinStrategy, onConflict flow.FailPolicy) *flow.Step {
	return &flow.Step{ID: "integrate", Needs: []string{"a", "b"}, Workspace: flow.WSIsolated,
		Join: &flow.Join{Strategy: strategy, Agent: "arbiter", OnConflict: onConflict}}
}
```

- [ ] **Step 2: Write failing tests for the git foundation**

Create `internal/join/git_test.go`:

```go
package join

import (
	"errors"
	"path/filepath"
	"testing"

	"concentus/internal/flow"
)

func TestCommittedResultEnumeratesTree(t *testing.T) {
	joinDir, _ := setupJoinRepo(t, false)
	// Put two tracked files in the join worktree and commit them.
	writeFile(t, joinDir, "x.txt", "x")
	writeFile(t, joinDir, "y.txt", "y")
	gitX(t, joinDir, "add", "-A")
	gitX(t, joinDir, "commit", "-m", "work")

	res, err := CommittedResult(joinDir, &flow.Step{ID: "integrate"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 2 {
		t.Fatalf("want 2 artifacts (x.txt, y.txt), got %+v", res.Artifacts)
	}
	if res.Artifacts[0].Branch != "step/integrate" || res.Artifacts[0].Commit == "" {
		t.Errorf("artifact refs = %+v, want branch step/integrate + a sha", res.Artifacts[0])
	}
	for _, a := range res.Artifacts {
		if a.StepID != "integrate" || a.Branch != "step/integrate" {
			t.Errorf("artifact not tagged with the join: %+v", a)
		}
		if filepath.Dir(a.Path) != joinDir {
			t.Errorf("artifact path %q not under join dir %q", a.Path, joinDir)
		}
	}
}

func TestConflictErrorIs(t *testing.T) {
	err := error(&ConflictError{Branch: "step/b", Paths: []string{"shared.txt"}, WorkDir: "/w"})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Paths[0] != "shared.txt" {
		t.Fatalf("ConflictError should unwrap via errors.As, got %v", err)
	}
}
```

- [ ] **Step 3: Run — verify it fails**

Run: `go test ./internal/join/ -run 'TestCommittedResult|TestConflictError'`
Expected: FAIL — `CommittedResult`/`ConflictError` undefined.

- [ ] **Step 4: Implement `internal/join/git.go`**

```go
package join

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// gitCmd runs git in workDir and returns combined output. Args are orchestrator-
// controlled (fixed subcommands, validated branch names); no shell is involved.
// Mirrors executor.discoverGit's direct-exec pattern.
func gitCmd(workDir string, args ...string) ([]byte, error) {
	// #nosec G204 -- fixed git subcommands in an operator-controlled worktree; no shell.
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(out))
	}
	return out, nil
}

// ConflictError reports that a git merge left unresolved conflicts. The engine
// distinguishes it (via errors.As) from an arbiter failure to drive the
// on_conflict=escalate resolve-then-approve ladder.
type ConflictError struct {
	Branch  string
	Paths   []string
	WorkDir string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("merge conflict in %v (merging %s)", e.Paths, e.Branch)
}

// upstreamBranches returns the distinct, first-seen-ordered branch refs carried
// by the inputs. An input with no branch (a shared step / mock) is skipped.
func upstreamBranches(inputs []core.Artifact) []string {
	var brs []string
	seen := map[string]bool{}
	for _, in := range inputs {
		if in.Branch != "" && !seen[in.Branch] {
			seen[in.Branch] = true
			brs = append(brs, in.Branch)
		}
	}
	return brs
}

// conflictedPaths returns the worktree's unmerged paths (relative to workDir).
func conflictedPaths(workDir string) []string {
	out, err := gitCmd(workDir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

// CommittedResult builds the result of a committed join worktree: its branch
// (the worktree's current branch), HEAD sha, and every tracked file as an
// artifact. Used by merge's clean path, synthesize, and the engine's escalate-
// finalize so all three enumerate artifacts identically.
func CommittedResult(workDir string, s *flow.Step) (core.Result, error) {
	branchOut, err := gitCmd(workDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return core.Result{}, err
	}
	branch := strings.TrimSpace(string(branchOut))
	shaOut, err := gitCmd(workDir, "rev-parse", "HEAD")
	if err != nil {
		return core.Result{}, err
	}
	sha := strings.TrimSpace(string(shaOut))
	filesOut, err := gitCmd(workDir, "ls-files")
	if err != nil {
		return core.Result{}, err
	}
	var artifacts []core.Artifact
	for _, rel := range strings.Split(strings.TrimSpace(string(filesOut)), "\n") {
		if rel == "" {
			continue
		}
		artifacts = append(artifacts, core.Artifact{
			StepID: s.ID, Path: filepath.Join(workDir, rel), Branch: branch, Commit: sha,
		})
	}
	// core.Result has no ref fields; branch/commit ride on each artifact.
	return core.Result{StepID: s.ID, Artifacts: artifacts}, nil
}

// ResolveConflictPrompt asks the arbiter to resolve every conflict marker in the
// listed files (in its current working directory) and leave a clean tree. Shared
// by synthesize and the engine's merge-escalate ladder.
func ResolveConflictPrompt(paths []string) string {
	var b strings.Builder
	b.WriteString("A git merge left conflicts. Resolve every <<<<<<< / ======= / >>>>>>> marker ")
	b.WriteString("in these files, keeping the best of both sides, and leave a clean tree:\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "  - %s\n", p)
	}
	return b.String()
}
```

(`core.Result` carries no ref fields; branch/commit ride on each artifact — the Step 2 test already asserts via `res.Artifacts[0]`.)

- [ ] **Step 5: Run — verify it passes**

Run: `go test ./internal/join/ -run 'TestCommittedResult|TestConflictError' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/join/git.go internal/join/git_test.go internal/join/gitfixture_test.go
git commit -m "feat(join): git foundation — gitCmd, ConflictError, CommittedResult, prompts"
```

---

## Task 5: merge goes git-native

**Files:**
- Modify: `internal/join/join.go` (`Merge.Join`)
- Test: `internal/join/join_test.go` (replace `TestMergeWritesManifest`)

- [ ] **Step 1: Replace the manifest test with git-native merge tests**

In `internal/join/join_test.go`, delete `TestMergeWritesManifest` and add (keep `TestDefaultRegistryHasAllStrategies`):

```go
func TestMergeCombinesBranches(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, false)
	res, err := Merge{}.Join(context.Background(), joinStep(flow.JoinMerge, flow.FailAbort), inputs, joinDir, nil)
	if err != nil {
		t.Fatalf("clean merge: %v", err)
	}
	got := map[string]bool{}
	for _, a := range res.Artifacts {
		got[filepath.Base(a.Path)] = true
	}
	if !got["a.txt"] || !got["b.txt"] {
		t.Fatalf("merged tree missing a.txt/b.txt: %+v", res.Artifacts)
	}
	if res.Artifacts[0].Branch != "step/integrate" {
		t.Errorf("result not on the join branch: %+v", res.Artifacts[0])
	}
}

func TestMergeConflictAbortFails(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, true)
	_, err := Merge{}.Join(context.Background(), joinStep(flow.JoinMerge, flow.FailAbort), inputs, joinDir, nil)
	if err == nil {
		t.Fatal("expected a conflict error with on_conflict=abort")
	}
	var ce *ConflictError
	if errors.As(err, &ce) {
		t.Fatal("abort should NOT surface a ConflictError (that is escalate-only)")
	}
	if len(conflictedPaths(joinDir)) != 0 {
		t.Error("abort should leave no conflict markers (git merge --abort)")
	}
}

func TestMergeConflictEscalateReturnsConflictError(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, true)
	_, err := Merge{}.Join(context.Background(), joinStep(flow.JoinMerge, flow.FailEscalate), inputs, joinDir, nil)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("escalate should surface a *ConflictError, got %v", err)
	}
	found := false
	for _, p := range ce.Paths {
		if p == "shared.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("ConflictError.Paths = %v, want shared.txt", ce.Paths)
	}
}
```

Add `"context"`, `"errors"`, `"path/filepath"`, and `"concentus/internal/flow"` to the test imports as needed (remove now-unused `os`/`strings` if the manifest test was their only user).

- [ ] **Step 2: Run — verify it fails**

Run: `go test ./internal/join/ -run TestMerge`
Expected: FAIL — `Merge` still writes a manifest; the merged tree / ConflictError assertions fail.

- [ ] **Step 3: Rewrite `Merge.Join`**

In `internal/join/join.go`, replace the `Merge` type and method (remove the `strings`/`os` manifest code), keeping the package doc + `RunAgent`/`Strategy`/`Registry`/`Default`:

```go
// Merge does a real git merge of the upstream branches into the join's worktree.
// A clean merge commits and returns the merged tree; a conflict is dispositioned
// by on_conflict — escalate surfaces a *ConflictError (the engine runs the
// resolve-then-approve ladder), anything else aborts the merge and fails.
type Merge struct{}

func (Merge) Join(_ context.Context, s *flow.Step, inputs []core.Artifact, workDir string, _ RunAgent) (core.Result, error) {
	branches := upstreamBranches(inputs)
	if len(branches) == 0 {
		return core.Result{}, fmt.Errorf("merge: no branch-backed inputs")
	}
	for _, br := range branches {
		if _, err := gitCmd(workDir, "merge", "--no-edit", br); err != nil {
			conflicted := conflictedPaths(workDir)
			if len(conflicted) == 0 {
				return core.Result{}, fmt.Errorf("merge %s: %w", br, err)
			}
			if s.Join.OnConflict == flow.FailEscalate {
				return core.Result{}, &ConflictError{Branch: br, Paths: conflicted, WorkDir: workDir}
			}
			_, _ = gitCmd(workDir, "merge", "--abort")
			return core.Result{}, fmt.Errorf("merge conflict in %v", conflicted)
		}
	}
	res, err := CommittedResult(workDir, s)
	if err != nil {
		return core.Result{}, err
	}
	res.Summary = fmt.Sprintf("merged %d branch(es)", len(branches))
	return res, nil
}
```

After this edit, `join.go` no longer uses `os`, `path/filepath`, or `strings` for `Merge`; leave any imports still used by `stageCandidates` (removed in Task 7). Run `goimports`/let the compiler flag unused imports and trim.

- [ ] **Step 4: Run — verify it passes**

Run: `go test ./internal/join/ -run TestMerge -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/join/join.go internal/join/join_test.go
git commit -m "feat(join): merge does a real git merge with on_conflict disposition"
```

---

## Task 6: synthesize goes git-native

**Files:**
- Modify: `internal/join/synthesize.go`
- Test: `internal/join/synthesize_test.go` (replace contents)

- [ ] **Step 1: Replace the synthesize tests**

Replace `internal/join/synthesize_test.go` with:

```go
package join

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

func TestSynthesizeAutoMergesWithoutArbiter(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, false) // disjoint files → no conflict
	run := func(context.Context, string, string, string, []core.Artifact) (core.Result, error) {
		t.Fatal("arbiter must not be called when the merge has no conflicts")
		return core.Result{}, nil
	}
	res, err := Synthesize{}.Join(context.Background(), joinStep(flow.JoinSynthesize, ""), inputs, joinDir, run)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	got := map[string]bool{}
	for _, a := range res.Artifacts {
		got[filepath.Base(a.Path)] = true
	}
	if !got["a.txt"] || !got["b.txt"] {
		t.Fatalf("auto-merged tree missing files: %+v", res.Artifacts)
	}
}

func TestSynthesizeArbiterResolvesConflict(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, true) // both write shared.txt → conflict
	run := func(_ context.Context, _, _ , wd string, _ []core.Artifact) (core.Result, error) {
		if err := os.WriteFile(filepath.Join(wd, "shared.txt"), []byte("reconciled"), 0o644); err != nil {
			t.Fatal(err)
		}
		return core.Result{Summary: "resolved", CostUSD: 0.03}, nil
	}
	res, err := Synthesize{}.Join(context.Background(), joinStep(flow.JoinSynthesize, ""), inputs, joinDir, run)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(joinDir, "shared.txt"))
	if err != nil || string(body) != "reconciled" {
		t.Fatalf("arbiter resolution not committed: body=%q err=%v", body, err)
	}
	if res.CostUSD != 0.03 {
		t.Errorf("arbiter cost not propagated: %v", res.CostUSD)
	}
}

func TestSynthesizeArbiterLeavesMarkersFails(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, true)
	run := func(context.Context, string, string, string, []core.Artifact) (core.Result, error) {
		return core.Result{Summary: "did nothing"}, nil // leaves markers
	}
	_, err := Synthesize{}.Join(context.Background(), joinStep(flow.JoinSynthesize, ""), inputs, joinDir, run)
	if err == nil {
		t.Fatal("expected an error when the arbiter leaves unresolved conflicts")
	}
}
```

- [ ] **Step 2: Run — verify it fails**

Run: `go test ./internal/join/ -run TestSynthesize`
Expected: FAIL — current `Synthesize` uses `.candidates/` staging, not git merge.

- [ ] **Step 3: Rewrite `Synthesize.Join`**

Replace `internal/join/synthesize.go` with:

```go
package join

import (
	"context"
	"fmt"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Synthesize merges every upstream branch into the join worktree, asking the
// arbiter to resolve only the true conflicts (non-conflicting changes merge
// automatically). The committed merged tree is the result.
type Synthesize struct{}

func (Synthesize) Join(ctx context.Context, s *flow.Step, inputs []core.Artifact, workDir string, run RunAgent) (core.Result, error) {
	branches := upstreamBranches(inputs)
	if len(branches) == 0 {
		return core.Result{}, fmt.Errorf("synthesize: no branch-backed inputs")
	}
	var cost float64
	for _, br := range branches {
		if _, err := gitCmd(workDir, "merge", "--no-edit", br); err == nil {
			continue // clean merge auto-committed
		}
		conflicted := conflictedPaths(workDir)
		if len(conflicted) == 0 {
			return core.Result{}, fmt.Errorf("synthesize: merge %s failed without conflicts", br)
		}
		ares, aerr := run(ctx, s.Join.Agent, ResolveConflictPrompt(conflicted), workDir, inputs)
		if aerr != nil {
			_, _ = gitCmd(workDir, "merge", "--abort")
			return core.Result{}, fmt.Errorf("synthesize: arbiter failed: %w", aerr)
		}
		cost += ares.CostUSD
		if rem := conflictedPaths(workDir); len(rem) > 0 {
			_, _ = gitCmd(workDir, "merge", "--abort")
			return core.Result{}, fmt.Errorf("synthesize: arbiter left unresolved conflicts in %v", rem)
		}
		if _, err := gitCmd(workDir, "add", "-A"); err != nil {
			return core.Result{}, err
		}
		if _, err := gitCmd(workDir, "commit", "--no-edit"); err != nil {
			return core.Result{}, err
		}
	}
	res, err := CommittedResult(workDir, s)
	if err != nil {
		return core.Result{}, err
	}
	res.Summary = fmt.Sprintf("synthesized %d branch(es)", len(branches))
	res.CostUSD = cost
	return res, nil
}
```

- [ ] **Step 4: Run — verify it passes**

Run: `go test ./internal/join/ -run TestSynthesize -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/join/synthesize.go internal/join/synthesize_test.go
git commit -m "feat(join): synthesize merges branches and arbitrates only conflicts"
```

---

## Task 7: select forwards a branch by ref; remove staging

**Files:**
- Modify: `internal/join/select.go`, `internal/join/join.go` (delete `stageCandidates`)
- Test: `internal/join/select_test.go`

- [ ] **Step 1: Update the select tests (branch-ref forwarding; drop staging tests)**

Replace `internal/join/select_test.go` with:

```go
package join

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

func writeArtifact(t *testing.T, dir, stepID, name, body, branch string) core.Artifact {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return core.Artifact{StepID: stepID, Path: p, Branch: branch, Commit: "sha-" + stepID}
}

func stubRun(res core.Result, err error) RunAgent {
	return func(context.Context, string, string, string, []core.Artifact) (core.Result, error) {
		return res, err
	}
}

func selectStep() *flow.Step {
	return &flow.Step{ID: "pick", Needs: []string{"a", "b"}, Workspace: flow.WSIsolated,
		Join: &flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter"}}
}

func TestSelectForwardsWinnerByRef(t *testing.T) {
	dir := t.TempDir()
	inA := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A", "step/a")
	inB := writeArtifact(t, t.TempDir(), "b", "b.out.md", "B", "step/b")
	run := stubRun(core.Result{Summary: "B is cleaner\nSELECTED: b", CostUSD: 0.02}, nil)

	res, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{inA, inB}, dir, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "b" || res.Artifacts[0].Branch != "step/b" {
		t.Fatalf("result = %+v, want b's artifact forwarded with its branch", res.Artifacts)
	}
	if res.StepID != "pick" || res.CostUSD != 0.02 {
		t.Errorf("result = %+v, want StepID=pick cost=0.02", res)
	}
}

func TestSelectNoTokenErrors(t *testing.T) {
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A", "step/a")
	run := stubRun(core.Result{Summary: "I cannot decide"}, nil)
	_, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{in}, t.TempDir(), run)
	if err == nil {
		t.Fatal("expected an error when the arbiter emits no SELECTED token")
	}
}

func TestSelectUnknownWinnerErrors(t *testing.T) {
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A", "step/a")
	run := stubRun(core.Result{Summary: "SELECTED: zzz"}, nil)
	_, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{in}, t.TempDir(), run)
	if err == nil {
		t.Fatal("expected an error when the chosen step is not a dependency")
	}
}

func TestSelectLastTokenWins(t *testing.T) {
	inA := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A", "step/a")
	inB := writeArtifact(t, t.TempDir(), "b", "b.out.md", "B", "step/b")
	run := stubRun(core.Result{Summary: "SELECTED: a\non reflection\nSELECTED: b"}, nil)
	res, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{inA, inB}, t.TempDir(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "b" {
		t.Fatalf("result artifacts = %+v, want b (last SELECTED token wins)", res.Artifacts)
	}
}

func TestSelectParsesTokenWithoutSpace(t *testing.T) {
	inA := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A", "step/a")
	inB := writeArtifact(t, t.TempDir(), "b", "b.out.md", "B", "step/b")
	run := stubRun(core.Result{Summary: "SELECTED:b"}, nil)
	res, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{inA, inB}, t.TempDir(), run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "b" {
		t.Fatalf("result artifacts = %+v, want b", res.Artifacts)
	}
}
```

- [ ] **Step 2: Run — verify it fails to compile**

Run: `go test ./internal/join/ -run TestSelect`
Expected: FAIL/compile error — `writeArtifact` now takes a `branch` arg and `selectPrompt` still references staging.

- [ ] **Step 3: Rewrite `Select.Join` + `selectPrompt` (no staging)**

In `internal/join/select.go`, replace the `Join` method and `selectPrompt`:

```go
func (Select) Join(ctx context.Context, s *flow.Step, inputs []core.Artifact, workDir string, run RunAgent) (core.Result, error) {
	res, err := run(ctx, s.Join.Agent, selectPrompt(s, inputs), workDir, inputs)
	if err != nil {
		return core.Result{}, fmt.Errorf("select: arbiter failed: %w", err)
	}
	winner, ok := parseSelected(res.Summary)
	if !ok {
		return core.Result{}, fmt.Errorf("select: no SELECTED token in arbiter output")
	}
	if !isDependency(s, winner) {
		return core.Result{}, fmt.Errorf("select: chosen step %q is not a dependency", winner)
	}
	// Forward the winner's original artifacts (with their branch/commit) by reference.
	var artifacts []core.Artifact
	for _, in := range inputs {
		if in.StepID == winner {
			artifacts = append(artifacts, in)
		}
	}
	return core.Result{StepID: s.ID, Summary: res.Summary, Artifacts: artifacts, CostUSD: res.CostUSD}, nil
}

// selectPrompt lists each candidate's files (at their upstream-worktree paths)
// and asks for a SELECTED token.
func selectPrompt(s *flow.Step, inputs []core.Artifact) string {
	byStep := map[string][]string{}
	for _, in := range inputs {
		byStep[in.StepID] = append(byStep[in.StepID], in.Path)
	}
	var b strings.Builder
	b.WriteString("You are choosing the single best candidate implementation.\n\n")
	for _, dep := range s.Needs {
		fmt.Fprintf(&b, "Candidate %s:\n", dep)
		for _, p := range byStep[dep] {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	b.WriteString("\nRead each candidate's files, decide which is best, explain briefly, ")
	b.WriteString("and end your reply with a line:\nSELECTED: <step-id>\n")
	return b.String()
}
```

- [ ] **Step 4: Delete `stageCandidates` and `underCandidates`**

In `internal/join/join.go`, delete the `stageCandidates` function (no longer used). In `internal/join/synthesize.go` you already removed `underCandidates` by rewriting the file in Task 6 — confirm no references remain:

Run: `grep -rn "stageCandidates\|underCandidates\|.candidates" internal/join/`
Expected: no matches (outside test history). Trim any now-unused imports the compiler flags.

- [ ] **Step 5: Run — verify it passes**

Run: `go test ./internal/join/ -v`
Expected: PASS (all join tests, including merge/synthesize/select).

- [ ] **Step 6: Commit**

```bash
git add internal/join/select.go internal/join/join.go internal/join/select_test.go
git commit -m "feat(join): select forwards the winner's branch by ref; drop .candidates staging"
```

---

## Task 8: engine escalate ladder for merge conflicts

**Files:**
- Modify: `internal/engine/engine.go` (`escalateJoin` + new `resolveConflictEscalation`)
- Test: `internal/engine/engine_test.go`

- [ ] **Step 1: Write failing integration tests (approve + reject)**

In `internal/engine/engine_test.go`, add a conflicting executor and two tests:

```go
// fileWriterExec writes a fixed filename with fixed content, so two such steps
// in separate worktrees collide on merge (used to drive the conflict ladder).
type fileWriterExec struct{ file, body string }

func (e fileWriterExec) Run(_ context.Context, t core.Task) (core.Result, error) {
	if err := os.WriteFile(filepath.Join(t.WorkDir, e.file), []byte(e.body), 0o644); err != nil {
		return core.Result{}, err
	}
	return core.Result{StepID: t.StepID, Summary: "wrote " + e.file, CostUSD: 0.01}, nil
}

func conflictFlow(onConflict flow.FailPolicy) *flow.Flow {
	autoGate := flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}
	return &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "a", Workspace: flow.WSIsolated, Gate: autoGate},
		{ID: "b", Agent: "b", Workspace: flow.WSIsolated, Gate: autoGate},
		{ID: "integrate", Needs: []string{"a", "b"}, Workspace: flow.WSIsolated,
			Join: &flow.Join{Strategy: flow.JoinMerge, Agent: "arbiter", OnConflict: onConflict}},
	}}
}

func TestMergeConflictEscalateApproveCommits(t *testing.T) {
	execs := map[string]core.Executor{
		"a":       fileWriterExec{file: "shared.md", body: "A"},
		"b":       fileWriterExec{file: "shared.md", body: "B"},
		"arbiter": fileWriterExec{file: "shared.md", body: "RESOLVED"},
	}
	eng, st := newGitEngine(t, execs)
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r1", Name: "f", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(context.Background(), "r1", conflictFlow(flow.FailEscalate)); err != nil {
		t.Fatalf("run should succeed after approve: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	var integrate core.StepState
	for _, s := range got.Steps {
		if s.StepID == "integrate" {
			integrate = s
		}
	}
	if integrate.Status != core.StepSucceeded {
		t.Fatalf("integrate status = %q, want succeeded", integrate.Status)
	}
	if len(integrate.Artifacts) == 0 || integrate.Artifacts[0].Branch != "step/integrate" {
		t.Fatalf("integrate not committed on its branch: %+v", integrate.Artifacts)
	}
}

func TestMergeConflictEscalateRejectFails(t *testing.T) {
	execs := map[string]core.Executor{
		"a":       fileWriterExec{file: "shared.md", body: "A"},
		"b":       fileWriterExec{file: "shared.md", body: "B"},
		"arbiter": fileWriterExec{file: "shared.md", body: "RESOLVED"},
	}
	eng, st := newGitEngine(t, execs)
	eng.Gate = &gate.Evaluator{Approver: rejectApprover{}, Verifier: gate.CommandVerifier{}}
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r1", Name: "f", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(context.Background(), "r1", conflictFlow(flow.FailEscalate)); err == nil {
		t.Fatal("run should fail when the human rejects the conflict resolution")
	}
}
```

Ensure `"os"` and `"path/filepath"` are imported in the test file.

- [ ] **Step 2: Run — verify it fails**

Run: `go test ./internal/engine/ -run TestMergeConflictEscalate`
Expected: FAIL — `escalateJoin` re-runs the join (re-conflicts) instead of running the resolve-then-approve ladder.

- [ ] **Step 3: Add the conflict branch to `escalateJoin`**

In `internal/engine/engine.go`, add `"errors"` and `"concentus/internal/join"` to imports if not present. At the top of `escalateJoin`, branch on a ConflictError:

```go
func (e *Engine) escalateJoin(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact, workDir string, joinErr error, attemptNum int) (core.Result, error) {
	var conflict *join.ConflictError
	if errors.As(joinErr, &conflict) {
		return e.resolveConflictEscalation(ctx, runID, s, inputs, workDir, conflict, attemptNum)
	}
	// ... existing arbiter-failure re-run body unchanged ...
```

- [ ] **Step 4: Implement `resolveConflictEscalation`**

Add after `escalateJoin`:

```go
// resolveConflictEscalation runs the merge-conflict ladder: the arbiter resolves
// the markers in the conflicted worktree (rung 1), then a human approves the
// resolution (rung 2). Approve commits the resolved tree as the result; reject
// fails (the worktree is reclaimed at run-end).
func (e *Engine) resolveConflictEscalation(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact, workDir string, conflict *join.ConflictError, attemptNum int) (core.Result, error) {
	next := attemptNum + 1
	// Frame the resolution as a fresh attempt (fixes the missing step.started gap).
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepRunning, next, workDir, core.Result{}, nil),
		event.Event{StepID: s.ID, Kind: event.StepStarted, Attempt: next})

	// Rung 1: arbiter resolves the conflict markers in place.
	if _, err := e.runAgent(ctx, runID, s.ID, "arbiter", s.Join.Agent, join.ResolveConflictPrompt(conflict.Paths), workDir, next, inputs); err != nil {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, next, workDir, core.Result{}, err),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: next, Err: err.Error()})
		return core.Result{}, err
	}

	// Rung 2: human reviews the resolution.
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepAwaitingGate, next, workDir, core.Result{}, conflict),
		event.Event{StepID: s.ID, Kind: event.GateAwaiting, Attempt: next, Err: conflict.Error()})
	ok, err := e.Gate.Escalate(ctx, runID, s, core.Result{})
	if err != nil {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, next, workDir, core.Result{}, err),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: next, Err: err.Error()})
		return core.Result{}, err
	}
	if !ok {
		rej := fmt.Errorf("escalated merge conflict rejected")
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, next, workDir, core.Result{}, rej),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: next, Err: rej.Error()})
		return core.Result{}, rej
	}

	// Approved: finalize the resolved worktree (concludes the merge) and build the result.
	if _, _, cerr := e.WS.Commit(runID, s, workDir); cerr != nil {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, next, workDir, core.Result{}, cerr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: next, Err: cerr.Error()})
		return core.Result{}, cerr
	}
	res, rerr := join.CommittedResult(workDir, s)
	if rerr != nil {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, next, workDir, core.Result{}, rerr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: next, Err: rerr.Error()})
		return core.Result{}, rerr
	}
	res.StepID = s.ID
	res.Summary = "merge conflict resolved (arbiter + human)"
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepSucceeded, next, workDir, res, nil),
		event.Event{StepID: s.ID, Kind: event.StepDone, Summary: res.Summary, CostUSD: res.CostUSD, Attempt: next})
	return res, nil
}
```

Note: `stepState` takes a `core.Result` and an `error` — passing `conflict` (a `*join.ConflictError`, which is an `error`) as the error arg is fine.

- [ ] **Step 5: Run — verify it passes**

Run: `go test ./internal/engine/ -run TestMergeConflictEscalate -v`
Expected: PASS (approve commits `RESOLVED`; reject fails the run).

- [ ] **Step 6: Run the full engine suite**

Run: `go test ./internal/engine/...`
Expected: PASS — the existing arbiter-failure escalate tests still pass (non-ConflictError path unchanged).

- [ ] **Step 7: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go
git commit -m "feat(engine): merge-conflict escalate ladder (arbiter resolves, human approves)"
```

---

## Task 9: validation rules

**Files:**
- Modify: `internal/flow/validate.go`
- Test: `internal/flow/validate_test.go`

- [ ] **Step 1: Write failing validation tests**

In `internal/flow/validate_test.go`, add:

```go
func TestValidateJoinRequiresIsolatedUpstreams(t *testing.T) {
	f := &Flow{Name: "f", Steps: []*Step{
		{ID: "a", Agent: "m", Workspace: WSShared, Gate: Gate{Policy: GateAuto, Verifier: &Verifier{Command: "true"}}},
		{ID: "j", Needs: []string{"a"}, Workspace: WSIsolated, Join: &Join{Strategy: JoinMerge}},
	}}
	if err := Validate(f); err == nil {
		t.Fatal("a join over a shared upstream must be rejected")
	}
}

func TestValidateJoinMustBeIsolated(t *testing.T) {
	f := &Flow{Name: "f", Steps: []*Step{
		{ID: "a", Agent: "m", Workspace: WSIsolated, Gate: Gate{Policy: GateAuto, Verifier: &Verifier{Command: "true"}}},
		{ID: "j", Needs: []string{"a"}, Workspace: WSShared, Join: &Join{Strategy: JoinMerge}},
	}}
	if err := Validate(f); err == nil {
		t.Fatal("a join step itself must be isolated")
	}
}

func TestValidateMergeEscalateRequiresAgent(t *testing.T) {
	f := &Flow{Name: "f", Steps: []*Step{
		{ID: "a", Agent: "m", Workspace: WSIsolated, Gate: Gate{Policy: GateAuto, Verifier: &Verifier{Command: "true"}}},
		{ID: "j", Needs: []string{"a"}, Workspace: WSIsolated, Join: &Join{Strategy: JoinMerge, OnConflict: FailEscalate}},
	}}
	if err := Validate(f); err == nil {
		t.Fatal("merge + on_conflict=escalate requires an arbiter agent")
	}
}

func TestValidateJoinRetryRequiresRetryPolicy(t *testing.T) {
	f := &Flow{Name: "f", Steps: []*Step{
		{ID: "a", Agent: "m", Workspace: WSIsolated, Gate: Gate{Policy: GateAuto, Verifier: &Verifier{Command: "true"}}},
		{ID: "j", Needs: []string{"a"}, Workspace: WSIsolated, Join: &Join{Strategy: JoinMerge, OnConflict: FailRetry}},
	}}
	if err := Validate(f); err == nil {
		t.Fatal("on_conflict=retry requires a retry policy")
	}
}
```

- [ ] **Step 2: Run — verify it fails**

Run: `go test ./internal/flow/ -run TestValidate`
Expected: FAIL — the new rules aren't enforced.

- [ ] **Step 3: Pass `byID` to `validateJoin` and add the rules**

In `internal/flow/validate.go`, change the call site (in `Validate`'s per-step loop):

```go
		if err := validateJoin(s, byID); err != nil {
			return err
		}
```

Replace `validateJoin` with:

```go
func validateJoin(s *Step, byID map[string]*Step) error {
	if s.Join == nil {
		return nil
	}
	if s.Workspace != WSIsolated {
		return fmt.Errorf("step %q: a join step must be workspace: isolated", s.ID)
	}
	for _, dep := range s.Needs {
		if up, ok := byID[dep]; ok && up.Workspace != WSIsolated {
			return fmt.Errorf("step %q: join upstream %q must be workspace: isolated", s.ID, dep)
		}
	}
	switch s.Join.Strategy {
	case JoinMerge:
		if s.Join.OnConflict == FailEscalate && s.Join.Agent == "" {
			return fmt.Errorf("step %q: merge with on_conflict=escalate requires an arbiter agent", s.ID)
		}
	case JoinSelect, JoinSynthesize:
		if s.Join.Agent == "" {
			return fmt.Errorf("step %q: %q join requires an arbiter agent", s.ID, s.Join.Strategy)
		}
	default:
		return fmt.Errorf("step %q: unknown join strategy %q", s.ID, s.Join.Strategy)
	}
	switch s.Join.OnConflict {
	case "", FailAbort, FailRetry, FailEscalate:
		// ok
	default:
		return fmt.Errorf("step %q: unknown join on_conflict %q", s.ID, s.Join.OnConflict)
	}
	if s.Join.OnConflict == FailRetry && s.Retry == nil {
		return fmt.Errorf("step %q: on_conflict=retry requires a retry policy", s.ID)
	}
	if len(s.Needs) == 0 {
		return fmt.Errorf("step %q: join step must depend on at least one step", s.ID)
	}
	return nil
}
```

- [ ] **Step 4: Run — verify it passes**

Run: `go test ./internal/flow/ -run TestValidate -v`
Expected: PASS.

- [ ] **Step 5: Fix any now-invalid existing flow fixtures**

Run: `go test ./...`
Expected: some pre-existing flow/daemon tests with non-isolated join steps may now fail validation. For each, set the join step and its upstreams to `Workspace: flow.WSIsolated` (and add an `agent` to a merge+escalate join). Fix until green.

- [ ] **Step 6: Commit**

```bash
git add internal/flow/validate.go internal/flow/validate_test.go
git commit -m "feat(flow): validate joins require isolated upstreams; merge+escalate needs an agent"
```

---

## Task 10: demo flow, skill doc, manual SSE proof

**Files:**
- Create: a demo flow YAML (path per `running-the-orchestrator` conventions)
- Modify: `.claude/skills/running-the-orchestrator/SKILL.md` (note isolated-upstream requirement + conflict demo)

- [ ] **Step 1: Find the demo-flow convention**

Run: `ls testdata 2>/dev/null; grep -rn "flow.yaml\|\.yaml" .claude/skills/running-the-orchestrator/`
Read the skill to see where example flows live and how the daemon is launched (mock agent, throwaway db/port).

- [ ] **Step 2: Write a git-native merge demo flow**

Create a flow with two isolated `mock` upstreams (each an auto gate `verifier: {command: "true"}`) fanning into an isolated `merge` join. For a conflict variant, give the upstreams a real (claude) agent or a mock that writes the same file, and set the join `on_conflict: escalate` with an `agent`. Use the exact YAML shape from an existing example flow in the repo (match keys: `workspace: isolated`, `gate.policy`, `join.strategy`).

- [ ] **Step 3: Run the daemon and watch SSE (manual proof)**

Follow `.claude/skills/running-the-orchestrator/SKILL.md`: build `magisterd`+`cm`, launch the daemon on a throwaway db/port (sandbox disabled so a real CLI child can reach the network; mock agent for a zero-cost smoke), `cm run --watch`, confirm the join's `agent.tool`/`step.done` events stream and the merge result lands. For the conflict variant, confirm `gate.awaiting` → `cm approve` → committed result over live SSE with `Last-Event-ID`.

- [ ] **Step 4: Update the skill doc**

Add a short note to `.claude/skills/running-the-orchestrator/SKILL.md`: git-native joins require the join step AND its upstreams to be `workspace: isolated`; a `merge` + `on_conflict: escalate` needs an `agent`. Include the demo flow path.

- [ ] **Step 5: Final full verification**

Run: `go test -race ./... && go vet ./...`
Expected: all green, vet clean.

- [ ] **Step 6: Commit**

```bash
git add .
git commit -m "docs(orchestrator): git-native merge demo flow + isolated-upstream note"
```

---

## Self-Review

**Spec coverage:** Artifact superset (T1) ✓; Workspace.Commit + identity (T2) ✓; engine commit-on-success (T3) ✓; join git foundation incl. ConflictError/CommittedResult/ResolveConflictPrompt (T4) ✓; merge git-native + on_conflict (T5) ✓; synthesize git-native (T6) ✓; select by-ref + remove staging (T7) ✓; escalate ladder + step.started fix (T8) ✓; validation rules incl. retry-asymmetry (T9) ✓; store migration (T1) ✓; demo + manual proof (T10) ✓. Non-goal external-repo correctly excluded.

**Type consistency:** `Workspace.Commit(runID, s, workDir) (branch, commit string, err error)` consistent across core/GitManager/Manager/engine. `core.Artifact{StepID,Path,Branch,Commit}` used uniformly; sqldb maps `Commit`↔`CommitSha`. `CommittedResult(workDir, s)` returns artifacts carrying refs (no `core.Result.Branch/Commit` field — corrected in T4 Step 4). `ConflictError{Branch,Paths,WorkDir}` and `errors.As` consistent in join (T4/T5) and engine (T8). `validateJoin(s, byID)` call site updated (T9).

**Placeholder scan:** No TBD/TODO; every code step shows complete code; T10 references the repo's existing flow YAML shape rather than inventing keys (correct — the exact demo path is discovered in T10 Step 1, not guessed).
