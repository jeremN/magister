package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
