package core

import (
	"context"

	"concentus/internal/event"
)

// RunState is the persisted state of a run (for queries and, from M2, resume).
type RunState struct {
	ID          RunID
	Name        string
	FlowYAML    string
	Status      RunStatus
	Concurrency int
	Err         string
	Steps       []StepState
}

// StepState is the persisted state of one step.
type StepState struct {
	RunID     RunID
	StepID    string
	Status    StepStatus
	Attempt   int
	Summary   string
	CostUSD   float64
	WorkDir   string
	Err       string
	Artifacts []Artifact
}

// RunSummary is a lightweight row for listing.
type RunSummary struct {
	ID     RunID
	Name   string
	Status RunStatus
}

// Filter narrows ListRuns. A zero value means "all".
type Filter struct {
	Status RunStatus
}

// Store is the durable, synchronous persistence port. The engine calls it at
// commit points (persist-then-publish). An in-memory implementation backs M1;
// SQLite backs M2 onward.
type Store interface {
	CreateRun(ctx context.Context, r RunState) error
	SaveStepTransition(ctx context.Context, st StepState, evs []event.Event) error
	SetRunStatus(ctx context.Context, id RunID, status RunStatus, errMsg string) error
	LoadIncompleteRuns(ctx context.Context) ([]RunState, error)
	GetRun(ctx context.Context, id RunID) (RunState, error)
	ListRuns(ctx context.Context, f Filter) ([]RunSummary, error)
	EventsSince(ctx context.Context, id RunID, seq int64) ([]event.Event, error)
}
