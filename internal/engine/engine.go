// Package engine executes a flow as a DAG: one goroutine per step, each blocking
// on its dependencies' channels and closing its own when it finishes. Fan-out
// and fan-in emerge from the graph — there is no explicit scheduler. Concurrency
// is bounded by a global weighted semaphore plus an optional per-run cap.
package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/flow"
	"concentus/internal/gate"
	"concentus/internal/join"
)

// Compile-time assertion: *event.Bus satisfies core.Publisher.
var _ core.Publisher = (*event.Bus)(nil)

type Engine struct {
	Execs map[string]core.Executor // registry: "opus"→CLIAgent, …, "mock"→Mock
	WS    core.Workspace
	Gate  *gate.Evaluator
	Joins join.Registry
	Store core.Store
	Bus   core.Publisher
	Sem   *semaphore.Weighted // global concurrency cap; nil = unbounded
	Clock core.Clock
	// Rand returns a jitter factor in [0,1); nil defaults to math/rand/v2 (auto-seeded).
	// Injected so backoff is deterministic in tests, mirroring Clock.
	Rand func() float64
	Log  *slog.Logger // non-fatal store/bus failures; nil = discard (M3 wires a real handler)
}

// Run executes one flow to completion from scratch. The run row must already
// exist in the store (the caller creates it); Run drives its status and all step
// transitions. The first failing step cancels the run's context; the rest unwind.
func (e *Engine) Run(parent context.Context, runID core.RunID, f *flow.Flow) error {
	return e.runDAG(parent, runID, f, nil)
}

// Resume continues an interrupted run. Steps already 'succeeded' in rs are not
// re-executed — their persisted artifacts seed downstream inputs; every other
// step is (re-)run from a fresh attempt. This is at-least-once (spec §7): a step
// that was mid-flight at crash runs again.
func (e *Engine) Resume(parent context.Context, rs core.RunState, f *flow.Flow) error {
	seed := make(map[string]core.Result, len(rs.Steps))
	for _, st := range rs.Steps {
		if st.Status == core.StepSucceeded {
			seed[st.StepID] = core.Result{
				StepID:    st.StepID,
				Summary:   st.Summary,
				CostUSD:   st.CostUSD,
				Artifacts: st.Artifacts,
			}
		}
	}
	return e.runDAG(parent, rs.ID, f, seed)
}

// runDAG is the shared executor for Run and Resume. seed pre-completes steps:
// their results are available to dependents and they are not executed.
func (e *Engine) runDAG(parent context.Context, runID core.RunID, f *flow.Flow, seed map[string]core.Result) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	if err := e.Store.SetRunStatus(ctx, runID, core.RunRunning, ""); err != nil {
		e.logger().Error("set run status running", "run", runID, "err", err)
	}
	// Persist run-level events (no associated step) so the SSE hub can replay
	// them from the store with real seqs. A Resume re-runs runDAG, so a resumed
	// run records a second run.started — consistent with at-least-once (§7).
	runStartedEv := event.Event{RunID: string(runID), Kind: event.RunStarted, At: e.Clock.Now()}
	if err := e.Store.AppendEvents(ctx, runID, []event.Event{runStartedEv}); err != nil {
		e.logger().Error("append run started event", "run", runID, "err", err)
	}
	e.Bus.Publish(runStartedEv)

	// per-run cap (0 = unlimited within the global semaphore)
	var perRun chan struct{}
	if f.Concurrency > 0 {
		perRun = make(chan struct{}, f.Concurrency)
	}

	done := make(map[string]chan struct{}, len(f.Steps))
	for _, s := range f.Steps {
		done[s.ID] = make(chan struct{})
	}

	var (
		mu       sync.Mutex
		results  = make(map[string]core.Result, len(f.Steps))
		firstErr error
		errOnce  sync.Once
		wg       sync.WaitGroup
	)
	for id, res := range seed { // pre-completed steps from a prior run
		results[id] = res
	}
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	for _, s := range f.Steps {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done[s.ID]) // always unblock dependents, even on bail-out

			// already completed on a prior run: inputs are seeded, skip execution.
			if _, ok := seed[s.ID]; ok {
				return
			}

			// 1. wait for dependencies (holding NO concurrency token).
			for _, dep := range s.Needs {
				select {
				case <-done[dep]:
				case <-ctx.Done():
					return
				}
			}
			if ctx.Err() != nil {
				return
			}

			// 2. gather upstream artifacts.
			mu.Lock()
			var inputs []core.Artifact
			for _, dep := range s.Needs {
				inputs = append(inputs, results[dep].Artifacts...)
			}
			mu.Unlock()

			// 3. acquire concurrency tokens (per-run, then global), held only
			//    around the work — never while waiting on deps (no hold-and-wait).
			if perRun != nil {
				select {
				case perRun <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-perRun }()
			}
			if e.Sem != nil {
				if err := e.Sem.Acquire(ctx, 1); err != nil {
					return // context canceled while queued
				}
				defer e.Sem.Release(1)
			}
			if ctx.Err() != nil {
				return
			}

			// 4. run the step (execute + gate, with retries).
			res, err := e.runStep(ctx, runID, s, inputs)
			if err != nil {
				fail(fmt.Errorf("step %q: %w", s.ID, err))
				return
			}
			mu.Lock()
			results[s.ID] = res
			mu.Unlock()
		}()
	}

	wg.Wait()

	final := context.WithoutCancel(ctx)
	switch {
	case parent.Err() != nil: // external cancellation wins over any step error it caused
		if err := e.Store.SetRunStatus(final, runID, core.RunCanceled, "canceled"); err != nil {
			e.logger().Error("set run status canceled", "run", runID, "err", err)
		}
		runDoneEv := event.Event{RunID: string(runID), Kind: event.RunDone, Err: "canceled", At: e.Clock.Now()}
		if err := e.Store.AppendEvents(final, runID, []event.Event{runDoneEv}); err != nil {
			e.logger().Error("append run done event", "run", runID, "err", err)
		}
		e.Bus.Publish(runDoneEv)
		return parent.Err()
	case firstErr != nil:
		if err := e.Store.SetRunStatus(final, runID, core.RunFailed, firstErr.Error()); err != nil {
			e.logger().Error("set run status failed", "run", runID, "err", err)
		}
		runDoneEv := event.Event{RunID: string(runID), Kind: event.RunDone, Err: firstErr.Error(), At: e.Clock.Now()}
		if err := e.Store.AppendEvents(final, runID, []event.Event{runDoneEv}); err != nil {
			e.logger().Error("append run done event", "run", runID, "err", err)
		}
		e.Bus.Publish(runDoneEv)
		return firstErr
	default:
		if err := e.Store.SetRunStatus(final, runID, core.RunSucceeded, ""); err != nil {
			e.logger().Error("set run status succeeded", "run", runID, "err", err)
		}
		runDoneEv := event.Event{RunID: string(runID), Kind: event.RunDone, At: e.Clock.Now()}
		if err := e.Store.AppendEvents(final, runID, []event.Event{runDoneEv}); err != nil {
			e.logger().Error("append run done event", "run", runID, "err", err)
		}
		e.Bus.Publish(runDoneEv)
		return nil
	}
}

// runStep runs one step: execute + gate, looping on the unified attempt budget.
func (e *Engine) runStep(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact) (core.Result, error) {
	workDir, cleanup, err := e.WS.For(runID, s)
	if err != nil {
		return core.Result{}, err
	}
	defer func() { _ = cleanup() }()

	maxAttempts := 1
	if s.Retry != nil {
		maxAttempts = s.Retry.Max
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			e.transition(ctx, runID, stepState(runID, s.ID, core.StepRetrying, attempt, workDir, core.Result{}, lastErr),
				event.Event{StepID: s.ID, Kind: event.StepRetrying, Attempt: attempt})
			if !e.backoff(ctx, s, attempt) {
				return core.Result{}, ctx.Err()
			}
		}

		e.transition(ctx, runID, stepState(runID, s.ID, core.StepRunning, attempt, workDir, core.Result{}, nil),
			event.Event{StepID: s.ID, Kind: event.StepStarted, Attempt: attempt})

		res, _, execErr := e.attempt(ctx, runID, s, inputs, attempt, workDir)
		if execErr == nil {
			e.transition(ctx, runID, stepState(runID, s.ID, core.StepSucceeded, attempt, workDir, res, nil),
				event.Event{StepID: s.ID, Kind: event.StepDone, Summary: res.Summary, CostUSD: res.CostUSD, Attempt: attempt})
			return res, nil
		}
		lastErr = execErr

		if attempt < maxAttempts && s.Retry != nil {
			continue // retry (covers both execution and gate failures)
		}
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attempt, workDir, core.Result{}, lastErr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attempt, Err: lastErr.Error()})
		return core.Result{}, lastErr
	}
	return core.Result{}, lastErr
}

// withTimeout returns a child context bounded by d, or the parent + a no-op cancel
// when d is unset. The no-op keeps callers' `defer cancel()` unconditional.
func withTimeout(ctx context.Context, d flow.Duration) (context.Context, context.CancelFunc) {
	if d > 0 {
		return context.WithTimeout(ctx, time.Duration(d))
	}
	return ctx, func() {}
}

// attempt runs one execute + gate. The per-step timeout bounds the executor and an
// AUTO gate's verifier (the automated work); a manual/conditional gate's approval
// runs on the un-timed parent ctx. gateFailed distinguishes a gate verdict from an
// executor/infra error so runStep can decide escalation.
func (e *Engine) attempt(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact, attemptNum int, workDir string) (res core.Result, gateFailed bool, err error) {
	attemptCtx, cancel := withTimeout(ctx, s.Timeout)
	defer cancel()

	res, err = e.execute(attemptCtx, runID, s, inputs, workDir)
	if err != nil {
		return core.Result{}, false, err
	}
	res.StepID = s.ID

	gateCtx := ctx // manual/conditional approval is NOT timed out
	if gatePolicyOf(s) == flow.GateAuto {
		gateCtx = attemptCtx // the verifier shares the step timeout
	} else if gateBlocks(s) {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepAwaitingGate, attemptNum, workDir, res, nil),
			event.Event{StepID: s.ID, Kind: event.GateAwaiting, Attempt: attemptNum})
	}
	ok, gerr := e.Gate.Evaluate(gateCtx, runID, s, res, workDir)
	switch {
	case gerr != nil:
		return res, false, gerr
	case !ok:
		return res, true, fmt.Errorf("gate failed (policy=%q)", gatePolicyOf(s))
	default:
		return res, false, nil
	}
}

// execute runs the step's work: a join strategy for fan-in steps, otherwise the
// named executor. The caller (attempt) bounds this by the step timeout.
func (e *Engine) execute(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact, workDir string) (core.Result, error) {
	if s.Join != nil {
		strat, ok := e.Joins[s.Join.Strategy]
		if !ok {
			return core.Result{}, fmt.Errorf("join strategy %q not implemented yet", s.Join.Strategy)
		}
		return strat.Join(ctx, s, inputs, workDir)
	}
	ag, ok := e.Execs[s.Agent]
	if !ok {
		return core.Result{}, fmt.Errorf("unknown agent %q", s.Agent)
	}
	return ag.Run(ctx, core.Task{
		RunID:   runID,
		StepID:  s.ID,
		Role:    s.Role,
		Prompt:  promptFor(s, inputs),
		Inputs:  inputs,
		WorkDir: workDir,
	})
}

// maxBackoff caps exponential backoff before jitter, so a high retry count can't
// schedule an unbounded sleep.
const maxBackoff = 30 * time.Second

func (e *Engine) randFloat() float64 {
	if e.Rand != nil {
		return e.Rand()
	}
	return rand.Float64()
}

// backoff sleeps before a retry using the injected clock. Returns false if the
// context was canceled while waiting. Exponential, clamped to maxBackoff, with
// full jitter (sleep ∈ [0, ceiling)) to spread concurrent retries.
func (e *Engine) backoff(ctx context.Context, s *flow.Step, attempt int) bool {
	if s.Retry == nil || s.Retry.Backoff <= 0 {
		return true
	}
	base := time.Duration(s.Retry.Backoff)
	if attempt < 2 {
		attempt = 2 // attempt 1 has no prior failure to back off from; guards a negative shift
	}
	d := base * (1 << (attempt - 2)) // attempt 2 → base, 3 → 2×base, …
	if d > maxBackoff || d <= 0 {    // d<=0 guards int64 overflow on huge attempt counts
		d = maxBackoff
	}
	d = time.Duration(e.randFloat() * float64(d)) // full jitter
	select {
	case <-e.Clock.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// transition persists a step state + its event in one store call (durable),
// then publishes the event to the live bus (persist-then-publish).
func (e *Engine) transition(ctx context.Context, runID core.RunID, st core.StepState, ev event.Event) {
	ev.RunID = string(runID)
	ev.At = e.Clock.Now()
	if err := e.Store.SaveStepTransition(context.WithoutCancel(ctx), st, []event.Event{ev}); err != nil {
		// persist-then-publish: the write did NOT happen, so do NOT publish the
		// original event. Surface the store error on the live stream and stop.
		e.Bus.Publish(event.Event{RunID: string(runID), StepID: st.StepID, Kind: event.StepFailed, Err: "store: " + err.Error(), At: e.Clock.Now()})
		e.logger().Error("save step transition", "run", runID, "step", st.StepID, "err", err)
		return
	}
	e.Bus.Publish(ev)
}

func stepState(runID core.RunID, stepID string, status core.StepStatus, attempt int, workDir string, res core.Result, err error) core.StepState {
	st := core.StepState{
		RunID:     runID,
		StepID:    stepID,
		Status:    status,
		Attempt:   attempt,
		Summary:   res.Summary,
		CostUSD:   res.CostUSD,
		WorkDir:   workDir,
		Artifacts: res.Artifacts,
	}
	if err != nil {
		st.Err = err.Error()
	}
	return st
}

func gatePolicyOf(s *flow.Step) flow.GatePolicy {
	if s.Gate.Policy == "" {
		return flow.GateManual
	}
	return s.Gate.Policy
}

// gateBlocks reports whether a step's gate can block on human approval. Auto
// gates resolve synchronously via the verifier and never block.
func gateBlocks(s *flow.Step) bool {
	switch gatePolicyOf(s) {
	case flow.GateManual, flow.GateConditional:
		return true
	default:
		return false
	}
}

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func (e *Engine) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return discardLogger
}
