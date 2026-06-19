# Observability — metrics round 2 (in-flight gauges + per-agent labels)

## Summary

A small extension of the round-1 `internal/metrics` Prometheus endpoint that adds the two metric shapes round 1 deliberately deferred:

1. **Three in-flight gauges** — concurrent runs, concurrent steps, and in-flight HTTP requests (current level, up/down).
2. **A `{agent}` label** on the two agent counters (`agent_tool_calls_total`, `agent_cost_usd_total`), with cost attributed per agent invocation.

Same posture as round 1: stdlib-only, **no new dependency**, no DB migration, no new store method, Go 1.22 floor. A nil `*metrics.Metrics` stays a safe no-op everywhere.

## Motivation

Round 1 exposes lifecycle counters/histograms but answers no "how much is happening *right now*" question, and lumps all agent cost/tool activity into one number. Round 2 adds saturation visibility (are we at the concurrency cap? how many runs are live?) and per-agent attribution (which agent is spending the money / making the tool calls).

## New / changed metrics

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `magister_runs_active` | gauge | — | runs between `run.started` and `run.done` |
| `magister_steps_active` | gauge | — | steps currently executing (≈ semaphore utilization) |
| `magister_http_requests_in_flight` | gauge | — | requests inside the handler chain right now |
| `magister_agent_tool_calls_total` | counter | **`agent`** | was unlabeled in round 1; now per registered agent name |
| `magister_agent_cost_usd_total` | counter | **`agent`** | was unlabeled in round 1; now per registered agent name |

The `agent` label value is the **registered agent name** — the key into the daemon's `e.Execs` registry (`mock`, `opus`, `sonnet`, `gemini`, `codex`). This is a bounded set, so cardinality is safe. No `role`/`model` sub-label: `s.Role` is free-form flow-YAML text (unbounded → cardinality hazard) and is intentionally excluded.

All other round-1 metrics are unchanged.

## Behavior change (deliberate, documented)

`magister_agent_cost_usd_total` now sums across **every agent invocation** — including retried attempts and join arbiters — because cost attribution moves to the `runAgent` seam (where the agent name is known). Round 1's cost was added once at the step-terminal site and counted only the final successful attempt of each step. The new total is a more truthful "total spend" and is consistent with how `agent_tool_calls_total` was *already* counted (per invocation in `runAgent`, since round 1). `agent_tool_calls_total`'s count semantics are unchanged by round 2 — it only gains the `agent` label. An unknown agent errors in `runAgent` *before* any metric call, so no junk label is ever recorded; a `merge` join with no agent never calls `runAgent`, so it contributes no agent cost (correct).

## Design

### `internal/metrics` additions

- **`Gauge.Add(delta float64)`** — atomic CAS loop (like `Counter.Add`, but `delta` may be negative). `Inc()` = `Add(1)`, `Dec()` = `Add(-1)`. The existing `Set`/`value` stay.
- **The two agent counters become `*CounterVec`** keyed on label `agent`. `WriteProm` renders them as labeled series, sorted deterministically, and emits only HELP/TYPE (no series) when no agent has run yet — identical to the other vecs.
- **Three `Gauge` fields** for the new families, rendered by `WriteProm` as `gauge` (always present; value can be `0`). Placed in the family render order after the existing agent counters, before the HTTP families.
- **New nil-safe methods:**
  - `RunStarted()` / `RunFinished()` — inc/dec `runs_active`.
  - `StepStarted()` / `StepFinished()` — inc/dec `steps_active`.
  - `HTTPStarted()` / `HTTPFinished()` — inc/dec `http_requests_in_flight`.
  - `AgentTool(agent string)` — `agent_tool_calls_total{agent}`++ (signature gains the `agent` arg).
  - `AddCost(agent string, usd float64)` — `agent_cost_usd_total{agent}` += usd (no-op on `usd == 0`; signature gains the `agent` arg).

Every method keeps the `if m == nil { return }` guard.

### Instrumentation (engine + middleware)

All additions are nil-safe and beside existing code; **no control-flow change**.

- **`runs_active`** — in `runDAG`, immediately after `e.Bus.Publish(runStartedEv)` (the existing `runStart` capture point): `e.Metrics.RunStarted()` then `defer e.Metrics.RunFinished()`. The `defer` guarantees the dec on every return path (canceled / failed / succeeded / early error), so the gauge cannot leak.
- **`steps_active`** — in the per-step goroutine, around the `runStep` call: `e.Metrics.StepStarted()` + `defer e.Metrics.StepFinished()` placed so only real step executions count (after the seed-skip / context-cancel early returns, before the `runStep` block).
- **Per-agent cost + tools** — in `runAgent`: capture `res, err := ag.Run(...)`, then `e.Metrics.AddCost(agentName, res.CostUSD)` before `return res, err`; change the existing milestone call to `e.Metrics.AgentTool(agentName)`. **Remove** the old `e.Metrics.AddCost(res.CostUSD)` at the step-terminal site (it would double-count).
- **`http_requests_in_flight`** — at the top of the `metricsMiddleware` handler: `m.HTTPStarted()` + `defer m.HTTPFinished()`, around `next.ServeHTTP`.

### Lifecycle-safety rationale

Gauges use defer-based inc/dec (RAII) rather than pairing inc/dec with lifecycle event emissions. A counter undercount is forgivable; a gauge that incs but misses its dec on an early-return or panic path *permanently* misreports the live level. The `defer` placed immediately after each `inc` is the robust choice.

## Testing

- **metrics unit:** `Gauge.Add`/`Inc`/`Dec` correctness + a concurrent `-race` test (N goroutines inc then dec → final 0); labeled agent-counter render (`{agent="mock"}` series, sorted, escaped); gauge render present at 0.
- **engine:** after a mock run completes, assert `magister_agent_cost_usd_total{agent="mock"}` is the expected sum, `magister_agent_tool_calls_total` renders per-agent, and `magister_runs_active` / `magister_steps_active` are back to `0` (gauges balanced — proves the defers fired).
- **api:** drive a request through the real server; assert `magister_http_requests_in_flight` exists and reads `0` after the request returns (balanced).
- Full `go test -race ./...` green; `go vet` + `gofmt` clean.

## Out of scope

- Per-agent *duration* histograms.
- `role` / `model` sub-labels (cardinality / free-form-text hazard).
- Gauges for queued or awaiting-gate steps.
- Any change to the round-1 lifecycle counters/histograms, the endpoint, auth-exemption, or route labeling.

## Global constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no DB migration; no new store method.
- A nil `*metrics.Metrics` is a safe no-op for every new method.
- New metric names exactly as in the table above; the `agent` label value is the registered agent name only.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.
