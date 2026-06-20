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

// The error-path tests use the plain Manager (not GitManager): Push fails on the
// repo/status/flow checks before it ever reaches the scratch-dir or git steps.
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

// TestPushNoBranch: a succeeded external-repo run whose terminal step committed no
// branch (e.g. a shared step) → 400, before any git.
func TestPushNoBranch(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Repo: "/abs/proj", Status: core.RunSucceeded,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n",
		Steps:    []core.StepState{{StepID: "a", Status: core.StepSucceeded}}, // no artifacts → no branch
	})
	_, err := sup.Push(context.Background(), "r1", PushOpts{})
	if got := pushErrStatus(t, err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (no branch)", got)
	}
}

// TestPickResultStep covers the result-step selection logic directly: default
// (unique terminal), an explicit --step (honored even when not terminal), an
// unknown --step (400), and an ambiguous default (two leaves, 400).
func TestPickResultStep(t *testing.T) {
	f := &flow.Flow{Steps: []*flow.Step{
		{ID: "a"}, {ID: "b"}, {ID: "c", Needs: []string{"a", "b"}},
	}}
	if step, perr := pickResultStep(f, ""); perr != nil || step.ID != "c" {
		t.Fatalf("default = %v / %v, want step c", step, perr)
	}
	if step, perr := pickResultStep(f, "a"); perr != nil || step.ID != "a" {
		t.Fatalf("--step a = %v / %v, want step a", step, perr)
	}
	if _, perr := pickResultStep(f, "ghost"); perr == nil || perr.Status != http.StatusBadRequest {
		t.Fatalf("unknown step = %v, want 400", perr)
	}
	two := &flow.Flow{Steps: []*flow.Step{{ID: "x"}, {ID: "y"}}}
	if _, perr := pickResultStep(two, ""); perr == nil || perr.Status != http.StatusBadRequest {
		t.Fatalf("ambiguous = %v, want 400", perr)
	}
}
