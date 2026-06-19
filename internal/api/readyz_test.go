package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"concentus/internal/metrics"
	"concentus/internal/store"
)

func readyBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatalf("decode readyz body: %v", err)
	}
	return b["status"]
}

func TestReadyzReady(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Get(hs.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz = %d, want 200", resp.StatusCode)
	}
	if s := readyBody(t, resp); s != "ready" {
		t.Errorf("status = %q, want ready", s)
	}
}

func TestReadyzDraining(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	srv.SetDraining(true)
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz while draining = %d, want 503", resp.StatusCode)
	}
	if s := readyBody(t, resp); s != "draining" {
		t.Errorf("status = %q, want draining", s)
	}
}

type pingErrStore struct{ *store.Mem }

func (pingErrStore) Ping(context.Context) error { return errors.New("store down") }

func TestReadyzStoreUnreachable(t *testing.T) {
	srv := &Server{
		Store:   pingErrStore{store.NewMem()},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: metrics.New("test"),
	}
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz with dead store = %d, want 503", resp.StatusCode)
	}
	if s := readyBody(t, resp); s != "store unreachable" {
		t.Errorf("status = %q, want store unreachable", s)
	}
}

func TestReadyzAuthExempt(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	hs := httptest.NewServer(srv.Router("secret"))
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("readyz should be auth-exempt, got 401")
	}
}
