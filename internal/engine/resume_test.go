package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"concentus/internal/core"
	"concentus/internal/engine"
	"concentus/internal/event"
	"concentus/internal/flow"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

// recordingExec is a mock executor that counts how many times each step runs,
// records the input artifact paths each step received, and writes a small
// artifact — so a resumed run (and the propagation of seeded artifacts into
// downstream inputs) is observable.
type recordingExec struct {
	mu     sync.Mutex
	ran    map[string]int
	inputs map[string][]string // step ID -> input artifact paths it was handed
}

func (m *recordingExec) Run(_ context.Context, t core.Task) (core.Result, error) {
	m.mu.Lock()
	m.ran[t.StepID]++
	if m.inputs == nil {
		m.inputs = map[string][]string{}
	}
	for _, in := range t.Inputs {
		m.inputs[t.StepID] = append(m.inputs[t.StepID], in.Path)
	}
	m.mu.Unlock()
	out := filepath.Join(t.WorkDir, t.StepID+".out.md")
	if err := os.WriteFile(out, []byte(t.StepID), 0o644); err != nil {
		return core.Result{}, err
	}
	return core.Result{StepID: t.StepID, Summary: t.StepID + " ok",
		Artifacts: []core.Artifact{{StepID: t.StepID, Path: out}}, CostUSD: 0.01}, nil
}

func newEngine(dir string, st core.Store, ex core.Executor) *engine.Engine {
	return &engine.Engine{
		Execs: map[string]core.Executor{"mock": ex},
		WS:    &workspace.Manager{Root: dir},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st,
		Bus:   event.NewBus(),
		Clock: core.SystemClock{},
	}
}

func TestResumeSkipsSucceededRerunsRest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := store.NewMem()

	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
		{ID: "b", Needs: []string{"a"}, Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	if err := flow.Validate(f); err != nil {
		t.Fatal(err)
	}

	// Persisted mid-flight state: run running, a already succeeded (+artifact), b was running.
	aArt := filepath.Join(dir, "a.out.md")
	if err := os.WriteFile(aArt, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(ctx, core.RunState{ID: "r1", Name: "f", FlowYAML: "x", Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveStepTransition(ctx,
		core.StepState{RunID: "r1", StepID: "a", Status: core.StepSucceeded, Attempt: 1,
			Summary: "a ok", Artifacts: []core.Artifact{{StepID: "a", Path: aArt}}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveStepTransition(ctx,
		core.StepState{RunID: "r1", StepID: "b", Status: core.StepRunning, Attempt: 1}, nil); err != nil {
		t.Fatal(err)
	}

	rs, err := st.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	ex := &recordingExec{ran: map[string]int{}}
	if err := newEngine(dir, st, ex).Resume(ctx, rs, f); err != nil {
		t.Fatalf("resume: %v", err)
	}

	if ex.ran["a"] != 0 {
		t.Errorf("succeeded step a must NOT re-run, ran %d times", ex.ran["a"])
	}
	if ex.ran["b"] != 1 {
		t.Errorf("interrupted step b must run once, ran %d times", ex.ran["b"])
	}
	// Resume's contract (spec §7): the seeded step's persisted artifacts must
	// feed downstream inputs. b should have received a's artifact, not run blind.
	foundA := false
	for _, p := range ex.inputs["b"] {
		if p == aArt {
			foundA = true
		}
	}
	if !foundA {
		t.Errorf("step b should receive seeded artifact %q as input, got %v", aArt, ex.inputs["b"])
	}
	got, err := st.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != core.RunSucceeded {
		t.Errorf("resumed run status = %s, want succeeded", got.Status)
	}
}

func TestKillAndResumeAcrossSQLiteReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "runs.db")

	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
		{ID: "b", Needs: []string{"a"}, Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
		{ID: "c", Needs: []string{"b"}, Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	if err := flow.Validate(f); err != nil {
		t.Fatal(err)
	}

	// --- pre-crash: persist run running, a succeeded (+artifact), b running. ---
	s1, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	aArt := filepath.Join(dir, "a.out.md")
	if err := os.WriteFile(aArt, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s1.CreateRun(ctx, core.RunState{ID: "r1", Name: "f", FlowYAML: "name: f\n", Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}
	if err := s1.SaveStepTransition(ctx,
		core.StepState{RunID: "r1", StepID: "a", Status: core.StepSucceeded, Attempt: 1,
			Summary: "a ok", Artifacts: []core.Artifact{{StepID: "a", Path: aArt}}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s1.SaveStepTransition(ctx,
		core.StepState{RunID: "r1", StepID: "b", Status: core.StepRunning, Attempt: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil { // "crash": no graceful terminal status write
		t.Fatal(err)
	}

	// --- post-crash: fresh process reopens the same file and resumes. ---
	s2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	inc, err := s2.LoadIncompleteRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inc) != 1 || inc[0].ID != "r1" {
		t.Fatalf("want r1 incomplete, got %+v", inc)
	}

	ex := &recordingExec{ran: map[string]int{}}
	if err := newEngine(dir, s2, ex).Resume(ctx, inc[0], f); err != nil {
		t.Fatalf("resume: %v", err)
	}

	if ex.ran["a"] != 0 {
		t.Errorf("a (succeeded pre-crash) must not re-run, ran %d", ex.ran["a"])
	}
	if ex.ran["b"] != 1 || ex.ran["c"] != 1 {
		t.Errorf("b and c must each run once; got b=%d c=%d", ex.ran["b"], ex.ran["c"])
	}
	got, err := s2.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != core.RunSucceeded {
		t.Errorf("resumed run status = %s, want succeeded", got.Status)
	}
	for _, st := range got.Steps {
		if st.Status != core.StepSucceeded {
			t.Errorf("step %s status = %s, want succeeded", st.StepID, st.Status)
		}
	}
}
