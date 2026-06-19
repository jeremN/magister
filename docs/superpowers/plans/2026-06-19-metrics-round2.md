# Metrics round 2 — in-flight gauges + per-agent labels Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend the round-1 `internal/metrics` endpoint with three in-flight gauges (active runs, active steps, in-flight HTTP requests) and an `{agent}` label on the two agent counters, with cost re-attributed per agent invocation.

**Architecture:** Add a signed `Gauge.Add`/`Inc`/`Dec` primitive; convert the two agent `Counter` fields to `*CounterVec` (label `agent`); add three `Gauge` fields. Instrument the engine (runs/steps gauges via defer-based inc/dec; cost+tool labeled at the `runAgent` seam) and the HTTP middleware (in-flight gauge via defer). Nil `*metrics.Metrics` stays a safe no-op.

**Tech Stack:** Go 1.22 stdlib only (`sync/atomic`, `net/http`). No Prometheus client library; the text format is hand-rolled (round 1).

## Global Constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no DB migration; no new store method.
- A nil `*metrics.Metrics` is a safe no-op for every new/changed method (each begins `if m == nil { return }`).
- New metric names EXACTLY: `magister_runs_active`, `magister_steps_active`, `magister_http_requests_in_flight` (all `gauge`). The `agent` label value is the registered agent name only (the `e.Execs` key — bounded). No `role`/`model` sub-label.
- **Deliberate behavior change:** `magister_agent_cost_usd_total` moves from the step-terminal site to the `runAgent` seam → now sums every agent invocation (retries + arbiters), labeled `{agent}`. `magister_agent_tool_calls_total` keeps its per-invocation count semantics and only gains the `{agent}` label.
- `magister_http_requests_in_flight` counts the in-flight `/metrics` scrape itself (standard); a scrape observes a minimum of 1.
- Commits: single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. `gofmt`/`go vet`/`go test -race ./...` clean before merge.

## File Structure

- `internal/metrics/primitives.go` — **modify**: add `Gauge.Add`/`Inc`/`Dec`.
- `internal/metrics/primitives_test.go` — **modify**: add gauge add/inc/dec + concurrency tests.
- `internal/metrics/metrics.go` — **modify**: agent counters → `*CounterVec`; 3 gauge fields; changed `AgentTool(agent)`/`AddCost(agent,usd)`; new `RunStarted`/`RunFinished`/`StepStarted`/`StepFinished`/`HTTPStarted`/`HTTPFinished`; `WriteProm` render updates.
- `internal/metrics/metrics_test.go` — **modify**: migrate round-1 calls to the new signatures; add labeled-render + gauge tests.
- `internal/engine/engine.go` — **modify**: runs/steps gauges (defer inc/dec); `AgentTool(agentName)`; cost at `runAgent`; remove line-202 `AddCost`.
- `internal/engine/metrics_test.go` — **modify**: assert labeled cost + balanced gauges.
- `internal/api/middleware.go` — **modify**: in-flight gauge in `metricsMiddleware`.
- `internal/api/metrics_test.go` — **modify**: assert `http_requests_in_flight` == 1 after completed requests.

---

### Task 1: `Gauge.Add`/`Inc`/`Dec` primitive

**Files:**
- Modify: `internal/metrics/primitives.go`
- Modify: `internal/metrics/primitives_test.go`

**Interfaces:**
- Consumes: the existing `Gauge` struct (`bits atomic.Uint64`).
- Produces: `func (g *Gauge) Add(delta float64)`, `func (g *Gauge) Inc()`, `func (g *Gauge) Dec()`. (`Set`/`value` unchanged.)

- [ ] **Step 1: Write the failing test**

Add to `internal/metrics/primitives_test.go`:

```go
func TestGaugeAddIncDec(t *testing.T) {
	var g Gauge
	g.Inc()
	g.Inc()
	g.Dec()
	if got := g.value(); got != 1 {
		t.Errorf("after inc,inc,dec = %v, want 1", got)
	}
	g.Add(2.5)
	g.Add(-0.5)
	if got := g.value(); got != 3 {
		t.Errorf("after +2.5,-0.5 = %v, want 3", got)
	}
}

func TestGaugeConcurrentBalanced(t *testing.T) {
	var g Gauge
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				g.Inc()
				g.Dec()
			}
		}()
	}
	wg.Wait()
	if got := g.value(); got != 0 {
		t.Errorf("balanced inc/dec = %v, want 0", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/metrics/ -run 'TestGauge'`
Expected: FAIL — `g.Inc`/`g.Dec`/`g.Add` undefined.

- [ ] **Step 3: Implement**

In `internal/metrics/primitives.go`, replace the two-line `Gauge` method block:

```go
func (g *Gauge) Set(v float64)  { g.bits.Store(math.Float64bits(v)) }
func (g *Gauge) value() float64 { return math.Float64frombits(g.bits.Load()) }
```

with:

```go
func (g *Gauge) Set(v float64)  { g.bits.Store(math.Float64bits(v)) }
func (g *Gauge) value() float64 { return math.Float64frombits(g.bits.Load()) }

// Add applies a signed delta (CAS loop); delta may be negative.
func (g *Gauge) Add(delta float64) {
	for {
		old := g.bits.Load()
		nv := math.Float64frombits(old) + delta
		if g.bits.CompareAndSwap(old, math.Float64bits(nv)) {
			return
		}
	}
}

func (g *Gauge) Inc() { g.Add(1) }
func (g *Gauge) Dec() { g.Add(-1) }
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/metrics/ -run 'TestGauge'`
Expected: PASS (incl. the `-race` balanced test).

- [ ] **Step 5: gofmt + commit**

```bash
gofmt -l internal/metrics/   # must be empty
git add internal/metrics/primitives.go internal/metrics/primitives_test.go
git commit -m "feat(metrics): signed Gauge.Add/Inc/Dec"
```

---

### Task 2: Registry — per-agent label + in-flight gauges

**Files:**
- Modify: `internal/metrics/metrics.go`
- Modify: `internal/metrics/metrics_test.go`

**Interfaces:**
- Consumes: `Gauge.Inc`/`Dec` (Task 1), the existing `CounterVec`/`writeCounterVec`/`writeGauge`.
- Produces: changed `func (m *Metrics) AgentTool(agent string)`, `func (m *Metrics) AddCost(agent string, usd float64)`; new `RunStarted()`/`RunFinished()`/`StepStarted()`/`StepFinished()`/`HTTPStarted()`/`HTTPFinished()`. Renders `magister_agent_{tool_calls_total,cost_usd_total}{agent}` and the 3 gauges.

- [ ] **Step 1: Update the round-1 tests + write the new failing tests**

In `internal/metrics/metrics_test.go`, the round-1 tests call the old signatures — migrate them and add new assertions. Make these exact changes:

In `TestNilMetricsIsNoOp`, replace:
```go
	m.AgentTool()
	m.AddCost(1.5)
```
with:
```go
	m.AgentTool("mock")
	m.AddCost("mock", 1.5)
	m.RunStarted()
	m.RunFinished()
	m.StepStarted()
	m.StepFinished()
	m.HTTPStarted()
	m.HTTPFinished()
```

In `TestRendersCountersAndHistogram`, replace:
```go
	m.AgentTool()
	m.AgentTool()
	m.AddCost(0.25)
```
with:
```go
	m.AgentTool("opus")
	m.AgentTool("opus")
	m.AddCost("opus", 0.25)
	m.RunStarted()
	m.StepStarted()
```
and replace these two expected-substring lines:
```go
		"magister_agent_tool_calls_total 2",
		"magister_agent_cost_usd_total 0.25",
```
with:
```go
		`magister_agent_tool_calls_total{agent="opus"} 2`,
		`magister_agent_cost_usd_total{agent="opus"} 0.25`,
		"# TYPE magister_runs_active gauge",
		"magister_runs_active 1",
		"magister_steps_active 1",
		"# TYPE magister_http_requests_in_flight gauge",
		"magister_http_requests_in_flight 0",
```

Then append a new test:
```go
func TestAgentLabelAndGaugeBalance(t *testing.T) {
	m := New("v")
	m.AddCost("opus", 0.10)
	m.AddCost("sonnet", 0.02)
	m.AgentTool("opus")
	m.HTTPStarted()
	m.HTTPStarted()
	m.HTTPFinished()
	out := scrape(m)
	for _, want := range []string{
		`magister_agent_cost_usd_total{agent="opus"} 0.1`,
		`magister_agent_cost_usd_total{agent="sonnet"} 0.02`,
		`magister_agent_tool_calls_total{agent="opus"} 1`,
		"magister_http_requests_in_flight 1", // 2 started, 1 finished
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape missing %q\n---\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/metrics/`
Expected: FAIL — `AgentTool`/`AddCost` arity mismatch + `RunStarted` etc. undefined.

- [ ] **Step 3: Change the struct + New**

In `internal/metrics/metrics.go`, change the agent fields and add the gauges. Replace:

```go
	gatesAwaiting Counter
	agentTools    Counter
	agentCost     Counter

	httpRequests *CounterVec   // labels: method, route, status
	httpDuration *HistogramVec // labels: method, route
}
```

with:

```go
	gatesAwaiting Counter
	agentTools    *CounterVec // label: agent
	agentCost     *CounterVec // label: agent

	runsActive   Gauge
	stepsActive  Gauge
	httpInFlight Gauge

	httpRequests *CounterVec   // labels: method, route, status
	httpDuration *HistogramVec // labels: method, route
}
```

In `New`, add the two vec initializers (gauges are zero-valued — no init needed). Replace:

```go
		stepsTotal:   newCounterVec(),
		stepDuration: newHistogram(stepBuckets),
		httpRequests: newCounterVec(),
```

with:

```go
		stepsTotal:   newCounterVec(),
		stepDuration: newHistogram(stepBuckets),
		agentTools:   newCounterVec(),
		agentCost:    newCounterVec(),
		httpRequests: newCounterVec(),
```

- [ ] **Step 4: Change the methods + add gauge methods**

Replace the `AgentTool` and `AddCost` methods:

```go
func (m *Metrics) AgentTool() {
	if m == nil {
		return
	}
	m.agentTools.Add(1)
}

func (m *Metrics) AddCost(usd float64) {
	if m == nil || usd == 0 {
		return
	}
	m.agentCost.Add(usd)
}
```

with:

```go
func (m *Metrics) AgentTool(agent string) {
	if m == nil {
		return
	}
	m.agentTools.Add(1, agent)
}

func (m *Metrics) AddCost(agent string, usd float64) {
	if m == nil || usd == 0 {
		return
	}
	m.agentCost.Add(usd, agent)
}

// RunStarted/RunFinished bracket a run; the gauge reflects concurrent runs.
func (m *Metrics) RunStarted() {
	if m == nil {
		return
	}
	m.runsActive.Inc()
}

func (m *Metrics) RunFinished() {
	if m == nil {
		return
	}
	m.runsActive.Dec()
}

// StepStarted/StepFinished bracket a step execution; the gauge reflects concurrent steps.
func (m *Metrics) StepStarted() {
	if m == nil {
		return
	}
	m.stepsActive.Inc()
}

func (m *Metrics) StepFinished() {
	if m == nil {
		return
	}
	m.stepsActive.Dec()
}

// HTTPStarted/HTTPFinished bracket a request; the gauge reflects in-flight requests.
func (m *Metrics) HTTPStarted() {
	if m == nil {
		return
	}
	m.httpInFlight.Inc()
}

func (m *Metrics) HTTPFinished() {
	if m == nil {
		return
	}
	m.httpInFlight.Dec()
}
```

- [ ] **Step 5: Update WriteProm render**

In `WriteProm`, replace the two agent `writeCounterRaw` lines:

```go
	writeCounterRaw(w, "magister_agent_tool_calls_total", "Total agent tool-use milestones.", m.agentTools.value())
	writeCounterRaw(w, "magister_agent_cost_usd_total", "Total agent cost in USD.", m.agentCost.value())
```

with the labeled-vec renders plus the three gauges (this is the spec's "after agent counters, before HTTP families" position):

```go
	writeCounterVec(w, "magister_agent_tool_calls_total", "Total agent tool-use milestones by agent.", []string{"agent"}, m.agentTools)
	writeCounterVec(w, "magister_agent_cost_usd_total", "Total agent cost in USD by agent.", []string{"agent"}, m.agentCost)
	writeGauge(w, "magister_runs_active", "Runs currently executing.", m.runsActive.value())
	writeGauge(w, "magister_steps_active", "Steps currently executing.", m.stepsActive.value())
	writeGauge(w, "magister_http_requests_in_flight", "HTTP requests currently being served.", m.httpInFlight.value())
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -race ./internal/metrics/`
Expected: PASS (migrated round-1 tests + new label/gauge tests).

- [ ] **Step 7: gofmt + vet + commit**

```bash
gofmt -l internal/metrics/ && go vet ./internal/metrics/
git add internal/metrics/metrics.go internal/metrics/metrics_test.go
git commit -m "feat(metrics): per-agent labels + in-flight gauges"
```

---

### Task 3: Engine instrumentation

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/metrics_test.go`

**Interfaces:**
- Consumes: `RunStarted`/`RunFinished`/`StepStarted`/`StepFinished`/`AgentTool(agent)`/`AddCost(agent,usd)` (Task 2).
- Produces: engine populates `runs_active`/`steps_active` and labels agent cost/tools.

- [ ] **Step 1: runs_active gauge (defer-based)**

In `internal/engine/engine.go`, in `runDAG`, after the run-start capture. Replace:

```go
	e.Bus.Publish(runStartedEv)
	runStart := e.Clock.Now()
```

with:

```go
	e.Bus.Publish(runStartedEv)
	runStart := e.Clock.Now()
	e.Metrics.RunStarted()
	defer e.Metrics.RunFinished()
```

- [ ] **Step 2: steps_active gauge (defer-based) + remove line-202 cost**

In the per-step goroutine, replace this block:

```go
			if ctx.Err() != nil {
				return
			}

			// 4. run the step (execute + gate, with retries).
			stepStart := e.Clock.Now()
			res, err := e.runStep(ctx, runID, s, inputs)
			stepDur := e.Clock.Now().Sub(stepStart)
			if err != nil {
				e.Metrics.ObserveStep("failed", stepDur)
				fail(fmt.Errorf("step %q: %w", s.ID, err))
				return
			}
			e.Metrics.ObserveStep("succeeded", stepDur)
			e.Metrics.AddCost(res.CostUSD)
			mu.Lock()
			results[s.ID] = res
			mu.Unlock()
```

with (add the gauge pair; DELETE the `AddCost` line — cost now lives in `runAgent`):

```go
			if ctx.Err() != nil {
				return
			}
			e.Metrics.StepStarted()
			defer e.Metrics.StepFinished()

			// 4. run the step (execute + gate, with retries).
			stepStart := e.Clock.Now()
			res, err := e.runStep(ctx, runID, s, inputs)
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
```

- [ ] **Step 3: Label tool calls + attribute cost at the runAgent seam**

In `runAgent`, change the milestone count. Replace:

```go
		if ev.Kind == event.AgentTool {
			e.Metrics.AgentTool()
		}
```

with:

```go
		if ev.Kind == event.AgentTool {
			e.Metrics.AgentTool(agentName)
		}
```

Then replace the return:

```go
	return ag.Run(ctx, core.Task{
		RunID:   runID,
		StepID:  stepID,
		Role:    role,
		Prompt:  prompt,
		Inputs:  inputs,
		WorkDir: workDir,
		Emit:    emit,
	})
}
```

with (capture the result so its cost is attributed to this agent invocation):

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

- [ ] **Step 4: Update the engine metrics test**

In `internal/engine/metrics_test.go`, the existing `TestEngineRecordsMetrics` runs a one-step `mock` flow. Add assertions for the new label + balanced gauges. Replace its `for _, want := range []string{ ... }` slice contents with:

```go
	for _, want := range []string{
		`magister_runs_total{status="succeeded"} 1`,
		`magister_steps_total{status="succeeded"} 1`,
		"magister_run_duration_seconds_count 1",
		"magister_step_duration_seconds_count 1",
		`magister_agent_cost_usd_total{agent="mock"}`, // mock has nonzero cost → labeled series exists
		"magister_runs_active 0",                       // balanced: defer RunFinished fired
		"magister_steps_active 0",                      // balanced: defer StepFinished fired
	} {
```

(`eng.Run` is synchronous and `runDAG` waits for all step goroutines before returning, so both gauges are back to 0 by the time the test scrapes.)

- [ ] **Step 5: Run + verify**

Run: `go test -race ./internal/engine/`
Expected: PASS (existing engine tests unaffected — `Metrics` nil there; the updated test asserts the label + balance).

- [ ] **Step 6: gofmt + commit**

```bash
gofmt -l internal/engine/ && go vet ./internal/engine/
git add internal/engine/engine.go internal/engine/metrics_test.go
git commit -m "feat(engine): active-run/step gauges + per-agent cost/tool labels"
```

---

### Task 4: HTTP in-flight gauge + full suite

**Files:**
- Modify: `internal/api/middleware.go`
- Modify: `internal/api/metrics_test.go`

**Interfaces:**
- Consumes: `HTTPStarted`/`HTTPFinished` (Task 2), the existing `metricsMiddleware`.
- Produces: `magister_http_requests_in_flight` populated per request.

- [ ] **Step 1: Instrument the middleware**

In `internal/api/middleware.go`, in `metricsMiddleware`, add the gauge pair at the top of the handler. Replace:

```go
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			m.ObserveHTTP(r.Method, routeLabel(r, routes), rec.status, time.Since(start))
		})
```

with:

```go
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.HTTPStarted()
			defer m.HTTPFinished()
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			m.ObserveHTTP(r.Method, routeLabel(r, routes), rec.status, time.Since(start))
		})
```

- [ ] **Step 2: Write the failing test**

In `internal/api/metrics_test.go`, append:

```go
func TestMetricsHTTPInFlightBalanced(t *testing.T) {
	hs, _, _ := testServer(t)
	// drive several COMPLETED requests; each must inc then dec the gauge
	for i := 0; i < 3; i++ {
		must200(t, hs.URL+"/v1/runs")
	}
	body, _ := scrapeMetrics(t, hs.URL)
	// The /metrics scrape itself is in flight while rendering, so the gauge reads
	// exactly 1 (only the scrape). A leak from the prior 3 requests would show 2+.
	if !strings.Contains(body, "magister_http_requests_in_flight 1\n") {
		t.Errorf("want in_flight == 1 (only the scrape), prior requests leaked?\n%s", body)
	}
}
```

(`must200`, `scrapeMetrics`, and `testServer` already exist in this file / `handlers_test.go` from round 1.)

- [ ] **Step 3: Run to verify it fails, then passes**

Run: `go test ./internal/api/ -run 'TestMetricsHTTPInFlight'`
Expected: FAIL before Step 1's edit is applied (gauge absent → `0`, not `1`); PASS after. (If implementing Step 1 first, it passes directly — then briefly revert mentally to confirm the assertion is load-bearing: without `HTTPStarted`/`HTTPFinished` the gauge renders `0`.)

- [ ] **Step 4: Full suite + vet + gofmt**

Run: `gofmt -l internal && go vet ./... && go test -race ./...`
Expected: `gofmt -l` empty; vet clean; ALL packages PASS (17 packages; report the pass count).

- [ ] **Step 5: Commit**

```bash
git add internal/api/middleware.go internal/api/metrics_test.go
git commit -m "feat(api): http_requests_in_flight gauge"
```

---

## Self-Review

**1. Spec coverage** (against `docs/superpowers/specs/2026-06-19-metrics-round2-design.md`):
- Three in-flight gauges (`runs_active`/`steps_active`/`http_requests_in_flight`) → Task 2 (fields + methods + render), Task 3 (runs/steps instrumentation), Task 4 (http instrumentation). ✓
- `{agent}` label on `agent_tool_calls_total` + `agent_cost_usd_total` → Task 2 (vec + render), Task 3 (`AgentTool(agentName)` + `AddCost(agentName,…)` at `runAgent`). ✓
- Cost re-attributed per invocation; line-202 `AddCost` removed → Task 3 Steps 2–3. ✓
- `Gauge.Add`/`Inc`/`Dec` primitive → Task 1. ✓
- Defer-based gauge lifecycle safety → Task 3 Steps 1–2 (`defer RunFinished`/`StepFinished`), Task 4 Step 1 (`defer HTTPFinished`). ✓
- Behavior-change documented; tool-call count semantics unchanged (only label added) → Global Constraints + Task 2 (tool calls already per-invocation; only signature/label change). ✓
- `/metrics` self-counts in in-flight → Task 4 test asserts `== 1`. ✓
- Testing (gauge unit+race, labeled render, engine balance, api in-flight) → Tasks 1–4. ✓
- Out-of-scope (per-agent duration, role/model labels, queued/awaiting gauges) → not built. ✓
- Global constraints (no dep, no migration, no store method, nil-safe, exact names) → held; no task touches `go.mod`/migrations/store. ✓

**2. Placeholder scan:** No TBD/TODO; every code step shows complete code. Edit anchors quote the exact current lines (verified against the files). ✓

**3. Type consistency:** `AgentTool(agent string)` / `AddCost(agent string, usd float64)` and the six gauge methods are defined in Task 2 and called with matching arity in Tasks 3 (engine) and 4 (api/middleware). The struct fields `agentTools`/`agentCost` (now `*CounterVec`) are constructed in `New` (Task 2) and rendered via `writeCounterVec` (Task 2). `runsActive`/`stepsActive`/`httpInFlight` (`Gauge`) are inc/dec'd via the methods and rendered via `writeGauge`. `Gauge.Inc`/`Dec`/`Add` (Task 1) back all six gauge methods. ✓
