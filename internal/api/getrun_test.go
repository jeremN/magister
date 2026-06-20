package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"concentus/internal/core"
	"concentus/internal/metrics"
	"concentus/internal/store"
)

// getErrStore is a core.Store whose GetRun fails with a non-sentinel (storage)
// error, to drive the 500 path. All other methods come from the embedded Mem.
type getErrStore struct{ *store.Mem }

func (getErrStore) GetRun(context.Context, core.RunID) (core.RunState, error) {
	return core.RunState{}, errors.New("boom")
}

func TestGetRunUnknownIs404(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Get(hs.URL + "/v1/runs/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown run: got %d, want 404", resp.StatusCode)
	}
}

func TestGetRunStorageErrorIs500(t *testing.T) {
	srv := &Server{
		Store:   getErrStore{store.NewMem()},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: metrics.New("test"),
	}
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/v1/runs/anything")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("storage error: got %d, want 500", resp.StatusCode)
	}
}
