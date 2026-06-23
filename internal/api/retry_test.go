package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"concentus/internal/core"
)

func TestRetryEndpointUnknownRun404(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs/nope/retry", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRetryEndpointSucceeded409(t *testing.T) {
	hs, _, st := testServer(t)
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Status: core.RunSucceeded,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n",
	})
	resp, err := http.Post(hs.URL+"/v1/runs/r1/retry", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestRetryEndpointResumesFailedRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	hs, st := newGitServer(t)
	flag := filepath.Join(t.TempDir(), "ok")
	yaml := "name: f\nsteps:\n  - id: a\n    agent: mock\n    workspace: isolated\n    gate: { policy: auto, verifier: { command: \"test -f " + flag + "\" } }\n"

	resp, err := http.Post(hs.URL+"/v1/runs", "application/x-yaml", strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()
	waitForStatus(t, st, rr.ID, core.RunFailed)

	if err := os.WriteFile(flag, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rresp, err := http.Post(hs.URL+"/v1/runs/"+string(rr.ID)+"/retry", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rresp.Body.Close()
	if rresp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(rresp.Body)
		t.Fatalf("retry = %d, want 202: %s", rresp.StatusCode, b)
	}
	var got runResponse
	json.NewDecoder(rresp.Body).Decode(&got)
	if got.ID != rr.ID {
		t.Errorf("retry id = %q, want the same id %q", got.ID, rr.ID)
	}
	waitForStatus(t, st, rr.ID, core.RunSucceeded)
}
