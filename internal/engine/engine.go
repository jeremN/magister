// Package engine executes a flow as a DAG: one goroutine per step, each blocking
// on its dependencies' channels and closing its own when it finishes. Fan-out
// and fan-in emerge from the graph — there is no explicit scheduler. Concurrency
// is bounded by a global weighted semaphore plus an optional per-run cap.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/semaphore"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/flow"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/logctx"
	"concentus/internal/metrics"
)

// Compile-time assertion: *event.Bus satisfies core.Publisher.
var _ core.Publisher = (*event.Bus)(nil)

// tracer is the engine's OTel tracer; a no-op until the daemon installs an SDK provider.
var tracer = otel.Tracer("concentus")

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
	// Metrics records run/step/gate/agent counters and durations; nil = no-op.
	Metrics *metrics.Metrics
}

// Provision records a run's source repo + pinned base SHA with the workspace
// before the run starts (see core.Workspace.Provision). Empty repo selects the
// synthetic empty-base scratch repo (default; today's behavior).
func (e *Engine) Provision(ctx context.Context, runID core.RunID, repo, base string) error {
	return e.WS.Provision(ctx, runID, repo, base)
}

// BasePath returns a run's scratch base repo path (see core.Workspace.BasePath),
// so the supervisor can reach it for post-run delivery without holding the WS itself.
func (e *Engine) BasePath(runID core.RunID) string {
	return e.WS.BasePath(runID)
}

// ReclaimScratch removes a run's scratch workspace and reports whether a directory
// was actually removed. Delegates to the workspace; the scratch janitor calls it
// after a run is terminal and past its retention TTL.
func (e *Engine) ReclaimScratch(ctx context.Context, runID core.RunID) (bool, error) {
	return e.WS.Reclaim(ctx, runID)
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
	ctx, span := tracer.Start(ctx, "run "+string(runID),
		trace.WithAttributes(attribute.String("magister.run_id", string(runID)), attribute.String("magister.flow", f.Name)))
	defer span.End()

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
	runStart := e.Clock.Now()
	e.Metrics.RunStarted()
	defer e.Metrics.RunFinished()

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
			queueStart := e.Clock.Now()
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
			e.logger().DebugContext(ctx, "step slot acquired", "run", string(runID), "step", s.ID, "waited", e.Clock.Now().Sub(queueStart))
			e.Metrics.StepStarted()
			defer e.Metrics.StepFinished()

			// 4. run the step (execute + gate, with retries).
			stepStart := e.Clock.Now()
			stepCtx, stepSpan := tracer.Start(ctx, "step "+s.ID,
				trace.WithAttributes(attribute.String("magister.step_id", s.ID)))
			res, err := e.runStep(stepCtx, runID, s, inputs)
			if err != nil {
				stepSpan.RecordError(err)
				stepSpan.SetStatus(codes.Error, err.Error())
			}
			stepSpan.End()
			stepDur := e.Clock.Now().Sub(stepStart)
			if err != nil {
				e.Metrics.ObserveStep("failed", stepDur)
				fail(fmt.Errorf("step %q: %w", s.ID, err))
				return
			}
			e.Metrics.ObserveStep("succeeded", stepDur)
			mu.Lock()
			results[s.ID] = res
			mu.Unlock()
		}()
	}

	wg.Wait()

	// Reclaim the run's isolated worktrees now that all steps have finished and
	// their artifact paths are no longer needed by dependents. Done before the
	// final status write so observers who poll "succeeded" see a clean wt/. Best-effort.
	// Use context.WithoutCancel so a run cancel doesn't skip teardown.
	if err := e.WS.TeardownRun(context.WithoutCancel(ctx), runID); err != nil {
		e.logger().Error("teardown run workspaces", "run", runID, "err", err)
	}

	switch {
	case parent.Err() != nil:
		span.SetStatus(codes.Error, "canceled")
	case firstErr != nil:
		span.RecordError(firstErr)
		span.SetStatus(codes.Error, firstErr.Error())
	}

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
		e.Metrics.ObserveRun("canceled", e.Clock.Now().Sub(runStart))
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
		e.Metrics.ObserveRun("failed", e.Clock.Now().Sub(runStart))
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
		e.Metrics.ObserveRun("succeeded", e.Clock.Now().Sub(runStart))
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
	var lastFeedback string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			e.transition(ctx, runID, stepState(runID, s.ID, core.StepRetrying, attempt, workDir, core.Result{}, lastErr),
				event.Event{StepID: s.ID, Kind: event.StepRetrying, Attempt: attempt})
			e.Metrics.StepRetried()
			if !e.backoff(ctx, runID, s, attempt) {
				return core.Result{}, ctx.Err()
			}
		}

		if attempt > 1 && lastFeedback != "" {
			e.logger().DebugContext(ctx, "retrying with verifier feedback",
				"run", string(runID), "step", s.ID, "attempt", attempt, "feedback_bytes", len(lastFeedback))
		}

		e.transition(ctx, runID, stepState(runID, s.ID, core.StepRunning, attempt, workDir, core.Result{}, nil),
			event.Event{StepID: s.ID, Kind: event.StepStarted, Attempt: attempt})

		res, gateFailed, execErr := e.attempt(ctx, runID, s, inputs, attempt, workDir, lastFeedback)
		if execErr == nil {
			if cerr := e.commitIsolated(ctx, runID, s, workDir, &res); cerr != nil {
				execErr = cerr // a failed commit is a step failure → normal disposition
			} else {
				e.transition(ctx, runID, stepState(runID, s.ID, core.StepSucceeded, attempt, workDir, res, nil),
					event.Event{StepID: s.ID, Kind: event.StepDone, Summary: res.Summary, CostUSD: res.CostUSD, Attempt: attempt})
				return res, nil
			}
		}
		lastErr = execErr
		lastFeedback = ""
		var vf *gate.VerifierFailure
		if errors.As(lastErr, &vf) {
			lastFeedback = vf.Output
		}

		// A join step's failure disposition is governed by on_conflict (only `retry`
		// re-runs via the generic budget); a normal/gate failure retries on s.Retry.
		canRetry := attempt < maxAttempts && s.Retry != nil
		if s.Join != nil {
			canRetry = canRetry && s.Join.OnConflict == flow.FailRetry
		}
		if canRetry {
			continue // retry (covers execution, gate, and on_conflict=retry join failures)
		}
		// budget spent — terminal disposition.
		if s.Join != nil && s.Join.OnConflict == flow.FailEscalate {
			return e.escalateJoin(ctx, runID, s, inputs, workDir, lastErr, attempt)
		}
		e.logger().WarnContext(ctx, "retry budget exhausted", "run", string(runID), "step", s.ID, "attempts", attempt, "last_err", lastErr, "escalating", gateFailed && gateEscalates(s))
		// A failed auto/conditional gate with on_fail=escalate becomes a human approval.
		if gateFailed && gateEscalates(s) {
			return e.escalate(ctx, runID, s, res, workDir, lastErr, attempt)
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
// AUTO gate's verifier (the automated work); a manual gate's human approval and a
// conditional gate's inline expr evaluation run on the un-timed parent ctx.
// gateFailed distinguishes a gate verdict from an executor/infra error so runStep
// can decide escalation.
func (e *Engine) attempt(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact, attemptNum int, workDir string, feedback string) (res core.Result, gateFailed bool, err error) {
	attemptCtx, cancel := withTimeout(ctx, s.Timeout)
	defer cancel()

	res, err = e.execute(attemptCtx, runID, s, inputs, attemptNum, workDir, feedback)
	if err != nil {
		return core.Result{}, false, err
	}
	res.StepID = s.ID

	gateCtx := ctx // manual approval blocks on a human; conditional eval is inline — neither is bounded by the step timeout
	if gatePolicyOf(s) == flow.GateAuto {
		gateCtx = attemptCtx // the verifier shares the step timeout
	} else if gateBlocks(s) {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepAwaitingGate, attemptNum, workDir, res, nil),
			event.Event{StepID: s.ID, Kind: event.GateAwaiting, Attempt: attemptNum})
		e.Metrics.GateAwaited()
	}
	gateCtx, gateSpan := tracer.Start(gateCtx, "gate "+s.ID,
		trace.WithAttributes(attribute.String("magister.gate_policy", string(gatePolicyOf(s)))))
	ok, output, gerr := e.Gate.Evaluate(gateCtx, runID, s, res, workDir)
	switch {
	case gerr != nil:
		gateSpan.RecordError(gerr)
		gateSpan.SetStatus(codes.Error, gerr.Error())
	case !ok:
		gateSpan.SetStatus(codes.Error, "gate failed")
	}
	gateSpan.End()
	gargs := []any{"run", string(runID), "step", s.ID, "attempt", attemptNum, "policy", gatePolicyOf(s), "pass", ok}
	if gerr != nil {
		gargs = append(gargs, "err", gerr)
	}
	e.logger().DebugContext(gateCtx, "gate evaluated", gargs...)
	switch {
	case gerr != nil:
		return res, false, gerr
	case !ok:
		if gatePolicyOf(s) == flow.GateAuto {
			// s.Gate.Verifier is non-nil here: flow.validateGate requires every auto gate to set a non-empty Command.
			return res, true, &gate.VerifierFailure{Command: s.Gate.Verifier.Command, Output: output}
		}
		return res, true, fmt.Errorf("gate failed (policy=%q)", gatePolicyOf(s))
	default:
		return res, false, nil
	}
}

// runAgent runs the named agent with prompt in workDir, binding Task.Emit to the
// persist-then-publish milestone path. Shared by normal steps and join arbiters
// so an arbiter streams agent.tool milestones exactly like a normal step.
func (e *Engine) runAgent(ctx context.Context, runID core.RunID, stepID, role, agentName, prompt, workDir string, attemptNum int, inputs []core.Artifact, feedback string) (core.Result, error) {
	ag, ok := e.Execs[agentName]
	if !ok {
		return core.Result{}, fmt.Errorf("unknown agent %q", agentName)
	}
	emit := func(ev event.Event) {
		ev.RunID, ev.StepID, ev.Attempt, ev.At = string(runID), stepID, attemptNum, e.Clock.Now()
		if ev.Kind == event.AgentTool {
			e.Metrics.AgentTool(agentName)
		}
		if err := e.Store.AppendEvents(context.WithoutCancel(ctx), runID, []event.Event{ev}); err != nil {
			e.logger().Error("append agent milestone", "run", runID, "step", stepID, "err", err)
			return
		}
		e.Bus.Publish(ev) // Seq is irrelevant on the bus — sse.go re-reads the store for real seqs
	}
	ctx, span := tracer.Start(ctx, "agent "+agentName, trace.WithAttributes(
		attribute.String("magister.agent", agentName),
		attribute.String("magister.role", role),
		attribute.Int("magister.attempt", attemptNum)))
	defer span.End()
	if len(feedback) > 0 {
		span.SetAttributes(attribute.Int("magister.feedback_bytes", len(feedback)))
	}
	agentCtx := logctx.With(ctx, e.logger().With("run", string(runID), "step", stepID, "agent", agentName))
	e.logger().DebugContext(ctx, "agent starting", "run", string(runID), "step", stepID, "agent", agentName, "role", role, "attempt", attemptNum)
	agentStart := e.Clock.Now()
	res, err := ag.Run(agentCtx, core.Task{
		RunID:    runID,
		StepID:   stepID,
		Role:     role,
		Prompt:   prompt,
		Inputs:   inputs,
		WorkDir:  workDir,
		Feedback: feedback,
		Emit:     emit,
	})
	dur := e.Clock.Now().Sub(agentStart)
	e.Metrics.ObserveAgentRun(agentName, dur) // every invocation, incl. errors
	e.Metrics.AddCost(agentName, res.CostUSD) // per-invocation; no-op on 0 cost
	span.SetAttributes(attribute.Float64("magister.cost_usd", res.CostUSD))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	args := []any{"run", string(runID), "step", stepID, "agent", agentName, "attempt", attemptNum, "dur", dur, "cost_usd", res.CostUSD}
	if err != nil {
		args = append(args, "err", err)
	}
	e.logger().DebugContext(ctx, "agent finished", args...)
	return res, err
}

// execute runs the step's work: a join strategy for fan-in steps, otherwise the
// named executor. The caller (attempt) bounds this by the step timeout. For the
// executor path it binds Task.Emit to persist-then-publish milestone events
// (mirroring transition() without a step-state row).
func (e *Engine) execute(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact, attemptNum int, workDir string, feedback string) (core.Result, error) {
	if s.Join != nil {
		strat, ok := e.Joins[s.Join.Strategy]
		if !ok {
			return core.Result{}, fmt.Errorf("join strategy %q not implemented yet", s.Join.Strategy)
		}
		run := func(ctx context.Context, agentName, prompt, wd string, in []core.Artifact) (core.Result, error) {
			return e.runAgent(ctx, runID, s.ID, "arbiter", agentName, prompt, wd, attemptNum, in, feedback)
		}
		e.logger().DebugContext(ctx, "join starting", "run", string(runID), "step", s.ID, "strategy", s.Join.Strategy, "inputs", len(inputs), "attempt", attemptNum)
		joinCtx, joinSpan := tracer.Start(ctx, "join "+s.ID, trace.WithAttributes(
			attribute.String("magister.join_strategy", string(s.Join.Strategy)),
			attribute.Int("magister.join_inputs", len(inputs))))
		res, err := strat.Join(joinCtx, s, inputs, workDir, run)
		if err != nil {
			joinSpan.RecordError(err)
			joinSpan.SetStatus(codes.Error, err.Error())
		}
		joinSpan.End()
		jargs := []any{"run", string(runID), "step", s.ID, "strategy", s.Join.Strategy, "attempt", attemptNum}
		if err != nil {
			jargs = append(jargs, "err", err)
		}
		e.logger().DebugContext(ctx, "join finished", jargs...)
		var conflict *join.ConflictError
		if errors.As(err, &conflict) {
			e.logger().WarnContext(ctx, "merge conflict detected", "run", string(runID), "step", s.ID, "branch", conflict.Branch, "paths", conflict.Paths, "attempt", attemptNum)
		}
		return res, err
	}
	return e.runAgent(ctx, runID, s.ID, s.Role, s.Agent, promptFor(s, inputs), workDir, attemptNum, inputs, feedback)
}

// commitIsolated records a successful isolated NON-join step's worktree on its
// branch and stamps the result's artifacts with the branch/commit. Joins
// self-commit (the strategy does the git work) and shared steps have no branch,
// so both are skipped. A commit failure is surfaced as a step failure.
func (e *Engine) commitIsolated(ctx context.Context, runID core.RunID, s *flow.Step, workDir string, res *core.Result) error {
	if s.Workspace != flow.WSIsolated || s.Join != nil {
		return nil
	}
	br, sha, err := e.WS.Commit(ctx, runID, s, workDir)
	if err != nil {
		return fmt.Errorf("commit step %q: %w", s.ID, err)
	}
	for i := range res.Artifacts {
		res.Artifacts[i].Branch = br
		res.Artifacts[i].Commit = sha
	}
	return nil
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
func (e *Engine) backoff(ctx context.Context, runID core.RunID, s *flow.Step, attempt int) bool {
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
	e.logger().Debug("step backoff", "run", string(runID), "step", s.ID, "attempt", attempt, "delay", d, "base", base)
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

// gateBlocks reports whether a step's gate can block on human approval. Auto and
// conditional gates resolve synchronously (verifier / expr) and never block.
func gateBlocks(s *flow.Step) bool {
	return gatePolicyOf(s) == flow.GateManual
}

// gateEscalates reports whether a failed gate should escalate to a human. Auto and
// conditional gates can escalate (both resolve automatically); a manual gate's
// rejection is itself a human decision, so it never escalates.
func gateEscalates(s *flow.Step) bool {
	p := gatePolicyOf(s)
	return (p == flow.GateAuto || p == flow.GateConditional) && s.Gate.OnFail == flow.FailEscalate
}

// escalate converts a failed auto/conditional gate into a human approval, reusing the manual
// block-on-channel path. The failure reason rides on the gate.awaiting event's Err.
func (e *Engine) escalate(ctx context.Context, runID core.RunID, s *flow.Step, res core.Result, workDir string, gateErr error, attemptNum int) (core.Result, error) {
	res.StepID = s.ID
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepAwaitingGate, attemptNum, workDir, res, gateErr),
		event.Event{StepID: s.ID, Kind: event.GateAwaiting, Attempt: attemptNum, Err: gateErr.Error()})
	e.Metrics.GateAwaited()

	ok, err := e.Gate.Escalate(ctx, runID, s, res)
	if err != nil {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attemptNum, workDir, core.Result{}, err),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attemptNum, Err: err.Error()})
		return core.Result{}, err
	}
	if !ok {
		rej := fmt.Errorf("escalated gate rejected")
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attemptNum, workDir, core.Result{}, rej),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attemptNum, Err: rej.Error()})
		return core.Result{}, rej
	}
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepSucceeded, attemptNum, workDir, res, nil),
		event.Event{StepID: s.ID, Kind: event.StepDone, Summary: res.Summary, CostUSD: res.CostUSD, Attempt: attemptNum})
	return res, nil
}

// escalateJoin handles a failed join with on_conflict=escalate. A merge conflict
// (a *join.ConflictError) takes the resolveConflictEscalation ladder below. For any
// other join failure (e.g. an arbiter error) there is no result, so approval
// RE-RUNS the join exactly once: approve -> one fresh attempt; reject -> abort. The
// failure reason rides on the gate.awaiting event's Err.
func (e *Engine) escalateJoin(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact, workDir string, joinErr error, attemptNum int) (core.Result, error) {
	var conflict *join.ConflictError
	if errors.As(joinErr, &conflict) {
		return e.resolveConflictEscalation(ctx, runID, s, inputs, workDir, conflict, attemptNum)
	}
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepAwaitingGate, attemptNum, workDir, core.Result{}, joinErr),
		event.Event{StepID: s.ID, Kind: event.GateAwaiting, Attempt: attemptNum, Err: joinErr.Error()})
	e.Metrics.GateAwaited()

	ok, err := e.Gate.Escalate(ctx, runID, s, core.Result{})
	if err != nil {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attemptNum, workDir, core.Result{}, err),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attemptNum, Err: err.Error()})
		return core.Result{}, err
	}
	if !ok {
		rej := fmt.Errorf("escalated join rejected")
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attemptNum, workDir, core.Result{}, rej),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attemptNum, Err: rej.Error()})
		return core.Result{}, rej
	}
	// approved: re-run the join exactly once. gateFailed is intentionally dropped —
	// a gate failure on the re-run is terminal (no nested escalation).
	res, _, execErr := e.attempt(ctx, runID, s, inputs, attemptNum+1, workDir, "")
	if execErr != nil {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attemptNum+1, workDir, core.Result{}, execErr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attemptNum + 1, Err: execErr.Error()})
		return core.Result{}, execErr
	}
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepSucceeded, attemptNum+1, workDir, res, nil),
		event.Event{StepID: s.ID, Kind: event.StepDone, Summary: res.Summary, CostUSD: res.CostUSD, Attempt: attemptNum + 1})
	return res, nil
}

// resolveConflictEscalation runs the merge-conflict ladder: the arbiter resolves
// the markers in the conflicted worktree (rung 1), then a human approves the
// resolution (rung 2). Approve commits the resolved tree (concluding the in-progress
// merge) as the result; reject fails (the worktree is reclaimed at run-end). The
// conflicted worktree arrives mid-merge (MERGE_HEAD set) and is resolved in place —
// it is never aborted or re-merged.
func (e *Engine) resolveConflictEscalation(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact, workDir string, conflict *join.ConflictError, attemptNum int) (core.Result, error) {
	next := attemptNum + 1
	// Frame the resolution as a fresh attempt (also closes the missing-step.started gap).
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepRunning, next, workDir, core.Result{}, nil),
		event.Event{StepID: s.ID, Kind: event.StepStarted, Attempt: next})

	// Rung 1: arbiter resolves the conflict markers in place.
	ares, err := e.runAgent(ctx, runID, s.ID, "arbiter", s.Join.Agent, join.ResolveConflictPrompt(conflict.Paths), workDir, next, inputs, "")
	if err != nil {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, next, workDir, core.Result{}, err),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: next, Err: err.Error()})
		return core.Result{}, err
	}
	// A botched resolution (markers still present) fails before the human reviews it,
	// so we never ask for approval of — or commit — an unresolved merge.
	if rerr := join.EnsureResolved(ctx, workDir); rerr != nil {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, next, workDir, core.Result{}, rerr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: next, Err: rerr.Error()})
		return core.Result{}, rerr
	}

	// Rung 2: human reviews the resolution.
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepAwaitingGate, next, workDir, core.Result{}, conflict),
		event.Event{StepID: s.ID, Kind: event.GateAwaiting, Attempt: next, Err: conflict.Error()})
	e.Metrics.GateAwaited()
	ok, err := e.Gate.Escalate(ctx, runID, s, core.Result{})
	if err != nil {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, next, workDir, core.Result{}, err),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: next, Err: err.Error()})
		return core.Result{}, err
	}
	if !ok {
		rej := fmt.Errorf("escalated merge conflict rejected")
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, next, workDir, core.Result{}, rej),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: next, Err: rej.Error()})
		return core.Result{}, rej
	}

	// Approved: finalize the resolved worktree, concluding the in-progress merge of
	// THIS conflicting branch. Branches after it in the merge order are not yet merged.
	if _, _, cerr := e.WS.Commit(ctx, runID, s, workDir); cerr != nil {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, next, workDir, core.Result{}, cerr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: next, Err: cerr.Error()})
		return core.Result{}, cerr
	}

	// Resume merging the remaining branches. Merge.Join is idempotent — git merge of
	// an already-merged branch is a clean no-op — so re-running it continues from the
	// first unmerged branch. This terminates because each escalation commits at least
	// one more branch, draining the finite branch set. The arbiter's resolution cost
	// rides on a one-artifact carry result so a clean resume (which has no arbiter)
	// still accrues it; a further conflict re-enters resolveConflictEscalation, which
	// re-carries the next arbiter's cost on top.
	resume := attemptNum + 2
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepRunning, resume, workDir, core.Result{}, nil),
		event.Event{StepID: s.ID, Kind: event.StepStarted, Attempt: resume})
	// gateFailed is intentionally ignored: a resumed join's gate failure is terminal
	// (no nested gate escalation), and a fresh conflict surfaces as a *ConflictError.
	res, _, execErr := e.attempt(ctx, runID, s, inputs, resume, workDir, "")
	if execErr != nil {
		// A fresh conflict on a LATER branch re-enters the escalation ladder so it is
		// resolved too; any other failure (incl. a gate verdict on the resumed join)
		// is terminal. escalateJoin persists the terminal disposition itself.
		var conflict *join.ConflictError
		if errors.As(execErr, &conflict) {
			rres, rerr := e.escalateJoin(ctx, runID, s, inputs, workDir, execErr, resume)
			rres.CostUSD += ares.CostUSD // accumulate this rung's arbiter cost across the ladder
			return rres, rerr
		}
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, resume, workDir, core.Result{}, execErr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: resume, Err: execErr.Error()})
		return core.Result{}, execErr
	}
	res.StepID = s.ID
	res.Summary = "merge conflict resolved (arbiter + human)"
	res.CostUSD += ares.CostUSD // carry the arbiter's resolution cost on top of the resumed merge's
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepSucceeded, resume, workDir, res, nil),
		event.Event{StepID: s.ID, Kind: event.StepDone, Summary: res.Summary, CostUSD: res.CostUSD, Attempt: resume})
	return res, nil
}

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func (e *Engine) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return discardLogger
}
