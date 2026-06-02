package core

import (
	"context"
	"time"

	"concentus/internal/event"
	"concentus/internal/flow"
)

// Artifact points at something a step produced on disk. The filesystem is the
// source of truth for handoffs; artifacts are just pointers.
type Artifact struct {
	StepID string
	Path   string
}

// Task is what the engine hands an executor for one step attempt.
type Task struct {
	RunID   RunID
	StepID  string
	Role    string
	Prompt  string
	Inputs  []Artifact
	WorkDir string
}

// Result is what an executor returns for one step.
type Result struct {
	StepID    string
	Summary   string
	Artifacts []Artifact
	CostUSD   float64
}

// Executor runs one step's work. This is the seam a future non-CLI executor
// slots into.
type Executor interface {
	Run(ctx context.Context, t Task) (Result, error)
}

// Workspace hands a step a working directory and a cleanup func.
type Workspace interface {
	For(runID RunID, s *flow.Step) (dir string, cleanup func() error, err error)
}

// Publisher receives engine events for live observers. Lossy by contract.
type Publisher interface {
	Publish(e event.Event)
}

// Clock is injected so retry/backoff/timeout logic is testable without sleeping.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// SystemClock is the real Clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time                         { return time.Now() }
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
