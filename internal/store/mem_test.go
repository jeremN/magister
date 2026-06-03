package store

import (
	"context"
	"testing"

	"concentus/internal/core"
	"concentus/internal/event"
)

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
