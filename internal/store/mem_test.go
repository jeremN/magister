package store

import (
	"context"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/event"
)

func sameRunIDSet(got, want []core.RunID) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[core.RunID]bool, len(got))
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			return false
		}
	}
	return true
}

func TestMemReclaimableRuns(t *testing.T) {
	st := NewMem()
	ctx := context.Background()
	mk := func(id core.RunID, status core.RunStatus) {
		if err := st.CreateRun(ctx, core.RunState{ID: id, Status: core.RunPending}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetRunStatus(ctx, id, status, ""); err != nil {
			t.Fatal(err)
		}
	}
	mk("done", core.RunSucceeded)
	mk("failed", core.RunFailed)
	mk("canceled", core.RunCanceled)
	mk("active", core.RunRunning)

	got, err := st.ReclaimableRuns(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !sameRunIDSet(got, []core.RunID{"done", "failed", "canceled"}) {
		t.Errorf("future cutoff = %v, want the 3 terminal runs", got)
	}

	got, err = st.ReclaimableRuns(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("past cutoff = %v, want none", got)
	}
}

func TestMemRecordsTransitionsAndEvents(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	if err := m.CreateRun(ctx, core.RunState{ID: "r1", Name: "f", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	err := m.SaveStepTransition(ctx,
		core.StepState{RunID: "r1", StepID: "a", Status: core.StepSucceeded, Attempt: 1, Summary: "ok"},
		[]event.Event{{RunID: "r1", StepID: "a", Kind: event.StepDone}})
	if err != nil {
		t.Fatal(err)
	}

	got, err := m.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Steps) != 1 || got.Steps[0].Status != core.StepSucceeded {
		t.Fatalf("step not recorded: %+v", got.Steps)
	}

	evs, err := m.EventsSince(ctx, "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Seq != 1 {
		t.Fatalf("events not recorded with seq: %+v", evs)
	}
}

func TestMemUpsertsStepAndSetsRunStatus(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	_ = m.CreateRun(ctx, core.RunState{ID: "r1", Status: core.RunPending})
	_ = m.SaveStepTransition(ctx, core.StepState{RunID: "r1", StepID: "a", Status: core.StepRunning}, nil)
	_ = m.SaveStepTransition(ctx, core.StepState{RunID: "r1", StepID: "a", Status: core.StepSucceeded}, nil)
	if err := m.SetRunStatus(ctx, "r1", core.RunSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetRun(ctx, "r1")
	if len(got.Steps) != 1 {
		t.Fatalf("step should be upserted, got %d rows", len(got.Steps))
	}
	if got.Status != core.RunSucceeded {
		t.Fatalf("run status = %q", got.Status)
	}
}

func TestMemGetRunDeepCopiesSlices(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	if err := m.CreateRun(ctx, core.RunState{ID: "r1", Name: "f", Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveStepTransition(ctx,
		core.StepState{RunID: "r1", StepID: "a", Status: core.StepSucceeded,
			Artifacts: []core.Artifact{{StepID: "a", Path: "/tmp/a.md"}}},
		nil); err != nil {
		t.Fatal(err)
	}

	got, err := m.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the returned slices must not corrupt the store's copy.
	got.Steps[0].Status = core.StepFailed
	got.Steps[0].Artifacts[0].Path = "/tmp/TAMPERED"

	again, err := m.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if again.Steps[0].Status != core.StepSucceeded {
		t.Errorf("step status mutated through returned slice: %s", again.Steps[0].Status)
	}
	if again.Steps[0].Artifacts[0].Path != "/tmp/a.md" {
		t.Errorf("artifact path mutated through returned slice: %s", again.Steps[0].Artifacts[0].Path)
	}
}

func TestMemLoadIncompleteRuns(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	if err := m.CreateRun(ctx, core.RunState{ID: "r1", Name: "a", Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateRun(ctx, core.RunState{ID: "r2", Name: "b", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateRun(ctx, core.RunState{ID: "r3", Name: "c", Status: core.RunSucceeded}); err != nil {
		t.Fatal(err)
	}

	inc, err := m.LoadIncompleteRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inc) != 2 { // r1 (running) + r2 (pending); r3 (succeeded) excluded
		t.Fatalf("want 2 incomplete runs, got %d: %+v", len(inc), inc)
	}
	for _, r := range inc {
		if r.Status != core.RunRunning && r.Status != core.RunPending {
			t.Errorf("run %q has terminal status %s in incomplete set", r.ID, r.Status)
		}
	}
}

func TestMemPing(t *testing.T) {
	if err := NewMem().Ping(context.Background()); err != nil {
		t.Fatalf("Mem.Ping = %v, want nil", err)
	}
}
