package core

import (
	"context"
	"time"

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
	Repo        string // source repo for external-repo runs; empty = synthetic empty base
	Base        string // pinned base commit SHA; empty when Repo is empty
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
	// AppendEvents persists run-level events (e.g. run.started, run.done) that
	// have no associated step transition. The SSE hub reads via EventsSince.
	AppendEvents(ctx context.Context, id RunID, evs []event.Event) error
	LoadIncompleteRuns(ctx context.Context) ([]RunState, error)
	GetRun(ctx context.Context, id RunID) (RunState, error)
	ListRuns(ctx context.Context, f Filter) ([]RunSummary, error)
	EventsSince(ctx context.Context, id RunID, seq int64) ([]event.Event, error)
	// ReclaimableRuns returns the IDs of terminal runs (succeeded/failed/canceled)
	// whose last update is strictly before the cutoff. The scratch janitor uses it to
	// find runs whose scratch is past its retention TTL.
	ReclaimableRuns(ctx context.Context, before time.Time) ([]RunID, error)
	// Ping verifies the store backend is reachable. The readiness probe uses it.
	Ping(ctx context.Context) error
}
