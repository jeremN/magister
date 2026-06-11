package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/engine"
	"concentus/internal/event"
	"concentus/internal/executor"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/store"
	"concentus/internal/supervisor"
	"concentus/internal/workspace"
)

func newServerOnly(t *testing.T) (*Server, *supervisor.Supervisor, core.Store) {
	t.Helper()
	st := store.NewMem()
	reg := supervisor.NewApprovalRegistry()
	bus := event.NewBus()
	eng := &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: &supervisor.RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: core.SystemClock{},
	}
	sup := supervisor.New(eng, st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	return &Server{Sup: sup, Store: st, Bus: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ShutdownTimeout: time.Second}, sup, st
}

func testServer(t *testing.T) (*httptest.Server, *supervisor.Supervisor, core.Store) {
	t.Helper()
	srv, sup, st := newServerOnly(t)
	hs := httptest.NewServer(srv.Router(""))
	t.Cleanup(func() { hs.Close() })
	return hs, sup, st
}

const oneStepFlow = "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"true\" } }\n"

func TestPostRunCreatesAndCompletes(t *testing.T) {
	hs, _, st := testServer(t)

	resp, err := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString(oneStepFlow))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v1/runs = %d: %s", resp.StatusCode, b)
	}
	var rr runResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatal(err)
	}
	if rr.ID == "" {
		t.Fatal("no run ID returned")
	}

	// auto gate → completes without approval
	waitForStatus(t, st, rr.ID, core.RunSucceeded)

	// GET snapshot reflects it
	gresp, err := http.Get(hs.URL + "/v1/runs/" + string(rr.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer gresp.Body.Close()
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("GET snapshot = %d", gresp.StatusCode)
	}
	var snap runSnapshot
	json.NewDecoder(gresp.Body).Decode(&snap)
	if snap.Status != "succeeded" || len(snap.Steps) != 1 {
		t.Errorf("snapshot wrong: %+v", snap)
	}
}

func TestPostRunRejectsInvalidFlow(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString("name: \nsteps: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid flow = %d, want 400", resp.StatusCode)
	}
}

func TestGetUnknownRun404(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Get(hs.URL + "/v1/runs/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown run = %d, want 404", resp.StatusCode)
	}
}

func TestApproveReleasesManualGate(t *testing.T) {
	hs, _, st := testServer(t)
	manualFlow := "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: manual }\n"
	resp, _ := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString(manualFlow))
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()

	// wait until the step is awaiting_gate
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s, _ := st.GetRun(nil, rr.ID) // nil ctx ok for Mem
		if len(s.Steps) == 1 && s.Steps[0].Status == core.StepAwaitingGate {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	body, _ := json.Marshal(approveRequest{Approve: true})
	areq, _ := http.NewRequest(http.MethodPost, hs.URL+"/v1/runs/"+string(rr.ID)+"/steps/a/approve", bytes.NewReader(body))
	aresp, err := http.DefaultClient.Do(areq)
	if err != nil {
		t.Fatal(err)
	}
	aresp.Body.Close()
	if aresp.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d", aresp.StatusCode)
	}
	waitForStatus(t, st, rr.ID, core.RunSucceeded)
}

func TestHealthz(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Get(hs.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}
}

func waitForStatus(t *testing.T, st core.Store, id core.RunID, want core.RunStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := st.GetRun(nil, id); err == nil && r.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %s", id, want)
}

// setupAPISourceRepo builds a committed fixture repo (skips if git absent).
func setupAPISourceRepo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init")
	run("config", "user.name", "fix")
	run("config", "user.email", "fix@example.com")
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "base")
	return src, run("rev-parse", "HEAD")
}

func TestCreateRunWithRepoPinsBase(t *testing.T) {
	src, sha := setupAPISourceRepo(t)
	hs, _, st := testServer(t)

	resp, err := http.Post(
		hs.URL+"/v1/runs?repo="+url.QueryEscape(src)+"&base=HEAD",
		"application/x-yaml", bytes.NewBufferString(oneStepFlow))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, b)
	}
	var rr runResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	waitForStatus(t, st, rr.ID, core.RunSucceeded)
	rs, err := st.GetRun(context.Background(), rr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Repo != src || rs.Base != sha {
		t.Errorf("persisted repo/base = %q/%q, want %q/%q", rs.Repo, rs.Base, src, sha)
	}
}

func TestCreateRunRejectsBadRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	hs, _, _ := testServer(t)
	resp, err := http.Post(
		hs.URL+"/v1/runs?repo=/no/such/repo",
		"application/x-yaml", bytes.NewBufferString(oneStepFlow))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, b)
	}
}
