package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"concentus/internal/core"
)

// errReadCloser returns a custom error partway through a read (NOT a
// MaxBytesError), simulating a torn connection / client disconnect.
type errReadCloser struct{ err error }

func (e errReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e errReadCloser) Close() error             { return nil }

// TestDecodeJSONNonMaxBytesErrorIsNot413 verifies that a genuine read error
// (not body-too-large) is surfaced raw by decodeJSON, so the caller's generic
// 400 path handles it — it must NOT be reclassified as errBodyTooLarge (413).
func TestDecodeJSONNonMaxBytesErrorIsNot413(t *testing.T) {
	boom := errors.New("connection reset by peer")
	r := httptest.NewRequest(http.MethodPost, "/v1/runs/r1/pr", errReadCloser{err: boom})
	w := httptest.NewRecorder()

	var dst struct{}
	err := decodeJSON(w, r, &dst)
	if err == nil {
		t.Fatal("expected a read error from decodeJSON")
	}
	if errors.Is(err, errBodyTooLarge) {
		t.Fatalf("non-MaxBytes read error must NOT map to errBodyTooLarge (413), got %v", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("decodeJSON dropped the raw read error: got %v, want it to wrap %v", err, boom)
	}
}

var _ io.ReadCloser = errReadCloser{}

// TestCancelUnknownRun404 verifies that canceling a run that was never created
// returns 404 (not 409 or any other status).
func TestCancelUnknownRun404(t *testing.T) {
	hs, _, _ := testServer(t)
	req, _ := http.NewRequest(http.MethodDelete, hs.URL+"/v1/runs/does-not-exist", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cancel unknown run = %d, want 404", resp.StatusCode)
	}
}

// TestCancelTerminalRun409 verifies that canceling a known but already-terminal
// (succeeded) run returns 409 Conflict, not 404.
func TestCancelTerminalRun409(t *testing.T) {
	hs, _, st := testServer(t)
	if err := st.CreateRun(context.Background(), core.RunState{
		ID:     "r1",
		Status: core.RunSucceeded,
	}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodDelete, hs.URL+"/v1/runs/r1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("cancel terminal run = %d, want 409", resp.StatusCode)
	}
}

// TestCancelActiveRun202 verifies that canceling a genuinely active run returns 202.
func TestCancelActiveRun202(t *testing.T) {
	hs, _, st := testServer(t)
	// Post a flow with a slow mock step so the run stays active long enough to cancel.
	manualFlow := "name: f\nsteps:\n  - id: a\n    agent: mock\n    prompt: p\n    gate: { policy: manual }\n"
	resp, err := http.Post(hs.URL+"/v1/runs", "application/x-yaml", strings.NewReader(manualFlow))
	if err != nil {
		t.Fatal(err)
	}
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr) //nolint:errcheck
	resp.Body.Close()

	// Wait until the run is at least running (not just pending).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r, _ := st.GetRun(nil, rr.ID); r.Status == core.RunRunning {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	req, _ := http.NewRequest(http.MethodDelete, hs.URL+"/v1/runs/"+string(rr.ID), nil)
	cresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer cresp.Body.Close()
	if cresp.StatusCode != http.StatusAccepted {
		t.Errorf("cancel active run = %d, want 202", cresp.StatusCode)
	}
}

// TestBodyTooLarge413 verifies that posting an over-limit body to a decodeJSON
// route returns 413 Request Entity Too Large, not 400.
func TestBodyTooLarge413(t *testing.T) {
	hs, _, st := testServer(t)
	// /v1/runs/{id}/pr uses decodeJSON; ensure the run exists so we get past the
	// route and into the body-decode path.
	if err := st.CreateRun(context.Background(), core.RunState{
		ID:     "r1",
		Status: core.RunSucceeded,
		Repo:   "/abs/proj",
	}); err != nil {
		t.Fatal(err)
	}
	// Build a body larger than maxBodyBytes (1 MiB).
	big := strings.Repeat("x", int(maxBodyBytes)+1)
	resp, err := http.Post(hs.URL+"/v1/runs/r1/pr", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("over-limit body = %d, want 413", resp.StatusCode)
	}
}
