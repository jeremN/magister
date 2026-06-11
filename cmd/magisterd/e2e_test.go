package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
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
	"concentus/internal/flow"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/store"
	"concentus/internal/supervisor"
	"concentus/internal/workspace"
)

// startDaemon runs the daemon on an ephemeral port and returns its base URL + a stop func.
func startDaemon(t *testing.T, db string) (string, func()) {
	t.Helper()
	stop := make(chan struct{})
	addrCh := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run([]string{"-addr", "127.0.0.1:0", "-db", db}, func(string) string { return "" }, stop, func(addr string) { addrCh <- addr })
	}()
	var addr string
	select {
	case addr = <-addrCh:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon never reported its address")
	}
	return "http://" + addr, func() { close(stop); <-done }
}

func postFlow(t *testing.T, base, yaml string) string {
	t.Helper()
	resp, err := http.Post(base+"/v1/runs", "application/x-yaml", bytes.NewBufferString(yaml))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("submit = %d", resp.StatusCode)
	}
	var rr struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&rr)
	return rr.ID
}

func runStatus(t *testing.T, base, id string) string {
	t.Helper()
	resp, err := http.Get(base + "/v1/runs/" + id)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var snap struct {
		Status string `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&snap)
	return snap.Status
}

func waitStatus(t *testing.T, base, id, want string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if runStatus(t, base, id) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %s (last=%s)", id, want, runStatus(t, base, id))
}

// waitStepStatus polls until the named step within a run reaches the given status.
func waitStepStatus(t *testing.T, base, id, stepID, want string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/v1/runs/" + id)
		if err == nil {
			var snap struct {
				Steps []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"steps"`
			}
			json.NewDecoder(resp.Body).Decode(&snap)
			resp.Body.Close()
			for _, s := range snap.Steps {
				if s.ID == stepID && s.Status == want {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("step %s in run %s never reached %s", stepID, id, want)
}

// approveStep approves a step, retrying while the API reports no gate is yet
// awaiting (409). After a resume, the DB shows the pre-crash awaiting_gate
// (stale) before the engine re-runs the step and re-registers the gate, so an
// approve can briefly arrive before there is anything to resolve — the correct
// client behaviour is to retry. Retries until accepted (200) or the deadline.
func approveStep(t *testing.T, base, id, stepID string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"approve": true})
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Post(base+"/v1/runs/"+id+"/steps/"+stepID+"/approve", "application/json", bytes.NewReader(body))
		if err == nil {
			code := resp.StatusCode
			resp.Body.Close()
			if code == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("approve of step %s in run %s was never accepted", stepID, id)
}

func TestE2EAutoFlowStreamsToCompletion(t *testing.T) {
	base, stop := startDaemon(t, filepath.Join(t.TempDir(), "e2e.db"))
	defer stop()
	id := postFlow(t, base, "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"true\" } }\n")

	resp, err := http.Get(base + "/v1/runs/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var kinds []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event: ") {
			kinds = append(kinds, strings.TrimPrefix(sc.Text(), "event: "))
		}
	}
	if len(kinds) == 0 || kinds[len(kinds)-1] != "run.done" {
		t.Fatalf("stream did not end on run.done: %v", kinds)
	}
	waitStatus(t, base, id, "succeeded")
}

func TestE2EManualGateBlocksThenApprove(t *testing.T) {
	base, stop := startDaemon(t, filepath.Join(t.TempDir(), "gate.db"))
	defer stop()
	id := postFlow(t, base, "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: manual }\n")

	waitStatus(t, base, id, "running") // run is running while the step awaits the gate
	waitStepStatus(t, base, id, "a", "awaiting_gate")
	approveStep(t, base, id, "a")
	waitStatus(t, base, id, "succeeded")
}

// crashDaemonAtGate simulates a hard kill (process death, no cleanup). It wires
// an in-process engine+supervisor on db, submits the flow, and polls the store
// directly until stepID is awaiting_gate — then closes the store WITHOUT a
// graceful shutdown. Because no run context is canceled, the engine never writes
// a terminal status: the run row stays "running" deterministically (the blocked
// run goroutine is abandoned, parked at the gate, exactly as it would vanish on a
// real crash). A daemon restarted on the same db then resumes it via
// LoadIncompleteRuns. This avoids the close-vs-RunCanceled race that a graceful
// stop would introduce. Returns the run ID.
func crashDaemonAtGate(t *testing.T, db, flowYAML, stepID string) string {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewApprovalRegistry()
	eng := &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.GitManager{Root: filepath.Join(filepath.Dir(db), "runs")},
		Gate:  &gate.Evaluator{Approver: &supervisor.RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: core.SystemClock{},
	}
	sup := supervisor.New(eng, st, reg)

	f, err := flow.ParseBytes([]byte(flowYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := flow.Validate(f); err != nil {
		t.Fatal(err)
	}
	id, err := sup.Submit(context.Background(), f, flowYAML, "", "")
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if rs, err := st.GetRun(context.Background(), id); err == nil {
			for _, s := range rs.Steps {
				if s.StepID == stepID && s.Status == core.StepAwaitingGate {
					// "crash": close the store without canceling/draining the run.
					// RunRunning was already persisted at run start, so the row
					// stays "running" for LoadIncompleteRuns to pick up.
					if err := st.Close(); err != nil {
						t.Fatal(err)
					}
					return string(id)
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("step %s never reached awaiting_gate before crash", stepID)
	return ""
}

func TestE2EEscalateBlocksThenApprove(t *testing.T) {
	base, stop := startDaemon(t, filepath.Join(t.TempDir(), "esc.db"))
	defer stop()
	// Auto gate whose verifier fails + on_fail: escalate, no retry → the gate
	// failure is escalated to a human; approving it completes the run.
	id := postFlow(t, base, "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"false\" }, on_fail: escalate }\n")

	waitStatus(t, base, id, "running")
	waitStepStatus(t, base, id, "a", "awaiting_gate")
	approveStep(t, base, id, "a")
	waitStatus(t, base, id, "succeeded")
}

// TestE2EEscalateKillAndResume covers spec §4.2/§7: an escalated gate re-escalates
// after a crash+resume (no special resume code — re-execution reconstructs it), and
// the resumed step shows pending (reset-to-pending) until it re-reaches the gate.
func TestE2EEscalateKillAndResume(t *testing.T) {
	db := filepath.Join(t.TempDir(), "esc-resume.db")
	const yaml = "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"false\" }, on_fail: escalate }\n"

	// Run until the escalated gate parks at awaiting_gate, then "crash".
	id := crashDaemonAtGate(t, db, yaml, "a")

	// Restart against the same DB → resume re-runs step a, the verifier fails again,
	// and it re-escalates. approveStep retries past any transient 409.
	base, stop := startDaemon(t, db)
	defer stop()
	waitStatus(t, base, id, "running")
	approveStep(t, base, id, "a")
	waitStatus(t, base, id, "succeeded")
}

func TestE2EKillAndResume(t *testing.T) {
	db := filepath.Join(t.TempDir(), "resume.db")
	// a two-step chain; step a has a manual gate, step b auto-passes after a.
	const yaml = "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: manual }\n  - id: b\n    needs: [a]\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"true\" } }\n"

	// Run until step a blocks at the gate, then "crash" — the run row is left
	// "running" in the DB with step a awaiting_gate.
	id := crashDaemonAtGate(t, db, yaml, "a")

	// Restart a real daemon against the same DB → resume on startup.
	base, stop := startDaemon(t, db)
	defer stop()
	waitStatus(t, base, id, "running") // resumed

	// Approve the re-blocked gate → both steps complete. approveStep retries on
	// 409: after a resume the DB still shows the stale awaiting_gate before the
	// engine re-runs step a and re-registers the gate.
	approveStep(t, base, id, "a")
	waitStatus(t, base, id, "succeeded")
}

// TestE2EIsolatedWorktreesTornDown runs fan-out isolated steps through the daemon
// (GitManager): each gets its own git worktree, and run-end teardown removes them
// while the base repo persists.
func TestE2EIsolatedWorktreesTornDown(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	base, stop := startDaemon(t, filepath.Join(tmp, "iso.db"))
	defer stop()
	id := postFlow(t, base, "name: f\nconcurrency: 2\nsteps:\n"+
		"  - id: root\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"true\" } }\n"+
		"  - id: a\n    needs: [root]\n    agent: mock\n    workspace: isolated\n    gate: { policy: auto, verifier: { command: \"true\" } }\n"+
		"  - id: b\n    needs: [root]\n    agent: mock\n    workspace: isolated\n    gate: { policy: auto, verifier: { command: \"true\" } }\n")
	waitStatus(t, base, id, "succeeded")

	runDir := filepath.Join(tmp, "runs", id)
	if _, err := os.Stat(filepath.Join(runDir, "base", ".git")); err != nil {
		t.Errorf("base repo should persist after the run: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(runDir, "wt")); err == nil && len(entries) != 0 {
		t.Errorf("isolated worktrees should be torn down at run end, found %d", len(entries))
	}
}
