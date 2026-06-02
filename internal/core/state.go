// Package core holds the orchestrator's domain types, state enums, and the
// interfaces (ports) the engine depends on. It imports only internal/flow and
// internal/event, so adapters can depend on it without import cycles.
package core

// RunID uniquely identifies one execution of a flow.
type RunID string

// RunStatus is the lifecycle state of a run.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCanceled  RunStatus = "canceled"
)

// StepStatus is the lifecycle state of a single step within a run.
type StepStatus string

const (
	StepPending      StepStatus = "pending"
	StepReady        StepStatus = "ready"
	StepRunning      StepStatus = "running"
	StepAwaitingGate StepStatus = "awaiting_gate"
	StepRetrying     StepStatus = "retrying"
	StepSucceeded    StepStatus = "succeeded"
	StepFailed       StepStatus = "failed"
	StepSkipped      StepStatus = "skipped"
	StepCanceled     StepStatus = "canceled"
)
