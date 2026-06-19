# Observability — Prometheus /metrics endpoint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a scrapeable `GET /metrics` endpoint (Prometheus text format, zero new deps) exposing run/step lifecycle, HTTP, agent cost/tool, and Go-runtime/build metrics.

**Architecture:** A new stdlib-only `internal/metrics` package with hand-rolled `Counter`/`Gauge`/`Histogram` primitives and a fixed-field `Metrics` registry. The Engine and the API Server each hold an optional `*metrics.Metrics` (mirroring how they already hold an optional `*slog.Logger`); a nil `*Metrics` is a safe no-op. The engine increments domain metrics at its existing lifecycle points, the HTTP middleware records request metrics labeled by route template, and the auth-exempt `/metrics` handler renders everything (runtime/build read on-scrape).

**Tech Stack:** Go 1.22 stdlib only (`sync/atomic`, `runtime`, `runtime/debug`, `net/http` ServeMux). No Prometheus client library — the text-exposition format is hand-rolled.

## Global Constraints

- Go 1.22; **stdlib-only, no new Go dependency** (do not touch `go.mod`); **no DB migration; no new store method; no engine control-flow change** (only nil-safe metric calls beside existing event emissions).
- **`http.Request.Pattern` is Go 1.23 and MUST NOT be used.** Derive the route label via `(*http.ServeMux).Handler(r)` (Go 1.22-safe).
- `GET /metrics` is **auth-exempt** (registered outside the `/v1` auth+timeout group, like `/healthz`).
- A nil `*metrics.Metrics` is a safe no-op for every method (each begins `if m == nil { return }`), so tests need not wire it and the daemon wires one instance shared by engine + server.
- Metric names/labels/buckets are EXACTLY as specified (the spec is the source of truth).
- Commits: single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. `gofmt`/`go vet`/`go test -race ./...` clean before merge.

## File Structure

- `internal/metrics/primitives.go` — **create**: `Counter`, `Gauge`, `Histogram`, `CounterVec`, `HistogramVec` + `joinKey`.
- `internal/metrics/primitives_test.go` — **create**: primitive unit + concurrency tests.
- `internal/metrics/metrics.go` — **create**: the `Metrics` registry (families, `New`, instrumentation methods, `WriteProm`), the Prometheus render helpers, `BuildVersion`.
- `internal/metrics/metrics_test.go` — **create**: render-format + concurrency tests.
- `internal/engine/engine.go` — **modify**: add `Metrics *metrics.Metrics` field + 6 nil-safe instrumentation calls.
- `internal/engine/metrics_test.go` — **create**: engine-instrumentation test (mock run → scrape).
- `internal/api/handlers.go` — **modify**: add `Metrics *metrics.Metrics` to `Server` + `handleMetrics`.
- `internal/api/middleware.go` — **modify**: add `metricsMiddleware` + `routeLabel` + `stripMethod`.
- `internal/api/router.go` — **modify**: register `GET /metrics`; insert `metricsMiddleware` into the outer chain.
- `internal/api/metrics_test.go` — **create**: endpoint + route-label + auth-exempt tests.
- `internal/api/handlers_test.go` — **modify**: set `Metrics` in `newServerOnly` so api tests exercise the middleware.
- `cmd/magisterd/main.go` — **modify**: construct `metrics.New(metrics.BuildVersion())`; wire into Engine + Server.

---

### Task 1: `internal/metrics` primitives

**Files:**
- Create: `internal/metrics/primitives.go`
- Create: `internal/metrics/primitives_test.go`

**Interfaces:**
- Consumes: nothing (stdlib only).
- Produces: `Counter` (`Add(float64)`, unexported `value() float64`), `Gauge` (`Set(float64)`, `value()`), `Histogram` (`newHistogram(bounds []float64) *Histogram`, `Observe(float64)`, fields `bounds`/`buckets`/`sumBits`/`count` used by the renderer in Task 2), `CounterVec` (`newCounterVec()`, `Add(delta float64, labelValues ...string)`, fields `mu`/`series`), `HistogramVec` (`newHistogramVec(bounds []float64)`, `Observe(val float64, labelValues ...string)`, fields `mu`/`series`/`bounds`), the series structs `counterSeries{labels []string; c Counter}` / `histogramSeries{labels []string; h *Histogram}`, and `joinKey([]string) string`. All in `package metrics`.

- [ ] **Step 1: Write the failing primitives test**

Create `internal/metrics/primitives_test.go`:

```go
package metrics

import (
	"sync"
	"testing"
)

func TestCounterAdd(t *testing.T) {
	var c Counter
	c.Add(1)
	c.Add(2.5)
	if got := c.value(); got != 3.5 {
		t.Errorf("value = %v, want 3.5", got)
	}
}

func TestGaugeSet(t *testing.T) {
	var g Gauge
	g.Set(7)
	g.Set(4)
	if got := g.value(); got != 4 {
		t.Errorf("value = %v, want 4", got)
	}
}

func TestHistogramObserve(t *testing.T) {
	h := newHistogram([]float64{1, 5, 10})
	for _, v := range []float64{0.5, 1, 5, 7, 20} { // buckets: <=1:{0.5,1}, <=5:{5}, <=10:{7}, +Inf:{20}
		h.Observe(v)
	}
	if h.count.Load() != 5 {
		t.Errorf("count = %d, want 5", h.count.Load())
	}
	// raw (non-cumulative) bucket tallies
	if got := []uint64{h.buckets[0].Load(), h.buckets[1].Load(), h.buckets[2].Load()}; got[0] != 2 || got[1] != 1 || got[2] != 1 {
		t.Errorf("buckets = %v, want [2 1 1]", got)
	}
}

func TestCounterVecSeries(t *testing.T) {
	v := newCounterVec()
	v.Add(1, "succeeded")
	v.Add(1, "succeeded")
	v.Add(1, "failed")
	if len(v.series) != 2 {
		t.Fatalf("series = %d, want 2", len(v.series))
	}
	if got := v.series[joinKey([]string{"succeeded"})].c.value(); got != 2 {
		t.Errorf("succeeded = %v, want 2", got)
	}
}

func TestHistogramVecSeries(t *testing.T) {
	v := newHistogramVec([]float64{1, 5})
	v.Observe(0.5, "GET", "/x")
	v.Observe(2, "GET", "/x")
	v.Observe(0.5, "POST", "/y")
	if len(v.series) != 2 {
		t.Fatalf("series = %d, want 2", len(v.series))
	}
	if got := v.series[joinKey([]string{"GET", "/x"})].h.count.Load(); got != 2 {
		t.Errorf("GET /x count = %d, want 2", got)
	}
}

func TestCounterConcurrent(t *testing.T) {
	var c Counter
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); for j := 0; j < 100; j++ { c.Add(1) } }()
	}
	wg.Wait()
	if got := c.value(); got != 10000 {
		t.Errorf("value = %v, want 10000", got)
	}
}

func TestCounterVecConcurrent(t *testing.T) {
	v := newCounterVec()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); for j := 0; j < 100; j++ { v.Add(1, "x") } }()
	}
	wg.Wait()
	if got := v.series[joinKey([]string{"x"})].c.value(); got != 5000 {
		t.Errorf("value = %v, want 5000", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/metrics/`
Expected: FAIL — `Counter`/`Gauge`/`Histogram`/etc. undefined (no `primitives.go` yet).

- [ ] **Step 3: Implement the primitives**

Create `internal/metrics/primitives.go`:

```go
// Package metrics is a tiny, dependency-free metrics registry rendered in the
// Prometheus text-exposition format. Primitives are safe for concurrent use via
// sync/atomic; labeled vectors guard their series map with an RWMutex and mutate
// the per-series primitive lock-free.
package metrics

import (
	"math"
	"strings"
	"sync"
	"sync/atomic"
)

// keySep joins label values into a map key; the unit separator does not appear
// in any label value we produce (statuses, HTTP methods, route templates, ints).
const keySep = "\x1f"

func joinKey(values []string) string { return strings.Join(values, keySep) }

// Counter is a monotonically increasing float64, safe for concurrent use.
type Counter struct{ bits atomic.Uint64 }

func (c *Counter) Add(delta float64) {
	for {
		old := c.bits.Load()
		nv := math.Float64frombits(old) + delta
		if c.bits.CompareAndSwap(old, math.Float64bits(nv)) {
			return
		}
	}
}

func (c *Counter) value() float64 { return math.Float64frombits(c.bits.Load()) }

// Gauge is a settable float64, safe for concurrent use.
type Gauge struct{ bits atomic.Uint64 }

func (g *Gauge) Set(v float64)    { g.bits.Store(math.Float64bits(v)) }
func (g *Gauge) value() float64   { return math.Float64frombits(g.bits.Load()) }

// Histogram observes samples into fixed ascending buckets. buckets[i] holds the
// raw (non-cumulative) count of samples in (bounds[i-1], bounds[i]]; samples above
// the top bound contribute only to count/sum (the implicit +Inf bucket). The
// renderer cumulates buckets at write time.
type Histogram struct {
	bounds  []float64
	buckets []atomic.Uint64
	sumBits atomic.Uint64
	count   atomic.Uint64
}

func newHistogram(bounds []float64) *Histogram {
	return &Histogram{bounds: bounds, buckets: make([]atomic.Uint64, len(bounds))}
}

func (h *Histogram) Observe(v float64) {
	i := 0
	for i < len(h.bounds) && v > h.bounds[i] {
		i++
	}
	if i < len(h.buckets) {
		h.buckets[i].Add(1)
	}
	h.count.Add(1)
	for {
		old := h.sumBits.Load()
		nv := math.Float64frombits(old) + v
		if h.sumBits.CompareAndSwap(old, math.Float64bits(nv)) {
			return
		}
	}
}

// CounterVec is a set of Counters partitioned by label values.
type CounterVec struct {
	mu     sync.RWMutex
	series map[string]*counterSeries
}

type counterSeries struct {
	labels []string
	c      Counter
}

func newCounterVec() *CounterVec { return &CounterVec{series: map[string]*counterSeries{}} }

func (v *CounterVec) Add(delta float64, labelValues ...string) {
	key := joinKey(labelValues)
	v.mu.RLock()
	s := v.series[key]
	v.mu.RUnlock()
	if s == nil {
		v.mu.Lock()
		if s = v.series[key]; s == nil {
			s = &counterSeries{labels: append([]string(nil), labelValues...)}
			v.series[key] = s
		}
		v.mu.Unlock()
	}
	s.c.Add(delta)
}

// HistogramVec is a set of Histograms partitioned by label values; all share bounds.
type HistogramVec struct {
	mu     sync.RWMutex
	bounds []float64
	series map[string]*histogramSeries
}

type histogramSeries struct {
	labels []string
	h      *Histogram
}

func newHistogramVec(bounds []float64) *HistogramVec {
	return &HistogramVec{bounds: bounds, series: map[string]*histogramSeries{}}
}

func (v *HistogramVec) Observe(val float64, labelValues ...string) {
	key := joinKey(labelValues)
	v.mu.RLock()
	s := v.series[key]
	v.mu.RUnlock()
	if s == nil {
		v.mu.Lock()
		if s = v.series[key]; s == nil {
			s = &histogramSeries{labels: append([]string(nil), labelValues...), h: newHistogram(v.bounds)}
			v.series[key] = s
		}
		v.mu.Unlock()
	}
	s.h.Observe(val)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/metrics/`
Expected: PASS (incl. the `-race` concurrency tests).

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/primitives.go internal/metrics/primitives_test.go
git commit -m "feat(metrics): concurrency-safe counter/gauge/histogram primitives"
```

---

### Task 2: `Metrics` registry + Prometheus rendering

**Files:**
- Create: `internal/metrics/metrics.go`
- Create: `internal/metrics/metrics_test.go`

**Interfaces:**
- Consumes: the Task 1 primitives (same package).
- Produces (all `package metrics`):
  - `type Metrics struct` (unexported fields).
  - `func New(version string) *Metrics`.
  - `func BuildVersion() string`.
  - Nil-safe methods: `ObserveRun(status string, d time.Duration)`, `ObserveStep(status string, d time.Duration)`, `StepRetried()`, `GateAwaited()`, `AgentTool()`, `AddCost(usd float64)`, `ObserveHTTP(method, route string, status int, d time.Duration)`.
  - `func (m *Metrics) WriteProm(w io.Writer)`.

- [ ] **Step 1: Write the failing registry test**

Create `internal/metrics/metrics_test.go`:

```go
package metrics

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func scrape(m *Metrics) string {
	var b strings.Builder
	m.WriteProm(&b)
	return b.String()
}

func TestNilMetricsIsNoOp(t *testing.T) {
	var m *Metrics // nil
	// must not panic
	m.ObserveRun("succeeded", time.Second)
	m.ObserveStep("failed", time.Second)
	m.StepRetried()
	m.GateAwaited()
	m.AgentTool()
	m.AddCost(1.5)
	m.ObserveHTTP("GET", "/x", 200, time.Second)
	var b strings.Builder
	m.WriteProm(&b)
	if b.Len() != 0 {
		t.Errorf("nil WriteProm wrote %q, want empty", b.String())
	}
}

func TestRendersCountersAndHistogram(t *testing.T) {
	m := New("abc123")
	m.ObserveRun("succeeded", 2*time.Second)
	m.ObserveRun("failed", 90*time.Second)
	m.ObserveStep("succeeded", 3*time.Second)
	m.StepRetried()
	m.GateAwaited()
	m.AgentTool()
	m.AgentTool()
	m.AddCost(0.25)
	m.ObserveHTTP("GET", "/v1/runs", 200, 30*time.Millisecond)
	out := scrape(m)

	for _, want := range []string{
		"# TYPE magister_runs_total counter",
		`magister_runs_total{status="succeeded"} 1`,
		`magister_runs_total{status="failed"} 1`,
		"# TYPE magister_run_duration_seconds histogram",
		"magister_run_duration_seconds_count 2",
		`magister_steps_total{status="succeeded"} 1`,
		`magister_steps_total{status="retrying"} 1`,
		"magister_gates_awaiting_total 1",
		"magister_agent_tool_calls_total 2",
		"magister_agent_cost_usd_total 0.25",
		`magister_http_requests_total{method="GET",route="/v1/runs",status="200"} 1`,
		"# TYPE magister_http_request_duration_seconds histogram",
		`magister_http_request_duration_seconds_count{method="GET",route="/v1/runs"} 1`,
		// runtime + build (on-scrape)
		"# TYPE go_goroutines gauge",
		"go_memstats_alloc_bytes",
		`magister_build_info{version="abc123",go_version="`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape missing %q\n---\n%s", want, out)
		}
	}
}

func TestHistogramCumulativeBucketsAndInf(t *testing.T) {
	m := New("v")
	m.ObserveStep("succeeded", 3*time.Second) // bucket le="5"
	m.ObserveStep("succeeded", 700*time.Second) // above top finite bucket → +Inf only
	out := scrape(m)
	for _, want := range []string{
		`magister_step_duration_seconds_bucket{le="1"} 0`,
		`magister_step_duration_seconds_bucket{le="5"} 1`,
		`magister_step_duration_seconds_bucket{le="600"} 1`,
		`magister_step_duration_seconds_bucket{le="+Inf"} 2`,
		"magister_step_duration_seconds_count 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

func TestLabelEscaping(t *testing.T) {
	m := New("v")
	m.ObserveHTTP("GET", `a"b\c`+"\n", 200, time.Millisecond)
	out := scrape(m)
	if !strings.Contains(out, `route="a\"b\\c\n"`) {
		t.Errorf("escaping wrong\n%s", out)
	}
}

func TestDeterministicOrdering(t *testing.T) {
	m := New("v")
	m.ObserveRun("failed", time.Second)
	m.ObserveRun("succeeded", time.Second)
	if a, b := scrape(m), scrape(m); a != b {
		t.Errorf("non-deterministic output:\n%s\n---\n%s", a, b)
	}
	out := scrape(m)
	if strings.Index(out, `status="failed"`) > strings.Index(out, `status="succeeded"`) {
		t.Errorf("series not sorted by label key:\n%s", out)
	}
}

func TestHTTPCounterConcurrent(t *testing.T) {
	m := New("v")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); for j := 0; j < 100; j++ { m.ObserveHTTP("GET", "/x", 200, time.Millisecond) } }()
	}
	wg.Wait()
	if !strings.Contains(scrape(m), `magister_http_requests_total{method="GET",route="/x",status="200"} 5000`) {
		t.Errorf("concurrent count wrong\n%s", scrape(m))
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/metrics/ -run 'TestRenders|TestNil|TestHistogramCumulative|TestLabel|TestDeterministic|TestHTTPCounter'`
Expected: FAIL — `New`/`Metrics`/`WriteProm` undefined.

- [ ] **Step 3: Implement the registry + renderer**

Create `internal/metrics/metrics.go`:

```go
package metrics

import (
	"fmt"
	"io"
	"math"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Metrics is the daemon's metric registry. Construct with New. A nil *Metrics is a
// safe no-op for every method, so an unwired collaborator records nothing.
type Metrics struct {
	version string

	runsTotal     *CounterVec // label: status
	runDuration   *Histogram
	stepsTotal    *CounterVec // label: status
	stepDuration  *Histogram
	gatesAwaiting Counter
	agentTools    Counter
	agentCost     Counter

	httpRequests *CounterVec   // labels: method, route, status
	httpDuration *HistogramVec // labels: method, route
}

var (
	runBuckets  = []float64{5, 30, 60, 300, 600, 1800, 3600}
	stepBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600}
	httpBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}
)

// New constructs the registry with its fixed metric families. version labels
// magister_build_info.
func New(version string) *Metrics {
	return &Metrics{
		version:      version,
		runsTotal:    newCounterVec(),
		runDuration:  newHistogram(runBuckets),
		stepsTotal:   newCounterVec(),
		stepDuration: newHistogram(stepBuckets),
		httpRequests: newCounterVec(),
		httpDuration: newHistogramVec(httpBuckets),
	}
}

// BuildVersion returns the VCS revision the binary was built from (short), or the
// main module version, or "unknown". Used by the daemon to label build info.
func BuildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			if len(s.Value) > 12 {
				return s.Value[:12]
			}
			return s.Value
		}
	}
	if bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "unknown"
}

// --- instrumentation (all nil-safe) ---

func (m *Metrics) ObserveRun(status string, d time.Duration) {
	if m == nil {
		return
	}
	m.runsTotal.Add(1, status)
	m.runDuration.Observe(d.Seconds())
}

func (m *Metrics) ObserveStep(status string, d time.Duration) {
	if m == nil {
		return
	}
	m.stepsTotal.Add(1, status)
	m.stepDuration.Observe(d.Seconds())
}

func (m *Metrics) StepRetried() {
	if m == nil {
		return
	}
	m.stepsTotal.Add(1, "retrying")
}

func (m *Metrics) GateAwaited() {
	if m == nil {
		return
	}
	m.gatesAwaiting.Add(1)
}

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

func (m *Metrics) ObserveHTTP(method, route string, status int, d time.Duration) {
	if m == nil {
		return
	}
	m.httpRequests.Add(1, method, route, strconv.Itoa(status))
	m.httpDuration.Observe(d.Seconds(), method, route)
}

// --- rendering ---

// WriteProm renders all families in the Prometheus text-exposition format, then
// appends on-scrape runtime/build metrics. Output is deterministic (series sorted
// by label key). Read-only; never errors the caller.
func (m *Metrics) WriteProm(w io.Writer) {
	if m == nil {
		return
	}
	writeCounterVec(w, "magister_runs_total", "Total runs by terminal status.", []string{"status"}, m.runsTotal)
	writeHistogram(w, "magister_run_duration_seconds", "Run wall-clock duration in seconds.", m.runDuration)
	writeCounterVec(w, "magister_steps_total", "Total step outcomes by status.", []string{"status"}, m.stepsTotal)
	writeHistogram(w, "magister_step_duration_seconds", "Step wall-clock duration in seconds.", m.stepDuration)
	writeCounterRaw(w, "magister_gates_awaiting_total", "Total times a gate began awaiting manual approval.", m.gatesAwaiting.value())
	writeCounterRaw(w, "magister_agent_tool_calls_total", "Total agent tool-use milestones.", m.agentTools.value())
	writeCounterRaw(w, "magister_agent_cost_usd_total", "Total agent cost in USD.", m.agentCost.value())
	writeCounterVec(w, "magister_http_requests_total", "Total HTTP requests by method, route, and status.", []string{"method", "route", "status"}, m.httpRequests)
	writeHistogramVec(w, "magister_http_request_duration_seconds", "HTTP request duration in seconds.", []string{"method", "route"}, m.httpDuration)
	writeRuntime(w)
	writeBuildInfo(w, m.version)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// labelPairs renders {n0="v0",n1="v1"} (escaped), or "" when there are no labels.
func labelPairs(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(labelEscaper.Replace(values[i]))
		b.WriteString(`"`)
	}
	b.WriteByte('}')
	return b.String()
}

// histLabels renders the base labels plus a trailing le="..." for a bucket line.
func histLabels(names, values []string, le string) string {
	return labelPairs(append(append([]string(nil), names...), "le"),
		append(append([]string(nil), values...), le))
}

func writeHelpType(w io.Writer, name, help, typ string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func writeCounterRaw(w io.Writer, name, help string, v float64) {
	writeHelpType(w, name, help, "counter")
	fmt.Fprintf(w, "%s %s\n", name, formatFloat(v))
}

func writeGauge(w io.Writer, name, help string, v float64) {
	writeHelpType(w, name, help, "gauge")
	fmt.Fprintf(w, "%s %s\n", name, formatFloat(v))
}

func writeCounterVec(w io.Writer, name, help string, labelNames []string, cv *CounterVec) {
	writeHelpType(w, name, help, "counter")
	cv.mu.RLock()
	series := make([]*counterSeries, 0, len(cv.series))
	for _, s := range cv.series {
		series = append(series, s)
	}
	cv.mu.RUnlock()
	sort.Slice(series, func(i, j int) bool { return joinKey(series[i].labels) < joinKey(series[j].labels) })
	for _, s := range series {
		fmt.Fprintf(w, "%s%s %s\n", name, labelPairs(labelNames, s.labels), formatFloat(s.c.value()))
	}
}

func writeHistSeries(w io.Writer, name string, names, values []string, h *Histogram) {
	var cum uint64
	for i, b := range h.bounds {
		cum += h.buckets[i].Load()
		fmt.Fprintf(w, "%s_bucket%s %d\n", name, histLabels(names, values, formatFloat(b)), cum)
	}
	total := h.count.Load()
	fmt.Fprintf(w, "%s_bucket%s %d\n", name, histLabels(names, values, "+Inf"), total)
	fmt.Fprintf(w, "%s_sum%s %s\n", name, labelPairs(names, values), formatFloat(math.Float64frombits(h.sumBits.Load())))
	fmt.Fprintf(w, "%s_count%s %d\n", name, labelPairs(names, values), total)
}

func writeHistogram(w io.Writer, name, help string, h *Histogram) {
	writeHelpType(w, name, help, "histogram")
	writeHistSeries(w, name, nil, nil, h)
}

func writeHistogramVec(w io.Writer, name, help string, labelNames []string, hv *HistogramVec) {
	writeHelpType(w, name, help, "histogram")
	hv.mu.RLock()
	series := make([]*histogramSeries, 0, len(hv.series))
	for _, s := range hv.series {
		series = append(series, s)
	}
	hv.mu.RUnlock()
	sort.Slice(series, func(i, j int) bool { return joinKey(series[i].labels) < joinKey(series[j].labels) })
	for _, s := range series {
		writeHistSeries(w, name, labelNames, s.labels, s.h)
	}
}

func writeRuntime(w io.Writer) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	writeGauge(w, "go_goroutines", "Number of goroutines that currently exist.", float64(runtime.NumGoroutine()))
	writeGauge(w, "go_memstats_alloc_bytes", "Number of bytes allocated in heap objects.", float64(ms.Alloc))
	writeGauge(w, "go_memstats_heap_sys_bytes", "Number of heap bytes obtained from the OS.", float64(ms.HeapSys))
	writeCounterRaw(w, "go_gc_count_total", "Number of completed GC cycles.", float64(ms.NumGC))
}

func writeBuildInfo(w io.Writer, version string) {
	writeHelpType(w, "magister_build_info", "Build metadata; value is always 1.", "gauge")
	fmt.Fprintf(w, "magister_build_info%s 1\n",
		labelPairs([]string{"version", "go_version"}, []string{version, runtime.Version()}))
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/metrics/`
Expected: PASS (all primitive + render + concurrency tests).

- [ ] **Step 5: gofmt + commit**

```bash
gofmt -l internal/metrics/   # must be empty
git add internal/metrics/metrics.go internal/metrics/metrics_test.go
git commit -m "feat(metrics): Metrics registry with Prometheus text rendering"
```

---

### Task 3: Engine instrumentation

**Files:**
- Modify: `internal/engine/engine.go`
- Create: `internal/engine/metrics_test.go`

**Interfaces:**
- Consumes: `metrics.Metrics` (Task 2) + its nil-safe methods.
- Produces: `Engine.Metrics *metrics.Metrics` field; metrics populated as runs execute.

- [ ] **Step 1: Add the field + import**

In `internal/engine/engine.go`, add the import `"concentus/internal/metrics"` to the import block, and add a field to the `Engine` struct beside `Log *slog.Logger`:

```go
	Log  *slog.Logger // non-fatal store/bus failures; nil = discard (M3 wires a real handler)
	// Metrics records run/step/gate/agent counters and durations; nil = no-op.
	Metrics *metrics.Metrics
```

- [ ] **Step 2: Instrument run start + the three run-done paths**

In `runDAG`, capture the run start time right after the run.started event is published (after `e.Bus.Publish(runStartedEv)`, ~line 106):

```go
	e.Bus.Publish(runStartedEv)
	runStart := e.Clock.Now()
```

Then in the final `switch` (~lines 210–241), add one `ObserveRun` call in each branch, right after that branch's `e.Bus.Publish(runDoneEv)`:

- canceled branch:
```go
		e.Bus.Publish(runDoneEv)
		e.Metrics.ObserveRun("canceled", e.Clock.Now().Sub(runStart))
		return parent.Err()
```
- failed branch:
```go
		e.Bus.Publish(runDoneEv)
		e.Metrics.ObserveRun("failed", e.Clock.Now().Sub(runStart))
		return firstErr
```
- succeeded (default) branch:
```go
		e.Bus.Publish(runDoneEv)
		e.Metrics.ObserveRun("succeeded", e.Clock.Now().Sub(runStart))
		return nil
```

- [ ] **Step 3: Instrument the step terminal outcome (covers escalate paths in one place)**

In `runDAG`'s per-step goroutine, replace the step-execution block (currently):

```go
			// 4. run the step (execute + gate, with retries).
			res, err := e.runStep(ctx, runID, s, inputs)
			if err != nil {
				fail(fmt.Errorf("step %q: %w", s.ID, err))
				return
			}
			mu.Lock()
			results[s.ID] = res
			mu.Unlock()
```

with (times the whole `runStep` span and records the terminal step metric + cost — `runStep`'s return value already folds in the escalate/escalateJoin outcomes):

```go
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

- [ ] **Step 4: Instrument the retry counter and the gate-awaiting counter**

In `runStep`, in the `attempt > 1` retry branch (~line 260), add `e.Metrics.StepRetried()` right after the `StepRetrying` transition:

```go
		if attempt > 1 {
			e.transition(ctx, runID, stepState(runID, s.ID, core.StepRetrying, attempt, workDir, core.Result{}, lastErr),
				event.Event{StepID: s.ID, Kind: event.StepRetrying, Attempt: attempt})
			e.Metrics.StepRetried()
			if !e.backoff(ctx, s, attempt) {
				return core.Result{}, ctx.Err()
			}
		}
```

In `attempt`, in the `gateBlocks(s)` branch (~line 334) that emits `GateAwaiting`, add `e.Metrics.GateAwaited()` right after the transition:

```go
	} else if gateBlocks(s) {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepAwaitingGate, attemptNum, workDir, res, nil),
			event.Event{StepID: s.ID, Kind: event.GateAwaiting, Attempt: attemptNum})
		e.Metrics.GateAwaited()
	}
```

- [ ] **Step 5: Instrument the agent.tool counter**

In `runAgent`, in the `emit` closure (~line 356), count agent.tool milestones right after the event's fields are stamped:

```go
	emit := func(ev event.Event) {
		ev.RunID, ev.StepID, ev.Attempt, ev.At = string(runID), stepID, attemptNum, e.Clock.Now()
		if ev.Kind == event.AgentTool {
			e.Metrics.AgentTool()
		}
		if err := e.Store.AppendEvents(context.WithoutCancel(ctx), runID, []event.Event{ev}); err != nil {
			e.logger().Error("append agent milestone", "run", runID, "step", stepID, "err", err)
			return
		}
		e.Bus.Publish(ev)
	}
```

- [ ] **Step 6: Write the engine-instrumentation test**

Create `internal/engine/metrics_test.go` (match the existing engine test package clause — if `internal/engine/engine_test.go` is `package engine_test`, prefix exported names with `engine.` and adjust; the code below assumes internal `package engine` like most engine tests):

```go
package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/executor"
	"concentus/internal/flow"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/metrics"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

func TestEngineRecordsMetrics(t *testing.T) {
	st := store.NewMem()
	m := metrics.New("test")
	eng := &Engine{
		Execs:   map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:      &workspace.Manager{Root: t.TempDir()},
		Gate:    &gate.Evaluator{Verifier: gate.CommandVerifier{}}, // auto gate needs no approver
		Joins:   join.Default(),
		Store:   st,
		Bus:     event.NewBus(),
		Clock:   core.SystemClock{},
		Metrics: m,
	}
	yaml := "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"true\" } }\n"
	f, err := flow.ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rid := core.RunID("r1")
	if err := st.CreateRun(context.Background(), core.RunState{ID: rid, Name: "f", FlowYAML: yaml}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := eng.Run(context.Background(), rid, f); err != nil {
		t.Fatalf("run: %v", err)
	}
	var buf bytes.Buffer
	m.WriteProm(&buf)
	out := buf.String()
	for _, want := range []string{
		`magister_runs_total{status="succeeded"} 1`,
		`magister_steps_total{status="succeeded"} 1`,
		"magister_run_duration_seconds_count 1",
		"magister_step_duration_seconds_count 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q\n---\n%s", want, out)
		}
	}
}
```

- [ ] **Step 7: Run + verify**

Run: `go test -race ./internal/engine/`
Expected: PASS (existing engine tests unaffected — `Metrics` is nil in those; the new test asserts the counters).

- [ ] **Step 8: gofmt + commit**

```bash
gofmt -l internal/engine/   # must be empty
git add internal/engine/engine.go internal/engine/metrics_test.go
git commit -m "feat(engine): record run/step/gate/agent metrics at lifecycle points"
```

---

### Task 4: HTTP middleware, `/metrics` endpoint & daemon wiring

**Files:**
- Modify: `internal/api/handlers.go` (Server field + `handleMetrics`)
- Modify: `internal/api/middleware.go` (`metricsMiddleware` + `routeLabel` + `stripMethod`)
- Modify: `internal/api/router.go` (register route + chain the middleware)
- Modify: `internal/api/handlers_test.go` (set `Metrics` in `newServerOnly`)
- Create: `internal/api/metrics_test.go`
- Modify: `cmd/magisterd/main.go` (construct + wire)

**Interfaces:**
- Consumes: `metrics.Metrics` (Task 2), the existing `statusRecorder`/`chain` (middleware.go), the `v1` sub-mux (router.go).
- Produces: `Server.Metrics *metrics.Metrics`; `GET /metrics` endpoint; HTTP metrics recorded per request labeled by route template.

- [ ] **Step 1: Add the Server field + handler**

In `internal/api/handlers.go`, add the import `"concentus/internal/metrics"`, add a field to `Server`:

```go
	// Metrics records HTTP + (via the engine) domain metrics; nil = no-op. Served at
	// GET /metrics (auth-exempt).
	Metrics *metrics.Metrics
```

and add the handler (near `handleHealthz`):

```go
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.Metrics.WriteProm(w) // nil-safe: a nil registry writes nothing
}
```

- [ ] **Step 2: Add the metrics middleware + route-label helpers**

In `internal/api/middleware.go`, add the import `"concentus/internal/metrics"`, and append:

```go
// metricsMiddleware records request count + duration per request, labeled by the
// matched route TEMPLATE (not the raw path) so cardinality stays bounded. routes is
// the /v1 sub-mux, consulted via Handler(r) to resolve the template — http.Request.
// Pattern is Go 1.23+ and unavailable here. Placed OUTSIDE recoverMiddleware so a
// recovered panic is recorded as its 500 status.
func metricsMiddleware(m *metrics.Metrics, routes *http.ServeMux) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			m.ObserveHTTP(r.Method, routeLabel(r, routes), rec.status, time.Since(start))
		})
	}
}

// routeLabel resolves a request to a bounded route-template label. /healthz and
// /metrics are matched by path (they live on the outer mux); /v1 routes are resolved
// via the v1 sub-mux's Handler; anything else is "unmatched".
func routeLabel(r *http.Request, routes *http.ServeMux) string {
	switch r.URL.Path {
	case "/healthz":
		return "/healthz"
	case "/metrics":
		return "/metrics"
	}
	if _, pattern := routes.Handler(r); pattern != "" {
		return stripMethod(pattern)
	}
	return "unmatched"
}

// stripMethod drops a leading "METHOD " from a ServeMux pattern, e.g.
// "GET /v1/runs/{id}" → "/v1/runs/{id}".
func stripMethod(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
}
```

- [ ] **Step 3: Register the route + chain the middleware**

In `internal/api/router.go`, register `/metrics` alongside `/healthz`:

```go
	// health + metrics are mounted outside the authed group
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
```

and insert `metricsMiddleware` into the outer chain (between logging and recover, so it captures the recovered-panic 500 and the route label resolves via `v1`):

```go
	return chain(mux,
		requestIDMiddleware,
		loggingMiddleware(s.Log),
		metricsMiddleware(s.Metrics, v1),
		recoverMiddleware(s.Log),
		securityHeaders,
	)
```

- [ ] **Step 4: Make api tests exercise the middleware**

In `internal/api/handlers_test.go`, add the import `"concentus/internal/metrics"` and set the field in `newServerOnly`'s returned `Server`:

```go
	return &Server{Sup: sup, Store: st, Bus: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: metrics.New("test")}, sup, st
```

- [ ] **Step 5: Write the endpoint tests**

Create `internal/api/metrics_test.go`:

```go
package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsEndpointRecordsHTTP(t *testing.T) {
	hs, _, _ := testServer(t) // Metrics wired via newServerOnly
	// generate traffic: a list (matched) and a get-by-id (template, not raw id)
	must200(t, hs.URL+"/v1/runs")
	resp, err := http.Get(hs.URL + "/v1/runs/01HZZZZZZZZZZZZZZZZZZZZZZZ")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	body, ct := scrapeMetrics(t, hs.URL)
	if !strings.Contains(ct, "text/plain; version=0.0.4") {
		t.Errorf("content-type = %q", ct)
	}
	for _, want := range []string{
		`magister_http_requests_total{method="GET",route="/v1/runs",status="200"}`,
		`route="/v1/runs/{id}"`, // the TEMPLATE, not the raw ULID
		"magister_http_request_duration_seconds_count",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n---\n%s", want, body)
		}
	}
	if strings.Contains(body, "01HZZZZZZZZZZZZZZZZZZZZZZZ") {
		t.Errorf("raw run id leaked into a metric label:\n%s", body)
	}
}

func TestMetricsAuthExempt(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	hs := httptest.NewServer(srv.Router("secret-token")) // auth ENABLED
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/metrics") // no Authorization header
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (auth-exempt)", resp.StatusCode)
	}
}

func must200(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func scrapeMetrics(t *testing.T, base string) (body, contentType string) {
	t.Helper()
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.Header.Get("Content-Type")
}
```

- [ ] **Step 6: Wire the daemon**

In `cmd/magisterd/main.go`, add the import `"concentus/internal/metrics"`. Construct the registry after `bus := event.NewBus()` (~line 71):

```go
	bus := event.NewBus()
	mx := metrics.New(metrics.BuildVersion())
```

Add `Metrics: mx,` to the `engine.Engine` struct literal:

```go
		Store: st, Bus: bus, Clock: core.SystemClock{}, Log: log,
		Metrics: mx,
	}
```

Add `Metrics: mx,` to the `api.Server` struct literal:

```go
	srv := &api.Server{Sup: sup, Store: st, Bus: bus, Log: log, ScratchRoot: runsRoot, Metrics: mx}
```

- [ ] **Step 7: Run the api + daemon tests**

Run: `go test ./internal/api/ ./cmd/magisterd/`
Expected: PASS (existing tests still green; the new metrics tests pass; route label is the template).

- [ ] **Step 8: Full suite + vet + gofmt**

Run: `gofmt -l internal cmd && go vet ./... && go test -race ./...`
Expected: `gofmt -l` empty; vet clean; all 17 packages PASS (the new `internal/metrics` package brings the count to 17).

- [ ] **Step 9: Commit**

```bash
git add internal/api/handlers.go internal/api/middleware.go internal/api/router.go internal/api/handlers_test.go internal/api/metrics_test.go cmd/magisterd/main.go
git commit -m "feat(api): auth-exempt /metrics endpoint with HTTP request metrics"
```

---

## Self-Review

**1. Spec coverage** (against `docs/superpowers/specs/2026-06-19-observability-metrics-design.md`):
- `internal/metrics` package: primitives (Counter/Gauge/Histogram + Vecs) → Task 1; `Metrics` struct + nil-safe methods + `WriteProm` + `New`/`BuildVersion` → Task 2. ✓
- Full metric set (runs/steps lifecycle + duration histograms, gates, agent tools/cost, HTTP count+duration, go runtime + build_info) with exact names/labels/buckets → Tasks 2 (definitions/render) + 3 (engine sources) + 4 (HTTP source). ✓
- Engine instrumentation at existing lifecycle points, local-scope duration timing, no control-flow change → Task 3 (run start + 3 done paths; step terminal at the runStep call site covering escalate; retry; gate; agent.tool). ✓
- HTTP middleware labeled by route template via `mux.Handler(r)` (NOT `r.Pattern`), unmatched→"unmatched", reuses `statusRecorder`, placed outside `recoverMiddleware` → Task 4. ✓
- Auth-exempt `GET /metrics` outside the `/v1` group, Prometheus content-type, cumulative buckets + `+Inf` + `_sum`/`_count`, label escaping, deterministic ordering → Tasks 2 (render) + 4 (route/handler). ✓
- Daemon wiring (one `Metrics` shared by engine+server; nil-safe so no other wiring) → Task 4 Step 6. ✓
- Testing: metrics unit + concurrency, render format, engine mock-run assertion, api route-label + auth-exempt → Tasks 1–4. ✓
- Out-of-scope (in-flight gauge, per-agent labels, full go-collector, tracing) → not built. ✓
- Global constraints (no new dep, no migration, no store method, Go 1.22 `mux.Handler` not `r.Pattern`) → held; no task touches `go.mod`/migrations/store. ✓

**2. Placeholder scan:** No TBD/TODO; every code step contains complete code. The `~line NNN` references are anchors into existing files (the surrounding code is quoted so the edit site is unambiguous), not placeholders. The one cross-file uncertainty — the engine test's `package` clause — is called out explicitly in Task 3 Step 6 with the adjustment rule. ✓

**3. Type consistency:** `metrics.New(version string) *Metrics`, `BuildVersion() string`, and the method set (`ObserveRun`/`ObserveStep`/`StepRetried`/`GateAwaited`/`AgentTool`/`AddCost`/`ObserveHTTP`/`WriteProm`) are defined in Task 2 and used identically in Tasks 3 (engine) and 4 (api/daemon). `Engine.Metrics`/`Server.Metrics` are both `*metrics.Metrics`. The renderer helpers in Task 2 consume exactly the Task 1 primitive fields (`bounds`/`buckets`/`sumBits`/`count`/`series`/`labels`/`c`/`h`). `metricsMiddleware(*metrics.Metrics, *http.ServeMux)` matches its Task 4 call site `metricsMiddleware(s.Metrics, v1)`. The daemon's `mx` feeds both struct literals. ✓
