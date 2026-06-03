package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/executor"
	"concentus/internal/flow"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

// newEngine wires a fully-mock engine for tests.
func newEngine(t *testing.T, exec map[string]core.Executor, sem *semaphore.Weighted) (*Engine, *store.Mem, *event.Bus) {
	t.Helper()
	st := store.NewMem()
	bus := event.NewBus()
	eng := &Engine{
		Execs: exec,
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st,
		Bus:   bus,
		Sem:   sem,
		Clock: core.SystemClock{},
	}
	return eng, st, bus
}

func mocks() map[string]core.Executor {
	return map[string]core.Executor{
		"opus":   executor.Mock{Name: "opus"},
		"sonnet": executor.Mock{Name: "sonnet"},
		"gemini": executor.Mock{Name: "gemini"},
	}
}

func mustCreate(t *testing.T, st *store.Mem, id core.RunID, f *flow.Flow) {
	t.Helper()
	if err := st.CreateRun(context.Background(), core.RunState{ID: id, Name: f.Name, Status: core.RunPending, Concurrency: f.Concurrency}); err != nil {
		t.Fatal(err)
	}
}

func TestEngineFanOutFanIn(t *testing.T) {
	f := &flow.Flow{Name: "feat", Concurrency: 2, Steps: []*flow.Step{
		{ID: "plan", Agent: "opus", Gate: flow.Gate{Policy: flow.GateManual}},
		{ID: "api", Needs: []string{"plan"}, Agent: "sonnet", Workspace: flow.WSIsolated,
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
		{ID: "ui", Needs: []string{"plan"}, Agent: "gemini", Workspace: flow.WSIsolated,
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
		{ID: "integrate", Needs: []string{"api", "ui"},
			Join: &flow.Join{Strategy: flow.JoinMerge}, Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	if err := flow.Validate(f); err != nil {
		t.Fatalf("flow invalid: %v", err)
	}

	eng, st, bus := newEngine(t, mocks(), semaphore.NewWeighted(4))
	mustCreate(t, st, "r1", f)
	ch, unsub := bus.Subscribe(64)

	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, _ := st.GetRun(context.Background(), "r1")
	if got.Status != core.RunSucceeded {
		t.Fatalf("run status = %q, want succeeded", got.Status)
	}
	if len(got.Steps) != 4 {
		t.Fatalf("want 4 steps recorded, got %d", len(got.Steps))
	}
	for _, s := range got.Steps {
		if s.Status != core.StepSucceeded {
			t.Errorf("step %q status = %q", s.StepID, s.Status)
		}
	}

	// All events were published synchronously during Run; closing the
	// subscription lets us drain the buffer and confirm the run bookends.
	unsub()
	var sawStart, sawDone bool
	for ev := range ch {
		switch ev.Kind {
		case event.RunStarted:
			sawStart = true
		case event.RunDone:
			sawDone = true
		}
	}
	if !sawStart || !sawDone {
		t.Errorf("missing run bookends: start=%v done=%v", sawStart, sawDone)
	}
}

func TestEngineAbortsOnStepError(t *testing.T) {
	// "ghost" agent is not registered → that step errors → run fails.
	f := &flow.Flow{Name: "boom", Steps: []*flow.Step{
		{ID: "plan", Agent: "opus", Gate: flow.Gate{Policy: flow.GateManual}},
		{ID: "bad", Needs: []string{"plan"}, Agent: "ghost", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	eng, st, _ := newEngine(t, mocks(), nil)
	mustCreate(t, st, "r1", f)
	if err := eng.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected run error")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Status != core.RunFailed {
		t.Fatalf("run status = %q, want failed", got.Status)
	}
}

func TestEngineCancellation(t *testing.T) {
	// A slow plan step; cancel the context almost immediately.
	f := &flow.Flow{Name: "slow", Steps: []*flow.Step{
		{ID: "plan", Agent: "slow", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	exec := map[string]core.Executor{"slow": executor.Mock{Name: "slow", Delay: 2 * time.Second}}
	eng, st, _ := newEngine(t, exec, nil)
	mustCreate(t, st, "r1", f)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := eng.Run(ctx, "r1", f); err == nil {
		t.Fatal("expected cancellation error")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Status != core.RunCanceled {
		t.Fatalf("run status = %q, want canceled", got.Status)
	}
}

func TestEngineWideFanInNoDeadlock(t *testing.T) {
	// 20 parallel steps feeding one merge, under a global semaphore of 2 and a
	// per-run cap of 2. If tokens were held while waiting on deps, the join would
	// deadlock. It must finish well within the timeout.
	steps := []*flow.Step{{ID: "root", Agent: "opus", Gate: flow.Gate{Policy: flow.GateManual}}}
	var needs []string
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("w%d", i)
		needs = append(needs, id)
		steps = append(steps, &flow.Step{ID: id, Needs: []string{"root"}, Agent: "sonnet",
			Gate: flow.Gate{Policy: flow.GateManual}})
	}
	steps = append(steps, &flow.Step{ID: "join", Needs: needs,
		Join: &flow.Join{Strategy: flow.JoinMerge}, Gate: flow.Gate{Policy: flow.GateManual}})
	f := &flow.Flow{Name: "wide", Concurrency: 2, Steps: steps}
	if err := flow.Validate(f); err != nil {
		t.Fatalf("invalid: %v", err)
	}

	eng, st, _ := newEngine(t, mocks(), semaphore.NewWeighted(2))
	mustCreate(t, st, "r1", f)

	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background(), "r1", f) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DEADLOCK: wide fan-in did not complete")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Status != core.RunSucceeded {
		t.Fatalf("status = %q", got.Status)
	}
}

func TestEngineRetryThenSucceed(t *testing.T) {
	// Fails twice, succeeds on the third attempt. A fake clock makes backoff
	// instant and deterministic.
	flaky := &flakyExecutor{failUntil: 3}
	exec := map[string]core.Executor{"flaky": flaky}
	st := store.NewMem()
	eng := &Engine{
		Execs: exec,
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st,
		Bus:   event.NewBus(),
		Sem:   nil,
		Clock: fakeClock{},
	}
	f := &flow.Flow{Name: "retry", Steps: []*flow.Step{
		{ID: "a", Agent: "flaky", Retry: &flow.RetryPolicy{Max: 3, Backoff: flow.Duration(time.Second)},
			Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	mustCreate(t, st, "r1", f)
	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	if flaky.calls != 3 {
		t.Fatalf("executor called %d times, want 3", flaky.calls)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepSucceeded {
		t.Fatalf("step status = %q", got.Steps[0].Status)
	}
}

type flakyExecutor struct {
	mu        sync.Mutex
	calls     int
	failUntil int
}

func (f *flakyExecutor) Run(_ context.Context, t core.Task) (core.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls < f.failUntil {
		return core.Result{}, fmt.Errorf("transient failure %d", f.calls)
	}
	return core.Result{StepID: t.StepID, Summary: "ok after " + strings.Repeat("x", f.calls)}, nil
}

// fakeClock makes After fire immediately, so retry/backoff tests don't sleep.
type fakeClock struct{}

func (fakeClock) Now() time.Time { return time.Unix(0, 0) }
func (fakeClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Unix(0, 0)
	return ch
}
