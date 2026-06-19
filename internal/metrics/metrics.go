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
	agentTools    *CounterVec // label: agent
	agentCost     *CounterVec // label: agent

	runsActive   Gauge
	stepsActive  Gauge
	httpInFlight Gauge

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
		agentTools:   newCounterVec(),
		agentCost:    newCounterVec(),
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
	writeCounterVec(w, "magister_agent_tool_calls_total", "Total agent tool-use milestones by agent.", []string{"agent"}, m.agentTools)
	writeCounterVec(w, "magister_agent_cost_usd_total", "Total agent cost in USD by agent.", []string{"agent"}, m.agentCost)
	writeGauge(w, "magister_runs_active", "Runs currently executing.", m.runsActive.value())
	writeGauge(w, "magister_steps_active", "Steps currently executing.", m.stepsActive.value())
	writeGauge(w, "magister_http_requests_in_flight", "HTTP requests currently being served.", m.httpInFlight.value())
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
