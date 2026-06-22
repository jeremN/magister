package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func levelOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatalf("decode loglevel body: %v", err)
	}
	return b["level"]
}

func TestGetLogLevelReturnsCurrent(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelWarn)
	srv.LogLevel = lv
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/loglevel")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/loglevel = %d, want 200", resp.StatusCode)
	}
	if got := levelOf(t, resp); got != "warn" {
		t.Errorf("level = %q, want warn", got)
	}
}

func TestSetLogLevelChangesLevel(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelInfo)
	srv.LogLevel = lv
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	resp, err := http.Post(hs.URL+"/v1/loglevel", "application/json", strings.NewReader(`{"level":"debug"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/loglevel = %d, want 200", resp.StatusCode)
	}
	if got := levelOf(t, resp); got != "debug" {
		t.Errorf("echoed level = %q, want debug", got)
	}
	if lv.Level() != slog.LevelDebug {
		t.Errorf("LevelVar = %v, want Debug (the live threshold did not change)", lv.Level())
	}
}

func TestSetLogLevelRejectsBadValue(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	srv.LogLevel = new(slog.LevelVar)
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	resp, err := http.Post(hs.URL+"/v1/loglevel", "application/json", strings.NewReader(`{"level":"bogus"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST bad level = %d, want 400", resp.StatusCode)
	}
	var b map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&b)
	if !strings.Contains(b["error"], "invalid log-level") {
		t.Errorf("error = %q, want it to mention invalid log-level", b["error"])
	}
}

func TestSetLogLevelRejectsBadJSON(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	srv.LogLevel = new(slog.LevelVar)
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	resp, err := http.Post(hs.URL+"/v1/loglevel", "application/json", strings.NewReader(`{bad`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST malformed json = %d, want 400", resp.StatusCode)
	}
}

func TestLogLevelNilReturns503(t *testing.T) {
	srv, _, _ := newServerOnly(t) // LogLevel left nil
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	get, err := http.Get(hs.URL + "/v1/loglevel")
	if err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if get.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET with nil LogLevel = %d, want 503", get.StatusCode)
	}
	post, err := http.Post(hs.URL+"/v1/loglevel", "application/json", strings.NewReader(`{"level":"debug"}`))
	if err != nil {
		t.Fatal(err)
	}
	post.Body.Close()
	if post.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("POST with nil LogLevel = %d, want 503", post.StatusCode)
	}
}

func TestLogLevelBehindAuth(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	srv.LogLevel = new(slog.LevelVar)
	hs := httptest.NewServer(srv.Router("secret"))
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/loglevel")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated /v1/loglevel = %d, want 401", resp.StatusCode)
	}
}
