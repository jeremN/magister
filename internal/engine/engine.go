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
	Log   *slog.Logger // non-fatal store/bus failures; nil = discard (M3 wires a real handler)
}

// Run executes one flow to completion. The first failing step cancels the run's
// context and the rest tear down. The run row must already exist in the store
// (the caller creates it); Run drives its status and all step transitions.
func (e *Engine) Run(parent context.Context, runID core.RunID, f *flow.Flow) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	if err := e.Store.SetRunStatus(ctx, runID, core.RunRunning, ""); err != nil {
		e.logger().Error("set run status running", "run", runID, "err", err)
	}
	e.Bus.Publish(event.Event{RunID: string(runID), Kind: event.RunStarted, At: e.Clock.Now()})

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
		e.Bus.Publish(event.Event{RunID: string(runID), Kind: event.RunDone, Err: "canceled", At: e.Clock.Now()})
		return parent.Err()
	case firstErr != nil:
		if err := e.Store.SetRunStatus(final, runID, core.RunFailed, firstErr.Error()); err != nil {
			e.logger().Error("set run status failed", "run", runID, "err", err)
		}
		e.Bus.Publish(event.Event{RunID: string(runID), Kind: event.RunDone, Err: firstErr.Error(), At: e.Clock.Now()})
		return firstErr
	default:
		if err := e.Store.SetRunStatus(final, runID, core.RunSucceeded, ""); err != nil {
			e.logger().Error("set run status succeeded", "run", runID, "err", err)
		}
		e.Bus.Publish(event.Event{RunID: string(runID), Kind: event.RunDone, At: e.Clock.Now()})
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
			e.transition(ctx, runID, stepState(runID, s.ID, core.StepRetrying, attempt, core.Result{}, lastErr),
				event.Event{StepID: s.ID, Kind: event.StepRetrying, Attempt: attempt})
			if !e.backoff(ctx, s, attempt) {
				return core.Result{}, ctx.Err()
			}
		}

		e.transition(ctx, runID, stepState(runID, s.ID, core.StepRunning, attempt, core.Result{}, nil),
			event.Event{StepID: s.ID, Kind: event.StepStarted, Attempt: attempt})

		res, execErr := e.execute(ctx, runID, s, inputs, workDir)
		if execErr == nil {
			res.StepID = s.ID
			ok, gerr := e.Gate.Evaluate(ctx, s, res, workDir)
			switch {
			case gerr != nil:
				execErr = gerr
			case !ok:
				execErr = fmt.Errorf("gate failed (policy=%q)", gatePolicyOf(s))
			default:
				e.transition(ctx, runID, stepState(runID, s.ID, core.StepSucceeded, attempt, res, nil),
					event.Event{StepID: s.ID, Kind: event.StepDone, Summary: res.Summary, CostUSD: res.CostUSD, Attempt: attempt})
				return res, nil
			}
		}

		lastErr = execErr
		if attempt < maxAttempts && s.Retry != nil {
			continue // retry (covers both execution and gate failures)
		}
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attempt, core.Result{}, lastErr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attempt, Err: lastErr.Error()})
		return core.Result{}, lastErr
	}
	return core.Result{}, lastErr
}

// execute runs the step's work: a join strategy for fan-in steps, otherwise the
// named executor. A per-step timeout wraps the call when set.
func (e *Engine) execute(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact, workDir string) (core.Result, error) {
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(s.Timeout))
		defer cancel()
	}
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

// backoff sleeps before a retry using the injected clock. Returns false if the
// context was canceled while waiting. Exponential; jitter arrives in M4.
func (e *Engine) backoff(ctx context.Context, s *flow.Step, attempt int) bool {
	if s.Retry == nil || s.Retry.Backoff <= 0 {
		return true
	}
	base := time.Duration(s.Retry.Backoff)
	d := base * (1 << (attempt - 2)) // attempt 2 → base, 3 → 2×base, …
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

func stepState(runID core.RunID, stepID string, status core.StepStatus, attempt int, res core.Result, err error) core.StepState {
	st := core.StepState{
		RunID:     runID,
		StepID:    stepID,
		Status:    status,
		Attempt:   attempt,
		Summary:   res.Summary,
		CostUSD:   res.CostUSD,
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

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func (e *Engine) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return discardLogger
}
