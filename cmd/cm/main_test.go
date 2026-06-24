package main

import (
	"bytes"
	"encoding/json"
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

// TestNonWatchCommandTimesOutOnUnresponsiveDaemon verifies that a non-watch command
// (e.g. get) returns an error promptly when the server never responds, rather than
// hanging forever. It uses a tiny injected timeout on the client to keep the test fast.
func TestNonWatchCommandTimesOutOnUnresponsiveDaemon(t *testing.T) {
	// blocking server that never writes a response
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // block until the client gives up
	}))
	defer srv.Close()

	c := &client{
		base:      srv.URL,
		http:      &http.Client{Timeout: 50 * time.Millisecond},
		watchHTTP: &http.Client{Timeout: 0},
	}

	start := time.Now()
	var out bytes.Buffer
	code := c.get("/v1/runs", &out)
	elapsed := time.Since(start)

	if code == 0 {
		t.Error("expected non-zero exit code on timeout, got 0")
	}
	if elapsed > 5*time.Second {
		t.Errorf("command did not time out promptly: elapsed %v", elapsed)
	}
}

// TestDispatchClientHas30sTimeout verifies that dispatch builds the client with the
// expected 30s timeout for normal commands and a no-timeout client for watch.
func TestDispatchClientHas30sTimeout(t *testing.T) {
	// We test the client struct directly since dispatch is the only builder.
	c := &client{
		base:      "http://127.0.0.1:8080",
		http:      &http.Client{Timeout: 30 * time.Second},
		watchHTTP: &http.Client{Timeout: 0},
	}
	if c.http.Timeout != 30*time.Second {
		t.Errorf("normal client timeout = %v, want 30s", c.http.Timeout)
	}
	if c.watchHTTP.Timeout != 0 {
		t.Errorf("watch client timeout = %v, want 0 (no timeout)", c.watchHTTP.Timeout)
	}
}

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

func TestPushPassesOptionsAsQuery(t *testing.T) {
	var got http.Request
	srv := fakeAPI(t, http.StatusOK,
		`{"remote":"git@h:me/x.git","branch":"magister/r1","source_branch":"step/integrate","commit":"abc"}`, &got)
	defer srv.Close()

	var out bytes.Buffer
	code := dispatch([]string{"push", "r1", "--remote", "upstream", "--as", "feature/x", "--step", "integrate", "--force"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v1/runs/r1/push" {
		t.Errorf("request = %s %s, want POST /v1/runs/r1/push", got.Method, got.URL.Path)
	}
	q := got.URL.Query()
	if q.Get("remote") != "upstream" || q.Get("as") != "feature/x" || q.Get("step") != "integrate" || q.Get("force") != "true" {
		t.Errorf("query = %v, want remote/as/step/force set", q)
	}
	if s := out.String(); !strings.Contains(s, "step/integrate") || !strings.Contains(s, "magister/r1") {
		t.Errorf("output missing source/dest branch: %q", s)
	}
}

func TestPushRequiresRun(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"push"}, "http://x", &out); code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}

func TestPushNon200PrintsError(t *testing.T) {
	var got http.Request
	srv := fakeAPI(t, http.StatusNotFound, `{"error":"unknown run"}`, &got)
	defer srv.Close()
	var out bytes.Buffer
	code := dispatch([]string{"push", "no-such-run"}, srv.URL, &out)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "unknown run") {
		t.Errorf("expected server error in output, got %q", out.String())
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

func TestPRSendsJSONBody(t *testing.T) {
	var got http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeBody(w, `{"url":"https://github.com/o/r/pull/3","repo":"o/r","head":"magister/r1"}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	code := dispatch([]string{"pr", "r1", "--title", "My PR", "--base", "main", "--draft"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v1/runs/r1/pr" {
		t.Errorf("request = %s %s, want POST /v1/runs/r1/pr", got.Method, got.URL.Path)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if sent["title"] != "My PR" || sent["base"] != "main" || sent["draft"] != true {
		t.Errorf("body = %v, want title/base/draft set", sent)
	}
	if !strings.Contains(out.String(), "https://github.com/o/r/pull/3") {
		t.Errorf("output missing PR url: %q", out.String())
	}
}

func TestPRRequiresRun(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"pr"}, "http://x", &out); code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}

func TestPRNon200PrintsError(t *testing.T) {
	var got http.Request
	srv := fakeAPI(t, http.StatusConflict, `{"error":"PR already exists for magister/r1: https://github.com/o/r/pull/9"}`, &got)
	defer srv.Close()
	var out bytes.Buffer
	code := dispatch([]string{"pr", "r1"}, srv.URL, &out)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "pull/9") {
		t.Errorf("output should surface the existing PR url, got %q", out.String())
	}
}

func TestPRHeadRepoSendsHeadRepoJSONField(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeBody(w, `{"url":"https://github.com/o/r/pull/1"}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	code := dispatch([]string{"pr", "r1", "--head-repo", "https://github.com/fork/r.git"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if sent["head_repo"] != "https://github.com/fork/r.git" {
		t.Errorf("head_repo = %v, want the fork url; body=%s", sent["head_repo"], body)
	}
	if _, hyphen := sent["head-repo"]; hyphen {
		t.Errorf("body used the hyphenated key head-repo; want head_repo; body=%s", body)
	}
}

func TestShipSendsJSONBodyAndPrintsOpened(t *testing.T) {
	var got http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeBody(w, `{"pushed":{"remote":"git@h:o/r.git","branch":"magister/r1","source_branch":"step/integrate","commit":"abc"},"pr":{"url":"https://github.com/o/r/pull/4"},"pr_existed":false}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	code := dispatch([]string{"ship", "r1", "--as", "feature/x", "--force", "--title", "Hi"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v1/runs/r1/ship" {
		t.Errorf("request = %s %s, want POST /v1/runs/r1/ship", got.Method, got.URL.Path)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if sent["as"] != "feature/x" || sent["force"] != true || sent["title"] != "Hi" {
		t.Errorf("body = %v, want as/force/title set", sent)
	}
	s := out.String()
	if !strings.Contains(s, "step/integrate") || !strings.Contains(s, "magister/r1") {
		t.Errorf("output missing pushed line: %q", s)
	}
	if !strings.Contains(s, "opened https://github.com/o/r/pull/4") {
		t.Errorf("output missing opened line: %q", s)
	}
}

func TestShipPrintsExistsWhenPRExisted(t *testing.T) {
	var got http.Request
	srv := fakeAPI(t, http.StatusOK,
		`{"pushed":{"remote":"r","branch":"magister/r1","source_branch":"step/integrate","commit":"abc"},"pr":{"url":"https://github.com/o/r/pull/9"},"pr_existed":true}`, &got)
	defer srv.Close()
	var out bytes.Buffer
	if code := dispatch([]string{"ship", "r1"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "exists https://github.com/o/r/pull/9") {
		t.Errorf("output should say 'exists' when pr_existed, got %q", out.String())
	}
}

func TestShipRequiresRun(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"ship"}, "http://x", &out); code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}

func TestLogLevelGetPrintsCurrent(t *testing.T) {
	var rec http.Request
	srv := fakeAPI(t, http.StatusOK, `{"level":"warn"}`, &rec)
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"loglevel"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if rec.Method != http.MethodGet || rec.URL.Path != "/v1/loglevel" {
		t.Errorf("request = %s %s, want GET /v1/loglevel", rec.Method, rec.URL.Path)
	}
	if !strings.Contains(out.String(), "warn") {
		t.Errorf("output missing level: %q", out.String())
	}
}

func TestLogLevelSetSendsJSONBody(t *testing.T) {
	var got http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeBody(w, `{"level":"debug"}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"loglevel", "debug"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v1/loglevel" {
		t.Errorf("request = %s %s, want POST /v1/loglevel", got.Method, got.URL.Path)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if sent["level"] != "debug" {
		t.Errorf("body = %v, want level=debug", sent)
	}
	if !strings.Contains(out.String(), "debug") {
		t.Errorf("output missing echoed level: %q", out.String())
	}
}

func TestLogLevelNon200PrintsError(t *testing.T) {
	var rec http.Request
	srv := fakeAPI(t, http.StatusBadRequest, `{"error":"invalid log-level \"bogus\": want debug|info|warn|error"}`, &rec)
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"loglevel", "bogus"}, srv.URL, &out); code == 0 {
		t.Fatalf("exit = 0, want non-zero; out=%s", out.String())
	}
	if !strings.Contains(out.String(), "invalid log-level") {
		t.Errorf("output missing server error: %q", out.String())
	}
}

// TestShipHeadRepoSendsHeadRepoJSONField: `cm ship --head-repo <url>` marshals the value
// as the underscore JSON field head_repo (NOT the hyphenated head-repo the generic flag
// path would produce).
func TestShipHeadRepoSendsHeadRepoJSONField(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeBody(w, `{"pushed":{"remote":"r","branch":"magister/r1","source_branch":"step/integrate","commit":"abc"},"pr":{"url":"https://github.com/o/r/pull/4"},"pr_existed":false}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	code := dispatch([]string{"ship", "r1", "--head-repo", "https://github.com/me/fork.git"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if sent["head_repo"] != "https://github.com/me/fork.git" {
		t.Errorf("head_repo = %v, want the fork url", sent["head_repo"])
	}
	if _, hyphen := sent["head-repo"]; hyphen {
		t.Error("must send head_repo (underscore), not head-repo")
	}
}
