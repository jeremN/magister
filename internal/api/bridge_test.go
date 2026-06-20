package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// syncBuf is a mutex-guarded sink so the request goroutine's access log and the
// test's read don't race under -race.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestRunSubmittedBridgeLog(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	sb := &syncBuf{}
	srv.Log = slog.New(slog.NewTextHandler(sb, nil))
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	resp, err := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString(oneStepFlow))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/runs = %d, want 201", resp.StatusCode)
	}
	reqID := resp.Header.Get("X-Request-ID")
	if reqID == "" {
		t.Fatal("response missing X-Request-ID")
	}

	out := sb.String()
	if !strings.Contains(out, "run submitted") {
		t.Errorf("missing 'run submitted' bridge log; got: %s", out)
	}
	if !strings.Contains(out, "req="+reqID) {
		t.Errorf("bridge log missing req=%s; got: %s", reqID, out)
	}
	if !strings.Contains(out, "run=") {
		t.Errorf("bridge log missing run=<id>; got: %s", out)
	}
}
