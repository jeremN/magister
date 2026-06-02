// Package event defines engine events and an in-process publish/subscribe bus.
// It imports no other internal package, so any layer can subscribe without
// creating an import cycle.
package event

import "time"

// Kind names a thing that happened during a run.
type Kind string

const (
	RunStarted   Kind = "run.started"
	RunDone      Kind = "run.done"
	StepStarted  Kind = "step.started"
	StepDone     Kind = "step.done"
	StepFailed   Kind = "step.failed"
	StepRetrying Kind = "step.retrying"
)

// Event is one occurrence during a run. Seq is assigned by the store on persist
// (0 for a live, not-yet-persisted event). RunID/StepID are plain strings so
// this package stays dependency-free.
type Event struct {
	Seq     int64     `json:"seq"`
	RunID   string    `json:"run"`
	StepID  string    `json:"step,omitempty"`
	Kind    Kind      `json:"kind"`
	Summary string    `json:"summary,omitempty"`
	CostUSD float64   `json:"cost_usd,omitempty"`
	Attempt int       `json:"attempt,omitempty"`
	Err     string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
}
