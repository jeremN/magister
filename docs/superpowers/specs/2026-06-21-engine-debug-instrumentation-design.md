# Observability — engine Debug/Warn instrumentation

## Summary

Add seven `slog` log points to the engine so an operator running with `-log-level debug` can see what the engine is *doing* between the SSE lifecycle events — agent invocation timing and cost, retry backoff, concurrency-slot waits, gate verdicts, and join execution — plus two `Warn`-level lines (retry-budget-exhausted, merge-conflict-detected) that surface notable-but-recoverable conditions at the *default* level.

This makes the just-merged `-log-level debug` knob genuinely useful: the engine logs only at `Error` today, so `debug` currently reveals nothing extra. All changes live in `internal/engine/engine.go` (plus tests). **Pure-additive logging** — no behavior change, no new SSE event, no config/schema/migration, no new dependency or package. Stdlib `slog` only.

## Motivation

Every engine lifecycle *transition* already emits an SSE `event.Event` (`run.started`, `step.started`, `step.done`, `step.failed`, `step.retrying`, `gate.awaiting`, `agent.tool`, `run.done`). What has **no** signal today is the engine's internal *decisions*: how long an agent invocation took and what it cost, the jittered backoff delay before a retry, how long a step waited for a concurrency slot, the verdict of a gate that passed, which join strategy ran, and the moment a merge conflict was detected. An operator debugging a stuck, slow, or failing run cannot see any of these. This slice instruments exactly those decision points — deliberately *not* duplicating facts that already ride the event stream.

## Design

### Approach: engine-boundary logging

The engine holds the only logger reachable from these seams (`e.logger()`, the nil-safe helper returning `e.Log` or the package `discardLogger`). The `gate` and `join` packages hold **no logger** and signal exclusively through returned errors. Rather than thread a logger into those packages (coupling two more packages to `slog`/`logctx` and widening the diff), every new line is emitted from `engine.go`, which already has each decision's inputs and return values in scope. Merge conflicts — produced inside `join` as a `*join.ConflictError` — are logged from `execute()` via `errors.As`, which the engine already imports and uses for the escalation ladder.

### The seven log points

Field keys reuse the existing engine convention (`"run"` = `string(runID)`, `"step"`, `"agent"`, `"err"`). `dur`/`delay`/`waited` are `time.Duration` (slog renders them); `cost_usd` is a `float64`; `paths` is a `[]string`.

| # | Level | Site | msg | Fields |
|---|---|---|---|---|
| A | Debug | `runAgent`, before/after `ag.Run` | `agent starting` / `agent finished` | start: `run`, `step`, `agent`, `role`, `attempt`; finish adds `dur`, `cost_usd` (and `err` only when non-nil) |
| B | Debug | `backoff`, after the jittered delay is computed | `step backoff` | `run`, `step`, `attempt`, `delay`, `base` |
| C | Debug | `runDAG` goroutine, after both slots are acquired | `step slot acquired` | `run`, `step`, `waited` |
| D | Debug | `attempt`, after `e.Gate.Evaluate` returns | `gate evaluated` | `run`, `step`, `attempt`, `policy`, `pass` (and `err` only when the gate returned an infra error) |
| E | **Warn** | `runStep`, budget spent, *after* the join-conflict-escalation early-return | `retry budget exhausted` | `run`, `step`, `attempts`, `last_err`, `escalating` |
| F | Debug | `execute`, bracketing `strat.Join` | `join starting` / `join finished` | `run`, `step`, `strategy`, `inputs`, `attempt`; finish adds `err` only when non-nil |
| G | **Warn** | `execute`, after `strat.Join` returns, via `errors.As(err, *join.ConflictError)` | `merge conflict detected` | `run`, `step`, `branch`, `paths`, `attempt` |

#### A — agent invocation start/finish (`runAgent`)

`runAgent` already measures `agentStart := e.Clock.Now()` and computes the duration for the metrics histogram (`e.Clock.Now().Sub(agentStart)`) right after `ag.Run` returns. Emit `agent starting` immediately before `ag.Run` and `agent finished` immediately after, reusing the duration already computed for metrics and `res.CostUSD` (also already read for `AddCost`). The `role` field distinguishes normal steps from join arbiters (which also flow through `runAgent` with `role="arbiter"`). The finish line includes `err` only when `err != nil`, so the happy path stays tidy:

```go
args := []any{"run", string(runID), "step", stepID, "agent", agentName, "attempt", attemptNum, "dur", dur, "cost_usd", res.CostUSD}
if err != nil {
    args = append(args, "err", err)
}
e.logger().Debug("agent finished", args...)
```

#### B — backoff delay (`backoff` gains a `runID` parameter)

`backoff` currently has signature `func (e *Engine) backoff(ctx context.Context, s *flow.Step, attempt int) bool` and computes the jittered sleep `delay` (and the pre-jitter `base`) internally. To log the real values it gains a `runID core.RunID` parameter — `func (e *Engine) backoff(ctx context.Context, runID core.RunID, s *flow.Step, attempt int) bool` — and its single caller in `runStep` passes the `runID` already in scope. The `step.retrying` event fires *before* `backoff` and carries no duration, so this is the only place the delay is observable. Log after the delay is finalized, before the sleep.

#### C — concurrency-slot wait (`runDAG` goroutine)

The per-step goroutine acquires the per-run cap then the global semaphore before `runStep`; a step can queue here invisibly. Capture `queueStart := e.Clock.Now()` immediately before the acquisition block and, once both slots are held, emit `step slot acquired` with `waited` = elapsed. When no limits are configured the wait is ~0 and the line still fires (honest: it was admitted immediately); it is Debug, so hidden at the default level. `e.Clock` is a hard engine dependency (already used unconditionally for agent timing), so no nil guard is needed.

#### D — gate verdict (`attempt`)

After `e.Gate.Evaluate(...)` returns `(ok, gerr)`, emit `gate evaluated` with `policy` = `gatePolicyOf(s)` and `pass` = `ok`. This fires for every step (every step has an effective gate policy, including join steps and fan-in steps that default to manual) and is the only signal for a gate that *passes* — today only a gate that fails-to-terminal produces an event. For manual gates the line fires after the human decision returns. Include `err` only when `gerr != nil` (an infra error from the verifier/approver).

#### E — retry budget exhausted (`runStep`) — Warn

When the retry loop's budget is spent (`canRetry` is false), emit one `Warn` — but placed *after* the join-conflict-escalation early-return (`escalateJoin`), so it covers exactly the gate-escalation and terminal-failure dispositions and does not double-log a conflict-escalate join (which never retries and is already covered by G). Fields: `attempts` = total attempts tried, `last_err` = the final attempt's error, `escalating` = `gateFailed && gateEscalates(s)` (true when the next signal will be `gate.awaiting` from gate-escalation rather than `step.failed`; the join-escalation case has already returned by this point, so this boolean is exact). This gives instant triage context — neither `step.failed` nor `gate.awaiting` records how many attempts preceded it.

#### F — join start/finish (`execute`)

In the join branch of `execute`, emit `join starting` before `strat.Join(...)` with `strategy` = `s.Join.Strategy` and `inputs` = `len(inputs)`, and `join finished` after, with `err` included only when non-nil. A merge/synthesize over several branches can take minutes (a git merge plus an LLM call per conflicting branch); `step.started` fires once at the attempt's start and cannot distinguish "join running" from "agent running".

#### G — merge conflict detected (`execute`) — Warn

After `strat.Join` returns, inspect the error with `errors.As(err, &conflict)` where `conflict` is `*join.ConflictError` (already imported and handled by the engine's escalation ladder). On a match, emit a `Warn` with `branch` = `conflict.Branch` and `paths` = `conflict.Paths`, giving operators a heads-up at the moment the conflict is detected — before the `gate.awaiting` escalation event. Non-conflict join failures are still covered by F's `join finished` `err`.

### Default-level visibility (deliberate)

E and G are `Warn`, so they appear at the **default `info` level** — the first default-level engine output beyond `Error`. This is intentional: retry-exhaustion and merge-conflicts are notable and rare (not per-step), so the added default-level volume is negligible, and an operator should see them without opting into `debug`. The five Debug lines (A, B, C, D, F) are invisible until `-log-level debug`. The `-log-level` slice's "default = byte-for-byte" guarantee applied to *that* slice; this slice deliberately adds these two default-visible Warn lines. (`Warn` at the default level is not unprecedented — the executor already emits `Warn("artifact discovery failed")`.)

### Why no `Enabled()` guards

`slog` evaluates the argument list eagerly, but every field here is a cheap value already in hand (string ids, ints, a float, durations, a pre-materialized `[]string`). No field requires formatting or computation, and these sites are coarse-grained (per step/attempt/agent-invocation, not hot loops), so the handler's own level check is sufficient — no `logger.Enabled()` pre-checks.

## Testing

- **Engine** (`internal/engine`): inject a Debug-level buffer logger — `slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))` — via `eng.Log`, then assert each new line by `msg` substring and the presence of its key fields (not nondeterministic values like `waited`/`dur`/`delay`), across representative mock flows:
  - a normal mock agent step → `agent starting`, `agent finished` (with `dur`/`cost_usd` keys), `step slot acquired` (with `waited`), `gate evaluated` (`pass=true`);
  - a step that fails to exhaustion (mock agent failing every attempt, small `retry.backoff`) → `step backoff` (with `delay`, `attempt=2`) and the `retry budget exhausted` Warn (with `attempts`, `last_err`);
  - a join flow → `join starting`/`join finished` (`strategy`, `inputs`).
  - The `merge conflict detected` Warn (G) is asserted in the existing conflict-escalation test, which already stages a real git merge conflict — assert the Warn line (with `branch`, `paths`) appears.
- Reuse existing engine test harnesses (mock agents, mem store, mock clock where present); do not build new infrastructure.
- The `backoff` signature change updates its single caller and any test that calls it directly.
- Full `go test -race ./...` green; `go vet` and `gofmt -l` clean.

## Out of scope

- The eighth candidate point — per-dependency-satisfied lines for tracing stuck DAG chains — `step.started` already implies unblocking; dropped to avoid noise.
- Threading a logger into the `gate` or `join` packages (Approach 2) — they stay logger-free.
- Any new SSE event, metric, config flag, or schema change.
- Runtime-adjustable level, per-component levels, OTel tracing.
- Changing existing `Error` log lines or their fields.

## Global constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no new package; no migration; no schema change; no new SSE event kind.
- Pure-additive logging: no change to engine control flow, results, events, or the run lifecycle. The only signature change is `backoff` gaining a `runID` parameter (one caller).
- Field keys reuse the existing convention (`run`, `step`, `agent`, `err`); finish/verdict lines include `err` only when non-nil.
- Debug lines (A, B, C, D, F) are invisible at the default `info` level; the two Warn lines (E, G) are deliberately default-visible.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.
