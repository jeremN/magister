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
	"concentus/internal/host"
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
	runGit(t, src, "init")
	runGit(t, src, "config", "user.name", "fix")
	runGit(t, src, "config", "user.email", "fix@example.com")
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", "-A")
	runGit(t, src, "commit", "-m", "base")
	return src, runGit(t, src, "rev-parse", "HEAD")
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

func TestGetRunSurfacesScratchPathForExternalRepo(t *testing.T) {
	srv, _, st := newServerOnly(t)
	srv.ScratchRoot = "/var/runs"
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	if err := st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Name: "f", Status: core.RunSucceeded, Repo: "/abs/proj", Base: "abc",
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(hs.URL + "/v1/runs/r1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var snap runSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Scratch != filepath.Join("/var/runs", "r1", "base") {
		t.Errorf("scratch = %q, want /var/runs/r1/base", snap.Scratch)
	}
}

func TestGetRunOmitsScratchForSyntheticRun(t *testing.T) {
	srv, _, st := newServerOnly(t)
	srv.ScratchRoot = "/var/runs"
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	if err := st.CreateRun(context.Background(), core.RunState{
		ID: "r2", Name: "f", Status: core.RunSucceeded, // no Repo
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(hs.URL + "/v1/runs/r2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), `"scratch"`) {
		t.Errorf("synthetic run should omit the scratch field, body=%s", body)
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

// newGitServer is like testServer but its engine uses a real GitManager, so
// external-repo runs actually clone + produce a scratch base (needed by push).
func newGitServer(t *testing.T) (*httptest.Server, core.Store) {
	t.Helper()
	st := store.NewMem()
	reg := supervisor.NewApprovalRegistry()
	bus := event.NewBus()
	eng := &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.GitManager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: &supervisor.RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: core.SystemClock{},
	}
	sup := supervisor.New(eng, st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	srv := &Server{Sup: sup, Store: st, Bus: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ShutdownTimeout: time.Second}
	hs := httptest.NewServer(srv.Router(""))
	t.Cleanup(func() { hs.Close() })
	return hs, st
}

func TestPushEndpointDeliversToRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src, _ := setupAPISourceRepo(t)
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")
	runGit(t, src, "remote", "add", "origin", bare)

	hs, st := newGitServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs?repo="+url.QueryEscape(src)+"&base=HEAD",
		"application/x-yaml", bytes.NewBufferString(extRepoFlowAPI))
	if err != nil {
		t.Fatal(err)
	}
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()
	waitForStatus(t, st, rr.ID, core.RunSucceeded)

	presp, err := http.Post(hs.URL+"/v1/runs/"+string(rr.ID)+"/push", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer presp.Body.Close()
	if presp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(presp.Body)
		t.Fatalf("push = %d, want 200: %s", presp.StatusCode, b)
	}
	var pr pushResponse
	if err := json.NewDecoder(presp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pr.Branch != "magister/"+string(rr.ID) {
		t.Errorf("dest = %q, want magister/%s", pr.Branch, rr.ID)
	}
	if got := runGit(t, bare, "rev-parse", pr.Branch); got != pr.Commit {
		t.Errorf("remote ref = %q, want %q", got, pr.Commit)
	}
}

func TestPushEndpointUnknownRun404(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs/nope/push", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPushEndpointNonExternalRepo400(t *testing.T) {
	hs, _, st := testServer(t)
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Status: core.RunSucceeded,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n",
	})
	resp, err := http.Post(hs.URL+"/v1/runs/r1/push", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPushEndpointNotSucceeded409(t *testing.T) {
	hs, _, st := testServer(t)
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Repo: "/abs/proj", Status: core.RunPending,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n",
	})
	resp, err := http.Post(hs.URL+"/v1/runs/r1/push", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// runGit is a local git helper for the api tests.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

const extRepoFlowAPI = `name: external-repo
concurrency: 2
steps:
  - id: build-api
    agent: mock
    workspace: isolated
    gate: { policy: auto, verifier: { command: "true" } }
  - id: build-ui
    agent: mock
    workspace: isolated
    gate: { policy: auto, verifier: { command: "true" } }
  - id: integrate
    needs: [build-api, build-ui]
    workspace: isolated
    join: { strategy: merge }
    gate: { policy: auto, verifier: { command: "true" } }
`

// ghAPIStub returns the absolute path to the shared fake-gh stub.
func ghAPIStub(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "host", "testdata", "fake-gh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("fake-gh stub missing: %v", err)
	}
	return abs
}

func TestPREndpointOpensPR(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src := t.TempDir()
	runGit(t, src, "init")
	runGit(t, src, "remote", "add", "origin", "https://github.com/o/r.git")

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
	sup.Host = &host.Runner{Bin: ghAPIStub(t)}
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	t.Setenv("FAKE_GH_PR_URL", "https://github.com/o/r/pull/5")
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Name: "demo", Repo: src, Status: core.RunSucceeded,
		FlowYAML: "name: demo\nsteps:\n  - id: integrate\n    agent: mock\n",
		Steps: []core.StepState{{
			RunID: "r1", StepID: "integrate", Status: core.StepSucceeded,
			Artifacts: []core.Artifact{{StepID: "integrate", Branch: "step/integrate", Commit: "abc"}},
		}},
	})
	srv := &Server{Sup: sup, Store: st, Bus: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ShutdownTimeout: time.Second}
	hs := httptest.NewServer(srv.Router(""))
	t.Cleanup(hs.Close)

	resp, err := http.Post(hs.URL+"/v1/runs/r1/pr", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("pr = %d, want 200: %s", resp.StatusCode, b)
	}
	var pr prResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pr.URL != "https://github.com/o/r/pull/5" || pr.Repo != "o/r" || pr.Head != "magister/r1" {
		t.Errorf("response = %+v", pr)
	}
}

func TestPREndpointUnknownRun404(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs/nope/pr", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestShipEndpointUnknownRun404(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs/nope/ship", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestShipEndpointNonExternal400(t *testing.T) {
	hs, _, st := testServer(t)
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Status: core.RunSucceeded,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n",
	})
	resp, err := http.Post(hs.URL+"/v1/runs/r1/ship", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestShipEndpointPropagatesPRErrorAfterPush: a real external-repo run with a local
// bare origin → /ship pushes (branch lands on the bare) then the PR step fails to
// parse the local origin as github → 400 from the *PRError, proving both the push
// side-effect and the *PRError mapping path.
func TestShipEndpointPropagatesPRErrorAfterPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src, _ := setupAPISourceRepo(t)
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")
	runGit(t, src, "remote", "add", "origin", bare)

	hs, st := newGitServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs?repo="+url.QueryEscape(src)+"&base=HEAD",
		"application/x-yaml", bytes.NewBufferString(extRepoFlowAPI))
	if err != nil {
		t.Fatal(err)
	}
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()
	waitForStatus(t, st, rr.ID, core.RunSucceeded)

	sresp, err := http.Post(hs.URL+"/v1/runs/"+string(rr.ID)+"/ship", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	if sresp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(sresp.Body)
		t.Fatalf("ship = %d, want 400 (pr parse of local origin): %s", sresp.StatusCode, b)
	}
	if sha := runGit(t, bare, "rev-parse", "--verify", "magister/"+string(rr.ID)); sha == "" {
		t.Error("push should have delivered magister/<run> before the pr step failed")
	}
}
