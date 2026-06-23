package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"concentus/internal/core"
)

func TestGCEndpointReturnsCount(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/gc", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Reclaimed int `json:"reclaimed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Reclaimed != 0 {
		t.Errorf("reclaimed = %d, want 0 (no scratch dirs)", body.Reclaimed)
	}
}

func TestGCEndpointBadOlderThanIs400(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/gc?older_than=notaduration", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestReclaimScratchEndpointUnknownIs404(t *testing.T) {
	hs, _, _ := testServer(t)
	req, _ := http.NewRequest(http.MethodDelete, hs.URL+"/v1/runs/nope/scratch", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestReclaimScratchEndpointNonTerminalIs409(t *testing.T) {
	hs, _, st := testServer(t)
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r", Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodDelete, hs.URL+"/v1/runs/r/scratch", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestReclaimScratchEndpointTerminalReturnsRemoved(t *testing.T) {
	hs, _, st := testServer(t)
	if err := st.CreateRun(context.Background(), core.RunState{ID: "done", Status: core.RunSucceeded}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodDelete, hs.URL+"/v1/runs/done/scratch", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Removed bool `json:"removed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Removed {
		t.Errorf("removed = true, want false (no scratch dir existed)")
	}
}
