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
