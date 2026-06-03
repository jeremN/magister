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
