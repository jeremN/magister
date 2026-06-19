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
	m.AgentTool("mock")
	m.AddCost("mock", 1.5)
	m.RunStarted()
	m.RunFinished()
	m.StepStarted()
	m.StepFinished()
	m.HTTPStarted()
	m.HTTPFinished()
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
	m.AgentTool("opus")
	m.AgentTool("opus")
	m.AddCost("opus", 0.25)
	m.RunStarted()
	m.StepStarted()
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
		`magister_agent_tool_calls_total{agent="opus"} 2`,
		`magister_agent_cost_usd_total{agent="opus"} 0.25`,
		"# TYPE magister_runs_active gauge",
		"magister_runs_active 1",
		"magister_steps_active 1",
		"# TYPE magister_http_requests_in_flight gauge",
		"magister_http_requests_in_flight 0",
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
	m.ObserveStep("succeeded", 3*time.Second)   // bucket le="5"
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
	// Strip the on-scrape runtime section (go_goroutines onward) before
	// comparing: runtime values (alloc bytes, goroutine count) are inherently
	// non-deterministic between two successive calls and are not part of the
	// ordering contract being tested here.
	trimRuntime := func(s string) string {
		if i := strings.Index(s, "# HELP go_goroutines"); i >= 0 {
			return s[:i]
		}
		return s
	}
	a, b := trimRuntime(scrape(m)), trimRuntime(scrape(m))
	if a != b {
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
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.ObserveHTTP("GET", "/x", 200, time.Millisecond)
			}
		}()
	}
	wg.Wait()
	if !strings.Contains(scrape(m), `magister_http_requests_total{method="GET",route="/x",status="200"} 5000`) {
		t.Errorf("concurrent count wrong\n%s", scrape(m))
	}
}

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
