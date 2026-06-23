# Self-repair via verifier feedback — Design

## Goal

Turn the engine's existing **blind** retry into an **informed** self-repair loop.
Today, when an `auto` gate's verifier fails and the step has `retry` +
`on_fail: retry`, the engine re-runs the agent up to `retry.max` times — but the
agent receives the **identical prompt** each attempt and the verifier's failing
output is **discarded**, so it has no idea what failed and tends to repeat the
mistake.

This slice captures the failed verifier's output and feeds it to the agent on
the **next** attempt via a new `core.Task.Feedback` field. The agent sees "your
previous attempt failed this check; here is the output" and can fix the specific
failure. **Always-on** (no new flow-YAML surface); the workspace is already
reused across attempts, so the agent builds on its prior partial work.

## Background (current state)

- `engine.runStep` (internal/engine/engine.go) allocates **one** `workDir` via
  `WS.For(runID, s)` outside the attempt loop and loops
  `for attempt := 1; attempt <= maxAttempts; attempt++`, where
  `maxAttempts = s.Retry.Max`. On a gate failure it `continue`s when
  `attempt < maxAttempts && s.Retry != nil`. So the retry loop and
  cross-attempt workspace reuse already exist.
- Each attempt: `attempt(...)` → `execute(...)` → `runAgent(..., promptFor(s, inputs), ...)`.
  `promptFor(s, inputs)` builds the prompt from the **step + inputs only** —
  identical on every attempt.
- `gate.Evaluator.Evaluate(ctx, runID, s, res, workDir) (bool, error)` resolves
  the gate. For `auto` it calls `gate.CommandVerifier.Verify(ctx, command, workDir) (bool, error)`,
  which runs `cmd.Run()` — **the verifier's stdout/stderr is never captured.**
- `engine.attempt` turns a failed verdict into a generic
  `fmt.Errorf("gate failed (policy=%q)", ...)`; the output is already gone.
- `flow.GateResult` exists but is **only** the projection a conditional gate's
  expression evaluates against — not a general gate-return channel. It is not
  reused here.

**The gap:** the loop retries blind. The missing pieces are (1) capturing the
verifier output and (2) delivering it to the next attempt's agent.

## Global Constraints

- **No new dependencies.** Stdlib only. No `go.mod` change. `go 1.22` unchanged;
  pinned deps untouched (modernc.org/sqlite v1.36.1, pressly/goose/v3 v3.24.1,
  oklog/ulid/v2 v2.1.1, expr-lang/expr v1.17.8, OTel v1.32.0).
- **No persistence / schema change.** Feedback is in-memory, attempt-to-attempt;
  no migration, no new event, no new store method. Persist-then-publish
  untouched.
- **Ports-and-adapters boundary preserved.** The engine owns *policy* (when to
  feed back, what bytes); the executor adapter owns *prompt formatting*. The
  engine never string-munges agent-facing prose.
- Commit hygiene: single conventional-commit subject lines, no body, no
  `Co-Authored-By` trailer, never `--no-verify`. `gofmt -l` clean.
- Real-git tests guard with `requireGit`/`exec.LookPath("git")` and run
  sandbox-disabled.

## Design

### 1. Capture — the verifier returns its output

`internal/gate/verifier.go`:

- New signature: `Verify(ctx, command, workDir string) (ok bool, output string, err error)`
  (the `gate.Verifier` interface changes identically).
- Implementation uses `cmd.CombinedOutput()` instead of `cmd.Run()`:
  - `command == ""` ⇒ `(true, "", nil)` (unchanged short-circuit).
  - run completes, exit 0 ⇒ `(true, "", nil)` (output irrelevant on pass).
  - non-zero **exit** (`*exec.ExitError`) ⇒ `(false, tailBytes(out, maxFeedbackBytes), nil)`
    — a verdict, not an infra error, exactly as today.
  - `ctx.Err() != nil` (timeout/cancel) ⇒ `(false, "", ctx.Err())` — infra error.
  - any other error ⇒ `(false, "", err)`.
- `maxFeedbackBytes = 8 << 10` (8192). `tailBytes(b, n)` keeps the **last** `n`
  bytes (verifier/test output prints its summary at the end); when truncated it
  prefixes a single `…(truncated)\n` marker line so the agent knows output was
  clipped. A new unexported helper in this package.

### 2. Surface — the failed-verifier error carries the output

`internal/gate/gate.go` (or a new `internal/gate/errors.go`):

- `Evaluator.Evaluate` gains the output return:
  `Evaluate(ctx, runID, s, res, workDir) (ok bool, output string, err error)`.
  - `manual` / `conditional` / `default` return `output == ""`.
  - `auto`: `ok, output, verr := e.Verifier.Verify(...)`; on `verr != nil` return
    `(false, "", fmt.Errorf("verifier error: %w", verr))`; else `(ok, output, nil)`.
- New typed error:

  ```go
  // VerifierFailure is the error a failed auto gate returns. Output carries the
  // verifier's captured tail so the engine can feed it to the next attempt.
  type VerifierFailure struct {
      Command string
      Output  string
  }
  func (f *VerifierFailure) Error() string {
      return fmt.Sprintf("gate failed (policy=%q, command=%q)", "auto", f.Command)
  }
  ```

  `Output` is **not** in `Error()` — it can be large; the run's `Err`/log stays
  concise. Output is structured data, extracted by type, not by message.

### 3. Thread — engine carries feedback across attempts

`internal/engine/engine.go`:

- `core.Task.Feedback` (see §4) is set from a `feedback string` parameter added to
  `runAgent`, `execute`, and `attempt` (passed **down** into the attempt).
- `attempt`, after `ok, output, gerr := e.Gate.Evaluate(...)`:
  - `gerr != nil` ⇒ `return res, false, gerr` (infra/verifier error — unchanged).
  - `!ok && gatePolicyOf(s) == flow.GateAuto` ⇒
    `return res, true, &gate.VerifierFailure{Command: s.Gate.Verifier.Command, Output: output}`.
  - `!ok` (conditional) ⇒ `return res, true, fmt.Errorf("gate failed (policy=%q)", gatePolicyOf(s))` (unchanged).
  - else ⇒ `return res, false, nil`.
- `execute` passes `feedback` to `runAgent` for a normal step, and into the join
  `run` closure for a join step (the arbiter receives it too — harmless and
  consistent; merge-conflict-specific feedback stays out of scope, see below).
- `runAgent` sets `core.Task{… , Feedback: feedback}` and, when
  `len(feedback) > 0`, adds `attribute.Int("magister.feedback_bytes", len(feedback))`
  to the agent span.
- `runStep` owns the loop state:

  ```go
  var lastErr error
  var lastFeedback string
  for attempt := 1; attempt <= maxAttempts; attempt++ {
      if attempt > 1 {
          // …existing StepRetrying transition + backoff…
          if lastFeedback != "" {
              e.logger().DebugContext(ctx, "retrying with verifier feedback",
                  "run", string(runID), "step", s.ID, "attempt", attempt,
                  "feedback_bytes", len(lastFeedback))
          }
      }
      // …existing StepRunning transition…
      res, gateFailed, execErr := e.attempt(ctx, runID, s, inputs, attempt, workDir, lastFeedback)
      // …existing success path…
      lastErr = execErr
      lastFeedback = ""
      var vf *gate.VerifierFailure
      if errors.As(lastErr, &vf) {
          lastFeedback = vf.Output
      }
      // …existing canRetry / escalate / terminal disposition…
  }
  ```

  Feedback is recomputed from `lastErr` each iteration: only an auto-gate
  `VerifierFailure` yields non-empty feedback. An agent execution error, a
  conditional-gate failure, or a join `ConflictError` extract to `""` — the next
  attempt's agent gets a clean prompt.

### 4. Deliver — `Task.Feedback` + the CLI adapter

`internal/core/ports.go`:

```go
type Task struct {
    // …existing fields…
    // Feedback is non-empty on a retry after an auto-gate verifier failed: the
    // previous attempt's captured verifier output (tail-capped). Executors
    // incorporate it into the model prompt so the agent can fix the specific
    // failure; empty on the first attempt. Mock ignores it.
    Feedback string
}
```

`internal/executor/cli.go` — the adapter owns formatting:

```go
func promptWithFeedback(prompt, feedback string) string {
    if feedback == "" {
        return prompt
    }
    return prompt + "\n\n## Previous attempt failed verification\n" +
        "The verifier for this step failed. Fix the problems shown below, then redo the work.\n\n" +
        "```\n" + feedback + "\n```\n"
}
```

Used at the one call site: `a.Spec.Args(a.Model, promptWithFeedback(t.Prompt, t.Feedback))`.
The three `CLISpec`s (Claude/Gemini/Codex) are untouched — they receive the
composed prompt. `Mock` ignores `Feedback`, so existing engine/Mock tests stay
deterministic.

### Components / files

- `internal/gate/verifier.go` — `Verify` new signature, `CombinedOutput`,
  `tailBytes`, `maxFeedbackBytes`.
- `internal/gate/gate.go` (+ optional `errors.go`) — `Evaluate` new signature;
  `VerifierFailure`.
- `internal/core/ports.go` — `Task.Feedback`.
- `internal/engine/engine.go` — `runStep` (extract + thread + debug log),
  `attempt`/`execute`/`runAgent` (feedback param + `VerifierFailure` + span attr).
- `internal/executor/cli.go` — `promptWithFeedback`.

### Testing (TDD)

- **gate** (verifier_test.go / gate_test.go): a failing command returns
  `ok=false` with its output; a passing command returns `ok=true, ""`; a command
  exceeding `maxFeedbackBytes` is tail-capped with the truncation marker;
  `Evaluate` auto-returns the output, manual/conditional return `""`.
- **executor** (cli_test.go): with a fake bin that echoes its prompt arg, a
  non-empty `Task.Feedback` appears (under the heading) in what the CLI receives;
  empty `Feedback` leaves the prompt byte-identical.
- **engine** (engine_test.go — the heart): a scripted executor that writes
  verifier-**failing** output when `t.Feedback == ""` and verifier-**passing**
  output once `t.Feedback != ""`, recording the feedback it received. Using the
  same workspace setup the existing engine retry tests use (a real-git workspace
  so the success path's `commitIsolated` works — `requireGit`, sandbox-disabled
  to match), a shell verifier (`CommandVerifier` runs `sh -c`), and
  `retry.max = 2`, assert: attempt 1 fails → the executor's recorded `Feedback`
  on attempt 2 equals the captured verifier output → the run succeeds. Plus: an
  agent **execution** error threads no feedback (next attempt `Feedback == ""`).
- Full `go test -race ./...` green; `gofmt -l` clean.

## Out of scope (deferred)

- **Join `on_conflict` (merge-conflict) feedback** — a `*join.ConflictError`
  carries the conflicting branch/paths; feeding that to the arbiter on retry is a
  separate enhancement. `errors.As` for `*VerifierFailure` naturally excludes it.
- **Agent-execution-error feedback** — surfacing the agent's own failure (vs a
  verifier's) to its next attempt.
- **Persisting feedback** to the store/events, or surfacing it in `cm get` —
  in-memory only; a debug log + span attribute are the observability surface.
- **Per-step opt-out / size config** — always-on, fixed 8 KB cap.
