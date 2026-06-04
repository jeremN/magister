package supervisor

import (
	"context"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/engine"
	"concentus/internal/event"
	"concentus/internal/executor"
	"concentus/internal/flow"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

func testEngine(t *testing.T, st core.Store, reg *ApprovalRegistry) *engine.Engine {
	t.Helper()
	return &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: &RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: core.SystemClock{},
	}
}

func TestSupervisorSubmitRunsToCompletion(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg), st, reg)

	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	id, err := sup.Submit(context.Background(), f, "name: f\n")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected a run ID")
	}

	// the manual gate blocks; approve it, then the run completes
	waitFor(t, func() bool { return sup.Approve(id, "a", true, "") })
	waitForStatus(t, st, id, core.RunSucceeded)
	sup.Shutdown(time.Second)
}

func TestSupervisorCancelStopsRun(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg), st, reg)
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	id, _ := sup.Submit(context.Background(), f, "x")
	// the gate is blocking; cancel the run
	waitFor(t, func() bool { return sup.Cancel(id) })
	waitForStatus(t, st, id, core.RunCanceled)
	sup.Shutdown(time.Second)
}

func waitForStatus(t *testing.T, st core.Store, id core.RunID, want core.RunStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := st.GetRun(context.Background(), id); err == nil && r.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never reached status %s", id, want)
}

func TestResetIncompleteStepsToPending(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg), st, reg)

	if err := st.CreateRun(ctx, core.RunState{ID: "r1", Name: "f", Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}
	for _, st0 := range []core.StepState{
		{RunID: "r1", StepID: "a", Status: core.StepSucceeded, Attempt: 1, Summary: "a ok"},
		{RunID: "r1", StepID: "b", Status: core.StepAwaitingGate, Attempt: 1},
		{RunID: "r1", StepID: "c", Status: core.StepRunning, Attempt: 2, Err: "boom"},
	} {
		if err := st.SaveStepTransition(ctx, st0, nil); err != nil {
			t.Fatal(err)
		}
	}
	rs, _ := st.GetRun(ctx, "r1")
	sup.resetIncompleteSteps(ctx, rs)

	got, _ := st.GetRun(ctx, "r1")
	want := map[string]core.StepStatus{"a": core.StepSucceeded, "b": core.StepPending, "c": core.StepPending}
	for _, s := range got.Steps {
		if s.Status != want[s.StepID] {
			t.Errorf("step %s status = %q, want %q", s.StepID, s.Status, want[s.StepID])
		}
		if s.StepID == "c" && (s.Err != "" || s.Attempt != 0) {
			t.Errorf("reset step c should clear err/attempt, got err=%q attempt=%d", s.Err, s.Attempt)
		}
	}
}

func TestResumeAllContinuesPastCorruptFlow(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg), st, reg)

	// r1: unparseable flow YAML. r2: a valid flow with a manual gate (will block).
	if err := st.CreateRun(ctx, core.RunState{ID: "r1", Name: "bad", FlowYAML: "::: not yaml :::", Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}
	const good = "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: manual }\n"
	if err := st.CreateRun(ctx, core.RunState{ID: "r2", Name: "f", FlowYAML: good, Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}

	if err := sup.ResumeAll(ctx); err != nil {
		t.Fatalf("ResumeAll should not fail on a corrupt row, got: %v", err)
	}
	// r2 must have been resumed despite r1 being corrupt: its manual gate blocks,
	// so it is running and approvable.
	waitFor(t, func() bool { return sup.Approve("r2", "a", true, "") })
	waitForStatus(t, st, "r2", core.RunSucceeded)
	sup.Shutdown(time.Second)
}
