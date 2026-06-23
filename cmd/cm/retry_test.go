package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRetrySubcommandPostsRetry(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"id": "01ABC"})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"retry", "01ABC"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, want 0; out=%s", code, out.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/runs/01ABC/retry" {
		t.Errorf("request = %s %s, want POST /v1/runs/01ABC/retry", gotMethod, gotPath)
	}
	if !strings.Contains(out.String(), "resuming 01ABC") {
		t.Errorf("output = %q, want it to contain 'resuming 01ABC'", out.String())
	}
}

func TestRetrySubcommandWatchStreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/01ABC/retry":
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"id": "01ABC"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/01ABC/events":
			io.WriteString(w, "event: run.done\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"retry", "01ABC", "--watch"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, want 0; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "run.done") {
		t.Errorf("watch output = %q, want streamed events", out.String())
	}
}

func TestRetrySubcommandUsage(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"retry"}, "http://x", &out); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestRetrySubcommandServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"error":"run \"x\" succeeded; nothing to retry"}`)
	}))
	defer srv.Close()
	var out bytes.Buffer
	if code := dispatch([]string{"retry", "x"}, srv.URL, &out); code != 1 {
		t.Errorf("exit = %d, want 1 (server 409)", code)
	}
}
