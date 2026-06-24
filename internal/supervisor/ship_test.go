package supervisor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n    prompt: p\n",
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

// TestShipForkPushesToHeadRepo: a fork ship (HeadRepo set) pushes the result branch to
// the FORK remote, not to the source origin. Proven with the fork as a local bare repo:
// magister/<run> lands on the fork bare. (prCore then 400s on the non-github origin base
// — irrelevant here; this test asserts only the push destination.)
func TestShipForkPushesToHeadRepo(t *testing.T) {
	requireGitS(t)
	src, _, sha := srcWithRemote(t) // origin = a bare local repo (the "upstream")
	fork := t.TempDir()
	gitS(t, fork, "init", "--bare") // the "fork": a second bare local repo
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

	// Fork ship: the push must route to the fork. (The PR half then fails 400 on the
	// non-github origin base; we ignore that and assert the push landed on the fork.)
	_, _ = sup.Ship(context.Background(), id, ShipOpts{HeadRepo: "file://" + fork})

	// gitS fatals if the ref is absent, so a successful rev-parse IS the assertion: the
	// branch is on the fork. If Ship had ignored HeadRepo and pushed to origin, the fork
	// bare would be empty and this would fail.
	gitS(t, fork, "rev-parse", "--verify", "magister/"+string(id))
}

// TestShipForkThreadsHeadRepoToPR: a fork ship threads HeadRepo into prCore. The source
// origin is a (never-contacted) github URL so the PR base parses; the fork is a local
// bare so the push succeeds. prCore then fails to parse the local-path HeadRepo as a
// github remote → a 400 whose message begins "head-repo:". That 400 can only arise if
// Ship forwarded HeadRepo to prCore (with HeadRepo unset, prCore would parse the github
// base and proceed to gh, not 400).
func TestShipForkThreadsHeadRepoToPR(t *testing.T) {
	requireGitS(t)
	// A committed source whose origin is a fake github URL (read by prCore, never fetched).
	src := t.TempDir()
	gitS(t, src, "init")
	gitS(t, src, "config", "user.name", "fix")
	gitS(t, src, "config", "user.email", "fix@example.com")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitS(t, src, "add", "-A")
	gitS(t, src, "commit", "-m", "base")
	gitS(t, src, "remote", "add", "origin", "https://github.com/test-owner/test-repo.git")
	sha := gitS(t, src, "rev-parse", "HEAD")

	fork := t.TempDir()
	gitS(t, fork, "init", "--bare")

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

	_, err = sup.Ship(context.Background(), id, ShipOpts{HeadRepo: "file://" + fork})
	if got := prErrStatus(t, err); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (head-repo not a github remote)", got)
	}
	var pe *PRError
	errors.As(err, &pe)
	if !strings.HasPrefix(pe.Msg, "head-repo:") {
		t.Errorf("error = %q, want it to start with %q (proves HeadRepo threaded to prCore)", pe.Msg, "head-repo:")
	}
	// The push half delivered the branch to the fork before the PR step failed.
	gitS(t, fork, "rev-parse", "--verify", "magister/"+string(id))
}
