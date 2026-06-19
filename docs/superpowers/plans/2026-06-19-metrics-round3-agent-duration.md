# Metrics Round 3 — per-agent run-duration histogram Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a labeled histogram `magister_agent_run_duration_seconds{agent}` recording the wall-clock of each agent invocation, observed at the engine's `runAgent` seam.

**Architecture:** Mirror the existing `httpDuration *HistogramVec` field end-to-end (the `HistogramVec` type, `newHistogramVec` constructor, `Observe` method, and `writeHistogramVec` renderer already exist and power `magister_http_request_duration_seconds`). One new metrics method `ObserveAgentRun`; one timing block added to `runAgent` beside the round-2 cost call. Two tasks: (1) the metrics registry + render + unit tests; (2) the engine instrumentation + engine test + full suite.

**Tech Stack:** Go 1.22, standard library only. Package `internal/metrics` (hand-rolled Prometheus registry) and `internal/engine`.

## Global Constraints

- Go 1.22; **stdlib only, NO new dependency** (do not touch `go.mod`); no DB migration; no new store method.
- A nil `*metrics.Metrics` is a safe no-op for every method, including the new `ObserveAgentRun`.
- New metric name EXACTLY `magister_agent_run_duration_seconds`; type `histogram`; one label `agent` whose value is the registered agent name passed through verbatim (no transformation, no `model`/`role` sub-label).
- Buckets EXACTLY `[]float64{1, 5, 10, 30, 60, 120, 300, 600, 1200}`.
- `WriteProm` render order places the new histogram immediately AFTER `magister_agent_cost_usd_total{agent}` and BEFORE `magister_runs_active`, keeping the `agent_*` family contiguous.
- Measure with the engine's injectable clock `e.Clock` (not `time.Now()`); observe every invocation, even when `ag.Run` returns an error.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`. `gofmt -l`, `go vet`, `go test -race ./...` clean before merge.

## File Structure

- `internal/metrics/metrics.go` — add `agentDurBuckets`, the `agentDuration *HistogramVec` struct field, its `New` initializer, the `ObserveAgentRun` method, and one `writeHistogramVec` line in `WriteProm`. (Task 1)
- `internal/metrics/metrics_test.go` — extend `TestNilMetricsIsNoOp`; add `TestAgentRunDurationHistogram`. (Task 1)
- `internal/engine/engine.go` — time the `ag.Run(...)` call in `runAgent` and call `ObserveAgentRun`. (Task 2)
- `internal/engine/metrics_test.go` — add one assertion to `TestEngineRecordsMetrics`. (Task 2)

---

### Task 1: metrics registry — `agentDuration` histogram + `ObserveAgentRun`

**Files:**
- Modify: `internal/metrics/metrics.go`
- Test: `internal/metrics/metrics_test.go`

**Interfaces:**
- Consumes: the existing `HistogramVec` type, `newHistogramVec(bounds []float64) *HistogramVec`, `(*HistogramVec).Observe(val float64, labelValues ...string)`, and `writeHistogramVec(w io.Writer, name, help string, labelNames []string, hv *HistogramVec)` — all already in `internal/metrics`.
- Produces: `func (m *Metrics) ObserveAgentRun(agent string, d time.Duration)` — nil-safe; later consumed by `internal/engine`. New metric family `magister_agent_run_duration_seconds{agent}` in `WriteProm` output.

- [ ] **Step 1: Write the failing tests**

In `internal/metrics/metrics_test.go`, add this new test function (the file already imports `strings`, `testing`, and `time`):

```go
func TestAgentRunDurationHistogram(t *testing.T) {
	m := New("v")
	m.ObserveAgentRun("opus", 3*time.Second)   // lands in le="5"
	m.ObserveAgentRun("opus", 700*time.Second) // > 600, <= 1200 → le="1200"
	m.ObserveAgentRun("codex", 8*time.Second)  // distinct agent series
	out := scrape(m)
	for _, want := range []string{
		"# TYPE magister_agent_run_duration_seconds histogram",
		`magister_agent_run_duration_seconds_bucket{agent="opus",le="5"} 1`,
		`magister_agent_run_duration_seconds_bucket{agent="opus",le="600"} 1`,
		`magister_agent_run_duration_seconds_bucket{agent="opus",le="1200"} 2`,
		`magister_agent_run_duration_seconds_bucket{agent="opus",le="+Inf"} 2`,
		`magister_agent_run_duration_seconds_sum{agent="opus"} 703`,
		`magister_agent_run_duration_seconds_count{agent="opus"} 2`,
		`magister_agent_run_duration_seconds_count{agent="codex"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape missing %q\n---\n%s", want, out)
		}
	}
}
```

Also add the nil-safe call to the existing `TestNilMetricsIsNoOp`, immediately after the `m.AddCost("mock", 1.5)` line (around line 24):

```go
	m.ObserveAgentRun("mock", time.Second)
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/metrics/ -run 'TestAgentRunDurationHistogram|TestNilMetricsIsNoOp'`
Expected: compile failure — `m.ObserveAgentRun undefined (type *Metrics has no field or method ObserveAgentRun)`.

- [ ] **Step 3: Add the bucket slice**

In `internal/metrics/metrics.go`, the `var ( ... )` block currently reads:

```go
var (
	runBuckets  = []float64{5, 30, 60, 300, 600, 1800, 3600}
	stepBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600}
	httpBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}
)
```

Add the agent bucket slice:

```go
var (
	runBuckets      = []float64{5, 30, 60, 300, 600, 1800, 3600}
	stepBuckets     = []float64{1, 5, 10, 30, 60, 120, 300, 600}
	httpBuckets     = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}
	agentDurBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600, 1200}
)
```

- [ ] **Step 4: Add the struct field**

In the `Metrics` struct, the agent counters currently read:

```go
	agentTools    *CounterVec // label: agent
	agentCost     *CounterVec // label: agent
```

Add the histogram field directly below them (keep the `agent_*` family together):

```go
	agentTools    *CounterVec   // label: agent
	agentCost     *CounterVec   // label: agent
	agentDuration *HistogramVec // label: agent
```

- [ ] **Step 5: Initialize it in `New`**

In `New`, after the `agentCost: newCounterVec(),` line, add:

```go
		agentCost:     newCounterVec(),
		agentDuration: newHistogramVec(agentDurBuckets),
```

(Re-run `gofmt -w internal/metrics/metrics.go` after editing — the struct-literal alignment will shift.)

- [ ] **Step 6: Add the `ObserveAgentRun` method**

In `internal/metrics/metrics.go`, immediately after the `AddCost` method (which ends at the `}` after `m.agentCost.Add(usd, agent)`), add:

```go
// ObserveAgentRun records the wall-clock duration of one agent invocation,
// labeled by registered agent name. Observed for every invocation (incl.
// retries and join arbiters), even when the invocation returned an error.
func (m *Metrics) ObserveAgentRun(agent string, d time.Duration) {
	if m == nil {
		return
	}
	m.agentDuration.Observe(d.Seconds(), agent)
}
```

- [ ] **Step 7: Render it in `WriteProm`**

In `WriteProm`, the two agent-counter lines and the first gauge line currently read:

```go
	writeCounterVec(w, "magister_agent_tool_calls_total", "Total agent tool-use milestones by agent.", []string{"agent"}, m.agentTools)
	writeCounterVec(w, "magister_agent_cost_usd_total", "Total agent cost in USD by agent.", []string{"agent"}, m.agentCost)
	writeGauge(w, "magister_runs_active", "Runs currently executing.", m.runsActive.value())
```

Insert the histogram render between the `agent_cost` line and the `runs_active` line:

```go
	writeCounterVec(w, "magister_agent_tool_calls_total", "Total agent tool-use milestones by agent.", []string{"agent"}, m.agentTools)
	writeCounterVec(w, "magister_agent_cost_usd_total", "Total agent cost in USD by agent.", []string{"agent"}, m.agentCost)
	writeHistogramVec(w, "magister_agent_run_duration_seconds", "Agent invocation wall-clock duration in seconds.", []string{"agent"}, m.agentDuration)
	writeGauge(w, "magister_runs_active", "Runs currently executing.", m.runsActive.value())
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test -race ./internal/metrics/`
Expected: PASS (all metrics tests, including the new `TestAgentRunDurationHistogram` and the updated `TestNilMetricsIsNoOp`).

- [ ] **Step 9: Verify formatting and vet**

Run: `gofmt -l internal/metrics/ && go vet ./internal/metrics/`
Expected: no output from `gofmt -l` (clean); `go vet` clean.

- [ ] **Step 10: Commit**

```bash
git add internal/metrics/metrics.go internal/metrics/metrics_test.go
git commit -m "feat(metrics): per-agent run-duration histogram"
```

---

### Task 2: engine — observe agent run duration at the `runAgent` seam

**Files:**
- Modify: `internal/engine/engine.go` (function `runAgent`, around lines 384–394)
- Test: `internal/engine/metrics_test.go` (function `TestEngineRecordsMetrics`)

**Interfaces:**
- Consumes: `(*metrics.Metrics).ObserveAgentRun(agent string, d time.Duration)` from Task 1; the engine's existing `e.Clock` (has `Now() time.Time`) and `e.Metrics` (a nil-safe `*metrics.Metrics`).
- Produces: nothing new (terminal task).

- [ ] **Step 1: Update the engine test (failing assertion first)**

In `internal/engine/metrics_test.go`, the `TestEngineRecordsMetrics` `want` slice currently ends with the `magister_steps_active 0` line. Add one assertion to that slice (the flow has exactly one mock step → exactly one `runAgent` call):

```go
		"magister_runs_active 0",                                  // balanced: defer RunFinished fired
		"magister_steps_active 0",                                 // balanced: defer StepFinished fired
		`magister_agent_run_duration_seconds_count{agent="mock"} 1`, // one mock invocation observed
	} {
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/engine/ -run TestEngineRecordsMetrics`
Expected: FAIL — `metrics missing "magister_agent_run_duration_seconds_count{agent=\"mock\"} 1"` (the family is absent until the engine observes it).

- [ ] **Step 3: Time the agent call in `runAgent`**

In `internal/engine/engine.go`, the tail of `runAgent` currently reads:

```go
	res, err := ag.Run(ctx, core.Task{
		RunID:   runID,
		StepID:  stepID,
		Role:    role,
		Prompt:  prompt,
		Inputs:  inputs,
		WorkDir: workDir,
		Emit:    emit,
	})
	e.Metrics.AddCost(agentName, res.CostUSD) // per-invocation; no-op on 0 cost
	return res, err
}
```

Capture the start time before the call and observe the elapsed duration after it:

```go
	agentStart := e.Clock.Now()
	res, err := ag.Run(ctx, core.Task{
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
}
```

- [ ] **Step 4: Run the engine test to verify it passes**

Run: `go test -race ./internal/engine/ -run TestEngineRecordsMetrics`
Expected: PASS.

- [ ] **Step 5: Run the full engine package and verify formatting/vet**

Run: `go test -race ./internal/engine/ && gofmt -l internal/engine/ && go vet ./internal/engine/`
Expected: all engine tests PASS; no `gofmt -l` output; `go vet` clean.

- [ ] **Step 6: Run the whole suite**

Run: `go test -race ./...`
Expected: ALL packages PASS (17 packages). Report the pass count.

- [ ] **Step 7: Commit**

```bash
git add internal/engine/engine.go internal/engine/metrics_test.go
git commit -m "feat(engine): observe agent run duration at the runAgent seam"
```

---

## Notes for the implementer

- The `agent` label value is whatever string is passed — no transformation. The engine only ever calls `ObserveAgentRun` with a registered `e.Execs` key (an unknown agent errors earlier in `runAgent`), so cardinality is bounded.
- Do NOT add an `if err == nil` guard around `ObserveAgentRun` — observing on error is required by the spec (the agent still spent the wall-clock).
- Bucket math for the Task 1 test: `3s` → first bucket ≥ 3 is `le="5"`; `700s` is `> 600` and `≤ 1200` → `le="1200"`. Cumulative: `le="600"` counts only the 3s sample (1), `le="1200"` counts both (2), `_sum` = `3 + 700 = 703`, `_count` = 2.
- The post-Edit hook emits a harmless path-doubling error on worktree edits; the edit still succeeds.
