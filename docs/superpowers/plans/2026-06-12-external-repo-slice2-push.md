# External-Repo Slice 2 (Push Result to a Remote) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an explicit `cm push <run>` / `POST /v1/runs/{id}/push` that pushes a succeeded external-repo run's result branch from the per-run scratch clone to a git remote (the source's `origin` by default), as a new branch (`magister/<runID>` by default).

**Architecture:** Pure post-run, store-driven operation — the engine lifecycle is untouched. Git primitives live in `internal/workspace` (`ResolveRemote`, `PushBranch`) beside Slice 1's `ResolveBase`; orchestration lives in a new `Supervisor.Push` (it owns runs + store + engine→WS) that identifies the result branch from persisted state (`flow.TerminalSteps` + the terminal step's persisted `Artifact.Branch`), resolves the remote, and pushes from the scratch base reached via a new `Workspace.BasePath(runID)`. A thin `POST /v1/runs/{id}/push` handler + `cm push` verb expose it.

**Tech Stack:** Go 1.22, stdlib `net/http` + `os/exec` git. No new deps. Credentials are the daemon's ambient git env (we never handle tokens).

**Spec:** `docs/superpowers/specs/2026-06-12-external-repo-slice2-push-design.md`

**Worktree:** Execute in an isolated worktree (`git worktree add .worktrees/slice2-push -b slice2-push` — native EnterWorktree is broken here).

**Conventions:** single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. `rtk` not installed — run `go`/`git`/`gofmt` directly. Run the WHOLE suite (`go test -race ./...`) between tasks; run `gofmt -l .` yourself (NOT hook-enforced — a pre-existing unformatted `internal/executor/gemini.go` is NOT yours; ignore it if it's the only file listed, but fix any file you touch). Verify `go build ./...` / the touched test BEFORE any `git --amend`.

---

## File Structure

**Created:**
- `internal/flow/graph.go` — `TerminalSteps` (pure DAG helper: steps nothing else `Needs`).
- `internal/workspace/push.go` — `ResolveRemote`, `PushBranch`, ref/remote-name safety helpers.
- `internal/flow/graph_test.go`, `internal/workspace/push_test.go`.

**Modified:**
- `internal/core/ports.go` — `Workspace` += `BasePath(runID RunID) string`.
- `internal/workspace/workspace.go` — `Manager.BasePath`.
- `internal/workspace/gitmanager.go` — `GitManager.BasePath`.
- `internal/engine/engine.go` — `Engine.BasePath` delegator.
- `internal/supervisor/supervisor.go` — `Push` + `PushOpts`/`PushResult`/`PushError` + helpers.
- `internal/api/dto.go` — `pushResponse`.
- `internal/api/handlers.go` — `handlePush`.
- `internal/api/router.go` — the route.
- `cmd/cm/main.go` — `push` dispatch + `client.push`.
- `.claude/skills/running-the-orchestrator/SKILL.md` — push note.

---

## Task 1: `flow.TerminalSteps`

**Files:**
- Create: `internal/flow/graph.go`
- Test: `internal/flow/graph_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/flow/graph_test.go`:

```go
package flow

import "testing"

func TestTerminalStepsFanIn(t *testing.T) {
	f := &Flow{Steps: []*Step{
		{ID: "a"},
		{ID: "b"},
		{ID: "c", Needs: []string{"a", "b"}},
	}}
	terms := TerminalSteps(f)
	if len(terms) != 1 || terms[0].ID != "c" {
		t.Fatalf("terminals = %v, want [c]", ids(terms))
	}
}

func TestTerminalStepsMultipleLeaves(t *testing.T) {
	f := &Flow{Steps: []*Step{
		{ID: "root"},
		{ID: "x", Needs: []string{"root"}},
		{ID: "y", Needs: []string{"root"}},
	}}
	terms := TerminalSteps(f)
	if len(terms) != 2 || terms[0].ID != "x" || terms[1].ID != "y" {
		t.Fatalf("terminals = %v, want [x y]", ids(terms))
	}
}

func TestTerminalStepsLinear(t *testing.T) {
	f := &Flow{Steps: []*Step{
		{ID: "a"},
		{ID: "b", Needs: []string{"a"}},
		{ID: "c", Needs: []string{"b"}},
	}}
	terms := TerminalSteps(f)
	if len(terms) != 1 || terms[0].ID != "c" {
		t.Fatalf("terminals = %v, want [c]", ids(terms))
	}
}

func TestTerminalStepsSingle(t *testing.T) {
	f := &Flow{Steps: []*Step{{ID: "only"}}}
	terms := TerminalSteps(f)
	if len(terms) != 1 || terms[0].ID != "only" {
		t.Fatalf("terminals = %v, want [only]", ids(terms))
	}
}

func ids(steps []*Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.ID
	}
	return out
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/flow/ -run TestTerminalSteps`
Expected: FAIL — `undefined: TerminalSteps`.

- [ ] **Step 3: Implement**

Create `internal/flow/graph.go`:

```go
package flow

// TerminalSteps returns the steps that no other step depends on (nothing lists
// them in its Needs) — the leaves of the DAG. Order follows the flow's step order.
// For a fan-in flow this is the single final join; a flow with independent leaves
// returns several.
func TerminalSteps(f *Flow) []*Step {
	needed := make(map[string]bool)
	for _, s := range f.Steps {
		for _, dep := range s.Needs {
			needed[dep] = true
		}
	}
	var terms []*Step
	for _, s := range f.Steps {
		if !needed[s.ID] {
			terms = append(terms, s)
		}
	}
	return terms
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/flow/ -run TestTerminalSteps -v`
Expected: PASS (all four).

- [ ] **Step 5: Whole suite + gofmt**

Run: `gofmt -l . && go test ./internal/flow/`
Expected: clean + PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/flow/graph.go internal/flow/graph_test.go
git commit -m "feat(flow): TerminalSteps (DAG leaves)"
```

---

## Task 2: `Workspace.BasePath` + `Engine.BasePath`

**Files:**
- Modify: `internal/core/ports.go` (Workspace interface)
- Modify: `internal/workspace/workspace.go` (Manager.BasePath)
- Modify: `internal/workspace/gitmanager.go` (GitManager.BasePath)
- Modify: `internal/engine/engine.go` (Engine.BasePath)
- Test: `internal/workspace/gitmanager_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/workspace/gitmanager_test.go`:

```go
func TestGitManagerBasePath(t *testing.T) {
	root := t.TempDir()
	m := &GitManager{Root: root}
	got := m.BasePath("run-7")
	if got != filepath.Join(root, "run-7", "base") {
		t.Errorf("BasePath = %q, want %q", got, filepath.Join(root, "run-7", "base"))
	}
}

func TestManagerBasePath(t *testing.T) {
	root := t.TempDir()
	m := &Manager{Root: root}
	got := m.BasePath("run-7")
	if got != filepath.Join(root, "run-7") {
		t.Errorf("BasePath = %q, want %q", got, filepath.Join(root, "run-7"))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/workspace/ -run BasePath`
Expected: FAIL — compile error (`m.BasePath undefined`).

- [ ] **Step 3: Add to the Workspace interface**

In `internal/core/ports.go`, add to the `Workspace` interface (after `Provision`, before `TeardownRun`):

```go
	// BasePath returns the on-disk path of a run's per-run base repo (the scratch
	// clone for an external-repo run). Safe to call any time; the path may not exist
	// yet. Post-run delivery (push) reads the result branch from here.
	BasePath(runID RunID) string
```

- [ ] **Step 4: Implement on both managers**

In `internal/workspace/gitmanager.go`, add (near `baseDir`):

```go
// BasePath exposes the per-run base repo path for post-run delivery (push).
func (m *GitManager) BasePath(runID core.RunID) string { return m.baseDir(runID) }
```

In `internal/workspace/workspace.go`, add:

```go
// BasePath returns the run's directory. The plain Manager has no git backing, so
// this is informational; push only targets GitManager-backed external-repo runs.
func (m *Manager) BasePath(runID core.RunID) string {
	return filepath.Join(m.Root, string(runID))
}
```

(`internal/workspace/workspace.go` already imports `path/filepath`.)

- [ ] **Step 5: Add the Engine delegator**

In `internal/engine/engine.go`, near `Provision`:

```go
// BasePath returns a run's scratch base repo path (see core.Workspace.BasePath),
// so the supervisor can reach it for post-run delivery without holding the WS itself.
func (e *Engine) BasePath(runID core.RunID) string {
	return e.WS.BasePath(runID)
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/workspace/ -run BasePath -v`
Expected: PASS.

- [ ] **Step 7: Whole suite -race + gofmt + vet**

Run: `gofmt -l . && go vet ./... && go test -race ./...`
Expected: clean + all PASS. The interface change forces both managers + the test doubles (`teardownSpy`/`provisionSpy` embed `*Manager`, inheriting `BasePath`) to satisfy it — the `var _ core.Workspace = (*GitManager)(nil)` assertion confirms it.

- [ ] **Step 8: Commit**

```bash
git add internal/core/ports.go internal/workspace internal/engine/engine.go
git commit -m "feat(workspace): BasePath seam (reach a run's scratch base)"
```

---

## Task 3: `workspace.ResolveRemote`

**Files:**
- Create: `internal/workspace/push.go`
- Test: `internal/workspace/push_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/workspace/push_test.go`. It reuses `requireGit`/`gitOut`/`setupSourceRepo` from the package's existing test files:

```go
package workspace

import "testing"

func TestResolveRemoteOriginDefault(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t)
	bare := t.TempDir()
	gitOut(t, bare, "init", "--bare")
	gitOut(t, src, "remote", "add", "origin", bare)

	got, err := ResolveRemote(src, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != bare {
		t.Errorf("ResolveRemote origin = %q, want %q", got, bare)
	}
}

func TestResolveRemoteByName(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t)
	bare := t.TempDir()
	gitOut(t, bare, "init", "--bare")
	gitOut(t, src, "remote", "add", "upstream", bare)

	got, err := ResolveRemote(src, "upstream")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != bare {
		t.Errorf("ResolveRemote upstream = %q, want %q", got, bare)
	}
}

func TestResolveRemoteURLPassthrough(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t)
	for _, url := range []string{"https://example.com/me/x.git", "git@github.com:me/x.git"} {
		got, err := ResolveRemote(src, url)
		if err != nil {
			t.Fatalf("resolve %q: %v", url, err)
		}
		if got != url {
			t.Errorf("ResolveRemote(%q) = %q, want passthrough", url, got)
		}
	}
}

func TestResolveRemoteMissing(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t) // no remotes configured
	if _, err := ResolveRemote(src, ""); err == nil {
		t.Error("expected error when origin is absent")
	}
}

func TestResolveRemoteRejectsBadName(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t)
	if _, err := ResolveRemote(src, "--upload-pack=x"); err == nil {
		t.Error("expected error for a flag-like remote name")
	}
}

func TestResolveRemoteRejectsRelativeSource(t *testing.T) {
	requireGit(t)
	if _, err := ResolveRemote("relative/path", ""); err == nil {
		t.Error("expected error for a relative source path")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/workspace/ -run TestResolveRemote`
Expected: FAIL — `undefined: ResolveRemote`.

- [ ] **Step 3: Implement**

Create `internal/workspace/push.go` (uses the package-private `gitRead` from `provision.go`):

```go
package workspace

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ResolveRemote resolves the push destination for a run's source repo, read-only:
//   - remote == ""        → the source's `origin` URL
//   - remote is a URL     → returned as-is (git validates/authenticates it)
//   - remote is a name    → the source's URL for that remote
// The source repo's refs/working tree are never written.
func ResolveRemote(sourceRepo, remote string) (string, error) {
	if sourceRepo == "" || !filepath.IsAbs(sourceRepo) {
		return "", fmt.Errorf("source repo path must be absolute: %q", sourceRepo)
	}
	if looksLikeURL(remote) {
		return remote, nil
	}
	name := remote
	if name == "" {
		name = "origin"
	}
	if !safeRemoteName(name) {
		return "", fmt.Errorf("invalid remote name %q", name)
	}
	out, err := gitRead(sourceRepo, "remote", "get-url", name)
	if err != nil {
		return "", fmt.Errorf("no remote %q in %q", name, sourceRepo)
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("remote %q has no url", name)
	}
	return url, nil
}

// looksLikeURL reports whether s is a git URL (scheme://… or scp-like user@host:path)
// rather than a remote name.
func looksLikeURL(s string) bool {
	if strings.Contains(s, "://") {
		return true
	}
	if i := strings.IndexByte(s, ':'); i > 0 && !strings.Contains(s[:i], "/") {
		return true // user@host:path
	}
	return false
}

// safeRemoteName accepts a conservative remote-name charset and rejects a leading
// "-", so a flag-like name can't be smuggled into `git remote get-url`.
func safeRemoteName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/workspace/ -run TestResolveRemote -v`
Expected: PASS (all six).

- [ ] **Step 5: Whole suite + gofmt**

Run: `gofmt -l . && go test ./internal/workspace/`
Expected: clean + PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/workspace/push.go internal/workspace/push_test.go
git commit -m "feat(workspace): ResolveRemote (source origin / name / URL)"
```

---

## Task 4: `workspace.PushBranch`

**Files:**
- Modify: `internal/workspace/push.go` (add `PushBranch` + `safeRef`)
- Test: `internal/workspace/push_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/workspace/push_test.go`:

```go
// setupScratchWithBranch builds a scratch repo with a committed branch and returns
// (dir, branch, sha). The branch carries one file so the commit is non-empty.
func setupScratchWithBranch(t *testing.T, branch string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	gitOut(t, dir, "init")
	gitOut(t, dir, "config", "user.name", "fix")
	gitOut(t, dir, "config", "user.email", "fix@example.com")
	gitOut(t, dir, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "-A")
	gitOut(t, dir, "commit", "-m", "work")
	return dir, gitOut(t, dir, "rev-parse", "HEAD")
}

func TestPushBranchNewBranch(t *testing.T) {
	requireGit(t)
	scratch, sha := setupScratchWithBranch(t, "step/integrate")
	bare := t.TempDir()
	gitOut(t, bare, "init", "--bare")

	if err := PushBranch(scratch, bare, "step/integrate", "magister/run-1", false); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := gitOut(t, bare, "rev-parse", "magister/run-1"); got != sha {
		t.Errorf("remote ref = %q, want %q", got, sha)
	}
}

func TestPushBranchRefusesNonFastForwardWithoutForce(t *testing.T) {
	requireGit(t)
	scratch, _ := setupScratchWithBranch(t, "step/integrate")
	bare := t.TempDir()
	gitOut(t, bare, "init", "--bare")
	if err := PushBranch(scratch, bare, "step/integrate", "magister/run-1", false); err != nil {
		t.Fatalf("first push: %v", err)
	}
	// Rewrite the branch to a different history (non-fast-forward).
	gitOut(t, scratch, "commit", "--amend", "-m", "rewritten")
	if err := PushBranch(scratch, bare, "step/integrate", "magister/run-1", false); err == nil {
		t.Error("expected non-fast-forward push to be refused without --force")
	}
	if err := PushBranch(scratch, bare, "step/integrate", "magister/run-1", true); err != nil {
		t.Errorf("force push should succeed: %v", err)
	}
}

func TestPushBranchRejectsFlaglikeBranch(t *testing.T) {
	requireGit(t)
	scratch, _ := setupScratchWithBranch(t, "step/integrate")
	bare := t.TempDir()
	gitOut(t, bare, "init", "--bare")
	if err := PushBranch(scratch, bare, "step/integrate", "--force", false); err == nil {
		t.Error("expected a flag-like destination branch to be rejected")
	}
}
```

Add `"os"` and `"path/filepath"` to `push_test.go`'s imports (currently only `testing`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/workspace/ -run TestPushBranch`
Expected: FAIL — `undefined: PushBranch`.

- [ ] **Step 3: Implement**

Add to `internal/workspace/push.go` — add `"os/exec"` to its imports, then:

```go
// PushBranch pushes srcBranch from the scratch clone to destBranch on remoteURL.
// Without force, git refuses a non-fast-forward overwrite of an existing ref; a
// new branch always succeeds. The combined git output rides on the error so push
// failures (auth/network/non-fast-forward) surface the remote's message. Credentials
// come from the ambient git environment — none are handled here.
func PushBranch(scratchBase, remoteURL, srcBranch, destBranch string, force bool) error {
	if !safeRef(srcBranch) {
		return fmt.Errorf("invalid source branch %q", srcBranch)
	}
	if !safeRef(destBranch) {
		return fmt.Errorf("invalid destination branch %q", destBranch)
	}
	args := []string{"push"}
	if force {
		args = append(args, "--force")
	}
	// -- separates the <repository> <refspec> operands from options, so a "-"-leading
	// remote/branch can't be parsed as a flag (the refs are also safeRef-validated).
	args = append(args, "--", remoteURL, srcBranch+":refs/heads/"+destBranch)
	// #nosec G204 -- git push without a shell; refs validated; operands after --.
	cmd := exec.Command("git", args...)
	cmd.Dir = scratchBase
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// safeRef accepts a conservative branch-name charset (rejecting a leading "-",
// "..", and anything outside [A-Za-z0-9/._-]) so a name cannot smuggle a flag or
// corrupt the src:refs/heads/dest refspec.
func safeRef(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/workspace/ -run TestPushBranch -v`
Expected: PASS (all three).

- [ ] **Step 5: Whole suite -race + gofmt**

Run: `gofmt -l . && go test -race ./internal/workspace/`
Expected: clean + PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/workspace/push.go internal/workspace/push_test.go
git commit -m "feat(workspace): PushBranch (push result branch to a remote)"
```

---

## Task 5: `Supervisor.Push`

**Files:**
- Modify: `internal/supervisor/supervisor.go` (Push + PushOpts/PushResult/PushError + helpers)
- Test: `internal/supervisor/push_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/supervisor/push_test.go`. The happy path runs a real GitManager-backed external-repo flow (like the engine/join tests) then pushes to a bare remote; the error paths seed the mem store and fail before any git:

```go
package supervisor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/flow"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

func requireGitS(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func gitS(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// srcWithRemote builds a committed fixture repo whose origin is a bare remote;
// returns (sourceDir, bareDir, baseSHA).
func srcWithRemote(t *testing.T) (string, string, string) {
	t.Helper()
	src := t.TempDir()
	gitS(t, src, "init")
	gitS(t, src, "config", "user.name", "fix")
	gitS(t, src, "config", "user.email", "fix@example.com")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitS(t, src, "add", "-A")
	gitS(t, src, "commit", "-m", "base")
	bare := t.TempDir()
	gitS(t, bare, "init", "--bare")
	gitS(t, src, "remote", "add", "origin", bare)
	return src, bare, gitS(t, src, "rev-parse", "HEAD")
}

const extRepoFlowYAML = `name: external-repo
concurrency: 2
steps:
  - id: build-api
    agent: mock
    workspace: isolated
    gate: { policy: auto, verifier: { command: "true" } }
  - id: build-ui
    agent: mock
    workspace: isolated
    gate: { policy: auto, verifier: { command: "true" } }
  - id: integrate
    needs: [build-api, build-ui]
    workspace: isolated
    join: { strategy: merge }
    gate: { policy: auto, verifier: { command: "true" } }
`

func TestPushDeliversResultToRemote(t *testing.T) {
	requireGitS(t)
	src, bare, sha := srcWithRemote(t)
	st := store.NewMem()
	reg := NewApprovalRegistry()
	gm := &workspace.GitManager{Root: t.TempDir()}
	sup := New(testEngine(t, st, reg, gm), st, reg)
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

	res, err := sup.Push(context.Background(), id, PushOpts{})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.Branch != "magister/"+string(id) {
		t.Errorf("dest branch = %q, want magister/%s", res.Branch, id)
	}
	if got := gitS(t, bare, "rev-parse", res.Branch); got != res.Commit {
		t.Errorf("remote ref = %q, want pushed commit %q", got, res.Commit)
	}
	// the pushed tree carries the cloned base + both step outputs
	tree := gitS(t, bare, "ls-tree", "--name-only", res.Branch)
	for _, want := range []string{"README.md", "build-api.out.md", "build-ui.out.md"} {
		if !strings.Contains(tree, want) {
			t.Errorf("remote tree missing %q; got %q", want, tree)
		}
	}
}

func pushErrStatus(t *testing.T, err error) int {
	t.Helper()
	var pe *PushError
	if !errors.As(err, &pe) {
		t.Fatalf("want *PushError, got %v", err)
	}
	return pe.Status
}

func TestPushRejectsNonExternalRepoRun(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Status: core.RunSucceeded,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n",
	})
	_, err := sup.Push(context.Background(), "r1", PushOpts{})
	if got := pushErrStatus(t, err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
}

func TestPushRejectsUnsucceededRun(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Repo: "/abs/proj", Status: core.RunRunning,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n",
	})
	_, err := sup.Push(context.Background(), "r1", PushOpts{})
	if got := pushErrStatus(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409", got)
	}
}

func TestPushAmbiguousTerminal(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Repo: "/abs/proj", Status: core.RunSucceeded,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n  - id: b\n    agent: mock\n",
	})
	_, err := sup.Push(context.Background(), "r1", PushOpts{})
	if got := pushErrStatus(t, err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (ambiguous)", got)
	}
}

func TestPushUnknownRun(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	_, err := sup.Push(context.Background(), "nope", PushOpts{})
	if got := pushErrStatus(t, err); got != http.StatusNotFound {
		t.Errorf("status = %d, want 404", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/supervisor/ -run TestPush`
Expected: FAIL — `undefined: PushOpts` / `sup.Push undefined`.

- [ ] **Step 3: Implement**

In `internal/supervisor/supervisor.go`: add `"net/http"`, `"os"`, `"path/filepath"`, and `"concentus/internal/workspace"` to the imports, then add:

```go
// PushOpts configures Push. Zero values mean: origin remote, magister/<runID>
// destination, the unique terminal step, no force.
type PushOpts struct {
	Remote string // "" → source's origin; a remote name or a URL otherwise
	As     string // "" → magister/<runID>
	Step   string // "" → the unique terminal step
	Force  bool
}

// PushResult is returned by Push on success.
type PushResult struct {
	Remote       string
	Branch       string // destination branch on the remote
	SourceBranch string // the run's result branch that was pushed
	Commit       string
}

// PushError carries an HTTP status so the API layer maps failures without
// string-matching.
type PushError struct {
	Status int
	Msg    string
}

func (e *PushError) Error() string { return e.Msg }

func pushErr(status int, format string, a ...any) *PushError {
	return &PushError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// Push delivers a succeeded external-repo run's result branch to a remote. It is a
// post-run, store-driven operation (the engine lifecycle is untouched): it reads the
// run, identifies the result step (the unique terminal, or opts.Step), reads that
// step's persisted branch, resolves the remote, and pushes from the scratch clone.
// Errors are *PushError with an HTTP status (see the slice-2 spec).
func (s *Supervisor) Push(ctx context.Context, runID core.RunID, opts PushOpts) (PushResult, error) {
	rs, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return PushResult{}, pushErr(http.StatusNotFound, "unknown run %q", runID)
	}
	if rs.Repo == "" {
		return PushResult{}, pushErr(http.StatusBadRequest, "run %q is not an external-repo run (no --repo)", runID)
	}
	if rs.Status != core.RunSucceeded {
		return PushResult{}, pushErr(http.StatusConflict, "run %q is %s, not succeeded", runID, rs.Status)
	}
	f, err := flow.ParseBytes([]byte(rs.FlowYAML))
	if err != nil {
		return PushResult{}, pushErr(http.StatusInternalServerError, "parse stored flow: %v", err)
	}
	step, perr := pickResultStep(f, opts.Step)
	if perr != nil {
		return PushResult{}, perr
	}
	branch, commit := stepBranch(rs, step.ID)
	if branch == "" {
		return PushResult{}, pushErr(http.StatusBadRequest, "step %q has no branch (not an isolated/committed step)", step.ID)
	}
	remoteURL, err := workspace.ResolveRemote(rs.Repo, opts.Remote)
	if err != nil {
		return PushResult{}, pushErr(http.StatusBadRequest, "remote: %v", err)
	}
	dest := opts.As
	if dest == "" {
		dest = "magister/" + string(runID)
	}
	base := s.engine.BasePath(runID)
	if base == "" || !dirHasGit(base) {
		return PushResult{}, pushErr(http.StatusNotFound, "scratch repo for run %q not found (reclaimed?)", runID)
	}
	if err := workspace.PushBranch(base, remoteURL, branch, dest, opts.Force); err != nil {
		return PushResult{}, pushErr(http.StatusBadGateway, "%v", err)
	}
	return PushResult{Remote: remoteURL, Branch: dest, SourceBranch: branch, Commit: commit}, nil
}

// pickResultStep selects the step whose branch to push: opts.Step if given, else
// the unique terminal step; zero/ambiguous → error.
func pickResultStep(f *flow.Flow, stepID string) (*flow.Step, *PushError) {
	if stepID != "" {
		for _, st := range f.Steps {
			if st.ID == stepID {
				return st, nil
			}
		}
		return nil, pushErr(http.StatusBadRequest, "unknown step %q", stepID)
	}
	terms := flow.TerminalSteps(f)
	switch len(terms) {
	case 1:
		return terms[0], nil
	case 0:
		return nil, pushErr(http.StatusBadRequest, "flow has no terminal step")
	default:
		ids := make([]string, len(terms))
		for i, t := range terms {
			ids[i] = t.ID
		}
		return nil, pushErr(http.StatusBadRequest, "ambiguous result: %d terminal steps %v; use --step", len(terms), ids)
	}
}

// stepBranch returns the persisted branch+commit a step committed to (carried on
// each of its artifacts); empty branch if the step committed nothing.
func stepBranch(rs core.RunState, stepID string) (branch, commit string) {
	for _, st := range rs.Steps {
		if st.StepID != stepID {
			continue
		}
		for _, a := range st.Artifacts {
			if a.Branch != "" {
				return a.Branch, a.Commit
			}
		}
	}
	return "", ""
}

func dirHasGit(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/supervisor/ -run TestPush -v`
Expected: PASS (happy path delivers to the bare remote; error paths return the right status).

- [ ] **Step 5: Whole suite -race + gofmt + vet**

Run: `gofmt -l . && go vet ./... && go test -race ./...`
Expected: clean + all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/supervisor/supervisor.go internal/supervisor/push_test.go
git commit -m "feat(supervisor): Push delivers a run's result branch to a remote"
```

---

## Task 6: API endpoint `POST /v1/runs/{id}/push`

**Files:**
- Modify: `internal/api/dto.go` (pushResponse)
- Modify: `internal/api/handlers.go` (handlePush)
- Modify: `internal/api/router.go` (route)
- Test: `internal/api/handlers_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/api/handlers_test.go`. `setupAPISourceRepo` already exists (Slice 1). The happy push test needs an httptest server whose engine uses a **GitManager** (so external-repo provisioning + the scratch base exist) — `testServer`/`newServerOnly` use the plain `workspace.Manager`, so add an additive `newGitServer` helper rather than perturbing the existing Slice-1 tests. The error paths reuse the plain `testServer`. The happy path runs a real external-repo flow over HTTP then pushes:

```go
// newGitServer is like testServer but its engine uses a real GitManager, so
// external-repo runs actually clone + produce a scratch base (needed by push).
func newGitServer(t *testing.T) (*httptest.Server, core.Store) {
	t.Helper()
	st := store.NewMem()
	reg := supervisor.NewApprovalRegistry()
	bus := event.NewBus()
	eng := &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.GitManager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: &supervisor.RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: core.SystemClock{},
	}
	sup := supervisor.New(eng, st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	srv := &Server{Sup: sup, Store: st, Bus: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ShutdownTimeout: time.Second}
	hs := httptest.NewServer(srv.Router(""))
	t.Cleanup(func() { hs.Close() })
	return hs, st
}

func TestPushEndpointDeliversToRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src, _ := setupAPISourceRepo(t)
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")
	runGit(t, src, "remote", "add", "origin", bare)

	hs, st := newGitServer(t)
	// run the merge flow against src
	body := extRepoFlowAPI
	resp, err := http.Post(hs.URL+"/v1/runs?repo="+url.QueryEscape(src)+"&base=HEAD",
		"application/x-yaml", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()
	waitForStatus(t, st, rr.ID, core.RunSucceeded)

	presp, err := http.Post(hs.URL+"/v1/runs/"+string(rr.ID)+"/push", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer presp.Body.Close()
	if presp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(presp.Body)
		t.Fatalf("push = %d, want 200: %s", presp.StatusCode, b)
	}
	var pr pushResponse
	if err := json.NewDecoder(presp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pr.Branch != "magister/"+string(rr.ID) {
		t.Errorf("dest = %q, want magister/%s", pr.Branch, rr.ID)
	}
	if got := runGit(t, bare, "rev-parse", pr.Branch); got != pr.Commit {
		t.Errorf("remote ref = %q, want %q", got, pr.Commit)
	}
}

func TestPushEndpointUnknownRun404(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs/nope/push", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPushEndpointNonExternalRepo400(t *testing.T) {
	hs, _, st := testServer(t)
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Status: core.RunSucceeded,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n",
	})
	resp, err := http.Post(hs.URL+"/v1/runs/r1/push", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// runGit is a local git helper for the api tests (mirrors setupAPISourceRepo's inner runner).
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

const extRepoFlowAPI = `name: external-repo
concurrency: 2
steps:
  - id: build-api
    agent: mock
    workspace: isolated
    gate: { policy: auto, verifier: { command: "true" } }
  - id: build-ui
    agent: mock
    workspace: isolated
    gate: { policy: auto, verifier: { command: "true" } }
  - id: integrate
    needs: [build-api, build-ui]
    workspace: isolated
    join: { strategy: merge }
    gate: { policy: auto, verifier: { command: "true" } }
`
```

NOTE: `newGitServer` needs the api test file to import `log/slog`, `io`, `net/http/httptest`, `time`, and `engine`/`event`/`executor`/`gate`/`join`/`store`/`supervisor`/`workspace` — `handlers_test.go` already imports all of these (it constructs the same engine in `newServerOnly`). Add `os/exec` + `strings` if not already present (for `runGit`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestPushEndpoint`
Expected: FAIL — `pushResponse` undefined / route 404 for the push path / `s.handlePush` undefined.

- [ ] **Step 3: Add the DTO**

In `internal/api/dto.go`:

```go
// pushResponse is returned from POST /v1/runs/{id}/push.
type pushResponse struct {
	Remote       string `json:"remote"`
	Branch       string `json:"branch"`
	SourceBranch string `json:"source_branch"`
	Commit       string `json:"commit"`
}
```

- [ ] **Step 4: Add the handler**

In `internal/api/handlers.go` (the file already imports `errors`, `net/http`, `core`, `supervisor`), add:

```go
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := supervisor.PushOpts{
		Remote: q.Get("remote"),
		As:     q.Get("as"),
		Step:   q.Get("step"),
		Force:  q.Get("force") == "true",
	}
	res, err := s.Sup.Push(r.Context(), core.RunID(r.PathValue("id")), opts)
	if err != nil {
		var pe *supervisor.PushError
		if errors.As(err, &pe) {
			writeError(w, pe.Status, pe.Msg)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pushResponse{
		Remote: res.Remote, Branch: res.Branch, SourceBranch: res.SourceBranch, Commit: res.Commit,
	})
}
```

- [ ] **Step 5: Add the route**

In `internal/api/router.go`, after the approve route:

```go
	v1.HandleFunc("POST /v1/runs/{id}/push", s.handlePush)
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/api/ -run TestPush -v`
Expected: PASS (delivery 200 + ref on the bare remote; 404; 400).

- [ ] **Step 7: Whole suite -race + gofmt + vet**

Run: `gofmt -l . && go vet ./... && go test -race ./...`
Expected: clean + all PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/api
git commit -m "feat(api): POST /v1/runs/{id}/push delivers the result to a remote"
```

---

## Task 7: `cm push`

**Files:**
- Modify: `cmd/cm/main.go` (dispatch case + client.push)
- Test: `cmd/cm/main_test.go`

- [ ] **Step 1: Write the failing test**

Add to `cmd/cm/main_test.go` (uses the existing `fakeAPI`):

```go
func TestPushPassesOptionsAsQuery(t *testing.T) {
	var got http.Request
	srv := fakeAPI(t, http.StatusOK,
		`{"remote":"git@h:me/x.git","branch":"magister/r1","source_branch":"step/integrate","commit":"abc"}`, &got)
	defer srv.Close()

	var out bytes.Buffer
	code := dispatch([]string{"push", "r1", "--remote", "upstream", "--as", "feature/x", "--step", "integrate", "--force"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v1/runs/r1/push" {
		t.Errorf("request = %s %s, want POST /v1/runs/r1/push", got.Method, got.URL.Path)
	}
	q := got.URL.Query()
	if q.Get("remote") != "upstream" || q.Get("as") != "feature/x" || q.Get("step") != "integrate" || q.Get("force") != "true" {
		t.Errorf("query = %v, want remote/as/step/force set", q)
	}
	if !strings.Contains(out.String(), "magister/r1") {
		t.Errorf("output missing dest branch: %q", out.String())
	}
}

func TestPushRequiresRun(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"push"}, "http://x", &out); code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/cm/ -run TestPush`
Expected: FAIL — `push` is an unknown command (exit 2, wrong path captured).

- [ ] **Step 3: Add the dispatch case**

In `cmd/cm/main.go` `dispatch`, add a case (and update the usage line to include `push`):

```go
	case "push":
		return c.push(args[1:], out)
```

Update the top usage string in `dispatch` to: `"usage: cm <run|ls|get|watch|approve|reject|cancel|push> ..."`.

- [ ] **Step 4: Implement `client.push`**

In `cmd/cm/main.go` (imports already include `net/url`, `encoding/json`, `net/http`, `fmt`, `io`), add:

```go
func (c *client) push(args []string, out io.Writer) int {
	var run, remote, as, step string
	force := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--force":
			force = true
		case "--remote":
			i++
			if i >= len(args) {
				fmt.Fprintln(out, "usage: --remote requires a value")
				return 2
			}
			remote = args[i]
		case "--as":
			i++
			if i >= len(args) {
				fmt.Fprintln(out, "usage: --as requires a value")
				return 2
			}
			as = args[i]
		case "--step":
			i++
			if i >= len(args) {
				fmt.Fprintln(out, "usage: --step requires a value")
				return 2
			}
			step = args[i]
		default:
			run = args[i]
		}
	}
	if run == "" {
		fmt.Fprintln(out, "usage: cm push <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--force]")
		return 2
	}
	q := url.Values{}
	if remote != "" {
		q.Set("remote", remote)
	}
	if as != "" {
		q.Set("as", as)
	}
	if step != "" {
		q.Set("step", step)
	}
	if force {
		q.Set("force", "true")
	}
	endpoint := c.base + "/v1/runs/" + run + "/push"
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	resp, err := c.http.Post(endpoint, "application/json", nil)
	if err != nil {
		fmt.Fprintln(out, "push:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	var pr struct {
		Remote       string `json:"remote"`
		Branch       string `json:"branch"`
		SourceBranch string `json:"source_branch"`
	}
	json.NewDecoder(resp.Body).Decode(&pr)
	fmt.Fprintf(out, "pushed %s → %s on %s\n", pr.SourceBranch, pr.Branch, pr.Remote)
	return 0
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./cmd/cm/ -run TestPush -v`
Expected: PASS.

- [ ] **Step 6: Whole suite + gofmt**

Run: `gofmt -l . && go test ./...`
Expected: clean + all PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/cm/main.go cmd/cm/main_test.go
git commit -m "feat(cm): push <run> delivers the result to a remote"
```

---

## Task 8: Docs + manual proof + final suite

**Files:**
- Modify: `.claude/skills/running-the-orchestrator/SKILL.md`

- [ ] **Step 1: Document the push in the run skill**

In `.claude/skills/running-the-orchestrator/SKILL.md`, under the *External repo* section, add a short subsection: after an external-repo run succeeds, `cm push <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--force]` pushes the result branch to a remote (default: the source's `origin`; default dest `magister/<runID>`). Credentials come from the daemon's ambient git env; a push failure returns the git error. Update the `cm command surface` line to add `push`.

- [ ] **Step 2: Manual proof**

Build + run the daemon (see the run skill). Then:

```bash
SRC=$(mktemp -d); BARE=$(mktemp -d)
git -C "$SRC" init -q && git -C "$SRC" config user.name demo && git -C "$SRC" config user.email demo@x
echo readme > "$SRC/README.md"; git -C "$SRC" add -A && git -C "$SRC" commit -qm base
git -C "$BARE" init -q --bare
git -C "$SRC" remote add origin "$BARE"

export MAGISTER_ADDR=http://127.0.0.1:8099   # must include the http:// scheme
RUN=$(cm run flows/external-repo.yaml --repo "$SRC" --base HEAD)
# wait for success (cm get <RUN> shows "succeeded"), then:
cm push "$RUN"
git -C "$BARE" log --oneline --graph "magister/$RUN"
git -C "$BARE" cat-file -p "magister/$RUN" | grep -c parent   # 2 = real merge commit
git -C "$BARE" ls-tree --name-only "magister/$RUN"            # README.md + build-*.out.md
```

Expected: the bare remote has `magister/<RUN>` as a 2-parent merge commit whose tree carries the cloned `README.md` plus both step outputs. Capture the output for the handoff.

- [ ] **Step 3: Final whole-suite gate**

Run: `gofmt -l . && go vet ./... && go test -race ./...`
Expected: `gofmt -l` clean (except the pre-existing `internal/executor/gemini.go`), vet clean, all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add .claude/skills/running-the-orchestrator/SKILL.md
git commit -m "docs(external-repo): cm push note in run skill"
```

---

## Done criteria

- `cm push <run>` / `POST /v1/runs/{id}/push` pushes a succeeded external-repo run's terminal-step branch to a remote (source `origin` default), as `magister/<runID>` (default), refusing to clobber an existing ref without `--force`.
- The source repo is never written; credentials are the daemon's ambient git env.
- Errors map cleanly: 404 unknown/missing-scratch, 400 not-external-repo/ambiguous/no-branch/bad-step, 409 not-succeeded, 502 push failed (git stderr surfaced).
- `gofmt -l` clean, `go vet ./...` clean, `go test -race ./...` green.

## Carried follow-ups (note in the handoff)

- Slice 3 (open a PR) builds on the pushed branch.
- `looksLikeURL` is a heuristic (scheme:// or scp-like `user@host:path`); a Windows-style `C:\path` remote name would misclassify — irrelevant on darwin/linux, note it.
- Scratch lifetime: push needs the scratch base to persist (it does); a future scratch-GC slice must order GC after push or the `404 missing scratch` path applies.
