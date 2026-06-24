package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// failGetRunStore wraps a Store and forces GetRun to return a non-ErrRunNotFound
// error, simulating a transient store failure (e.g. DB unreachable).
type failGetRunStore struct {
	core.Store
	err error
}

func (f failGetRunStore) GetRun(context.Context, core.RunID) (core.RunState, error) {
	return core.RunState{}, f.err
}

// TestSupervisorCancelStoreErrorReturns500 verifies that a store-load failure on
// Cancel (for an id not in the active map) surfaces as a plain (non-*CancelError)
// error, which the handler maps to 500 — NOT a 404 "unknown run".
func TestSupervisorCancelStoreErrorReturns500(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	boom := fmt.Errorf("db unreachable")
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), failGetRunStore{Store: st, err: boom}, reg)

	err := sup.Cancel(context.Background(), "absent")
	if err == nil {
		t.Fatal("expected an error on store-load failure")
	}
	var ce *CancelError
	if errors.As(err, &ce) {
		t.Fatalf("store error must NOT be a *CancelError (would mislead 404/409), got %#v", ce)
	}
	if !errors.Is(err, boom) {
		t.Errorf("store error not wrapped: got %v, want it to wrap %v", err, boom)
	}
}

// TestSupervisorCancelUnknownReturns404 / Terminal409 cover the supervisor-level
// branches directly (the API handler tests cover the HTTP mapping).
func TestSupervisorCancelUnknownReturns404(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)

	err := sup.Cancel(context.Background(), "nope")
	var ce *CancelError
	if !errors.As(err, &ce) || ce.Status != 404 {
		t.Fatalf("unknown run Cancel = %v, want *CancelError{404}", err)
	}
}

func TestSupervisorCancelTerminalReturns409(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	if err := st.CreateRun(ctx, core.RunState{ID: "done", Status: core.RunSucceeded}); err != nil {
		t.Fatal(err)
	}

	err := sup.Cancel(ctx, "done")
	var ce *CancelError
	if !errors.As(err, &ce) || ce.Status != 409 {
		t.Fatalf("terminal run Cancel = %v, want *CancelError{409}", err)
	}
}

func testEngine(t *testing.T, st core.Store, reg *ApprovalRegistry, ws core.Workspace) *engine.Engine {
	t.Helper()
	return &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    ws,
		Gate:  &gate.Evaluator{Approver: &RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: core.SystemClock{},
	}
}

func TestSupervisorSubmitRunsToCompletion(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)

	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	id, err := sup.Submit(context.Background(), f, "name: f\n", "", "")
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
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	id, _ := sup.Submit(context.Background(), f, "x", "", "")
	// the gate is blocking; cancel the run
	waitFor(t, func() bool { return sup.Cancel(context.Background(), id) == nil })
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
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)

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
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)

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

// provisionSpy records Provision calls while delegating the rest to a real Manager.
type provisionSpy struct {
	*workspace.Manager
	mu  sync.Mutex
	got []string // "repo|base" per call
}

func (p *provisionSpy) Provision(_ context.Context, id core.RunID, repo, base string) error {
	p.mu.Lock()
	p.got = append(p.got, repo+"|"+base)
	p.mu.Unlock()
	return nil
}

// autoStepYAML is a one-step flow with an auto gate, so the run completes without approval.
const autoStepYAML = "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"true\" } }\n"

func TestSubmitProvisionsAndPersistsRepoBase(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	spy := &provisionSpy{Manager: &workspace.Manager{Root: t.TempDir()}}
	sup := New(testEngine(t, st, reg, spy), st, reg)
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
		t.Errorf("persisted repo/base = %q/%q, want %q/%q", rs.Repo, rs.Base, "/abs/proj", "deadbeef")
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
	sup := New(testEngine(t, st, reg, spy), st, reg)
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
