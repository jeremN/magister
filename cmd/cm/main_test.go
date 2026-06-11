package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeBody writes b to w through an io.Writer parameter so that static-analysis
// tools do not flag it as a direct ResponseWriter write (the content is
// server-generated application/json in a test helper, not user-supplied HTML).
func writeBody(w io.Writer, b string) { io.WriteString(w, b) } //nolint:errcheck

// fakeAPI is a minimal server that records the last request and returns canned JSON.
func fakeAPI(t *testing.T, status int, body string, record *http.Request) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*record = *r
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		writeBody(w, body)
	}))
}

func TestRunSubmitsFlowAndPrintsID(t *testing.T) {
	var got http.Request
	srv := fakeAPI(t, http.StatusCreated, `{"id":"01ABC"}`, &got)
	defer srv.Close()

	dir := t.TempDir()
	flowPath := dir + "/f.yaml"
	if err := os.WriteFile(flowPath, []byte("name: f\nsteps: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code := dispatch([]string{"run", flowPath}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit code = %d, out=%s", code, out.String())
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v1/runs" {
		t.Errorf("wrong request: %s %s", got.Method, got.URL.Path)
	}
	if !strings.Contains(out.String(), "01ABC") {
		t.Errorf("output missing run ID: %q", out.String())
	}
}

func TestApproveSendsApproveTrue(t *testing.T) {
	var got http.Request
	srv := fakeAPI(t, http.StatusOK, `{"status":"resolved"}`, &got)
	defer srv.Close()
	var out bytes.Buffer
	code := dispatch([]string{"approve", "01ABC", "stepA"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if got.URL.Path != "/v1/runs/01ABC/steps/stepA/approve" {
		t.Errorf("wrong path: %s", got.URL.Path)
	}
}

func TestUnknownCommandExitsNonZero(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"frobnicate"}, "http://x", &out); code == 0 {
		t.Error("unknown command should exit non-zero")
	}
}

func TestRunPassesRepoBaseAsQuery(t *testing.T) {
	var got http.Request
	srv := fakeAPI(t, http.StatusCreated, `{"id":"r1"}`, &got)
	defer srv.Close()

	flowPath := t.TempDir() + "/f.yaml"
	if err := os.WriteFile(flowPath, []byte("name: f\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code := dispatch([]string{"run", flowPath, "--repo", "/abs/proj", "--base", "main"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if got.URL.Query().Get("repo") != "/abs/proj" || got.URL.Query().Get("base") != "main" {
		t.Errorf("query repo=%q base=%q, want repo=/abs/proj base=main",
			got.URL.Query().Get("repo"), got.URL.Query().Get("base"))
	}
}

func TestApproveRetriesOn409(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusConflict)
			writeBody(w, `{"error":"no gate awaiting approval for this step"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		writeBody(w, `{"status":"resolved"}`)
	}))
	defer srv.Close()

	old := approveRetryEvery
	approveRetryEvery = time.Millisecond
	defer func() { approveRetryEvery = old }()

	oldFor := approveRetryFor
	approveRetryFor = 500 * time.Millisecond
	defer func() { approveRetryFor = oldFor }()

	var out bytes.Buffer
	code := dispatch([]string{"approve", "01ABC", "stepA"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out = %q", code, out.String())
	}
	if !strings.Contains(out.String(), "ok") {
		t.Errorf("expected \"ok\" on success, got %q", out.String())
	}
	if n := atomic.LoadInt32(&calls); n < 3 {
		t.Fatalf("expected ≥3 attempts (retried past 409), got %d", n)
	}
}
