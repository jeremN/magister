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
