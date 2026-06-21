# Engine Debug/Warn Instrumentation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add seven `slog` log points to the engine so `-log-level debug` reveals the engine's internal decisions (agent timing/cost, backoff, slot waits, gate verdicts, joins) plus two default-visible `Warn` lines (retry-budget-exhausted, merge-conflict-detected).

**Architecture:** Pure-additive logging emitted entirely from `internal/engine/engine.go`, which holds the only logger (`e.logger()`, nil-safe). The `gate` and `join` packages stay logger-free; merge conflicts are logged from `execute()` via `errors.As(*join.ConflictError)` (already imported). The one structural change is `backoff` gaining a `runID core.RunID` parameter (one caller). No new SSE event, config, schema, dependency, or control-flow change.

**Tech Stack:** Go 1.22, stdlib `log/slog`. Tests use the existing engine harness (`newEngine`, `newGitEngine`, `flakyExecutor`, `fakeClock`, `fileWriterExec`, `conflictFlow`, `mustCreate`) in `internal/engine/engine_test.go`.

## Global Constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no new package; no migration; no schema change; no new SSE event kind.
- Pure-additive logging: no change to engine control flow, results, events, or the run lifecycle. The only signature change is `backoff` gaining a `runID core.RunID` parameter (one caller).
- Field keys reuse the existing convention: `"run"` = `string(runID)`, `"step"`, `"agent"`, `"err"`. Finish/verdict lines include `err` only when non-nil.
- Debug lines (agent start/finish, backoff, slot acquired, gate evaluated, join start/finish) are invisible at the default `info` level; the two `Warn` lines (retry budget exhausted, merge conflict detected) are deliberately default-visible.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.

---

## File Structure

- **Modify** `internal/engine/engine.go` — all seven log points; `backoff` signature change. Touched functions: `backoff`, `runStep` (caller + retry-exhausted Warn), `runAgent` (agent start/finish), `runDAG` goroutine (slot acquired), `attempt` (gate verdict), `execute` (join start/finish + conflict Warn).
- **Create** `internal/engine/instrumentation_test.go` — new tests for the six Debug/first-Warn points, plus two shared test helpers (`debugLogger`, `hasLine`). Same package (`engine`), so it reuses the harness in `engine_test.go`.
- **Modify** `internal/engine/engine_test.go` — add the point-G assertion to the existing `TestMergeConflictEscalateApproveCommits` (it already stages a real merge conflict); add the `bytes` import.

Three tasks, each an independently reviewable deliverable touching `engine.go` + tests:
- **Task 1 — retry path:** `backoff` signature + backoff log (B) + retry-budget-exhausted Warn (E). Creates the test file with the shared helpers.
- **Task 2 — per-step path:** agent start/finish (A) + slot acquired (C) + gate verdict (D).
- **Task 3 — join path:** join start/finish (F) + merge-conflict-detected Warn (G).

Tasks are sequential and all edit `engine.go`, but touch disjoint functions — no cross-task interface coupling beyond the two helpers Task 1 creates.

---

## Task 1: Retry-path instrumentation (backoff log + retry-exhausted Warn)

**Files:**
- Modify: `internal/engine/engine.go` — `backoff` (signature + log), `runStep` (caller + Warn)
- Create: `internal/engine/instrumentation_test.go` — helpers + `TestRetryBackoffAndExhaustionLogs`

**Interfaces:**
- Consumes: existing harness from `engine_test.go` — `flakyExecutor{failUntil int}`, `fakeClock{}`, `mustCreate`.
- Produces (for Tasks 2 & 3): two helpers in `instrumentation_test.go`:
  - `func debugLogger(buf *bytes.Buffer) *slog.Logger` — a DEBUG-level text logger writing to `buf`.
  - `func hasLine(out string, subs ...string) bool` — true when one line of `out` contains every substring in `subs`.
- Produces: `func (e *Engine) backoff(ctx context.Context, runID core.RunID, s *flow.Step, attempt int) bool` (was `backoff(ctx, s, attempt)`).

- [ ] **Step 1: Write the failing test**

Create `internal/engine/instrumentation_test.go`:

```go
package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/flow"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

// debugLogger returns a slog logger writing DEBUG-and-above to buf, so a test
// can assert on the engine's Debug/Warn instrumentation.
func debugLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// hasLine reports whether some single line of out contains every substring in
// subs — a precise per-line check (avoids matching fields across separate lines).
func hasLine(out string, subs ...string) bool {
	for _, ln := range strings.Split(out, "\n") {
		all := true
		for _, s := range subs {
			if !strings.Contains(ln, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func TestRetryBackoffAndExhaustionLogs(t *testing.T) {
	var buf bytes.Buffer
	st := store.NewMem()
	eng := &Engine{
		Execs: map[string]core.Executor{"flaky": &flakyExecutor{failUntil: 99}}, // fails every attempt
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st,
		Bus:   event.NewBus(),
		Clock: fakeClock{}, // makes backoff instant
		Log:   debugLogger(&buf),
	}
	f := &flow.Flow{Name: "retry", Steps: []*flow.Step{
		{ID: "a", Agent: "flaky", Retry: &flow.RetryPolicy{Max: 2, Backoff: flow.Duration(time.Second)},
			Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	mustCreate(t, st, "r1", f)
	if err := eng.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected the run to fail after exhausting retries")
	}
	out := buf.String()
	if !hasLine(out, "step backoff", "attempt=2", "delay=", "base=") {
		t.Errorf("missing/incomplete backoff Debug line:\n%s", out)
	}
	if !hasLine(out, "level=WARN", "retry budget exhausted", "attempts=2", "escalating=false") {
		t.Errorf("missing retry-budget-exhausted Warn line:\n%s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestRetryBackoffAndExhaustionLogs -v`
Expected: FAIL — the build fails first because the engine does not yet log these lines; once it compiles, the assertions fail (no `step backoff` / `retry budget exhausted` lines). (The signature change in Step 3 is what makes the new code compile.)

- [ ] **Step 3: Change `backoff` signature and add the backoff log**

In `internal/engine/engine.go`, replace the whole `backoff` function:

```go
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
```

with:

```go
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
```

- [ ] **Step 4: Update the single `backoff` caller in `runStep`**

In `internal/engine/engine.go`, in `runStep`, replace:

```go
			if !e.backoff(ctx, s, attempt) {
				return core.Result{}, ctx.Err()
			}
```

with:

```go
			if !e.backoff(ctx, runID, s, attempt) {
				return core.Result{}, ctx.Err()
			}
```

- [ ] **Step 5: Add the retry-budget-exhausted Warn**

In `internal/engine/engine.go`, in `runStep`, the budget-spent block currently reads:

```go
		// budget spent — terminal disposition.
		if s.Join != nil && s.Join.OnConflict == flow.FailEscalate {
			return e.escalateJoin(ctx, runID, s, inputs, workDir, lastErr, attempt)
		}
		// A failed auto/conditional gate with on_fail=escalate becomes a human approval.
		if gateFailed && gateEscalates(s) {
			return e.escalate(ctx, runID, s, res, workDir, lastErr, attempt)
		}
```

Insert the Warn between the join-escalate early-return and the gate-escalate check (so it covers exactly the gate-escalation and terminal-failure dispositions; a conflict-escalate join has already returned and is covered by Task 3's point G):

```go
		// budget spent — terminal disposition.
		if s.Join != nil && s.Join.OnConflict == flow.FailEscalate {
			return e.escalateJoin(ctx, runID, s, inputs, workDir, lastErr, attempt)
		}
		e.logger().Warn("retry budget exhausted", "run", string(runID), "step", s.ID, "attempts", attempt, "last_err", lastErr, "escalating", gateFailed && gateEscalates(s))
		// A failed auto/conditional gate with on_fail=escalate becomes a human approval.
		if gateFailed && gateEscalates(s) {
			return e.escalate(ctx, runID, s, res, workDir, lastErr, attempt)
		}
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./internal/engine/ -run TestRetryBackoffAndExhaustionLogs -v`
Expected: PASS.

- [ ] **Step 7: Run the full suite + gofmt + vet**

Run: `go test -race ./... && go vet ./... && gofmt -l internal/engine/`
Expected: all packages PASS; `go vet` silent; `gofmt -l` prints nothing (no unformatted files).

- [ ] **Step 8: Commit**

```bash
git add internal/engine/engine.go internal/engine/instrumentation_test.go
git commit -m "feat(engine): log backoff delay and retry-budget-exhausted"
```

---

## Task 2: Per-step instrumentation (agent start/finish + slot acquired + gate verdict)

**Files:**
- Modify: `internal/engine/engine.go` — `runAgent` (agent start/finish), `runDAG` goroutine (slot acquired), `attempt` (gate verdict)
- Modify: `internal/engine/instrumentation_test.go` — add `TestNormalStepLogs`; add the `concentus/internal/executor` import

**Interfaces:**
- Consumes: `debugLogger`, `hasLine` (from Task 1); harness `newEngine`, `mustCreate` (from `engine_test.go`); `executor.Mock`.
- Produces: no new exported names — three Debug log lines (`agent starting`, `agent finished`, `step slot acquired`, `gate evaluated`).

- [ ] **Step 1: Write the failing test**

Add the `executor` import to `internal/engine/instrumentation_test.go`'s import block (insert `"concentus/internal/executor"` in alphabetical position, after `"concentus/internal/event"`), then append this test:

```go
func TestNormalStepLogs(t *testing.T) {
	var buf bytes.Buffer
	eng, st, _ := newEngine(t, map[string]core.Executor{"mock": executor.Mock{Name: "mock"}}, nil)
	eng.Log = debugLogger(&buf)
	f := &flow.Flow{Name: "one", Steps: []*flow.Step{
		{ID: "greet", Agent: "mock", Role: "implementer",
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
	}}
	mustCreate(t, st, "r1", f)
	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	if !hasLine(out, "agent starting", "agent=mock", "role=implementer", "attempt=1") {
		t.Errorf("missing agent-starting line:\n%s", out)
	}
	if !hasLine(out, "agent finished", "agent=mock", "dur=", "cost_usd=") {
		t.Errorf("missing agent-finished line:\n%s", out)
	}
	if !hasLine(out, "step slot acquired", "step=greet", "waited=") {
		t.Errorf("missing slot-acquired line:\n%s", out)
	}
	if !hasLine(out, "gate evaluated", "step=greet", "policy=auto", "pass=true") {
		t.Errorf("missing gate-evaluated line:\n%s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestNormalStepLogs -v`
Expected: FAIL — none of the four lines are emitted yet.

- [ ] **Step 3: Add agent start/finish logs in `runAgent`**

In `internal/engine/engine.go`, in `runAgent`, replace:

```go
	agentCtx := logctx.With(ctx, e.logger().With("run", string(runID), "step", stepID, "agent", agentName))
	agentStart := e.Clock.Now()
	res, err := ag.Run(agentCtx, core.Task{
		RunID:   runID,
		StepID:  stepID,
		Role:    role,
		Prompt:  prompt,
		Inputs:  inputs,
		WorkDir: workDir,
		Emit:    emit,
	})
	e.Metrics.ObserveAgentRun(agentName, e.Clock.Now().Sub(agentStart)) // every invocation, incl. errors
	e.Metrics.AddCost(agentName, res.CostUSD)                           // per-invocation; no-op on 0 cost
	return res, err
```

with:

```go
	agentCtx := logctx.With(ctx, e.logger().With("run", string(runID), "step", stepID, "agent", agentName))
	e.logger().Debug("agent starting", "run", string(runID), "step", stepID, "agent", agentName, "role", role, "attempt", attemptNum)
	agentStart := e.Clock.Now()
	res, err := ag.Run(agentCtx, core.Task{
		RunID:   runID,
		StepID:  stepID,
		Role:    role,
		Prompt:  prompt,
		Inputs:  inputs,
		WorkDir: workDir,
		Emit:    emit,
	})
	dur := e.Clock.Now().Sub(agentStart)
	e.Metrics.ObserveAgentRun(agentName, dur) // every invocation, incl. errors
	e.Metrics.AddCost(agentName, res.CostUSD) // per-invocation; no-op on 0 cost
	args := []any{"run", string(runID), "step", stepID, "agent", agentName, "attempt", attemptNum, "dur", dur, "cost_usd", res.CostUSD}
	if err != nil {
		args = append(args, "err", err)
	}
	e.logger().Debug("agent finished", args...)
	return res, err
```

(The duration is hoisted into `dur` so the metric and the log share one `Clock.Now()` read.)

- [ ] **Step 4: Add the slot-acquired log in the `runDAG` goroutine**

In `internal/engine/engine.go`, in the `runDAG` per-step goroutine, replace:

```go
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
			e.Metrics.StepStarted()
```

with:

```go
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
			e.logger().Debug("step slot acquired", "run", string(runID), "step", s.ID, "waited", e.Clock.Now().Sub(queueStart))
			e.Metrics.StepStarted()
```

- [ ] **Step 5: Add the gate-verdict log in `attempt`**

In `internal/engine/engine.go`, in `attempt`, replace:

```go
	ok, gerr := e.Gate.Evaluate(gateCtx, runID, s, res, workDir)
	switch {
```

with:

```go
	ok, gerr := e.Gate.Evaluate(gateCtx, runID, s, res, workDir)
	gargs := []any{"run", string(runID), "step", s.ID, "attempt", attemptNum, "policy", gatePolicyOf(s), "pass", ok}
	if gerr != nil {
		gargs = append(gargs, "err", gerr)
	}
	e.logger().Debug("gate evaluated", gargs...)
	switch {
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./internal/engine/ -run TestNormalStepLogs -v`
Expected: PASS.

- [ ] **Step 7: Run the full suite + gofmt + vet**

Run: `go test -race ./... && go vet ./... && gofmt -l internal/engine/`
Expected: all packages PASS (including `TestRunAgentInjectsScopedLogger`, whose INFO-level handler suppresses the new Debug lines); `go vet` silent; `gofmt -l` prints nothing.

- [ ] **Step 8: Commit**

```bash
git add internal/engine/engine.go internal/engine/instrumentation_test.go
git commit -m "feat(engine): log agent timing, slot waits, and gate verdicts"
```

---

## Task 3: Join-path instrumentation (join start/finish + merge-conflict Warn)

**Files:**
- Modify: `internal/engine/engine.go` — `execute` (join branch: start/finish logs + conflict Warn)
- Modify: `internal/engine/instrumentation_test.go` — add `TestJoinStartFinishLogs`
- Modify: `internal/engine/engine_test.go` — add the point-G assertion to `TestMergeConflictEscalateApproveCommits`; add the `bytes` import

**Interfaces:**
- Consumes: `debugLogger`, `hasLine` (from Task 1); harness `newGitEngine`, `fileWriterExec`, `conflictFlow`, `mustCreate` (from `engine_test.go`); `*join.ConflictError{Branch string, Paths []string}` (already imported in `engine.go`).
- Produces: no new exported names — Debug `join starting`/`join finished` and Warn `merge conflict detected`.

- [ ] **Step 1: Write the failing tests**

(1a) Append the clean-merge test (points F) to `internal/engine/instrumentation_test.go` (no import change — `fileWriterExec`/`newGitEngine` are in-package):

```go
func TestJoinStartFinishLogs(t *testing.T) {
	var buf bytes.Buffer
	execs := map[string]core.Executor{
		"a": fileWriterExec{file: "a.md", body: "A"},
		"b": fileWriterExec{file: "b.md", body: "B"}, // different files → clean merge, no conflict
	}
	eng, st := newGitEngine(t, execs)
	eng.Log = debugLogger(&buf)
	f := &flow.Flow{Name: "merge", Steps: []*flow.Step{
		{ID: "a", Agent: "a", Workspace: flow.WSIsolated, Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
		{ID: "b", Agent: "b", Workspace: flow.WSIsolated, Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
		{ID: "integrate", Needs: []string{"a", "b"}, Workspace: flow.WSIsolated,
			Join: &flow.Join{Strategy: flow.JoinMerge}},
	}}
	mustCreate(t, st, "r1", f)
	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	if !hasLine(out, "join starting", "step=integrate", "strategy=merge", "inputs=") {
		t.Errorf("missing join-starting line:\n%s", out)
	}
	if !hasLine(out, "join finished", "step=integrate", "strategy=merge") {
		t.Errorf("missing join-finished line:\n%s", out)
	}
}
```

(1b) In `internal/engine/engine_test.go`, add `"bytes"` to the import block (first entry, before `"context"`), then modify `TestMergeConflictEscalateApproveCommits`. Replace:

```go
	eng, st := newGitEngine(t, execs)
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r1", Name: "f", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(context.Background(), "r1", conflictFlow(flow.FailEscalate)); err != nil {
		t.Fatalf("run should succeed after approve: %v", err)
	}
```

with:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/engine/ -run 'TestJoinStartFinishLogs|TestMergeConflictEscalateApproveCommits' -v`
Expected: FAIL — neither the join-start/finish lines nor the conflict Warn are emitted yet. (Both skip if `git` is not on PATH via `newGitEngine`.)

- [ ] **Step 3: Add join start/finish logs and the conflict Warn in `execute`**

In `internal/engine/engine.go`, in `execute`, replace the join branch:

```go
	if s.Join != nil {
		strat, ok := e.Joins[s.Join.Strategy]
		if !ok {
			return core.Result{}, fmt.Errorf("join strategy %q not implemented yet", s.Join.Strategy)
		}
		run := func(ctx context.Context, agentName, prompt, wd string, in []core.Artifact) (core.Result, error) {
			return e.runAgent(ctx, runID, s.ID, "arbiter", agentName, prompt, wd, attemptNum, in)
		}
		return strat.Join(ctx, s, inputs, workDir, run)
	}
```

with:

```go
	if s.Join != nil {
		strat, ok := e.Joins[s.Join.Strategy]
		if !ok {
			return core.Result{}, fmt.Errorf("join strategy %q not implemented yet", s.Join.Strategy)
		}
		run := func(ctx context.Context, agentName, prompt, wd string, in []core.Artifact) (core.Result, error) {
			return e.runAgent(ctx, runID, s.ID, "arbiter", agentName, prompt, wd, attemptNum, in)
		}
		e.logger().Debug("join starting", "run", string(runID), "step", s.ID, "strategy", s.Join.Strategy, "inputs", len(inputs), "attempt", attemptNum)
		res, err := strat.Join(ctx, s, inputs, workDir, run)
		jargs := []any{"run", string(runID), "step", s.ID, "strategy", s.Join.Strategy, "attempt", attemptNum}
		if err != nil {
			jargs = append(jargs, "err", err)
		}
		e.logger().Debug("join finished", jargs...)
		var conflict *join.ConflictError
		if errors.As(err, &conflict) {
			e.logger().Warn("merge conflict detected", "run", string(runID), "step", s.ID, "branch", conflict.Branch, "paths", conflict.Paths, "attempt", attemptNum)
		}
		return res, err
	}
```

(`errors` and `join` are already imported in `engine.go`; no import change.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/engine/ -run 'TestJoinStartFinishLogs|TestMergeConflictEscalateApproveCommits' -v`
Expected: PASS (or SKIP if `git` is unavailable).

- [ ] **Step 5: Run the full suite + gofmt + vet**

Run: `go test -race ./... && go vet ./... && gofmt -l internal/engine/`
Expected: all packages PASS; `go vet` silent; `gofmt -l` prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/engine.go internal/engine/instrumentation_test.go internal/engine/engine_test.go
git commit -m "feat(engine): log join execution and merge-conflict detection"
```

---

## Self-Review (completed by plan author)

**1. Spec coverage:** All seven points map to tasks — A/C/D → Task 2, B/E → Task 1, F/G → Task 3. The `backoff` signature change (spec §B) → Task 1 Steps 3-4. Engine-boundary approach (no gate/join logger) → conflict logged in `execute` via `errors.As` (Task 3). Default-visibility of E/G `Warn` → both are `.Warn(...)`. Testing strategy (Debug buffer logger, representative flows, G in existing conflict test) → all three tasks' tests. Out-of-scope items (dep-wait lines, gate/join loggers, new events) → not present.

**2. Placeholder scan:** No TBD/TODO; every code step shows complete code; every command has expected output.

**3. Type consistency:** `backoff(ctx, runID, s, attempt)` defined in Task 1 Step 3 and called in Step 4 — signatures match. `debugLogger`/`hasLine` defined in Task 1, consumed by Tasks 2/3. `*join.ConflictError{Branch, Paths}` matches the struct in `internal/join/git.go`. `flow.RetryPolicy{Max, Backoff}`, `flow.Gate{Policy, Verifier}`, `flow.JoinMerge`, `flow.FailEscalate` all match existing usages in `engine_test.go`.
