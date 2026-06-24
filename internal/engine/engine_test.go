package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
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
		"mock":   executor.Mock{Name: "mock"},
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
		{ID: "plan", Agent: "opus", Prompt: "p", Gate: flow.Gate{Policy: flow.GateManual}},
		{ID: "api", Needs: []string{"plan"}, Agent: "sonnet", Prompt: "p", Workspace: flow.WSIsolated,
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
		{ID: "ui", Needs: []string{"plan"}, Agent: "gemini", Prompt: "p", Workspace: flow.WSIsolated,
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
		{ID: "integrate", Needs: []string{"api", "ui"}, Workspace: flow.WSIsolated,
			Join: &flow.Join{Strategy: flow.JoinMerge}, Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	if err := flow.Validate(f); err != nil {
		t.Fatalf("flow invalid: %v", err)
	}

	eng, st := newGitEngine(t, mocks())
	eng.Sem = semaphore.NewWeighted(4)
	bus := eng.Bus.(*event.Bus)
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
	// deadlock. The timeout only distinguishes "completes" from "hangs forever":
	// 20 real git worktrees + a 20-branch merge take a few seconds, and much longer
	// under CI load, so the bound is generous on purpose (a true deadlock never
	// returns regardless).
	steps := []*flow.Step{{ID: "root", Agent: "opus", Prompt: "p", Gate: flow.Gate{Policy: flow.GateManual}}}
	var needs []string
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("w%d", i)
		needs = append(needs, id)
		steps = append(steps, &flow.Step{ID: id, Needs: []string{"root"}, Agent: "sonnet", Prompt: "p", Workspace: flow.WSIsolated,
			Gate: flow.Gate{Policy: flow.GateManual}})
	}
	steps = append(steps, &flow.Step{ID: "join", Needs: needs, Workspace: flow.WSIsolated,
		Join: &flow.Join{Strategy: flow.JoinMerge}, Gate: flow.Gate{Policy: flow.GateManual}})
	f := &flow.Flow{Name: "wide", Concurrency: 2, Steps: steps}
	if err := flow.Validate(f); err != nil {
		t.Fatalf("invalid: %v", err)
	}

	eng, st := newGitEngine(t, mocks())
	eng.Sem = semaphore.NewWeighted(2)
	mustCreate(t, st, "r1", f)

	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background(), "r1", f) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(30 * time.Second):
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

// TestStepSuccessTransitionPersistFailureFails asserts that when the store
// rejects the StepSucceeded persist, eng.Run returns a non-nil error (the run
// fails) rather than silently reporting success.
func TestStepSuccessTransitionPersistFailureFails(t *testing.T) {
	f := &flow.Flow{Name: "persist-fail", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Prompt: "p", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	if err := flow.Validate(f); err != nil {
		t.Fatalf("flow invalid: %v", err)
	}

	bus := event.NewBus()
	eng := &Engine{
		Execs: mocks(),
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: failingStore{store.NewMem()},
		Bus:   bus,
		Sem:   nil,
		Clock: core.SystemClock{},
	}
	// failingStore.CreateRun is not overridden, so seed via the inner store.
	inner := store.NewMem()
	if err := inner.CreateRun(context.Background(), core.RunState{ID: "r1", Name: f.Name, Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	eng.Store = failingStore{inner}

	err := eng.Run(context.Background(), "r1", f)
	if err == nil {
		t.Fatal("eng.Run returned nil; want non-nil error because the StepSucceeded persist failed")
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
		if !e.backoff(context.Background(), "test-run", s, attempt) {
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

// rejectApprover always rejects, to drive the escalate/reject and abort paths.
type rejectApprover struct{}

func (rejectApprover) Approve(context.Context, core.RunID, *flow.Step, core.Result) (bool, error) {
	return false, nil
}

func TestEscalateApproveSucceeds(t *testing.T) {
	st := store.NewMem()
	bus := event.NewBus()
	ch, unsub := bus.Subscribe(32)
	defer unsub()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: fakeClock{},
	}
	f := &flow.Flow{Name: "esc", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{
			Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "false"}, OnFail: flow.FailEscalate}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepSucceeded {
		t.Fatalf("step status = %q, want succeeded", got.Steps[0].Status)
	}
	if got.Steps[0].CostUSD != 0.01 {
		t.Errorf("approved escalation should keep the original result, got CostUSD=%v want 0.01", got.Steps[0].CostUSD)
	}
	unsub()
	var sawEscalation bool
	for ev := range ch {
		if ev.Kind == event.GateAwaiting && ev.Err != "" {
			sawEscalation = true
		}
	}
	if !sawEscalation {
		t.Error("expected a gate.awaiting event with a failure reason")
	}
}

func TestEscalateRejectAborts(t *testing.T) {
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: rejectApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	f := &flow.Flow{Name: "esc", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{
			Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "false"}, OnFail: flow.FailEscalate}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected run to fail on escalation reject")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepFailed {
		t.Fatalf("step status = %q, want failed", got.Steps[0].Status)
	}
}

func TestManualGateRejectDoesNotEscalate(t *testing.T) {
	st := store.NewMem()
	bus := event.NewBus()
	ch, unsub := bus.Subscribe(32)
	defer unsub()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: rejectApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: fakeClock{},
	}
	f := &flow.Flow{Name: "mg", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual, OnFail: flow.FailEscalate}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected run to fail (manual reject aborts)")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepFailed {
		t.Fatalf("step status = %q, want failed", got.Steps[0].Status)
	}
	// Escalation is AUTO-only: a rejected manual gate must NOT escalate. The escalation
	// path is the ONLY one that emits a gate.awaiting carrying a failure reason in Err,
	// so its absence proves the manual reject aborted rather than escalating.
	unsub()
	for ev := range ch {
		if ev.Kind == event.GateAwaiting && ev.Err != "" {
			t.Errorf("manual reject must not escalate, but saw gate.awaiting with Err=%q", ev.Err)
		}
	}
}

func TestTimeoutBoundsVerifier(t *testing.T) {
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: core.SystemClock{},
	}
	// Auto gate whose verifier hangs; a 100ms step timeout must kill it → the step
	// fails (no retry policy) instead of waiting for `sleep` to finish.
	f := &flow.Flow{Name: "to", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Timeout: flow.Duration(100 * time.Millisecond),
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "sleep 2"}}},
	}}
	mustCreate(t, st, "r1", f)

	start := time.Now()
	if err := e.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected run to fail on verifier timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("run took %v — verifier was not bounded by the 100ms timeout", elapsed)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepFailed {
		t.Fatalf("step status = %q, want failed", got.Steps[0].Status)
	}
}

func TestTimeoutDoesNotBoundManualGate(t *testing.T) {
	// Regression guard for the refactor: a manual gate must NOT be killed by the
	// step timeout (humans take arbitrary time). Approve well after the timeout
	// would have fired; the step must still succeed.
	st := store.NewMem()
	ba := &blockingApprover{gate: make(chan bool, 1), await: make(chan struct{})}
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: ba, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: core.SystemClock{},
	}
	f := &flow.Flow{Name: "mg", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Timeout: flow.Duration(50 * time.Millisecond),
			Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	mustCreate(t, st, "r1", f)

	done := make(chan error, 1)
	go func() { done <- e.Run(context.Background(), "r1", f) }()
	<-ba.await
	time.Sleep(120 * time.Millisecond) // past the 50ms step timeout
	ba.gate <- true                    // approve
	if err := <-done; err != nil {
		t.Fatalf("manual gate should survive the step timeout, got: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepSucceeded {
		t.Fatalf("step status = %q, want succeeded", got.Steps[0].Status)
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

func TestRetryThenEscalate(t *testing.T) {
	st := store.NewMem()
	bus := event.NewBus()
	ch, unsub := bus.Subscribe(64)
	defer unsub()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: rejectApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: fakeClock{},
	}
	// retry:{max:2} + failing auto verifier + on_fail:escalate: the unified budget
	// retries (re-executing) once, THEN escalates the spent gate to a human — who
	// rejects, failing the run. Proves retry-then-escalate ordering, not escalate-first.
	f := &flow.Flow{Name: "re", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Retry: &flow.RetryPolicy{Max: 2, Backoff: flow.Duration(time.Second)},
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "false"}, OnFail: flow.FailEscalate}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected run to fail (escalation rejected)")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepFailed {
		t.Fatalf("step status = %q, want failed", got.Steps[0].Status)
	}
	unsub()
	var sawRetry, sawEscalation bool
	for ev := range ch {
		switch {
		case ev.Kind == event.StepRetrying:
			sawRetry = true
		case ev.Kind == event.GateAwaiting && ev.Err != "":
			sawEscalation = true
		}
	}
	if !sawRetry {
		t.Error("expected a step.retrying event (budget should be spent before escalating)")
	}
	if !sawEscalation {
		t.Error("expected a gate.awaiting event with a reason (escalation after retries)")
	}
}

// teardownSpy records TeardownRun calls while delegating For to a real Manager.
type teardownSpy struct {
	*workspace.Manager
	mu   sync.Mutex
	runs []core.RunID
}

func (s *teardownSpy) TeardownRun(ctx context.Context, id core.RunID) error {
	s.mu.Lock()
	s.runs = append(s.runs, id)
	s.mu.Unlock()
	return s.Manager.TeardownRun(ctx, id)
}

func TestRunDAGTearsDownWorkspaceAtEnd(t *testing.T) {
	st := store.NewMem()
	spy := &teardownSpy{Manager: &workspace.Manager{Root: t.TempDir()}}
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    spy,
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: core.SystemClock{},
	}
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.runs) != 1 || spy.runs[0] != "r1" {
		t.Fatalf("expected TeardownRun(r1) once at run end, got %v", spy.runs)
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

// emittingExec is a test executor that emits one milestone via Task.Emit, to prove
// the engine binds Emit and that the milestone is persisted + published.
type emittingExec struct{}

func (emittingExec) Run(ctx context.Context, tk core.Task) (core.Result, error) {
	if tk.Emit != nil {
		tk.Emit(event.Event{Kind: event.AgentTool, Summary: "Edit foo.go"})
	}
	return core.Result{StepID: tk.StepID, Summary: "ok", CostUSD: 0.01}, nil
}

func TestEngineForwardsAgentMilestones(t *testing.T) {
	f := &flow.Flow{Name: "feat", Concurrency: 1, Steps: []*flow.Step{
		{ID: "s1", Agent: "x", Prompt: "p", Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
	}}
	if err := flow.Validate(f); err != nil {
		t.Fatalf("flow invalid: %v", err)
	}
	eng, st, bus := newEngine(t, map[string]core.Executor{"x": emittingExec{}}, semaphore.NewWeighted(1))
	mustCreate(t, st, "r1", f)
	ch, unsub := bus.Subscribe(64)

	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Persisted: SSE reads the durable events table, so the milestone must be there.
	evs, err := st.EventsSince(context.Background(), "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var milestone *event.Event
	for i := range evs {
		if evs[i].Kind == event.AgentTool {
			milestone = &evs[i]
		}
	}
	if milestone == nil {
		t.Fatalf("no agent.tool event persisted; got %+v", evs)
	}
	if milestone.Summary != "Edit foo.go" || milestone.StepID != "s1" {
		t.Errorf("milestone = %+v, want Summary=\"Edit foo.go\" StepID=s1", *milestone)
	}

	// Published: the same milestone reached the live bus.
	unsub()
	var published bool
	for ev := range ch {
		if ev.Kind == event.AgentTool && ev.Summary == "Edit foo.go" {
			published = true
		}
	}
	if !published {
		t.Error("agent.tool milestone was not published to the bus")
	}
}

func TestConditionalGateTrueProceedsWithoutAwaiting(t *testing.T) {
	st := store.NewMem()
	bus := event.NewBus()
	ch, unsub := bus.Subscribe(32)
	defer unsub()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: fakeClock{},
	}
	cond := &flow.Condition{Expr: "result.cost_usd < 1.0"} // mock cost 0.01 -> true
	if err := cond.Compile(); err != nil {
		t.Fatal(err)
	}
	f := &flow.Flow{Name: "cond", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateConditional, Condition: cond}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepSucceeded {
		t.Fatalf("step status = %q, want succeeded", got.Steps[0].Status)
	}
	unsub()
	for ev := range ch {
		if ev.Kind == event.GateAwaiting {
			t.Error("a conditional gate must not emit gate.awaiting (it resolves synchronously)")
		}
	}
}

func TestConditionalGateFalseAborts(t *testing.T) {
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	cond := &flow.Condition{Expr: "result.cost_usd > 1.0"} // mock cost 0.01 -> false
	if err := cond.Compile(); err != nil {
		t.Fatal(err)
	}
	f := &flow.Flow{Name: "cond", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateConditional, Condition: cond}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected the run to fail on a false conditional gate")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepFailed {
		t.Fatalf("step status = %q, want failed", got.Steps[0].Status)
	}
}

func TestConditionalGateEscalatesOnFalse(t *testing.T) {
	st := store.NewMem()
	bus := event.NewBus()
	ch, unsub := bus.Subscribe(32)
	defer unsub()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}}, // approves the escalation
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: fakeClock{},
	}
	cond := &flow.Condition{Expr: "result.cost_usd > 1.0"} // false -> escalate
	if err := cond.Compile(); err != nil {
		t.Fatal(err)
	}
	f := &flow.Flow{Name: "cond", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{
			Policy: flow.GateConditional, Condition: cond, OnFail: flow.FailEscalate}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("escalation approved, run should succeed: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepSucceeded {
		t.Fatalf("step status = %q, want succeeded (escalation approved)", got.Steps[0].Status)
	}
	unsub()
	var sawEscalation bool
	for ev := range ch {
		if ev.Kind == event.GateAwaiting && ev.Err != "" {
			sawEscalation = true
		}
	}
	if !sawEscalation {
		t.Error("expected a gate.awaiting event with a failure reason (conditional escalation)")
	}
}

// A conditional gate whose Eval errors at runtime (here: an uncompiled condition) is
// a gate ERROR, not a gate FALSE: gateFailed=false, so it must short-circuit the run
// WITHOUT escalating. With OnFail=escalate and an approving gate, a gate FALSE would
// escalate and succeed — so a failed run + StepFailed proves error ≠ false (spec §6).
func TestConditionalGateEvalErrorFailsWithoutEscalating(t *testing.T) {
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}}, // would approve an escalation
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	cond := &flow.Condition{Expr: "true"} // deliberately NOT compiled -> Eval returns an error
	f := &flow.Flow{Name: "cond", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{
			Policy: flow.GateConditional, Condition: cond, OnFail: flow.FailEscalate}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected the run to fail on a gate eval error (an error must not escalate)")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepFailed {
		t.Fatalf("step status = %q, want failed (eval error, not an escalated approval)", got.Steps[0].Status)
	}
}

// pickExec is a test arbiter that always selects `pick` (emits the SELECTED token).
type pickExec struct{ pick string }

func (p pickExec) Run(_ context.Context, t core.Task) (core.Result, error) {
	return core.Result{StepID: t.StepID, Summary: "choosing\nSELECTED: " + p.pick, CostUSD: 0.02}, nil
}

// fanInFlow builds a two-upstream-mock fan-in into a join step `j`. The steps are
// isolated so the flow is valid under the join validator (joins require isolated
// upstreams); on the plain Manager the join still degrades to path-only inputs.
func fanInFlow(j *flow.Join) *flow.Flow {
	return &flow.Flow{Name: "fanin", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Workspace: flow.WSIsolated},
		{ID: "b", Agent: "mock", Workspace: flow.WSIsolated},
		{ID: "j", Needs: []string{"a", "b"}, Workspace: flow.WSIsolated, Join: j},
	}}
}

func TestSelectJoinForwardsWinner(t *testing.T) {
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}, "arbiter": pickExec{pick: "a"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	f := fanInFlow(&flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter"})
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	j := got.Steps[2]
	if j.Status != core.StepSucceeded {
		t.Fatalf("join status = %q, want succeeded", j.Status)
	}
	if len(j.Artifacts) != 1 || filepath.Base(j.Artifacts[0].Path) != "a.out.md" {
		t.Fatalf("join artifacts = %v, want a's a.out.md (the selected winner)", j.Artifacts)
	}
	if j.CostUSD != 0.02 {
		t.Errorf("join cost = %v, want the arbiter's 0.02 (not the upstream mock's 0.01)", j.CostUSD)
	}
}

func TestSynthesizeJoinReturnsMergedOutput(t *testing.T) {
	execs := map[string]core.Executor{
		"a":       fileWriterExec{file: "shared.md", body: "A"},
		"b":       fileWriterExec{file: "shared.md", body: "B"},
		"arbiter": fileWriterExec{file: "shared.md", body: "RECONCILED", cost: 0.05}, // distinct from upstreams' 0.01
	}
	eng, st := newGitEngine(t, execs)
	f := &flow.Flow{Name: "fanin", Steps: []*flow.Step{
		{ID: "a", Agent: "a", Workspace: flow.WSIsolated, Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
		{ID: "b", Agent: "b", Workspace: flow.WSIsolated, Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
		{ID: "j", Needs: []string{"a", "b"}, Workspace: flow.WSIsolated,
			Join: &flow.Join{Strategy: flow.JoinSynthesize, Agent: "arbiter"}},
	}}
	mustCreate(t, st, "r1", f)
	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	var j core.StepState
	for _, s := range got.Steps {
		if s.StepID == "j" {
			j = s
		}
	}
	if j.Status != core.StepSucceeded {
		t.Fatalf("synthesize join status = %q, want succeeded", j.Status)
	}
	if len(j.Artifacts) == 0 || j.Artifacts[0].Branch != "step/j" {
		t.Fatalf("synthesize result not committed on its branch: %+v", j.Artifacts)
	}
	if j.CostUSD != 0.05 {
		t.Errorf("synthesize cost = %v, want the arbiter's 0.05 (not the upstreams' 0.01)", j.CostUSD)
	}
}

// flakyPick fails the select once (no token), then selects `pick` on re-run.
type flakyPick struct {
	pick  string
	calls *int
}

func (f flakyPick) Run(_ context.Context, t core.Task) (core.Result, error) {
	*f.calls++
	if *f.calls == 1 {
		return core.Result{StepID: t.StepID, Summary: "undecided"}, nil // no SELECTED -> select errors
	}
	return core.Result{StepID: t.StepID, Summary: "SELECTED: " + f.pick, CostUSD: 0.02}, nil
}

// noPick never emits a SELECTED token, so the select join always fails.
type noPick struct{}

func (noPick) Run(_ context.Context, t core.Task) (core.Result, error) {
	return core.Result{StepID: t.StepID, Summary: "undecided"}, nil
}

func TestJoinConflictAbortFailsRun(t *testing.T) {
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}, "arbiter": noPick{}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	f := fanInFlow(&flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter", OnConflict: flow.FailAbort})
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected the run to fail on a join conflict with on_conflict=abort")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[2].Status != core.StepFailed {
		t.Fatalf("join status = %q, want failed", got.Steps[2].Status)
	}
}

func TestJoinConflictEscalateApproveReRuns(t *testing.T) {
	st := store.NewMem()
	bus := event.NewBus()
	ch, unsub := bus.Subscribe(64)
	defer unsub()
	calls := 0
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}, "arbiter": flakyPick{pick: "a", calls: &calls}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}}, // approves the escalation
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: fakeClock{},
	}
	f := fanInFlow(&flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter", OnConflict: flow.FailEscalate})
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("escalation approved, run should succeed: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[2].Status != core.StepSucceeded {
		t.Fatalf("join status = %q, want succeeded (approve re-ran the join)", got.Steps[2].Status)
	}
	unsub()
	var sawAwaiting bool
	for ev := range ch {
		if ev.Kind == event.GateAwaiting && ev.Err != "" {
			sawAwaiting = true
		}
	}
	if !sawAwaiting {
		t.Error("expected a gate.awaiting event with the join failure reason")
	}
}

// joinStep returns the fan-in step (ID "j") from a fetched run.
func joinStep(t *testing.T, got core.RunState) core.StepState {
	t.Helper()
	for _, s := range got.Steps {
		if s.StepID == "j" {
			return s
		}
	}
	t.Fatalf("join step %q not found in %+v", "j", got.Steps)
	return core.StepState{}
}

func TestJoinConflictRetrySucceedsOnSecondAttempt(t *testing.T) {
	st := store.NewMem()
	calls := 0
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}, "arbiter": flakyPick{pick: "a", calls: &calls}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	f := fanInFlow(&flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter", OnConflict: flow.FailRetry})
	f.Steps[2].Retry = &flow.RetryPolicy{Max: 2, Backoff: flow.Duration(time.Second)}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("on_conflict=retry should re-run and succeed on attempt 2: %v", err)
	}
	if calls != 2 {
		t.Fatalf("arbiter called %d times, want 2 (fail then succeed)", calls)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if j := joinStep(t, got); j.Status != core.StepSucceeded {
		t.Fatalf("join status = %q, want succeeded after retry", j.Status)
	}
}

func TestJoinConflictRetryExhaustsBudgetThenFails(t *testing.T) {
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}, "arbiter": noPick{}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	f := fanInFlow(&flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter", OnConflict: flow.FailRetry})
	f.Steps[2].Retry = &flow.RetryPolicy{Max: 2, Backoff: flow.Duration(time.Second)}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected the run to fail after the retry budget is exhausted")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if j := joinStep(t, got); j.Status != core.StepFailed {
		t.Fatalf("join status = %q, want failed after budget exhausted", j.Status)
	}
}

// newGitEngine wires an engine whose workspace is a real GitManager, so isolated
// steps commit and joins can git-merge. Returns the engine and its store.
func newGitEngine(t *testing.T, execs map[string]core.Executor) (*Engine, *store.Mem) {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	st := store.NewMem()
	return &Engine{
		Execs: execs,
		WS:    &workspace.GitManager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st,
		Bus:   event.NewBus(),
		Clock: core.SystemClock{},
	}, st
}

func TestIsolatedStepCommitsAndStampsRefs(t *testing.T) {
	eng, st := newGitEngine(t, mocks())
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Workspace: flow.WSIsolated,
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
	}}
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r1", Name: "f", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	arts := got.Steps[0].Artifacts
	if len(arts) == 0 || arts[0].Branch != "step/a" || arts[0].Commit == "" {
		t.Fatalf("isolated step artifacts not stamped with refs: %+v", arts)
	}
}

// TestIsolatedStepArtifactAliasingRace is a -race regression test for the
// store.Mem artifact-aliasing bug: SaveStepTransition previously stored the
// caller's Artifacts slice directly, so commitIsolated's in-place branch/commit
// stamp raced with a concurrent GetRun's cloneRun. The fix was a write-side
// cloneArtifacts in SaveStepTransition; this test would catch a regression.
//
// Strategy: run an isolated step (which triggers commitIsolated → in-place
// branch/commit stamp) while concurrently polling GetRun from multiple
// goroutines. Under -race this surfaces any shared-slice write/read conflict.
func TestIsolatedStepArtifactAliasingRace(t *testing.T) {
	execs := map[string]core.Executor{
		"writer": fileWriterExec{file: "out.txt", body: "data"},
	}
	eng, st := newGitEngine(t, execs)
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "writer", Workspace: flow.WSIsolated,
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
	}}
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r1", Name: "f", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}

	// Spawn goroutines that poll GetRun concurrently while the engine runs.
	// They read .Steps[*].Artifacts, which commitIsolated stamps in-place; under
	// the old bug this was a data race. Goroutines stop once done is closed.
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					rs, _ := st.GetRun(context.Background(), "r1")
					for _, step := range rs.Steps {
						// Access artifact fields to exercise the read side
						// of the formerly-aliased slice.
						for _, a := range step.Artifacts {
							_ = a.Branch
							_ = a.Commit
						}
					}
				}
			}
		}()
	}

	if err := eng.Run(context.Background(), "r1", f); err != nil {
		close(done)
		wg.Wait()
		t.Fatalf("run: %v", err)
	}
	close(done)
	wg.Wait()

	// Confirm the step succeeded and artifacts were stamped.
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Status != core.RunSucceeded {
		t.Fatalf("run status = %q, want succeeded", got.Status)
	}
	if len(got.Steps) == 0 || len(got.Steps[0].Artifacts) == 0 {
		t.Fatalf("no artifacts on isolated step: %+v", got.Steps)
	}
	a := got.Steps[0].Artifacts[0]
	if a.Branch != "step/a" || a.Commit == "" {
		t.Errorf("artifact not stamped: branch=%q commit=%q", a.Branch, a.Commit)
	}
}

func TestJoinConflictEscalateRejectAborts(t *testing.T) {
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}, "arbiter": noPick{}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: rejectApprover{}, Verifier: gate.CommandVerifier{}}, // rejects the escalation
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	// rejectApprover rejects EVERY manual gate, so the upstream steps use a passing
	// auto gate; only the escalated join then routes through the rejecting Approver.
	f := &flow.Flow{Name: "fanin", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
		{ID: "b", Agent: "mock", Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
		{ID: "j", Needs: []string{"a", "b"},
			Join: &flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter", OnConflict: flow.FailEscalate}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected the run to fail when the escalated join is rejected")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	var j *core.StepState
	for i := range got.Steps {
		if got.Steps[i].StepID == "j" {
			j = &got.Steps[i]
		}
	}
	if j == nil {
		t.Fatalf("join step %q not recorded; steps=%+v", "j", got.Steps)
	}
	if j.Status != core.StepFailed {
		t.Fatalf("join status = %q, want failed (escalation rejected)", j.Status)
	}
}

// fileWriterExec writes a fixed filename with fixed content, so two such steps
// in separate worktrees collide on merge (used to drive the conflict ladder).
// cost defaults to 0.01 when zero, so callers that only set file/body are unchanged.
type fileWriterExec struct {
	file, body string
	cost       float64
}

func (e fileWriterExec) Run(_ context.Context, t core.Task) (core.Result, error) {
	out := filepath.Join(t.WorkDir, e.file)
	if err := os.WriteFile(out, []byte(e.body), 0o644); err != nil {
		return core.Result{}, err
	}
	cost := e.cost
	if cost == 0 {
		cost = 0.01
	}
	// Return the written file as an artifact so an isolated step's commitIsolated
	// stamps its branch onto it — the merge join needs branch-backed inputs.
	return core.Result{StepID: t.StepID, Summary: "wrote " + e.file,
		Artifacts: []core.Artifact{{StepID: t.StepID, Path: out}}, CostUSD: cost}, nil
}

// multiFileWriterExec writes several fixed files, so one base step can seed several
// shared files that later branches each conflict on independently (used to drive a
// multi-conflict cascade through the escalation ladder).
type multiFileWriterExec struct{ files map[string]string }

func (e multiFileWriterExec) Run(_ context.Context, t core.Task) (core.Result, error) {
	var arts []core.Artifact
	for name, body := range e.files {
		out := filepath.Join(t.WorkDir, name)
		if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
			return core.Result{}, err
		}
		arts = append(arts, core.Artifact{StepID: t.StepID, Path: out})
	}
	return core.Result{StepID: t.StepID, Summary: "wrote files", Artifacts: arts, CostUSD: 0.01}, nil
}

func conflictFlow(onConflict flow.FailPolicy) *flow.Flow {
	autoGate := flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}
	return &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "a", Workspace: flow.WSIsolated, Gate: autoGate},
		{ID: "b", Agent: "b", Workspace: flow.WSIsolated, Gate: autoGate},
		{ID: "integrate", Needs: []string{"a", "b"}, Workspace: flow.WSIsolated,
			Join: &flow.Join{Strategy: flow.JoinMerge, Agent: "arbiter", OnConflict: onConflict}},
	}}
}

func TestMergeConflictEscalateApproveCommits(t *testing.T) {
	execs := map[string]core.Executor{
		"a":       fileWriterExec{file: "shared.md", body: "A"},
		"b":       fileWriterExec{file: "shared.md", body: "B"},
		"arbiter": fileWriterExec{file: "shared.md", body: "RESOLVED"},
	}
	eng, st := newGitEngine(t, execs)
	var logBuf bytes.Buffer
	eng.Log = debugLogger(&logBuf)
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r1", Name: "f", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(context.Background(), "r1", conflictFlow(flow.FailEscalate)); err != nil {
		t.Fatalf("run should succeed after approve: %v", err)
	}
	if !hasLine(logBuf.String(), "level=WARN", "merge conflict detected", "branch=", "paths=") {
		t.Errorf("missing merge-conflict-detected Warn line:\n%s", logBuf.String())
	}
	got, _ := st.GetRun(context.Background(), "r1")
	var integrate core.StepState
	for _, s := range got.Steps {
		if s.StepID == "integrate" {
			integrate = s
		}
	}
	if integrate.Status != core.StepSucceeded {
		t.Fatalf("integrate status = %q, want succeeded", integrate.Status)
	}
	if len(integrate.Artifacts) == 0 || integrate.Artifacts[0].Branch != "step/integrate" {
		t.Fatalf("integrate not committed on its branch: %+v", integrate.Artifacts)
	}
	if integrate.CostUSD != 0.01 {
		t.Errorf("integrate cost = %v, want the arbiter's 0.01 carried onto the result", integrate.CostUSD)
	}
}

// noopArbiterExec succeeds without touching the worktree, leaving the conflict
// markers in place — used to exercise the resolve-then-verify guard.
type noopArbiterExec struct{}

func (noopArbiterExec) Run(_ context.Context, t core.Task) (core.Result, error) {
	return core.Result{StepID: t.StepID, Summary: "did nothing"}, nil
}

func TestMergeConflictEscalateArbiterLeavesMarkersFails(t *testing.T) {
	execs := map[string]core.Executor{
		"a":       fileWriterExec{file: "shared.md", body: "A"},
		"b":       fileWriterExec{file: "shared.md", body: "B"},
		"arbiter": noopArbiterExec{}, // leaves the markers unresolved
	}
	eng, st := newGitEngine(t, execs) // AutoApprover would approve, but the guard fails first
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r1", Name: "f", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(context.Background(), "r1", conflictFlow(flow.FailEscalate)); err == nil {
		t.Fatal("run should fail when the arbiter leaves conflict markers, even with auto-approve")
	}
}

func TestMergeConflictEscalateRejectFails(t *testing.T) {
	execs := map[string]core.Executor{
		"a":       fileWriterExec{file: "shared.md", body: "A"},
		"b":       fileWriterExec{file: "shared.md", body: "B"},
		"arbiter": fileWriterExec{file: "shared.md", body: "RESOLVED"},
	}
	eng, st := newGitEngine(t, execs)
	eng.Gate = &gate.Evaluator{Approver: rejectApprover{}, Verifier: gate.CommandVerifier{}}
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r1", Name: "f", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(context.Background(), "r1", conflictFlow(flow.FailEscalate)); err == nil {
		t.Fatal("run should fail when the human rejects the conflict resolution")
	}
}

// TestMergeConflictEscalateResumesRemainingBranches proves that an escalation
// which resolves the FIRST conflicting branch does not silently drop the branches
// merged after it. Three isolated steps feed one merge join: a and b both write
// shared.md (so b conflicts when merged after a), while c adds an independent file.
// The arbiter resolves the a/b conflict; after the human approves, the join must
// RESUME and merge c too — so the committed integrate tree carries all three files.
func TestMergeConflictEscalateResumesRemainingBranches(t *testing.T) {
	execs := map[string]core.Executor{
		"a":       fileWriterExec{file: "shared.md", body: "A"},
		"b":       fileWriterExec{file: "shared.md", body: "B"},
		"c":       fileWriterExec{file: "c.md", body: "C"}, // independent file, merged AFTER the conflict
		"arbiter": fileWriterExec{file: "shared.md", body: "RESOLVED"},
	}
	eng, st := newGitEngine(t, execs)
	autoGate := flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}
	// Needs order [a,b,c] fixes the merge order: a (clean), b (conflict), c (clean).
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "a", Prompt: "p", Workspace: flow.WSIsolated, Gate: autoGate},
		{ID: "b", Agent: "b", Prompt: "p", Workspace: flow.WSIsolated, Gate: autoGate},
		{ID: "c", Agent: "c", Prompt: "p", Workspace: flow.WSIsolated, Gate: autoGate},
		{ID: "integrate", Needs: []string{"a", "b", "c"}, Workspace: flow.WSIsolated,
			Join: &flow.Join{Strategy: flow.JoinMerge, Agent: "arbiter", OnConflict: flow.FailEscalate}},
	}}
	if err := flow.Validate(f); err != nil {
		t.Fatalf("flow invalid: %v", err)
	}
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r1", Name: "f", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run should succeed after the conflict is resolved and remaining branches resume: %v", err)
	}

	got, _ := st.GetRun(context.Background(), "r1")
	var integrate core.StepState
	for _, s := range got.Steps {
		if s.StepID == "integrate" {
			integrate = s
		}
	}
	if integrate.Status != core.StepSucceeded {
		t.Fatalf("integrate status = %q, want succeeded", integrate.Status)
	}
	// The committed integrate tree must carry BOTH the resolved conflict file AND
	// branch c's independent file — c is merged only if the join resumed after the
	// escalation. CommittedResult enumerates every tracked file as an artifact.
	names := map[string]bool{}
	for _, a := range integrate.Artifacts {
		names[filepath.Base(a.Path)] = true
	}
	if !names["shared.md"] {
		t.Errorf("integrate tree missing shared.md (the resolved conflict): %+v", integrate.Artifacts)
	}
	if !names["c.md"] {
		t.Errorf("integrate tree missing c.md — branch c was silently dropped after the conflict: %+v", integrate.Artifacts)
	}
}

// markerResolvingExec resolves a merge conflict by rewriting EVERY file under the
// worktree that still carries conflict markers with clean content — so it resolves
// whichever file conflicts on each escalation rung (a fixed-filename arbiter can't,
// since a cascade conflicts on a different file each time). It skips .git.
type markerResolvingExec struct{}

func (markerResolvingExec) Run(_ context.Context, t core.Task) (core.Result, error) {
	err := filepath.Walk(t.WorkDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), "<<<<<<<") {
			if err := os.WriteFile(p, []byte("RESOLVED\n"), 0o644); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return core.Result{}, err
	}
	return core.Result{StepID: t.StepID, Summary: "resolved conflict markers", CostUSD: 0.01}, nil
}

// TestMergeConflictEscalateResumesAcrossTwoConflicts drives the escalation ladder to
// depth 2: four isolated branches feed one merge join in order [a,b,c,d] where b
// conflicts with a on shared.md AND c conflicts on a SECOND file shared2.md, while d
// adds an independent file. The merge sequence is a (clean) → b (conflict #1, resolved
// + approved) → resume → c (conflict #2, resolved + approved via the recursion) →
// resume → d (clean). The committed integrate tree must carry ALL FOUR files, proving
// no branch is dropped across two cascaded conflicts.
func TestMergeConflictEscalateResumesAcrossTwoConflicts(t *testing.T) {
	execs := map[string]core.Executor{
		// a seeds both shared files; b conflicts on shared.md, c on shared2.md.
		"a":       multiFileWriterExec{files: map[string]string{"shared.md": "A1", "shared2.md": "A2"}},
		"b":       fileWriterExec{file: "shared.md", body: "B"},
		"c":       fileWriterExec{file: "shared2.md", body: "C"},
		"d":       fileWriterExec{file: "d.md", body: "D"}, // independent, merged after BOTH conflicts
		"arbiter": markerResolvingExec{},
	}
	eng, st := newGitEngine(t, execs)
	autoGate := flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "a", Prompt: "p", Workspace: flow.WSIsolated, Gate: autoGate},
		{ID: "b", Agent: "b", Prompt: "p", Workspace: flow.WSIsolated, Gate: autoGate},
		{ID: "c", Agent: "c", Prompt: "p", Workspace: flow.WSIsolated, Gate: autoGate},
		{ID: "d", Agent: "d", Prompt: "p", Workspace: flow.WSIsolated, Gate: autoGate},
		{ID: "integrate", Needs: []string{"a", "b", "c", "d"}, Workspace: flow.WSIsolated,
			Join: &flow.Join{Strategy: flow.JoinMerge, Agent: "arbiter", OnConflict: flow.FailEscalate}},
	}}
	if err := flow.Validate(f); err != nil {
		t.Fatalf("flow invalid: %v", err)
	}
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r1", Name: "f", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run should succeed after both cascaded conflicts resolve and remaining branches resume: %v", err)
	}

	got, _ := st.GetRun(context.Background(), "r1")
	var integrate core.StepState
	for _, s := range got.Steps {
		if s.StepID == "integrate" {
			integrate = s
		}
	}
	if integrate.Status != core.StepSucceeded {
		t.Fatalf("integrate status = %q, want succeeded", integrate.Status)
	}
	names := map[string]bool{}
	for _, a := range integrate.Artifacts {
		names[filepath.Base(a.Path)] = true
	}
	for _, want := range []string{"shared.md", "shared2.md", "d.md"} {
		if !names[want] {
			t.Errorf("integrate tree missing %s — a branch was dropped across the two-conflict cascade: %+v", want, integrate.Artifacts)
		}
	}
}

func TestAttemptAutoGateFailureCarriesVerifierOutput(t *testing.T) {
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		Gate:  &gate.Evaluator{Verifier: gate.CommandVerifier{}},
		Store: store.NewMem(),
		Bus:   event.NewBus(),
		Clock: core.SystemClock{},
	}
	s := &flow.Step{ID: "a", Agent: "mock", Gate: flow.Gate{
		Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: `echo "verifier: boom"; exit 1`}}}
	_, gateFailed, err := e.attempt(context.Background(), "r1", s, nil, 1, t.TempDir(), "")
	if !gateFailed {
		t.Fatal("want gateFailed=true on a failed auto gate")
	}
	var vf *gate.VerifierFailure
	if !errors.As(err, &vf) {
		t.Fatalf("err = %v (%T), want *gate.VerifierFailure", err, err)
	}
	if !strings.Contains(vf.Output, "verifier: boom") {
		t.Errorf("vf.Output = %q, want the verifier stdout", vf.Output)
	}
	if vf.Command != `echo "verifier: boom"; exit 1` {
		t.Errorf("vf.Command = %q", vf.Command)
	}
}

// selfRepairExec writes verifier-failing content until it receives feedback,
// then writes passing content, recording the Feedback it saw on each call.
type selfRepairExec struct {
	mu       sync.Mutex
	feedback []string
}

func (e *selfRepairExec) Run(_ context.Context, t core.Task) (core.Result, error) {
	e.mu.Lock()
	e.feedback = append(e.feedback, t.Feedback)
	e.mu.Unlock()
	marker := "BAD"
	if t.Feedback != "" {
		marker = "GOOD"
	}
	p := filepath.Join(t.WorkDir, "result.txt")
	if err := os.WriteFile(p, []byte(marker+"\n"), 0o644); err != nil {
		return core.Result{}, err
	}
	return core.Result{StepID: t.StepID, Summary: "wrote " + marker,
		Artifacts: []core.Artifact{{StepID: t.StepID, Path: p}}}, nil
}

func TestSelfRepairFeedsVerifierOutputToRetry(t *testing.T) {
	sr := &selfRepairExec{}
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"repair": sr},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	// Verifier passes only when result.txt contains GOOD; on failure it prints to stdout.
	cmd := `if grep -q GOOD result.txt 2>/dev/null; then exit 0; else echo "verifier: result.txt missing GOOD marker"; exit 1; fi`
	f := &flow.Flow{Name: "selfrepair", Steps: []*flow.Step{
		{ID: "a", Agent: "repair", Retry: &flow.RetryPolicy{Max: 2, Backoff: flow.Duration(time.Second)},
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: cmd}, OnFail: flow.FailRetry}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepSucceeded {
		t.Fatalf("step status = %q, want succeeded", got.Steps[0].Status)
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if len(sr.feedback) != 2 {
		t.Fatalf("executor called %d times, want 2", len(sr.feedback))
	}
	if sr.feedback[0] != "" {
		t.Errorf("attempt 1 feedback = %q, want empty", sr.feedback[0])
	}
	if !strings.Contains(sr.feedback[1], "missing GOOD marker") {
		t.Errorf("attempt 2 feedback = %q, want the verifier stdout", sr.feedback[1])
	}
}

// recordingFlaky errors on the first call (an execution error, not a gate
// failure) then succeeds, recording the Feedback seen on each call.
type recordingFlaky struct {
	mu       sync.Mutex
	calls    int
	feedback []string
}

func (r *recordingFlaky) Run(_ context.Context, t core.Task) (core.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.feedback = append(r.feedback, t.Feedback)
	if r.calls < 2 {
		return core.Result{}, fmt.Errorf("transient boom")
	}
	return core.Result{StepID: t.StepID, Summary: "ok"}, nil
}

func TestExecErrorThreadsNoFeedback(t *testing.T) {
	rec := &recordingFlaky{}
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"flaky": rec},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	f := &flow.Flow{Name: "execerr", Steps: []*flow.Step{
		{ID: "a", Agent: "flaky", Retry: &flow.RetryPolicy{Max: 2, Backoff: flow.Duration(time.Second)},
			Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.feedback) != 2 {
		t.Fatalf("executor called %d times, want 2", len(rec.feedback))
	}
	for i, fb := range rec.feedback {
		if fb != "" {
			t.Errorf("attempt %d feedback = %q, want empty (exec errors carry no feedback)", i+1, fb)
		}
	}
}

// TestStepCanceledWhileAwaitingGate asserts that a step canceled while blocked
// on a manual gate is recorded as StepCanceled (not StepFailed).
func TestStepCanceledWhileAwaitingGate(t *testing.T) {
	st := store.NewMem()
	ba := &blockingApprover{gate: make(chan bool, 1), await: make(chan struct{})}
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: ba, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: core.SystemClock{},
	}
	f := &flow.Flow{Name: "gc", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	mustCreate(t, st, "r1", f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, "r1", f) }()

	// Wait until the step is blocked inside the gate's Approve call.
	<-ba.await
	// Cancel the run while the gate is still waiting for a human decision.
	cancel()

	if err := <-done; err == nil {
		t.Fatal("expected cancellation error from Run")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Status != core.RunCanceled {
		t.Fatalf("run status = %q, want canceled", got.Status)
	}
	if len(got.Steps) == 0 {
		t.Fatal("no step state recorded")
	}
	if got.Steps[0].Status != core.StepCanceled {
		t.Fatalf("step status = %q, want canceled", got.Steps[0].Status)
	}
}
