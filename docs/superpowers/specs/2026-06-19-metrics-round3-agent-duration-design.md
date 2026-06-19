# Observability — metrics round 3 (per-agent run-duration histogram)

## Summary

A small extension of the `internal/metrics` Prometheus endpoint that adds one labeled histogram:

- **`magister_agent_run_duration_seconds{agent}`** — the wall-clock duration of each agent invocation, partitioned by registered agent name.

This completes the per-agent observability triad. Round 1 added the unlabeled agent counters; round 2 labeled them per agent (`magister_agent_tool_calls_total{agent}` = **calls**, `magister_agent_cost_usd_total{agent}` = **spend**). Round 3 adds **latency**.

Same posture as rounds 1–2: stdlib-only, **no new dependency**, no DB migration, no new store method, Go 1.22 floor. A nil `*metrics.Metrics` stays a safe no-op.

## Motivation

We can already answer "how many tool calls and how much money per agent" but not "how long does each agent take." Latency is the third axis operators need to spot a slow or hung agent, compare agent backends (claude vs codex vs gemini), and set timeout budgets. The `HistogramVec` infrastructure already exists (it powers `magister_http_request_duration_seconds{method,route}`), so this is a near-exact mirror of the existing `httpDuration` field — minimal new surface.

## New metric

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `magister_agent_run_duration_seconds` | histogram | **`agent`** | wall-clock of one `ag.Run(...)` invocation, in seconds |

- **Label** `agent` = the registered agent name (the key into the daemon's `e.Execs` registry: `mock`, `opus`, `sonnet`, `gemini`, `codex`). Bounded set → cardinality is safe. Identical label semantics to the round-2 agent counters. No `model`/`role` sub-label.
- **Buckets** (`le`, seconds): `1, 5, 10, 30, 60, 120, 300, 600, 1200` (then `+Inf`). Step-like, plus a 20-minute top bucket because real agents (claude/codex on large tasks) can run well past 10 minutes. No sub-second buckets: only `mock` is that fast and its latency is not interesting.

All round-1 and round-2 metrics are unchanged.

## Behavior

Duration is measured around the single `ag.Run(...)` call at the engine's `runAgent` seam — the **same seam round 2 uses to attribute cost**. Therefore the observation semantics match cost exactly:

- Observed for **every agent invocation**: normal steps, each retried attempt (one `runAgent` call per attempt), and join arbiters.
- Observed **even when `ag.Run` returns an error** — the agent still spent that wall-clock; the duration is real regardless of the orchestrator's success verdict.
- A `merge` join has no agent and never calls `runAgent`, so it contributes no observation (correct).
- The measurement covers `ag.Run` only. Time spent queued on the concurrency semaphore *before* the agent starts, and post-run artifact discovery, are out of scope.

The clock is the engine's injectable `e.Clock` (not `time.Now()`), consistent with the existing `runStart := e.Clock.Now()` capture, so fake-clock unit tests stay deterministic.

## Design

### `internal/metrics` additions (mirror the existing `httpDuration *HistogramVec`)

- **`agentDurBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600, 1200}`** — a new package-level bucket slice beside `runBuckets`/`stepBuckets`/`httpBuckets`.
- **New field** `agentDuration *HistogramVec // label: agent` on the `Metrics` struct.
- **`New`** initializes it: `agentDuration: newHistogramVec(agentDurBuckets)`.
- **New nil-safe method** `ObserveAgentRun(agent string, d time.Duration)`:
  ```go
  func (m *Metrics) ObserveAgentRun(agent string, d time.Duration) {
      if m == nil {
          return
      }
      m.agentDuration.Observe(d.Seconds(), agent)
  }
  ```
- **`WriteProm`** renders it via the existing `writeHistogramVec` helper, placed immediately after the `magister_agent_cost_usd_total{agent}` line so the whole `agent_*` family stays contiguous:
  ```
  writeHistogramVec(w, "magister_agent_run_duration_seconds",
      "Agent invocation wall-clock duration in seconds.",
      []string{"agent"}, m.agentDuration)
  ```
  Resulting family order: …`gates_awaiting_total` → `agent_tool_calls_total{agent}` → `agent_cost_usd_total{agent}` → **`agent_run_duration_seconds{agent}`** → `runs_active` → `steps_active` → `http_requests_in_flight` → `http_requests_total`…

### Engine instrumentation (`runAgent`)

The round-2 cost block becomes a time-and-observe block — one added local, one added call, no control-flow change:

```go
start := e.Clock.Now()
res, err := ag.Run(ctx, core.Task{...})
e.Metrics.ObserveAgentRun(agentName, e.Clock.Now().Sub(start))
e.Metrics.AddCost(agentName, res.CostUSD)
return res, err
```

`ObserveAgentRun` is ordered before `AddCost` for readability (timing wraps the call); both are nil-safe and beside existing code.

## Testing

- **metrics unit:** call `ObserveAgentRun("opus", d)` and assert the rendered `magister_agent_run_duration_seconds_bucket{agent="opus",le="..."}` cumulative series (each `le` ≥ `d.Seconds()` counts 1), plus `_sum` ≈ `d.Seconds()` and `_count` = 1; assert a second agent produces a distinct labeled series. Add the nil-safe `ObserveAgentRun` call to the no-op test (`TestNilMetricsIsNoOp`).
- **engine:** extend the existing engine metrics test (which runs a mock flow and already asserts the per-agent cost `magister_agent_cost_usd_total{agent="mock"}`) to also assert `magister_agent_run_duration_seconds_count{agent="mock"}` equals the number of mock agent invocations that flow makes — the same invocation count the existing cost assertion already implies (one `runAgent` call per mock step; a `merge` join, if any, is never observed). The plan pins the exact number against the test's actual flow. This proves the label + observation wiring end to end.
- Full `go test -race ./...` green; `go vet` + `gofmt` clean.

## Out of scope

- Per-tool duration (this measures the whole `ag.Run`, not individual tool calls).
- Queue/wait time before the agent starts (semaphore acquisition), or post-run artifact discovery.
- A `model` or `role` sub-label (cardinality / free-form-text hazard, as in round 2).
- Any change to the round-1/round-2 metrics, the endpoint, auth-exemption, or route labeling.

## Global constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no DB migration; no new store method.
- A nil `*metrics.Metrics` is a safe no-op for `ObserveAgentRun`.
- New metric name exactly `magister_agent_run_duration_seconds`; the `agent` label value is the registered agent name only.
- Buckets exactly `1, 5, 10, 30, 60, 120, 300, 600, 1200`.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.
