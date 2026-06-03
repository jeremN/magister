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

// failingStore wraps a store whose SaveStepTransition always errors, to assert
// that transition() does NOT publish the original event on a store failure.
type failingStore struct{ core.Store }

func (failingStore) SaveStepTransition(context.Context, core.StepState, []event.Event) error {
	return fmt.Errorf("disk full")
}

func TestTransitionDoesNotPublishOriginalOnStoreError(t *testing.T) {
	bus := event.NewBus()
	ch, unsub := bus.Subscribe(8)
	defer unsub()

	e := &Engine{Store: failingStore{store.NewMem()}, Bus: bus, Clock: core.SystemClock{}}
	e.transition(context.Background(), "r1",
		core.StepState{RunID: "r1", StepID: "a", Status: core.StepSucceeded},
		event.Event{StepID: "a", Kind: event.StepDone})

	// Exactly one frame: the store-error frame. The original step.done must NOT appear.
	var got []event.Event
	for {
		select {
		case ev := <-ch:
			got = append(got, ev)
			continue
		case <-time.After(50 * time.Millisecond):
		}
		break
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event (store error only), got %d: %+v", len(got), got)
	}
	if got[0].Kind != event.StepFailed || !strings.Contains(got[0].Err, "store:") {
		t.Errorf("expected store-error frame, got %+v", got[0])
	}
}

// recordingClock records each duration passed to After (which fires immediately,
// so tests never sleep) — used to assert the exact backoff schedule.
type recordingClock struct{ durs []time.Duration }

func (c *recordingClock) Now() time.Time { return time.Unix(0, 0) }
func (c *recordingClock) After(d time.Duration) <-chan time.Time {
	c.durs = append(c.durs, d)
	ch := make(chan time.Time, 1)
	ch <- time.Unix(0, 0)
	return ch
}

func TestBackoffJitterAndCap(t *testing.T) {
	clk := &recordingClock{}
	// Rand fixed at 0.5 → jittered sleep is exactly half the (capped) ceiling.
	e := &Engine{Clock: clk, Rand: func() float64 { return 0.5 }}
	s := &flow.Step{Retry: &flow.RetryPolicy{Max: 200, Backoff: flow.Duration(time.Second)}}

	for _, attempt := range []int{2, 3, 4, 7, 200} {
		if !e.backoff(context.Background(), s, attempt) {
			t.Fatalf("backoff(attempt=%d) returned false", attempt)
		}
	}
	// attempt 2 → 1s, 3 → 2s, 4 → 4s (all ×0.5); attempt 7 → 32s clamps to 30s cap
	// (natural cap, no overflow) ×0.5 = 15s; attempt 200 overflows the shift and
	// clamps to maxBackoff=30s (×0.5 = 15s).
	want := []time.Duration{
		500 * time.Millisecond,
		1 * time.Second,
		2 * time.Second,
		15 * time.Second, // attempt 7: 32s clamps to 30s cap (natural cap, no overflow)
		15 * time.Second, // attempt 200: overflow clamps to 30s cap
	}
	if len(clk.durs) != len(want) {
		t.Fatalf("recorded %d durations, want %d: %v", len(clk.durs), len(want), clk.durs)
	}
	for i, w := range want {
		if clk.durs[i] != w {
			t.Errorf("durs[%d] = %v, want %v", i, clk.durs[i], w)
		}
	}
}

// blockingApprover blocks Approve until the test sends a decision, so we can
// assert the step is observably awaiting_gate before it resolves.
type blockingApprover struct {
	gate  chan bool // test sends the approve/reject decision
	await chan struct{}
}

func (b *blockingApprover) Approve(ctx context.Context, _ core.RunID, _ *flow.Step, _ core.Result) (bool, error) {
	close(b.await) // signal that we've entered the gate
	select {
	case ok := <-b.gate:
		return ok, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestEngineEmitsAwaitingGateAndBlocks(t *testing.T) {
	st := store.NewMem()
	bus := event.NewBus()
	ch, unsub := bus.Subscribe(32)
	defer unsub()
	ba := &blockingApprover{gate: make(chan bool, 1), await: make(chan struct{})}

	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: ba, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: core.SystemClock{},
	}
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r1", Name: "f", FlowYAML: "x", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- e.Run(context.Background(), "r1", f) }()

	<-ba.await // the step has entered the gate
	// the step must be persisted as awaiting_gate while blocked
	got, err := st.GetRun(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Steps) != 1 || got.Steps[0].Status != core.StepAwaitingGate {
		t.Fatalf("step should be awaiting_gate while blocked, got %+v", got.Steps)
	}

	ba.gate <- true // approve
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	final, _ := st.GetRun(context.Background(), "r1")
	if final.Steps[0].Status != core.StepSucceeded {
		t.Errorf("approved step should be succeeded, got %s", final.Steps[0].Status)
	}

	// a gate.awaiting frame must have been published
	unsub()
	var sawAwaiting bool
	for ev := range ch {
		if ev.Kind == event.GateAwaiting {
			sawAwaiting = true
		}
	}
	if !sawAwaiting {
		t.Error("expected a gate.awaiting event")
	}
}
