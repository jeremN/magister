# M4 Slice C: Git-Worktree Workspaces Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give each `isolated` step a real `git worktree` (branched off a scratch per-run repo) with run-end teardown and resume-safe creation, behind the existing `core.Workspace` seam — handoff stays path-based, no executor/join changes.

**Architecture:** A new `workspace.GitManager` lazily `git init`s a per-run repo at `<Root>/<runID>/base/` (one empty `base` commit); `shared` steps use that base working tree, `isolated` steps get a linked worktree at `<Root>/<runID>/wt/<stepID>` on branch `step/<stepID>` off `HEAD`. The `core.Workspace` port gains `TeardownRun`, which the engine `defer`s at run end (after every downstream step has read upstream artifact paths). The plain `mkdir` `Manager` stays for fast unit tests; the daemon wires `GitManager`.

**Tech Stack:** Go 1.22, `os/exec` shelling to `git` (no new dependency, no go-git), the in-repo `core.Workspace` port. Tests: standard `testing`, `t.Skip` when `git` is absent.

**Spec:** `docs/superpowers/specs/2026-06-04-m4c-git-worktree-workspaces-design.md`

**Commit convention (user CLAUDE.md):** single conventional-commit subject, NO body, NO `Co-Authored-By`, never `--no-verify`. Commit with the explicit identity:
```bash
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "<subject>"
```
**Raw test output:** the RTK hook reformats `go test`; use `rtk proxy go test ...` for per-test PASS/FAIL.

**Note on `git` dependency:** wiring `GitManager` into the daemon makes the daemon (and the e2e suite) require `git` on PATH. Workspace unit tests `t.Skip` without it; the e2e suite assumes it (this repo is itself a git repo). The lightweight `Manager` remains git-free for engine/supervisor unit tests.

---

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/core/ports.go` | the `Workspace` port | modify: add `TeardownRun(RunID) error` |
| `internal/workspace/workspace.go` | plain `mkdir` Manager | modify: add no-op `TeardownRun` + compile assertion |
| `internal/workspace/gitmanager.go` | git-worktree Workspace | **create**: `GitManager` (per-run repo, worktree add/remove/prune, per-run lock) |
| `internal/workspace/gitmanager_test.go` | GitManager tests | **create** |
| `internal/engine/engine.go` | run lifecycle | modify: `defer e.WS.TeardownRun(runID)` in `runDAG` |
| `internal/engine/engine_test.go` | engine tests | add: `teardownSpy` + run-end teardown test |
| `internal/flow/validate.go` | flow validation | modify: `stepID` slug rule |
| `internal/flow/validate_test.go` | validation tests | add: stepID slug cases |
| `cmd/magisterd/main.go` | daemon wiring | modify: wire `GitManager` |
| `cmd/magisterd/e2e_test.go` | end-to-end | modify: `crashDaemonAtGate` → `GitManager`; add isolated-worktree e2e |

---

## Task 1: Add `TeardownRun` to the `Workspace` port

**Files:**
- Modify: `internal/core/ports.go` (the `Workspace` interface)
- Modify: `internal/workspace/workspace.go`
- Test: `internal/workspace/workspace_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/workspace/workspace_test.go`:

```go
import "concentus/internal/core"

// compile-time assertion that Manager satisfies the (extended) Workspace port.
var _ core.Workspace = (*Manager)(nil)

func TestManagerTeardownRunIsNoop(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	if err := m.TeardownRun("run1"); err != nil {
		t.Fatalf("plain Manager TeardownRun should be a no-op, got %v", err)
	}
}
```

(The existing `workspace_test.go` imports `os`, `testing`, and `concentus/internal/flow`; add `concentus/internal/core`.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `rtk proxy go test ./internal/workspace/ -run TestManagerTeardownRunIsNoop -v`
Expected: FAIL — compile error: `*Manager` has no method `TeardownRun`, and `*Manager` does not satisfy `core.Workspace`.

- [ ] **Step 3: Extend the interface and add the no-op**

In `internal/core/ports.go`, replace the `Workspace` interface:

```go
// Workspace hands a step a working directory and a cleanup func, and tears down a
// run's isolated worktrees when the run ends.
type Workspace interface {
	For(runID RunID, s *flow.Step) (dir string, cleanup func() error, err error)
	// TeardownRun removes the run's isolated worktrees (the base repo persists). It
	// is best-effort, idempotent, and a no-op for a run with no worktrees.
	TeardownRun(runID RunID) error
}
```

In `internal/workspace/workspace.go`, add to `Manager`:

```go
// TeardownRun is a no-op: the plain Manager allocates plain directories, which the
// caller's run dir cleanup (or the OS temp dir) reclaims. GitManager does real teardown.
func (m *Manager) TeardownRun(core.RunID) error { return nil }
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `rtk proxy go test ./internal/workspace/ -v`
Expected: PASS (new test + existing `TestSharedReusesRunRoot`, `TestIsolatedGetsOwnDir`).

Then confirm the whole module still builds (the interface changed):
Run: `rtk proxy go build ./... && rtk proxy go test ./internal/engine/ ./internal/supervisor/ ./cmd/...`
Expected: builds; all pass (the daemon + tests still wire `Manager`, which now satisfies the port).

- [ ] **Step 5: Commit**

```bash
git add internal/core/ports.go internal/workspace/workspace.go internal/workspace/workspace_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(workspace): add TeardownRun to the Workspace port"
```

---

## Task 2: `GitManager` — a git worktree per isolated step

**Files:**
- Create: `internal/workspace/gitmanager.go`
- Test: `internal/workspace/gitmanager_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/workspace/gitmanager_test.go`:

```go
package workspace

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

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGitManagerSharedUsesBaseTree(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	d1, _, err := m.For("r1", &flow.Step{ID: "a", Workspace: flow.WSShared})
	if err != nil {
		t.Fatal(err)
	}
	d2, _, err := m.For("r1", &flow.Step{ID: "b", Workspace: flow.WSShared})
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Errorf("shared steps should share the base tree: %q vs %q", d1, d2)
	}
	// the shared dir is the primary worktree: .git is a directory.
	info, err := os.Stat(filepath.Join(d1, ".git"))
	if err != nil || !info.IsDir() {
		t.Errorf("shared dir should be the base repo (.git dir), got err=%v", err)
	}
}

func TestGitManagerIsolatedGetsWorktree(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	m := &GitManager{Root: root}
	dir, _, err := m.For("r1", &flow.Step{ID: "step-a", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(root, "r1", "wt", "step-a") {
		t.Errorf("unexpected worktree dir: %q", dir)
	}
	// a linked worktree's .git is a FILE, and HEAD is on the step branch.
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil || info.IsDir() {
		t.Errorf("isolated dir should be a linked worktree (.git file), got err=%v", err)
	}
	if br := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); br != "step/step-a" {
		t.Errorf("worktree branch = %q, want step/step-a", br)
	}
}

func TestGitManagerTeardownRemovesWorktrees(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	m := &GitManager{Root: root}
	dir, _, err := m.For("r1", &flow.Step{ID: "a", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.TeardownRun("r1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("worktree should be removed after teardown, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "r1", "base", ".git")); err != nil {
		t.Errorf("base repo should persist after teardown: %v", err)
	}
}

func TestGitManagerForIsResumeIdempotent(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	step := &flow.Step{ID: "a", Workspace: flow.WSIsolated}
	if _, _, err := m.For("r1", step); err != nil {
		t.Fatalf("first For: %v", err)
	}
	// a re-run (resume) of the same step must succeed by replacing the stale worktree.
	dir, _, err := m.For("r1", step)
	if err != nil {
		t.Fatalf("second For (resume) should succeed, got %v", err)
	}
	if br := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); br != "step/a" {
		t.Errorf("re-created worktree branch = %q, want step/a", br)
	}
}

func TestGitManagerTeardownNoRepoIsNoop(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	if err := m.TeardownRun("never-started"); err != nil {
		t.Errorf("teardown of an unknown run should be a no-op, got %v", err)
	}
}

var _ core.Workspace = (*GitManager)(nil)
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `rtk proxy go test ./internal/workspace/ -run TestGitManager -v`
Expected: FAIL — `GitManager` undefined (compile error).

- [ ] **Step 3: Implement `GitManager`**

Create `internal/workspace/gitmanager.go`:

```go
package workspace

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// GitManager gives each isolated step a real git worktree off a scratch per-run
// repo, and tears the worktrees down at run end. Shared steps use the base working
// tree. It is stateless aside from a per-run lock that serialises a run's git
// invocations (concurrent isolated steps must not race on the repo index).
type GitManager struct {
	Root  string
	Name  string // commit identity for the empty base commit; defaulted if empty
	Email string

	mu    sync.Mutex
	locks map[core.RunID]*sync.Mutex
}

func (m *GitManager) name() string {
	if m.Name != "" {
		return m.Name
	}
	return "magisterd"
}

func (m *GitManager) email() string {
	if m.Email != "" {
		return m.Email
	}
	return "magisterd@localhost"
}

func (m *GitManager) runLock(id core.RunID) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = make(map[core.RunID]*sync.Mutex)
	}
	l, ok := m.locks[id]
	if !ok {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	return l
}

func (m *GitManager) baseDir(id core.RunID) string { return filepath.Join(m.Root, string(id), "base") }
func (m *GitManager) wtDir(id core.RunID) string   { return filepath.Join(m.Root, string(id), "wt") }

// run executes git in dir and returns combined output. Args are orchestrator-
// controlled (run/step IDs, fixed subcommands); no shell is involved.
func (m *GitManager) run(dir string, args ...string) ([]byte, error) {
	// #nosec G204 -- git with orchestrator-supplied args (validated run/step IDs),
	// invoked without a shell. This is the intended capability, not user input.
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(out))
	}
	return out, nil
}

// ensureRepo lazily inits the per-run base repo with one empty commit. Idempotent.
func (m *GitManager) ensureRepo(base string) error {
	if _, err := os.Stat(filepath.Join(base, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	if _, err := m.run(base, "init"); err != nil {
		return err
	}
	if _, err := m.run(base,
		"-c", "user.name="+m.name(), "-c", "user.email="+m.email(),
		"commit", "--allow-empty", "-m", "base"); err != nil {
		return err
	}
	return nil
}

// freshWorktree (re-)creates a clean worktree at wt on branch step/<stepID>. Any
// stale worktree/branch (e.g. left by a crashed run) is removed first, so this is
// safe to call on resume.
func (m *GitManager) freshWorktree(base, wt, stepID string) error {
	branch := "step/" + stepID
	if _, err := os.Stat(wt); err == nil {
		_, _ = m.run(base, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(wt) // clear the path even if it wasn't a registered worktree
	}
	_, _ = m.run(base, "worktree", "prune")
	_, _ = m.run(base, "branch", "-D", branch) // best-effort; no-op if absent
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return err
	}
	_, err := m.run(base, "worktree", "add", wt, "-b", branch, "HEAD")
	return err
}

func (m *GitManager) For(runID core.RunID, s *flow.Step) (string, func() error, error) {
	lock := m.runLock(runID)
	lock.Lock()
	defer lock.Unlock()

	base := m.baseDir(runID)
	if err := m.ensureRepo(base); err != nil {
		return "", nil, err
	}
	noop := func() error { return nil } // worktrees outlive the step; TeardownRun reclaims them

	if s.Workspace != flow.WSIsolated {
		return base, noop, nil
	}
	wt := filepath.Join(m.wtDir(runID), s.ID)
	if err := m.freshWorktree(base, wt, s.ID); err != nil {
		return "", nil, err
	}
	return wt, noop, nil
}

// TeardownRun removes the run's isolated worktrees (the base repo persists). Best-
// effort and idempotent: a no-op if the run never set up a repo.
func (m *GitManager) TeardownRun(runID core.RunID) error {
	lock := m.runLock(runID)
	lock.Lock()
	defer lock.Unlock()

	base := m.baseDir(runID)
	if _, err := os.Stat(filepath.Join(base, ".git")); err != nil {
		return nil // never started
	}
	entries, err := os.ReadDir(m.wtDir(runID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no isolated steps
		}
		return err
	}
	var firstErr error
	for _, e := range entries {
		wt := filepath.Join(m.wtDir(runID), e.Name())
		if _, err := m.run(base, "worktree", "remove", "--force", wt); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	_, _ = m.run(base, "worktree", "prune")
	return firstErr
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `rtk proxy go test ./internal/workspace/ -v`
Expected: PASS for all `TestGitManager*` (and the existing `Manager` tests).

Run under race: `rtk proxy go test -race ./internal/workspace/`
Expected: ok. Then `go vet ./internal/workspace/` → clean.

- [ ] **Step 5: Commit**

```bash
git add internal/workspace/gitmanager.go internal/workspace/gitmanager_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(workspace): add GitManager (git worktree per isolated step)"
```

---

## Task 3: Tear down run workspaces at run end

**Files:**
- Modify: `internal/engine/engine.go` (`runDAG`, near the top after `defer cancel()`)
- Test: `internal/engine/engine_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/engine/engine_test.go`:

```go
// teardownSpy records TeardownRun calls while delegating For to a real Manager.
type teardownSpy struct {
	*workspace.Manager
	mu   sync.Mutex
	runs []core.RunID
}

func (s *teardownSpy) TeardownRun(id core.RunID) error {
	s.mu.Lock()
	s.runs = append(s.runs, id)
	s.mu.Unlock()
	return s.Manager.TeardownRun(id)
}

func TestRunDAGTearsDownWorkspaceAtEnd(t *testing.T) {
	st := store.NewMem()
	spy := &teardownSpy{Manager: &workspace.Manager{Root: t.TempDir()}}
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    spy,
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: core.SystemClock{},
	}
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.runs) != 1 || spy.runs[0] != "r1" {
		t.Fatalf("expected TeardownRun(r1) once at run end, got %v", spy.runs)
	}
}
```

(`engine_test.go` already imports `sync`, `workspace`, `core`, `executor`, `gate`, `join`, `event`, `store`, `flow`.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `rtk proxy go test ./internal/engine/ -run TestRunDAGTearsDownWorkspaceAtEnd -v`
Expected: FAIL — `TeardownRun` is never called, so `spy.runs` is empty.

- [ ] **Step 3: Add the teardown defer**

In `internal/engine/engine.go`, in `runDAG`, immediately after the opening `ctx, cancel := context.WithCancel(parent)` / `defer cancel()` lines, add:

```go
	// Reclaim the run's isolated worktrees once every step has finished (wg.Wait
	// below) and downstream steps have read upstream artifact paths. Best-effort.
	defer func() {
		if err := e.WS.TeardownRun(runID); err != nil {
			e.logger().Error("teardown run workspaces", "run", runID, "err", err)
		}
	}()
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `rtk proxy go test ./internal/engine/ -v`
Expected: PASS for the new test and all existing engine tests (teardown via the plain `Manager` is a no-op, so nothing else changes).

Run: `rtk proxy go test -race ./internal/engine/` → ok; `go vet ./internal/engine/` → clean.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(engine): tear down run workspaces at run end"
```

---

## Task 4: Validate `stepID` as a path/ref-safe slug

**Files:**
- Modify: `internal/flow/validate.go` (the per-step loop)
- Test: `internal/flow/validate_test.go`

**Why:** `stepID` now becomes a filesystem path segment (`wt/<stepID>`) and a git branch (`step/<stepID>`). Restrict it to a safe slug.

- [ ] **Step 1: Write the failing test**

Add to `internal/flow/validate_test.go`:

```go
func TestValidateRejectsUnsafeStepID(t *testing.T) {
	for _, bad := range []string{"a/b", "has space", "..", ".", "-leading", "weird*char"} {
		f := &Flow{Name: "f", Steps: []*Step{
			{ID: bad, Agent: "mock", Gate: Gate{Policy: GateManual}},
		}}
		if err := Validate(f); err == nil {
			t.Errorf("step id %q should be rejected", bad)
		}
	}
}

func TestValidateAcceptsSlugStepIDs(t *testing.T) {
	for _, ok := range []string{"a", "plan", "impl-api", "w0", "step_1", "v1.2"} {
		f := &Flow{Name: "f", Steps: []*Step{
			{ID: ok, Agent: "mock", Gate: Gate{Policy: GateManual}},
		}}
		if err := Validate(f); err != nil {
			t.Errorf("step id %q should be accepted, got %v", ok, err)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `rtk proxy go test ./internal/flow/ -run 'TestValidateRejectsUnsafeStepID|TestValidateAcceptsSlugStepIDs' -v`
Expected: FAIL — `TestValidateRejectsUnsafeStepID` fails (unsafe ids currently pass validation).

- [ ] **Step 3: Add the slug rule**

In `internal/flow/validate.go`, in the first per-step loop (where `s.ID == ""` and duplicate ids are checked), add the slug check after the empty-id check. The block currently reads:

```go
	for _, s := range f.Steps {
		if s.ID == "" {
			return fmt.Errorf("a step has no id")
		}
		if _, dup := byID[s.ID]; dup {
			return fmt.Errorf("duplicate step id %q", s.ID)
		}
		byID[s.ID] = s
	}
```

Change it to:

```go
	for _, s := range f.Steps {
		if s.ID == "" {
			return fmt.Errorf("a step has no id")
		}
		if !validStepID(s.ID) {
			return fmt.Errorf("step id %q must match [A-Za-z0-9._-], not start with '-', and not be '.'/'..'", s.ID)
		}
		if _, dup := byID[s.ID]; dup {
			return fmt.Errorf("duplicate step id %q", s.ID)
		}
		byID[s.ID] = s
	}
```

Add this helper at the end of `internal/flow/validate.go` (no new imports — byte scan, not regexp):

```go
// validStepID reports whether id is safe as a filesystem path segment and a git
// branch name: [A-Za-z0-9._-], not a leading '-', and not "." or "..".
func validStepID(id string) bool {
	if id == "." || id == ".." || id[0] == '-' {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}
```

(`id[0]` is safe here: the empty-id case already returned above, so `id` is non-empty.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `rtk proxy go test ./internal/flow/ -v`
Expected: PASS for the new tests and all existing flow tests (existing fixtures use slug ids like `a`, `impl-api`, `w0`).

Also confirm the bundled flow still validates:
Run: `rtk proxy go test ./... 2>&1 | grep -E 'FAIL' || echo "no failures"`
Expected: `no failures` (no test or fixture uses an unsafe step id).

- [ ] **Step 5: Commit**

```bash
git add internal/flow/validate.go internal/flow/validate_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(flow): validate step id as a path/ref-safe slug"
```

---

## Task 5: Wire `GitManager` into the daemon + end-to-end coverage

**Files:**
- Modify: `cmd/magisterd/main.go` (the engine wiring)
- Modify: `cmd/magisterd/e2e_test.go` (`crashDaemonAtGate` + a new isolated-worktree test)

**Why:** Make the daemon use real worktrees. The existing e2e flows are all `shared`, so they keep working through `GitManager` (base tree); `crashDaemonAtGate` must switch too so kill/resume uses one consistent Workspace.

- [ ] **Step 1: Write the failing e2e test**

Add to `cmd/magisterd/e2e_test.go` the imports `"os"` and `"os/exec"`, then:

```go
// TestE2EIsolatedWorktreesTornDown runs fan-out isolated steps through the daemon
// (GitManager): each gets its own git worktree, and run-end teardown removes them
// while the base repo persists.
func TestE2EIsolatedWorktreesTornDown(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	base, stop := startDaemon(t, filepath.Join(tmp, "iso.db"))
	defer stop()
	id := postFlow(t, base, "name: f\nconcurrency: 2\nsteps:\n"+
		"  - id: root\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"true\" } }\n"+
		"  - id: a\n    needs: [root]\n    agent: mock\n    workspace: isolated\n    gate: { policy: auto, verifier: { command: \"true\" } }\n"+
		"  - id: b\n    needs: [root]\n    agent: mock\n    workspace: isolated\n    gate: { policy: auto, verifier: { command: \"true\" } }\n")
	waitStatus(t, base, id, "succeeded")

	runDir := filepath.Join(tmp, "runs", id)
	if _, err := os.Stat(filepath.Join(runDir, "base", ".git")); err != nil {
		t.Errorf("base repo should persist after the run: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(runDir, "wt")); err == nil && len(entries) != 0 {
		t.Errorf("isolated worktrees should be torn down at run end, found %d", len(entries))
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `rtk proxy go test ./cmd/magisterd/ -run TestE2EIsolatedWorktreesTornDown -v`
Expected: FAIL — the daemon still wires the plain `Manager`, so isolated steps get `mkdir` dirs at `<runs>/<id>/a` (not `<runs>/<id>/wt/...`), there is no `base/.git`, and nothing is torn down. (The base-repo assertion fails.)

- [ ] **Step 3: Wire `GitManager` in the daemon**

In `cmd/magisterd/main.go`, change the engine's `WS` field. Replace:

```go
		WS:    &workspace.Manager{Root: filepath.Join(filepath.Dir(cfg.DBPath), "runs")},
```
with:
```go
		WS:    &workspace.GitManager{Root: filepath.Join(filepath.Dir(cfg.DBPath), "runs")},
```

- [ ] **Step 4: Switch `crashDaemonAtGate` to `GitManager`**

In `cmd/magisterd/e2e_test.go`, inside `crashDaemonAtGate`, change the inline engine's `WS` so the pre-crash run and the resumed daemon use the same Workspace implementation. Replace:

```go
		WS:    &workspace.Manager{Root: filepath.Join(filepath.Dir(db), "runs")},
```
with:
```go
		WS:    &workspace.GitManager{Root: filepath.Join(filepath.Dir(db), "runs")},
```

- [ ] **Step 5: Run the e2e suite to verify it passes**

Run: `rtk proxy go test ./cmd/magisterd/ -v`
Expected: PASS for `TestE2EIsolatedWorktreesTornDown` and all existing e2e tests — `TestE2EAutoFlowStreamsToCompletion`, `TestE2EManualGateBlocksThenApprove`, `TestE2EKillAndResume`, `TestE2EEscalateBlocksThenApprove`, `TestE2EEscalateKillAndResume` — now routed through `GitManager` (shared flows use the base tree; kill/resume is consistent).

- [ ] **Step 6: Full suite under race + stress + vet**

Run: `rtk proxy go test -race -count=1 ./...`
Expected: every package `ok` (no FAIL). Report the summary.

Run: `GOMAXPROCS=8 rtk proxy go test -race -count=20 ./cmd/magisterd/ ./internal/workspace/ ./internal/engine/`
Expected: all 20 iterations ok (shakes out git/worktree concurrency + resume races). If ANY iteration fails, report it (do NOT paper over).

Run: `rtk proxy go vet ./...` → no issues. Confirm `grep '^go ' go.mod` is still `go 1.22`.

- [ ] **Step 7: Commit**

```bash
git add cmd/magisterd/main.go cmd/magisterd/e2e_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(magisterd): use GitManager for isolated worktrees"
```

---

## Done criteria

- `go test -race ./...` + `go vet ./...` clean; `GOMAXPROCS=8 -count=20` stable on workspace/engine/magisterd.
- `go.mod` still `go 1.22`; no new `require` entries (git via `os/exec`).
- The daemon gives `isolated` steps real `git worktree`s off a scratch per-run repo; `shared` steps use the base tree; worktrees are torn down at run end while the base repo persists; resume re-creates worktrees idempotently; `stepID` is validated as a slug.
- No migration, no new YAML fields, no executor/join changes.
