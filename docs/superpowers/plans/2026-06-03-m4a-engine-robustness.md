# M4 Slice A: Engine Robustness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the engine's attempt loop correct and complete — jittered/capped backoff, real `on_fail: escalate`, a per-step timeout that bounds the auto-verifier, and the resume-staleness 409 fix on both server and client.

**Architecture:** All changes live in the existing engine/gate/supervisor/cm layers behind their current interfaces. The attempt loop in `engine.runStep` is refactored to extract a single `attempt` (execute + gate) helper so the timeout can bound the automated work while leaving human approval un-timed; `on_fail: escalate` reuses the manual-gate `ApprovalRegistry` block-on-channel path. No migration, no new YAML fields, no new external dependencies.

**Tech Stack:** Go 1.22, `math/rand/v2` (stdlib), `log/slog`, `context`, the in-repo `core.Store`/`gate.Evaluator`/`supervisor` ports. Tests: standard `testing` with the existing `fakeClock`/`blockingApprover`/`httptest` patterns.

**Spec:** `docs/superpowers/specs/2026-06-03-m4a-engine-robustness-design.md`

**Risky units (flag for opus review during execution):** Task 2 (runStep refactor / timeout scope) and Task 3 (escalate disposition) — they restructure the core attempt loop. Tasks 1, 4, 5, 6 are lower-risk.

**Commit convention (user CLAUDE.md):** single conventional-commit subject, NO body, NO `Co-Authored-By`, never `--no-verify`. Commit with the explicit identity:
```bash
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "<subject>"
```
**Raw test output:** the RTK hook reformats `go test` (e.g. "Go test: N passed"). For per-test PASS/FAIL or flake hunts use `rtk proxy go test ...`.

---

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/engine/engine.go` | attempt loop, backoff, timeout, escalate | modify: add `Rand` field + `randFloat`/`maxBackoff`; jitter+cap in `backoff`; extract `attempt`; move timeout to bound execute+auto-gate; add `escalate`/`gateEscalates`; escalate branch in `runStep` |
| `internal/engine/engine_test.go` | engine unit tests | add: `recordingClock`, `rejectApprover`, backoff/timeout/escalate tests |
| `internal/gate/gate.go` | gate evaluation | modify: add `Evaluator.Escalate` |
| `internal/gate/verifier.go` | auto-gate command runner | modify: surface ctx timeout/cancel as an error (not a verdict) |
| `internal/gate/gate_test.go` | gate unit tests | add: `TestEscalateUsesApprover` |
| `internal/flow/flow.go` | schema | modify: doc-comment `FailPolicy` (retry≡abort; escalate=auto-only) |
| `internal/supervisor/supervisor.go` | run lifecycle / resume | modify: add `Log` + `logger()`; `resetIncompleteSteps`; `ResumeAll` reset + log-and-continue |
| `internal/supervisor/supervisor_test.go` | supervisor unit tests | add: reset + log-and-continue tests |
| `cmd/magisterd/main.go` | daemon wiring | modify: set `sup.Log = log` |
| `cmd/cm/main.go` | CLI client | modify: `approve` retries on 409 |
| `cmd/cm/main_test.go` | CLI unit tests | add: `TestApproveRetriesOn409` |
| `cmd/magisterd/e2e_test.go` | end-to-end | add: `TestE2EEscalateBlocksThenApprove` |

---

## Task 1: Jittered backoff with a max-backoff cap

**Files:**
- Modify: `internal/engine/engine.go` (the `Engine` struct ~27-37; `backoff` ~298-312)
- Test: `internal/engine/engine_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/engine/engine_test.go`:

```go
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

	for _, attempt := range []int{2, 3, 4, 200} {
		if !e.backoff(context.Background(), s, attempt) {
			t.Fatalf("backoff(attempt=%d) returned false", attempt)
		}
	}
	// attempt 2 → 1s, 3 → 2s, 4 → 4s (all ×0.5); attempt 200 overflows the shift
	// and clamps to maxBackoff=30s (×0.5 = 15s).
	want := []time.Duration{
		500 * time.Millisecond,
		1 * time.Second,
		2 * time.Second,
		15 * time.Second,
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `rtk proxy go test ./internal/engine/ -run TestBackoffJitterAndCap -v`
Expected: FAIL — `Engine` has no field `Rand` (compile error), or (if you add the field first) durations are un-jittered/uncapped.

- [ ] **Step 3: Implement the field, helper, cap, and jitter**

In `internal/engine/engine.go`, add the import:

```go
	"math/rand/v2"
```

Add a field to the `Engine` struct (after `Clock core.Clock`):

```go
	// Rand returns a jitter factor in [0,1); nil defaults to math/rand/v2 (auto-seeded).
	// Injected so backoff is deterministic in tests, mirroring Clock.
	Rand func() float64
```

Add near `backoff`:

```go
// maxBackoff caps exponential backoff before jitter, so a high retry count can't
// schedule an unbounded sleep.
const maxBackoff = 30 * time.Second

func (e *Engine) randFloat() float64 {
	if e.Rand != nil {
		return e.Rand()
	}
	return rand.Float64()
}
```

Replace the body of `backoff` with the capped, jittered version:

```go
// backoff sleeps before a retry using the injected clock. Returns false if the
// context was canceled while waiting. Exponential, clamped to maxBackoff, with
// full jitter (sleep ∈ [0, ceiling)) to spread concurrent retries.
func (e *Engine) backoff(ctx context.Context, s *flow.Step, attempt int) bool {
	if s.Retry == nil || s.Retry.Backoff <= 0 {
		return true
	}
	base := time.Duration(s.Retry.Backoff)
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
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `rtk proxy go test ./internal/engine/ -run TestBackoffJitterAndCap -v`
Expected: PASS

Then the whole engine package (the existing retry test uses `fakeClock`, which ignores the jittered duration, so it stays green):
Run: `rtk proxy go test ./internal/engine/`
Expected: ok

- [ ] **Step 5: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(engine): add jittered backoff with max-backoff cap"
```

---

## Task 2: Bound the auto-gate verifier by the per-step timeout

**Files:**
- Modify: `internal/engine/engine.go` (`runStep` ~213-267, `execute` ~271-296)
- Modify: `internal/gate/verifier.go` (`Verify` ~24-39)
- Test: `internal/engine/engine_test.go`

**Why:** Today only the executor call is timeout-bounded; a hung verifier (`go test` that never exits) runs forever. We move the per-attempt timeout into a new `attempt` helper that bounds **execute + an auto gate's verifier**, while manual/escalate approval runs on the un-timed run context. A timeout becomes a retryable error (consumes an attempt under the unified budget).

- [ ] **Step 1: Write the failing test (verifier timeout) + the refactor guard (manual gate survives)**

Add to `internal/engine/engine_test.go`:

```go
// rejectApprover always rejects, to drive the escalate/reject and abort paths.
type rejectApprover struct{}

func (rejectApprover) Approve(context.Context, core.RunID, *flow.Step, core.Result) (bool, error) {
	return false, nil
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
```

- [ ] **Step 2: Run the tests to verify the new one fails**

Run: `rtk proxy go test ./internal/engine/ -run 'TestTimeoutBoundsVerifier|TestTimeoutDoesNotBoundManualGate' -v`
Expected: `TestTimeoutBoundsVerifier` FAILS — current code runs the verifier on the un-timed run context, so `sleep 2` completes (gate passes, step succeeds) and the run takes ~2s. `TestTimeoutDoesNotBoundManualGate` already PASSES (it is a guard).

- [ ] **Step 3: Make the verifier surface a timeout as an error**

In `internal/gate/verifier.go`, replace the `if err := cmd.Run(); err != nil { ... }` block in `Verify`:

```go
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			// Killed by the step timeout / cancellation — an infra error, not a
			// gate verdict. The engine treats it as a retryable failure.
			return false, ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return false, nil // non-zero exit = check failed, not an infra error
		}
		return false, err
	}
	return true, nil
```

- [ ] **Step 4: Extract `attempt`, move the timeout into it, and simplify `execute`**

In `internal/engine/engine.go`, add a timeout helper near `backoff`:

```go
// withTimeout returns a child context bounded by d, or the parent + a no-op cancel
// when d is unset. The no-op keeps callers' `defer cancel()` unconditional.
func withTimeout(ctx context.Context, d flow.Duration) (context.Context, context.CancelFunc) {
	if d > 0 {
		return context.WithTimeout(ctx, time.Duration(d))
	}
	return ctx, func() {}
}
```

Add the `attempt` method (one execute + gate; bounds the automated work; reports whether the failure was a gate verdict):

```go
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
```

Replace `runStep`'s body (keep its signature) so it calls `attempt`. NOTE: in this task `gateFailed` is unused (assigned to `_`); Task 3 wires it in:

```go
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
```

Simplify `execute` — remove its own timeout block (the timeout now lives in `attempt`):

```go
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
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `rtk proxy go test ./internal/engine/ ./internal/gate/ -v`
Expected: PASS for `TestTimeoutBoundsVerifier`, `TestTimeoutDoesNotBoundManualGate`, and all existing engine/gate tests (`TestEngineFanOutFanIn`, `TestEngineRetryThenSucceed`, `TestEngineEmitsAwaitingGateAndBlocks`, etc.).

- [ ] **Step 6: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go internal/gate/verifier.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(engine): bound auto-gate verifier by per-step timeout"
```

---

## Task 3: Implement `on_fail: escalate`

**Files:**
- Modify: `internal/gate/gate.go` (add `Escalate`)
- Modify: `internal/engine/engine.go` (`runStep` terminal branch; add `escalate` + `gateEscalates`)
- Modify: `internal/flow/flow.go` (`FailPolicy` doc-comment)
- Test: `internal/gate/gate_test.go`, `internal/engine/engine_test.go`

**Why:** `on_fail` is validated but never consumed. Under the unified budget, `escalate` is the terminal disposition for an exhausted AUTO gate: convert the failed gate into a human approval (reusing the manual-gate registry). Approve → step succeeds; reject → run aborts.

- [ ] **Step 1: Write the failing test for `Evaluator.Escalate`**

Add to `internal/gate/gate_test.go`:

```go
func TestEscalateUsesApprover(t *testing.T) {
	s := &flow.Step{ID: "a", Gate: flow.Gate{
		Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "false"}, OnFail: flow.FailEscalate}}

	approved := &Evaluator{Approver: fixedApprover(true), Verifier: CommandVerifier{}}
	if ok, err := approved.Escalate(context.Background(), "r1", s, core.Result{}); err != nil || !ok {
		t.Fatalf("approve path: ok=%v err=%v, want true/nil", ok, err)
	}
	rejected := &Evaluator{Approver: fixedApprover(false), Verifier: CommandVerifier{}}
	if ok, _ := rejected.Escalate(context.Background(), "r1", s, core.Result{}); ok {
		t.Fatal("reject path: ok=true, want false")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `rtk proxy go test ./internal/gate/ -run TestEscalateUsesApprover -v`
Expected: FAIL — `e.Escalate` undefined (compile error).

- [ ] **Step 3: Add `Evaluator.Escalate`**

In `internal/gate/gate.go`, add after `Evaluate`:

```go
// Escalate turns a failed AUTO gate into a human approval, reusing the same Approver
// path as a manual gate (the engine calls this when on_fail=escalate and the attempt
// budget is spent). approve → the step's existing result stands; reject → the run aborts.
func (e *Evaluator) Escalate(ctx context.Context, runID core.RunID, s *flow.Step, res core.Result) (bool, error) {
	return e.Approver.Approve(ctx, runID, s, res)
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `rtk proxy go test ./internal/gate/ -run TestEscalateUsesApprover -v`
Expected: PASS

- [ ] **Step 5: Write the failing engine escalate tests**

Add to `internal/engine/engine_test.go`:

```go
func TestEscalateApproveSucceeds(t *testing.T) {
	st := store.NewMem()
	bus := event.NewBus()
	ch, unsub := bus.Subscribe(32)
	defer unsub()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		// AutoApprover auto-approves the escalation.
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: fakeClock{},
	}
	// Failing auto verifier + escalate, no retry: the single attempt fails the gate
	// → escalate → (auto-)approved → step succeeds.
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
	// A gate.awaiting carrying the failure reason must have been emitted.
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
	// escalate is auto-only: a manual gate with on_fail=escalate that is rejected
	// must abort, not escalate (which would loop a human onto a human).
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: rejectApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
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
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `rtk proxy go test ./internal/engine/ -run 'TestEscalate|TestManualGateRejectDoesNotEscalate' -v`
Expected: FAIL — current `runStep` ignores `on_fail`, so the escalate steps end `failed` (approve case wrongly fails; the reject/manual cases pass by luck). The compile is fine; assertions on the approve case fail.

- [ ] **Step 7: Wire escalation into `runStep`**

In `internal/engine/engine.go`, change the `attempt` call in `runStep` to capture `gateFailed`, and add the escalate branch at the terminal point. Replace:

```go
		res, _, execErr := e.attempt(ctx, runID, s, inputs, attempt, workDir)
```
with:
```go
		res, gateFailed, execErr := e.attempt(ctx, runID, s, inputs, attempt, workDir)
```

and replace the terminal failure block:

```go
		if attempt < maxAttempts && s.Retry != nil {
			continue // retry (covers both execution and gate failures)
		}
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attempt, workDir, core.Result{}, lastErr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attempt, Err: lastErr.Error()})
		return core.Result{}, lastErr
```
with:
```go
		if attempt < maxAttempts && s.Retry != nil {
			continue // retry (covers both execution and gate failures)
		}
		// budget spent — terminal disposition. A failed AUTO gate with on_fail=escalate
		// becomes a human approval; everything else fails the run.
		if gateFailed && gateEscalates(s) {
			return e.escalate(ctx, runID, s, res, workDir, lastErr, attempt)
		}
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attempt, workDir, core.Result{}, lastErr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attempt, Err: lastErr.Error()})
		return core.Result{}, lastErr
```

Add `gateEscalates` and `escalate` near `gateBlocks`:

```go
// gateEscalates reports whether a failed gate should be escalated to a human.
// Escalation is auto-only: a manual gate's rejection is itself a human decision.
func gateEscalates(s *flow.Step) bool {
	return gatePolicyOf(s) == flow.GateAuto && s.Gate.OnFail == flow.FailEscalate
}

// escalate converts a failed auto gate into a human approval, reusing the manual
// block-on-channel path. The failure reason rides on the gate.awaiting event's Err.
func (e *Engine) escalate(ctx context.Context, runID core.RunID, s *flow.Step, res core.Result, workDir string, gateErr error, attemptNum int) (core.Result, error) {
	res.StepID = s.ID
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepAwaitingGate, attemptNum, workDir, res, gateErr),
		event.Event{StepID: s.ID, Kind: event.GateAwaiting, Attempt: attemptNum, Err: gateErr.Error()})

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
```

- [ ] **Step 8: Document the `FailPolicy` semantics**

In `internal/flow/flow.go`, replace the comment above `type FailPolicy string`:

```go
// FailPolicy controls what happens when a gate fails.
//
// Under the unified attempt budget (engine.runStep), a Retry policy already re-runs
// the whole attempt (execute + gate) on any failure, so:
//   - abort (default): fail the run once the budget is spent.
//   - retry: an explicit synonym for the default — behaviourally identical to abort
//     (the validator still requires a Retry policy with it); kept to document intent.
//   - escalate: when an AUTO gate's budget is spent, convert the failed gate into a
//     human approval instead of failing — approve continues, reject aborts. No-op for
//     manual gates, where a rejection is already a human decision.
```

- [ ] **Step 9: Run the tests to verify everything passes**

Run: `rtk proxy go test ./internal/engine/ ./internal/gate/ ./internal/flow/ -v`
Expected: PASS for all escalate tests and all existing tests.

- [ ] **Step 10: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go internal/gate/gate.go internal/gate/gate_test.go internal/flow/flow.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(engine): implement on_fail=escalate via human approval"
```

---

## Task 4: Reset non-succeeded steps to pending on resume (+ log-and-continue)

**Files:**
- Modify: `internal/supervisor/supervisor.go` (add `Log` + `logger()`; `resetIncompleteSteps`; `ResumeAll`)
- Modify: `cmd/magisterd/main.go` (set `sup.Log = log`)
- Test: `internal/supervisor/supervisor_test.go`

**Why:** On resume the engine re-runs every non-succeeded step from scratch, but their pre-crash status lingers in the store — a watcher sees a stale `awaiting_gate` and fires a premature approve → 409. Reset every non-succeeded step to `pending` so the visible status is honest. Also fix M3 follow-up #2: `ResumeAll` currently aborts on the first corrupt `FlowYAML`, stranding later runs.

- [ ] **Step 1: Write the failing tests**

Add to `internal/supervisor/supervisor_test.go`:

```go
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
```

Both `waitFor(t, cond func() bool)` and `waitForStatus(t, st, id, want)` already exist in the supervisor test package (they back `TestSupervisorSubmitRunsToCompletion`). Use them directly — do NOT redefine them.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `rtk proxy go test ./internal/supervisor/ -run 'TestResetIncompleteSteps|TestResumeAllContinuesPastCorruptFlow' -v`
Expected: FAIL — `sup.resetIncompleteSteps` undefined (compile error); and `ResumeAll` currently returns an error on the corrupt `r1`.

- [ ] **Step 3: Add the logger, reset method, and ResumeAll changes**

In `internal/supervisor/supervisor.go`, add imports:

```go
	"io"
	"log/slog"
```

Add a field to the `Supervisor` struct (after `reg *ApprovalRegistry`):

```go
	// Log records non-fatal resume issues; nil = discard. The daemon wires a real one.
	Log *slog.Logger
```

Add the nil-safe logger + the reset method (near `ResumeAll`):

```go
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func (s *Supervisor) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return discardLogger
}

// resetIncompleteSteps marks every non-succeeded step of a resumed run as pending,
// so observers don't see a stale actionable status (e.g. awaiting_gate) before the
// engine re-runs the step. Succeeded steps are left intact — they seed downstream
// inputs (spec §7). Startup reconciliation, so no event is emitted.
func (s *Supervisor) resetIncompleteSteps(ctx context.Context, rs core.RunState) {
	for _, st := range rs.Steps {
		if st.Status == core.StepSucceeded {
			continue
		}
		reset := core.StepState{RunID: rs.ID, StepID: st.StepID, Status: core.StepPending}
		if err := s.store.SaveStepTransition(ctx, reset, nil); err != nil {
			// Non-fatal: the engine re-runs the step regardless; only the visible
			// status stays stale. Log and continue.
			s.logger().Error("resume: reset step to pending", "run", rs.ID, "step", st.StepID, "err", err)
		}
	}
}
```

Replace `ResumeAll`'s loop body so it logs-and-continues and resets before starting:

```go
// ResumeAll loads incomplete runs from the store and resumes each (startup). A run
// with an unparseable/invalid flow is skipped (logged), not fatal to the others.
func (s *Supervisor) ResumeAll(ctx context.Context) error {
	runs, err := s.store.LoadIncompleteRuns(ctx)
	if err != nil {
		return fmt.Errorf("load incomplete runs: %w", err)
	}
	for _, rs := range runs {
		f, err := flow.ParseBytes([]byte(rs.FlowYAML))
		if err != nil {
			s.logger().Error("resume: skip run with unparseable flow", "run", rs.ID, "err", err)
			continue
		}
		if err := flow.Validate(f); err != nil {
			s.logger().Error("resume: skip run with invalid flow", "run", rs.ID, "err", err)
			continue
		}
		s.resetIncompleteSteps(ctx, rs)
		rs := rs
		s.start(rs.ID, func(runCtx context.Context) error { return s.engine.Resume(runCtx, rs, f) })
	}
	return nil
}
```

- [ ] **Step 4: Wire the daemon logger**

In `cmd/magisterd/main.go`, after `sup := supervisor.New(eng, st, reg)`:

```go
	sup.Log = log
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `rtk proxy go test ./internal/supervisor/ ./cmd/magisterd/ -v`
Expected: PASS for the new tests and all existing supervisor/daemon tests.

- [ ] **Step 6: Commit**

```bash
git add internal/supervisor/supervisor.go internal/supervisor/supervisor_test.go cmd/magisterd/main.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "fix(supervisor): reset non-succeeded steps to pending on resume"
```

---

## Task 5: `cm approve`/`reject` retries on 409

**Files:**
- Modify: `cmd/cm/main.go` (`approve` ~119-141; imports)
- Test: `cmd/cm/main_test.go`

**Why:** After a resume, the gate re-registers slightly after the run resumes, so an approve can race ahead → transient 409 ("no gate awaiting yet"). The correct client behaviour is to retry for a bounded window (the e2e helper already proves it). Other statuses return immediately.

- [ ] **Step 1: Write the failing test**

Add to `cmd/cm/main_test.go` the imports `"sync/atomic"` and `"time"`, then:

```go
func TestApproveRetriesOn409(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusConflict)
			writeBody(w, `{"error":"no gate awaiting approval for this step"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		writeBody(w, `{"status":"resolved"}`)
	}))
	defer srv.Close()

	old := approveRetryEvery
	approveRetryEvery = time.Millisecond
	defer func() { approveRetryEvery = old }()

	var out bytes.Buffer
	code := dispatch([]string{"approve", "01ABC", "stepA"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out = %q", code, out.String())
	}
	if n := atomic.LoadInt32(&calls); n < 3 {
		t.Fatalf("expected ≥3 attempts (retried past 409), got %d", n)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `rtk proxy go test ./cmd/cm/ -run TestApproveRetriesOn409 -v`
Expected: FAIL — `approveRetryEvery` undefined (compile error); current `approve` does a single POST and returns 1 on the first 409.

- [ ] **Step 3: Add the retry loop**

In `cmd/cm/main.go`, add `"time"` to the imports, and add package-level tunables (above `main`):

```go
// approve retry window for the transient 409 a resumed run briefly returns before
// its gate re-registers. Package vars so tests can shrink the interval.
var (
	approveRetryFor   = 10 * time.Second
	approveRetryEvery = 100 * time.Millisecond
)
```

Replace the request/response part of `approve` (from the `body, _ := json.Marshal(...)` line to the end of the function) with:

```go
	body, _ := json.Marshal(map[string]any{"approve": approve, "reason": reason})
	url := c.base + "/v1/runs/" + args[0] + "/steps/" + args[1] + "/approve"

	deadline := time.Now().Add(approveRetryFor)
	for {
		resp, err := c.http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			fmt.Fprintln(out, "approve:", err)
			return 1
		}
		if resp.StatusCode == http.StatusConflict && time.Now().Before(deadline) {
			resp.Body.Close()
			time.Sleep(approveRetryEvery)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			code := printErr(resp, out)
			resp.Body.Close()
			return code
		}
		resp.Body.Close()
		fmt.Fprintln(out, "ok")
		return 0
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `rtk proxy go test ./cmd/cm/ -v`
Expected: PASS for `TestApproveRetriesOn409`, `TestApproveSendsApproveTrue` (single 200, no retry), and the rest.

- [ ] **Step 5: Commit**

```bash
git add cmd/cm/main.go cmd/cm/main_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(cm): retry approve/reject on 409 while gate re-registers"
```

---

## Task 6: End-to-end escalate coverage + stress + final verification

**Files:**
- Test: `cmd/magisterd/e2e_test.go`

**Why:** Prove `on_fail: escalate` works through the real daemon (RegistryApprover + HTTP approve), then stress the resume/escalate/reset paths per M3 discipline.

- [ ] **Step 1: Write the e2e escalate test**

Add to `cmd/magisterd/e2e_test.go`:

```go
func TestE2EEscalateBlocksThenApprove(t *testing.T) {
	base, stop := startDaemon(t, filepath.Join(t.TempDir(), "esc.db"))
	defer stop()
	// Auto gate whose verifier fails + on_fail: escalate, no retry → the gate
	// failure is escalated to a human; approving it completes the run.
	id := postFlow(t, base, "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"false\" }, on_fail: escalate }\n")

	waitStatus(t, base, id, "running")
	waitStepStatus(t, base, id, "a", "awaiting_gate")
	approveStep(t, base, id, "a")
	waitStatus(t, base, id, "succeeded")
}

// TestE2EEscalateKillAndResume covers spec §4.2/§7: an escalated gate re-escalates
// after a crash+resume (no special resume code — re-execution reconstructs it), and
// the resumed step shows pending (reset-to-pending) until it re-reaches the gate.
func TestE2EEscalateKillAndResume(t *testing.T) {
	db := filepath.Join(t.TempDir(), "esc-resume.db")
	const yaml = "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"false\" }, on_fail: escalate }\n"

	// Run until the escalated gate parks at awaiting_gate, then "crash".
	id := crashDaemonAtGate(t, db, yaml, "a")

	// Restart against the same DB → resume re-runs step a, the verifier fails again,
	// and it re-escalates. approveStep retries past any transient 409.
	base, stop := startDaemon(t, db)
	defer stop()
	waitStatus(t, base, id, "running")
	approveStep(t, base, id, "a")
	waitStatus(t, base, id, "succeeded")
}
```

- [ ] **Step 2: Run both to verify they pass**

Run: `rtk proxy go test ./cmd/magisterd/ -run 'TestE2EEscalate' -v`
Expected: PASS for `TestE2EEscalateBlocksThenApprove` (escalate → approve → succeeds) and `TestE2EEscalateKillAndResume` (re-escalation after resume → approve → succeeds).

- [ ] **Step 3: Run the full suite under -race**

Run: `rtk proxy go test -race -count=1 ./...`
Expected: ok for all packages (was 83 tests; now higher with the additions). If any flake appears, do NOT paper over it — debug per superpowers:systematic-debugging.

- [ ] **Step 4: Stress the concurrency/lifecycle paths (M3 Task 15 discipline)**

Run: `GOMAXPROCS=8 rtk proxy go test -race -count=20 ./cmd/magisterd/ ./internal/supervisor/ ./internal/engine/`
Expected: ok across all 20 iterations (shakes out scheduling races in resume/escalate/reset that a single run misses).

- [ ] **Step 5: Vet**

Run: `rtk proxy go vet ./...`
Expected: no issues.

- [ ] **Step 6: Commit**

```bash
git add cmd/magisterd/e2e_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "test(e2e): cover escalate gate end-to-end"
```

---

## Done criteria

- `go test -race ./...` and `go vet ./...` both clean; `GOMAXPROCS=8 ... -count=20` stable.
- `go.mod` still `go 1.22` (no new deps; `math/rand/v2` is stdlib — confirm with `grep '^go ' go.mod`).
- `on_fail: escalate` works through the daemon; jitter+cap deterministic under injected `Rand`; verifier bounded by `step.Timeout`; resumed runs show `pending` (not stale `awaiting_gate`); a corrupt flow row no longer strands later runs; `cm approve` survives the resume race.
- No migration, no new YAML fields.
