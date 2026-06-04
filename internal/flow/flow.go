// Package flow defines the declarative flow schema and its validation.
//
// Parallelism is not a dedicated construct: each Step declares its dependencies
// via Needs, and the engine derives the DAG from them. Fan-out = N steps sharing
// a dependency; fan-in = one step depending on several.
package flow

// WSMode controls how a step gets its working directory.
type WSMode string

const (
	WSShared   WSMode = "shared"
	WSIsolated WSMode = "isolated"
)

// GatePolicy controls how the gate after a step is resolved.
type GatePolicy string

const (
	GateManual      GatePolicy = "manual"
	GateAuto        GatePolicy = "auto"
	GateConditional GatePolicy = "conditional"
)

// JoinStrategy controls how a fan-in step combines its inputs.
type JoinStrategy string

const (
	JoinMerge      JoinStrategy = "merge"
	JoinSelect     JoinStrategy = "select"
	JoinSynthesize JoinStrategy = "synthesize"
)

// FailPolicy controls what happens when a gate fails.
//
// Under the unified attempt budget (engine.runStep), a Retry policy already re-runs
// the whole attempt (execute + gate) on any failure, so:
//   - abort (default): fail the run once the budget is spent.
//   - retry: an explicit synonym for the default — behaviourally identical to abort
//     (the validator still requires a Retry policy with it); kept to document intent.
//   - escalate: when an AUTO gate's budget is spent, convert the failed gate into a
//     human approval instead of failing — approve continues, reject aborts. No-op for
//     manual gates, where a rejection is already a human decision.
type FailPolicy string

const (
	FailAbort    FailPolicy = "abort"
	FailRetry    FailPolicy = "retry"
	FailEscalate FailPolicy = "escalate"
)

// Flow is a whole pipeline loaded from a YAML file.
type Flow struct {
	Name        string  `yaml:"name"`
	Concurrency int     `yaml:"concurrency"`
	Steps       []*Step `yaml:"steps"`
}

// Step is a node in the DAG: either a regular agent step (Agent set) or a join
// step (Join set), never both.
type Step struct {
	ID        string       `yaml:"id"`
	Needs     []string     `yaml:"needs"`
	Agent     string       `yaml:"agent"`
	Role      string       `yaml:"role"`
	Prompt    string       `yaml:"prompt"`
	Workspace WSMode       `yaml:"workspace"`
	Timeout   Duration     `yaml:"timeout"`
	Retry     *RetryPolicy `yaml:"retry"`
	Join      *Join        `yaml:"join"`
	Gate      Gate         `yaml:"gate"`
}

// RetryPolicy bounds re-execution of a step (and gate-driven retries).
type RetryPolicy struct {
	Max     int      `yaml:"max"`
	Backoff Duration `yaml:"backoff"`
}

// Gate sits after a step and decides whether the flow proceeds.
type Gate struct {
	Policy    GatePolicy `yaml:"policy"`
	Verifier  *Verifier  `yaml:"verifier"`
	Condition *Condition `yaml:"condition"`
	OnFail    FailPolicy `yaml:"on_fail"`
}

// Verifier configures an auto gate: a shell command, exit 0 = pass.
type Verifier struct {
	Command string `yaml:"command"`
}

// Condition configures a conditional gate. The expr is compiled and evaluated
// in M5; M0/M1 only validate its presence.
type Condition struct {
	Expr string `yaml:"expr"`
}

// Join marks a fan-in step and says how to combine upstream branches.
type Join struct {
	Strategy   JoinStrategy `yaml:"strategy"`
	Agent      string       `yaml:"agent"`
	OnConflict FailPolicy   `yaml:"on_conflict"`
}
