# Self-repair via verifier feedback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When an `auto` gate's verifier fails, capture its output and feed it to the agent on the next retry attempt via a new `core.Task.Feedback`, turning the existing blind retry into an informed self-repair loop.

**Architecture:** The verifier output travels **up** as a typed `*gate.VerifierFailure` error (it is born inside a failure), and **down** as a `core.Task.Feedback` field the engine sets on the next attempt (it is consumed when handing the executor its task). The engine owns policy (what bytes, when); the `CLIAgent` adapter owns prompt formatting. Always-on; in-memory only; the workspace is already reused across attempts.

**Tech Stack:** Go 1.22, stdlib only. Packages: `internal/gate`, `internal/core`, `internal/engine`, `internal/executor`.

## Global Constraints

- **No new dependencies.** Stdlib only. No `go.mod` change. `go 1.22` unchanged; pinned deps untouched (modernc.org/sqlite v1.36.1, pressly/goose/v3 v3.24.1, oklog/ulid/v2 v2.1.1, expr-lang/expr v1.17.8, OTel v1.32.0).
- **No persistence / schema change.** Feedback is in-memory, attempt-to-attempt; no migration, no new event, no new store method. Persist-then-publish untouched.
- **Ports-and-adapters boundary preserved.** The engine never string-munges agent-facing prose; the executor adapter formats the prompt.
- **Scope:** only auto-gate verifier failures produce feedback. Join `on_conflict` (merge-conflict) feedback, agent-execution-error feedback, and persisting feedback are OUT of scope.
- Commit hygiene: single conventional-commit subject lines, no body, no `Co-Authored-By` trailer, never `--no-verify`. `gofmt -l` must be clean.
- Run package tests **sandbox-disabled** if the sandbox blocks them (real-process exec); whole suite: `go test -race ./...`.

---

## File Structure

- `internal/gate/verifier.go` (modify) — `Verify` returns captured output; `tailBytes` + `maxFeedbackBytes`.
- `internal/gate/gate.go` (modify) — `Evaluate` returns output.
- `internal/gate/errors.go` (create) — `VerifierFailure` typed error.
- `internal/gate/verifier_test.go` (create) — `Verify` capture/cap tests.
- `internal/gate/gate_test.go` (modify) — update `Evaluate` call arity; add an output test.
- `internal/core/ports.go` (modify) — `Task.Feedback` field.
- `internal/engine/engine.go` (modify) — build `VerifierFailure`; thread feedback through `attempt`/`execute`/`runAgent`; `runStep` extract + debug log.
- `internal/engine/engine_test.go` (modify) — white-box `attempt` test (Task 1); self-repair + exec-error tests (Task 2).
- `internal/executor/cli.go` (modify) — `promptWithFeedback` + call-site.
- `internal/executor/cli_test.go` (modify) — `promptWithFeedback` + wiring tests.

---

## Task 1: Gate captures verifier output and surfaces `VerifierFailure`

**Files:**
- Modify: `internal/gate/verifier.go`
- Modify: `internal/gate/gate.go`
- Create: `internal/gate/errors.go`
- Modify: `internal/engine/engine.go:385-406` (the `attempt` gate block)
- Create: `internal/gate/verifier_test.go`
- Modify: `internal/gate/gate_test.go`
- Modify: `internal/engine/engine_test.go` (add one white-box test)

**Interfaces:**
- Produces: `gate.Verifier.Verify(ctx, command, workDir string) (ok bool, output string, err error)`; `gate.Evaluator.Evaluate(ctx, runID core.RunID, s *flow.Step, res core.Result, workDir string) (ok bool, output string, err error)`; `type gate.VerifierFailure struct { Command string; Output string }` implementing `error`.
- Consumes: nothing from earlier tasks.

- [ ] **Step 1: Write the failing verifier tests**

Create `internal/gate/verifier_test.go`:

```go
package gate

import (
	"context"
	"strings"
	"testing"
)

func TestCommandVerifierPassesWithNoOutput(t *testing.T) {
	ok, out, err := CommandVerifier{}.Verify(context.Background(), "true", t.TempDir())
	if err != nil || !ok || out != "" {
		t.Fatalf("Verify(true) = (%v, %q, %v), want (true, \"\", nil)", ok, out, err)
	}
}

func TestCommandVerifierEmptyCommandPasses(t *testing.T) {
	ok, out, err := CommandVerifier{}.Verify(context.Background(), "", t.TempDir())
	if err != nil || !ok || out != "" {
		t.Fatalf("Verify(\"\") = (%v, %q, %v), want (true, \"\", nil)", ok, out, err)
	}
}

func TestCommandVerifierCapturesFailureOutput(t *testing.T) {
	ok, out, err := CommandVerifier{}.Verify(context.Background(), `echo "boom: bad"; exit 1`, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	if ok {
		t.Fatal("want ok=false on non-zero exit")
	}
	if !strings.Contains(out, "boom: bad") {
		t.Errorf("output = %q, want it to contain the command's stdout", out)
	}
}

func TestCommandVerifierTailCapsLargeOutput(t *testing.T) {
	// Emit ~25 KiB then a tail marker; the captured feedback is capped near
	// maxFeedbackBytes and keeps the tail.
	cmd := `i=0; while [ $i -lt 5000 ]; do echo LINE; i=$((i+1)); done; echo TAILMARK; exit 1`
	ok, out, err := CommandVerifier{}.Verify(context.Background(), cmd, t.TempDir())
	if err != nil || ok {
		t.Fatalf("Verify = (%v, %v), want (false, nil)", ok, err)
	}
	if len(out) > maxFeedbackBytes+64 {
		t.Errorf("output len %d exceeds cap %d (+marker slack)", len(out), maxFeedbackBytes)
	}
	if !strings.Contains(out, "TAILMARK") {
		t.Error("tail (with TAILMARK) must be kept")
	}
	if !strings.Contains(out, "truncated") {
		t.Error("truncation marker missing")
	}
}
```

- [ ] **Step 2: Run the verifier tests to verify they fail**

Run: `go test ./internal/gate/ -run TestCommandVerifier -v`
Expected: FAIL to compile — `Verify` returns 2 values, not 3; `maxFeedbackBytes` undefined.

- [ ] **Step 3: Rewrite `Verify` to capture output**

In `internal/gate/verifier.go`, change the `Verifier` interface and `CommandVerifier.Verify`. Replace the interface and method body:

```go
// Verifier resolves an auto gate by running a check.
type Verifier interface {
	Verify(ctx context.Context, command, workDir string) (ok bool, output string, err error)
}
```

```go
const maxFeedbackBytes = 8 << 10 // 8 KiB cap on verifier output fed back to the agent

func (CommandVerifier) Verify(ctx context.Context, command, workDir string) (bool, string, error) {
	if command == "" {
		return true, "", nil
	}
	// #nosec G204 -- command is operator-supplied config (flow YAML), not user input.
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			// Killed by the step timeout / cancellation — an infra error, not a
			// gate verdict. The engine treats it as a retryable failure.
			return false, "", ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return false, tailBytes(out, maxFeedbackBytes), nil // non-zero exit = check failed
		}
		return false, "", err
	}
	return true, "", nil
}

// tailBytes returns the last n bytes of b as a string. Verifier/test output
// prints its summary at the end, so the tail is the useful part; when b is
// longer than n a single truncation marker is prefixed.
func tailBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "…(truncated)\n" + string(b[len(b)-n:])
}
```

The existing imports (`context`, `errors`, `os/exec`) already cover this — no import change.

- [ ] **Step 4: Run the verifier tests to verify they pass**

Run: `go test ./internal/gate/ -run TestCommandVerifier -v`
Expected: the four `TestCommandVerifier*` tests PASS. (Other gate tests still fail to compile — fixed in Steps 5–7.)

- [ ] **Step 5: Add the `VerifierFailure` typed error**

Create `internal/gate/errors.go`:

```go
package gate

import "fmt"

// VerifierFailure is the error a failed auto gate returns. Output carries the
// verifier's captured stdout/stderr tail so the engine can feed it to the next
// attempt's agent. Output is deliberately not in Error() — it can be large; the
// run's recorded Err and logs stay concise.
type VerifierFailure struct {
	Command string
	Output  string
}

func (f *VerifierFailure) Error() string {
	return fmt.Sprintf("gate failed (policy=%q, command=%q)", "auto", f.Command)
}
```

- [ ] **Step 6: Thread output through `Evaluate` and update gate tests**

In `internal/gate/gate.go`, change `Evaluate` to return the output:

```go
func (e *Evaluator) Evaluate(ctx context.Context, runID core.RunID, s *flow.Step, res core.Result, workDir string) (bool, string, error) {
	switch s.Gate.Policy {
	case "", flow.GateManual:
		ok, err := e.Approver.Approve(ctx, runID, s, res)
		return ok, "", err
	case flow.GateConditional:
		env := flow.GateEnv{Result: flow.GateResult{
			Summary:   res.Summary,
			CostUSD:   res.CostUSD,
			Artifacts: artifactPaths(res.Artifacts),
			StepID:    res.StepID,
		}}
		ok, err := s.Gate.Condition.Eval(env)
		return ok, "", err
	case flow.GateAuto:
		ok, output, err := e.Verifier.Verify(ctx, s.Gate.Verifier.Command, workDir)
		if err != nil {
			return false, "", fmt.Errorf("verifier error: %w", err)
		}
		return ok, output, nil
	default:
		return false, "", fmt.Errorf("step %q: unknown gate policy %q", s.ID, s.Gate.Policy)
	}
}
```

In `internal/gate/gate_test.go`: add `"strings"` to the import block, then update every `Evaluate` call to the 3-value form:
- the five `ok, err := e.Evaluate(...)` calls become `ok, _, err := e.Evaluate(...)`.
- the one `ok, _ := e.Evaluate(...)` (in `TestManualGateUsesApprover`) becomes `ok, _, _ := e.Evaluate(...)`.

Then add this test asserting the auto path returns output:

```go
func TestAutoGateReturnsVerifierOutput(t *testing.T) {
	e := &Evaluator{Approver: AutoApprover{}, Verifier: CommandVerifier{}}
	s := &flow.Step{ID: "a", Gate: flow.Gate{
		Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: `echo nope; exit 1`}}}
	ok, out, err := e.Evaluate(context.Background(), "r1", s, core.Result{}, t.TempDir())
	if err != nil || ok {
		t.Fatalf("Evaluate = (%v, %v), want (false, nil)", ok, err)
	}
	if !strings.Contains(out, "nope") {
		t.Errorf("output = %q, want the verifier stdout", out)
	}
}
```

- [ ] **Step 7: Update `engine.attempt` to build `VerifierFailure`**

In `internal/engine/engine.go`, in `attempt`, change the `Evaluate` call and the final disposition switch (currently lines ~385–406):

```go
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
			return res, true, &gate.VerifierFailure{Command: s.Gate.Verifier.Command, Output: output}
		}
		return res, true, fmt.Errorf("gate failed (policy=%q)", gatePolicyOf(s))
	default:
		return res, false, nil
	}
```

(`internal/engine/engine.go` already imports `concentus/internal/gate`.)

- [ ] **Step 8: Write the white-box `attempt` test**

In `internal/engine/engine_test.go`, add `"errors"` to the import block, then add:

```go
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
	_, gateFailed, err := e.attempt(context.Background(), "r1", s, nil, 1, t.TempDir())
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
```

- [ ] **Step 9: Run gate + engine tests to verify they pass**

Run: `go test ./internal/gate/ ./internal/engine/ -count=1`
Expected: PASS (both packages compile and all tests green). If the sandbox blocks the real-process exec in `internal/engine`, re-run that package sandbox-disabled.

- [ ] **Step 10: gofmt + commit**

```bash
gofmt -w internal/gate/verifier.go internal/gate/gate.go internal/gate/errors.go internal/gate/verifier_test.go internal/gate/gate_test.go internal/engine/engine.go internal/engine/engine_test.go
git add internal/gate/verifier.go internal/gate/gate.go internal/gate/errors.go internal/gate/verifier_test.go internal/gate/gate_test.go internal/engine/engine.go internal/engine/engine_test.go
git commit -m "feat(gate): capture verifier output and surface it as VerifierFailure"
```

---

## Task 2: Engine threads feedback to the next attempt's agent

**Files:**
- Modify: `internal/core/ports.go` (add `Task.Feedback`)
- Modify: `internal/engine/engine.go` (`runStep`, `attempt`, `execute`, `runAgent`, + escalate-join call sites)
- Modify: `internal/engine/engine_test.go` (two integration tests; update the Task 1 white-box call)

**Interfaces:**
- Consumes: `gate.VerifierFailure{Command, Output}` (Task 1).
- Produces: `core.Task.Feedback string`; `attempt`/`execute`/`runAgent` each gain a trailing `feedback string` parameter; `runStep` sets `Task.Feedback` on retries after an auto-gate verifier failure.

- [ ] **Step 1: Add the `Task.Feedback` field**

In `internal/core/ports.go`, add to the `Task` struct (after `WorkDir`, before `Emit`):

```go
	// Feedback is non-empty on a retry after an auto-gate verifier failed: the
	// previous attempt's captured verifier output (tail-capped). Executors
	// incorporate it into the model prompt so the agent can fix the specific
	// failure; empty on the first attempt. Mock ignores it.
	Feedback string
```

- [ ] **Step 2: Write the failing self-repair integration test**

In `internal/engine/engine_test.go`, add the scripted executor and the test:

```go
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
```

- [ ] **Step 3: Run the self-repair test to verify it fails**

Run: `go test ./internal/engine/ -run TestSelfRepairFeedsVerifierOutputToRetry -count=1` (sandbox-disabled if needed).
Expected: FAIL — feedback is never threaded, so the executor always sees `""`, always writes BAD, the verifier fails both attempts, and `e.Run` returns an error (`run: gate failed …`). This proves the test exercises the missing threading.

- [ ] **Step 4: Thread `feedback` through `runAgent`, `execute`, `attempt`**

In `internal/engine/engine.go`:

**`runAgent`** — add a trailing `feedback string` parameter, set it on the `Task`, and record a span attribute. Change the signature line to end with `inputs []core.Artifact, feedback string)`. Inside, after `defer span.End()`, add:

```go
	if len(feedback) > 0 {
		span.SetAttributes(attribute.Int("magister.feedback_bytes", len(feedback)))
	}
```

and add `Feedback: feedback,` to the `core.Task{...}` literal passed to `ag.Run`.

**`execute`** — add a trailing `feedback string` parameter. Pass it to both `runAgent` calls:
- the join arbiter closure (line ~472): `return e.runAgent(ctx, runID, s.ID, "arbiter", agentName, prompt, wd, attemptNum, in, feedback)`.
- the normal-step return (line ~495): `return e.runAgent(ctx, runID, s.ID, s.Role, s.Agent, promptFor(s, inputs), workDir, attemptNum, inputs, feedback)`.

**`attempt`** — add a trailing `feedback string` parameter and pass it to `execute` (line ~369): `res, err = e.execute(attemptCtx, runID, s, inputs, attemptNum, workDir, feedback)`.

**Escalate-join call sites** (out of scope for feedback — pass `""`):
- line ~659 (`escalateJoin`'s `attempt` call): `res, _, execErr := e.attempt(ctx, runID, s, inputs, attemptNum+1, workDir, "")`.
- line ~683 (conflict-resolution `runAgent` call): append `, ""` so it ends `... workDir, next, inputs, "")`.

- [ ] **Step 5: Wire `runStep` to extract and pass feedback**

In `internal/engine/engine.go`, `runStep`:

Add `var lastFeedback string` next to `var lastErr error`.

In the `if attempt > 1 { … }` block, after the backoff call returns (just before the `StepRunning` transition that follows the block), add:

```go
		if attempt > 1 && lastFeedback != "" {
			e.logger().DebugContext(ctx, "retrying with verifier feedback",
				"run", string(runID), "step", s.ID, "attempt", attempt, "feedback_bytes", len(lastFeedback))
		}
```

Change the `attempt` call to pass `lastFeedback`:

```go
		res, gateFailed, execErr := e.attempt(ctx, runID, s, inputs, attempt, workDir, lastFeedback)
```

After `lastErr = execErr`, recompute the feedback for the next iteration:

```go
		lastFeedback = ""
		var vf *gate.VerifierFailure
		if errors.As(lastErr, &vf) {
			lastFeedback = vf.Output
		}
```

(`internal/engine/engine.go` already imports `errors` and `gate`.)

- [ ] **Step 6: Update the Task 1 white-box test call**

In `internal/engine/engine_test.go`, `TestAttemptAutoGateFailureCarriesVerifierOutput`, add the new trailing arg to the `attempt` call:

```go
	_, gateFailed, err := e.attempt(context.Background(), "r1", s, nil, 1, t.TempDir(), "")
```

- [ ] **Step 7: Run the self-repair test to verify it passes**

Run: `go test ./internal/engine/ -run 'TestSelfRepair|TestAttemptAutoGate' -count=1` (sandbox-disabled if needed).
Expected: PASS — attempt 1 fails, attempt 2 receives the verifier output and writes GOOD, the run succeeds.

- [ ] **Step 8: Add the exec-error guard test**

This test locks the scope: an agent **execution** error (not a gate failure) must thread no feedback. Add to `internal/engine/engine_test.go`:

```go
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
```

- [ ] **Step 9: Run the exec-error guard + full engine package**

Run: `go test ./internal/engine/ -count=1` (sandbox-disabled if needed).
Expected: PASS, including `TestExecErrorThreadsNoFeedback` (a manual/auto-approved gate means only the exec error drives the retry, and an exec error is not a `VerifierFailure`, so feedback stays empty).

- [ ] **Step 10: gofmt + commit**

```bash
gofmt -w internal/core/ports.go internal/engine/engine.go internal/engine/engine_test.go
git add internal/core/ports.go internal/engine/engine.go internal/engine/engine_test.go
git commit -m "feat(engine): feed failed-verifier output to the next attempt's agent"
```

---

## Task 3: CLI adapter folds feedback into the agent prompt

**Files:**
- Modify: `internal/executor/cli.go` (`promptWithFeedback` + call-site)
- Modify: `internal/executor/cli_test.go` (two tests)

**Interfaces:**
- Consumes: `core.Task.Feedback` (Task 2).
- Produces: `promptWithFeedback(prompt, feedback string) string`; `CLIAgent.Run` composes it before `Spec.Args`.

- [ ] **Step 1: Write the failing adapter tests**

In `internal/executor/cli_test.go`, add `"io"` to the import block, then add a recording spec and two tests:

```go
type recordingSpec struct{ gotPrompt string }

func (s *recordingSpec) Args(model, prompt string) []string { s.gotPrompt = prompt; return nil }
func (s *recordingSpec) Parse(io.Reader, func(event.Event)) (string, float64, error) {
	return "ok", 0, nil
}

func TestPromptWithFeedbackEmptyIsIdentity(t *testing.T) {
	if got := promptWithFeedback("do the work", ""); got != "do the work" {
		t.Errorf("empty feedback must not change the prompt, got %q", got)
	}
}

func TestCLIAgentRunIncludesFeedbackInPrompt(t *testing.T) {
	spec := &recordingSpec{}
	a := &CLIAgent{Bin: stubPath(t, "fake-claude-ok"), Model: "opus", Spec: spec}
	_, err := a.Run(context.Background(), core.Task{
		StepID: "s1", Prompt: "do the work", Feedback: "verifier: missing GOOD marker", WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(spec.gotPrompt, "do the work") {
		t.Errorf("prompt lost the original task: %q", spec.gotPrompt)
	}
	if !strings.Contains(spec.gotPrompt, "Previous attempt failed verification") {
		t.Errorf("prompt missing the feedback heading: %q", spec.gotPrompt)
	}
	if !strings.Contains(spec.gotPrompt, "verifier: missing GOOD marker") {
		t.Errorf("prompt missing the verifier output: %q", spec.gotPrompt)
	}
}
```

- [ ] **Step 2: Run the adapter tests to verify they fail**

Run: `go test ./internal/executor/ -run 'TestPromptWithFeedback|TestCLIAgentRunIncludesFeedback' -v`
Expected: FAIL to compile — `promptWithFeedback` undefined.

- [ ] **Step 3: Add `promptWithFeedback` and use it**

In `internal/executor/cli.go`, add the function (no new import — plain string concatenation):

```go
// promptWithFeedback appends the previous attempt's verifier output to the prompt
// on a retry, so the agent can fix the specific failure. Empty feedback (the
// first attempt) returns the prompt unchanged.
func promptWithFeedback(prompt, feedback string) string {
	if feedback == "" {
		return prompt
	}
	return prompt +
		"\n\n## Previous attempt failed verification\n" +
		"The verifier for this step failed. Fix the problems shown below, then redo the work.\n\n" +
		"```\n" + feedback + "\n```\n"
}
```

Change the `exec.CommandContext` line in `Run` to compose the prompt:

```go
	cmd := exec.CommandContext(ctx, a.Bin, a.Spec.Args(a.Model, promptWithFeedback(t.Prompt, t.Feedback))...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
```

(Keep the existing `// #nosec G204` comment on the preceding line unchanged.)

- [ ] **Step 4: Run the adapter tests to verify they pass**

Run: `go test ./internal/executor/ -run 'TestPromptWithFeedback|TestCLIAgentRunIncludesFeedback' -v`
Expected: PASS. (`TestCLIAgentRunIncludesFeedbackInPrompt` execs the `fake-claude-ok` stub, which exits 0; the recording spec captures the composed prompt.)

- [ ] **Step 5: gofmt + commit**

```bash
gofmt -w internal/executor/cli.go internal/executor/cli_test.go
git add internal/executor/cli.go internal/executor/cli_test.go
git commit -m "feat(executor): fold verifier feedback into the CLI agent prompt"
```

---

## Final verification (after all tasks)

- [ ] Run the whole suite with the race detector: `go test -race ./...` (sandbox-disabled if the sandbox blocks the real-process exec in `internal/engine`, `internal/executor`, `internal/supervisor`, `internal/api`).
  Expected: all packages green.
- [ ] `gofmt -l internal/ cmd/` prints nothing.
