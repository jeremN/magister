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
	"concentus/internal/event"
	"concentus/internal/flow"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

func retryErrStatus(t *testing.T, err error) int {
	t.Helper()
	var re *RetryError
	if !errors.As(err, &re) {
		t.Fatalf("error %v is not a *RetryError", err)
	}
	return re.Status
}

func mustFlow(t *testing.T, yaml string) *flow.Flow {
	t.Helper()
	f, err := flow.ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("parse flow: %v", err)
	}
	return f
}

func stepStatus(rs core.RunState, id string) core.StepStatus {
	for _, s := range rs.Steps {
		if s.StepID == id {
			return s.Status
		}
	}
	return ""
}

// countStarts returns how many step.started events each of steps a and b recorded.
func countStarts(t *testing.T, st core.Store, id core.RunID) (a, b int) {
	t.Helper()
	evs, err := st.EventsSince(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind != event.StepStarted {
			continue
		}
		switch e.StepID {
		case "a":
			a++
		case "b":
			b++
		}
	}
	return a, b
}

func TestRetryUnknownRun404(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	_, err := sup.Retry(context.Background(), "nope")
	if got := retryErrStatus(t, err); got != http.StatusNotFound {
		t.Errorf("status = %d, want 404", got)
	}
}

func TestRetryRejectsSucceeded(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	if err := st.CreateRun(ctx, core.RunState{ID: "r1", Name: "f", FlowYAML: autoStepYAML, Status: core.RunSucceeded}); err != nil {
		t.Fatal(err)
	}
	_, err := sup.Retry(ctx, "r1")
	if got := retryErrStatus(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409", got)
	}
}

func TestRetryRejectsActiveRun(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	// A manual gate blocks, so the run stays active (registered in s.runs).
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	id, err := sup.Submit(context.Background(), f, "name: f\n", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = sup.Retry(context.Background(), id)
	if got := retryErrStatus(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409 (active)", got)
	}
}

func TestRetryScratchReclaimedReverts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	gm := &workspace.GitManager{Root: root}
	sup := New(testEngine(t, st, reg, gm), st, reg)
	// Seed a terminal run whose scratch was never created (as if GC-reclaimed):
	// BasePath(root/r1/base) does not exist, so dirHasGit is false.
	if err := st.CreateRun(ctx, core.RunState{ID: "r1", Name: "f", FlowYAML: autoStepYAML, Status: core.RunFailed, Err: "boom"}); err != nil {
		t.Fatal(err)
	}
	_, err := sup.Retry(ctx, "r1")
	if got := retryErrStatus(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409 (reclaimed)", got)
	}
	rs, _ := st.GetRun(ctx, "r1")
	if rs.Status != core.RunFailed || rs.Err != "boom" {
		t.Errorf("status/err = %s/%q, want failed/boom (fully reverted)", rs.Status, rs.Err)
	}
}

func TestRetryResumesSkippingSucceeded(t *testing.T) {
	requireGitS(t)
	ctx := context.Background()
	root := t.TempDir()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	gm := &workspace.GitManager{Root: root}
	sup := New(testEngine(t, st, reg, gm), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })

	// Step a always passes; step b's gate passes only once `flag` exists.
	flag := filepath.Join(t.TempDir(), "ok")
	yaml := "name: f\nsteps:\n" +
		"  - id: a\n    agent: mock\n    workspace: isolated\n    gate: { policy: auto, verifier: { command: \"true\" } }\n" +
		"  - id: b\n    agent: mock\n    workspace: isolated\n    needs: [a]\n    gate: { policy: auto, verifier: { command: \"test -f " + flag + "\" } }\n"

	id, err := sup.Submit(ctx, mustFlow(t, yaml), yaml, "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, st, id, core.RunFailed) // b's gate fails (flag absent) → run fails

	rs, _ := st.GetRun(ctx, id)
	if stepStatus(rs, "a") != core.StepSucceeded {
		t.Fatalf("step a = %s, want succeeded", stepStatus(rs, "a"))
	}
	if stepStatus(rs, "b") != core.StepFailed {
		t.Fatalf("step b = %s, want failed", stepStatus(rs, "b"))
	}

	// Fix the condition and retry in place.
	if err := os.WriteFile(flag, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sup.Retry(ctx, id)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if got != id {
		t.Errorf("Retry returned id %q, want the same id %q", got, id)
	}
	waitForStatus(t, st, id, core.RunSucceeded)

	// Proof of skip: a started once (seeded on retry); b started twice (orig + retry).
	a, b := countStarts(t, st, id)
	if a != 1 {
		t.Errorf("step a started %d times, want 1 (skipped on retry)", a)
	}
	if b != 2 {
		t.Errorf("step b started %d times, want 2 (original + retry)", b)
	}
}

func TestRetryResumesCanceledRun(t *testing.T) {
	requireGitS(t)
	ctx := context.Background()
	root := t.TempDir()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	gm := &workspace.GitManager{Root: root}
	sup := New(testEngine(t, st, reg, gm), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })

	yaml := "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: manual }\n"
	id, err := sup.Submit(ctx, mustFlow(t, yaml), yaml, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Wait until step a is awaiting gate approval (For() has run → base .git exists),
	// then cancel so Retry can find the scratch.
	waitForStatus(t, st, id, core.RunRunning)
	waitFor(t, func() bool {
		rs, _ := st.GetRun(ctx, id)
		return stepStatus(rs, "a") == core.StepAwaitingGate
	})
	waitFor(t, func() bool { return sup.Cancel(id) })
	waitForStatus(t, st, id, core.RunCanceled)

	got, err := sup.Retry(ctx, id)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if got != id {
		t.Errorf("Retry returned id %q, want %q", got, id)
	}
	// The manual gate blocks again on resume; approve to finish.
	waitFor(t, func() bool { return sup.Approve(id, "a", true, "") })
	waitForStatus(t, st, id, core.RunSucceeded)
}
