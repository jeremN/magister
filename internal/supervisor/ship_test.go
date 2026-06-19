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
