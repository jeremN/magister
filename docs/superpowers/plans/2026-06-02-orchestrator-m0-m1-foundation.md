# Orchestrator M0 + M1 (Domain + In-Memory Engine) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the domain model (`flow` schema + validation), the core ports/types, the event bus, a mock executor, and the concurrency-capped DAG engine — so a mock flow runs fan-out/fan-in end-to-end under the race detector with state persisted to an in-memory store.

**Architecture:** Ports & adapters with an in-process event bus (design Approach A). `internal/core` holds pure domain types, state enums, and the interfaces (`Executor`, `Workspace`, `Store`, `Publisher`, `Clock`). `internal/engine` holds the goroutine-per-step DAG executor (a global `semaphore.Weighted` + a per-run cap bound concurrency). Adapters (`gate`, `join`, `executor`, `workspace`, `store`, `event`) implement or feed the core interfaces. **Note:** the engine lives in `internal/engine`, not `internal/core` as the spec sketched, to avoid an import cycle (`gate`/`join` depend on `core`'s domain types, so `core` cannot depend on them).

**Tech Stack:** Go 1.22, `github.com/goccy/go-yaml` (strict decode), `golang.org/x/sync/semaphore`. Stdlib for everything else. Tests use the standard `testing` package and run under `-race`.

**Spec:** `docs/superpowers/specs/2026-06-02-orchestrator-design.md`

---

## File structure (created by this plan)

```
concentus-magister/
├── go.mod                         # module concentus
├── internal/
│   ├── flow/
│   │   ├── duration.go            # Duration (YAML "5m" → time.Duration)
│   │   ├── flow.go                # Flow/Step/Gate/Join/RetryPolicy/Condition/Verifier + enums
│   │   ├── parse.go               # Parse / ParseBytes (strict YAML)
│   │   ├── validate.go            # Validate + cycle detection
│   │   ├── duration_test.go
│   │   ├── parse_test.go
│   │   └── validate_test.go
│   ├── core/
│   │   ├── state.go               # RunID, RunStatus, StepStatus enums
│   │   ├── ports.go               # Task/Result/Artifact, Executor, Workspace, Clock, Publisher
│   │   └── store.go               # RunState/StepState/RunSummary/Filter + Store interface
│   ├── event/
│   │   ├── event.go               # Event + Kind
│   │   ├── bus.go                 # in-process pub/sub Bus
│   │   └── bus_test.go
│   ├── executor/
│   │   ├── mock.go                # Mock executor (no real CLIs)
│   │   └── mock_test.go
│   ├── workspace/
│   │   ├── workspace.go           # dir-based Manager (worktrees deferred to M4)
│   │   └── workspace_test.go
│   ├── gate/
│   │   ├── approver.go            # Approver + AutoApprover
│   │   ├── verifier.go            # Verifier + CommandVerifier
│   │   ├── gate.go                # Evaluator
│   │   └── gate_test.go
│   ├── join/
│   │   ├── join.go                # Strategy + Merge + Registry
│   │   └── join_test.go
│   ├── store/
│   │   ├── mem.go                 # in-memory Store (SQLite arrives in M2)
│   │   └── mem_test.go
│   └── engine/
│       ├── engine.go              # the DAG executor
│       ├── prompt.go              # promptFor helper
│       └── engine_test.go         # fan-out/in, abort, cancel, deadlock-freedom, retry
└── flows/
    └── feature-flow.yaml          # example flow used by parse/e2e tests
```

---

## Task 1: Initialize the module

**Files:**
- Create: `go.mod`

- [ ] **Step 1: Initialize the module**

Run:
```bash
go mod init concentus
```
Expected: creates `go.mod` containing `module concentus` and a `go 1.x` line.

- [ ] **Step 2: Pin the Go version**

Edit `go.mod` so the version line reads exactly:
```
go 1.22
```

- [ ] **Step 3: Verify the toolchain builds an empty module**

Run: `go build ./...`
Expected: no output, exit 0 (no packages yet).

- [ ] **Step 4: Commit**

```bash
git add go.mod
git commit -m "chore: initialize go module"
```

---

## Task 2: `flow.Duration` (YAML duration type)

**Files:**
- Create: `internal/flow/duration.go`
- Test: `internal/flow/duration_test.go`

- [ ] **Step 1: Write the failing test**

`internal/flow/duration_test.go`:
```go
package flow

import (
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

func TestDurationUnmarshal(t *testing.T) {
	var h struct {
		T Duration `yaml:"t"`
	}
	if err := yaml.Unmarshal([]byte("t: 1m30s\n"), &h); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := h.T.Std(); got != 90*time.Second {
		t.Fatalf("got %v, want 90s", got)
	}
}

func TestDurationRejectsUnitless(t *testing.T) {
	var h struct {
		T Duration `yaml:"t"`
	}
	if err := yaml.Unmarshal([]byte("t: 5\n"), &h); err == nil {
		t.Fatal("expected error for unitless duration, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/flow/ -run TestDuration`
Expected: FAIL — `Duration` undefined / package has no Go files. (Also pulls the YAML dep on first build; if `go test` errors with "no required module provides package github.com/goccy/go-yaml", run `go get github.com/goccy/go-yaml@v1.19.2` then re-run.)

- [ ] **Step 3: Write minimal implementation**

`internal/flow/duration.go`:
```go
package flow

import (
	"time"

	"github.com/goccy/go-yaml"
)

// Duration is a time.Duration that unmarshals from a YAML string like "5m" or
// "2s". A bare number is rejected — durations must carry a unit, removing the
// "5 what?" ambiguity.
type Duration time.Duration

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalYAML implements goccy's BytesUnmarshaler.
func (d *Duration) UnmarshalYAML(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/flow/ -run TestDuration`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/flow/duration.go internal/flow/duration_test.go
git commit -m "feat(flow): add YAML Duration type"
```

---

## Task 3: `flow` schema types & enums

**Files:**
- Create: `internal/flow/flow.go`

- [ ] **Step 1: Write the implementation**

(These are pure type declarations; their behavior is exercised by the Parse and Validate tasks. No standalone test.)

`internal/flow/flow.go`:
```go
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
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./internal/flow/`
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
git add internal/flow/flow.go
git commit -m "feat(flow): add schema types and enums"
```

---

## Task 4: `flow.Parse` / `flow.ParseBytes` (strict YAML)

**Files:**
- Create: `internal/flow/parse.go`
- Create: `flows/feature-flow.yaml`
- Test: `internal/flow/parse_test.go`

- [ ] **Step 1: Create the example flow**

`flows/feature-flow.yaml`:
```yaml
name: feature-flow
concurrency: 4

steps:
  - id: plan
    agent: opus
    role: planner
    gate: { policy: manual }

  - id: impl-api
    needs: [plan]
    agent: sonnet
    role: implementer
    workspace: isolated
    gate: { policy: auto, verifier: { command: "true" } }

  - id: impl-ui
    needs: [plan]
    agent: gemini
    role: implementer
    workspace: isolated
    gate: { policy: auto, verifier: { command: "true" } }

  - id: integrate
    needs: [impl-api, impl-ui]
    join:
      strategy: merge
      on_conflict: manual
    gate: { policy: manual }
```

- [ ] **Step 2: Write the failing test**

`internal/flow/parse_test.go`:
```go
package flow

import "testing"

func TestParseExampleFlow(t *testing.T) {
	f, err := Parse("../../flows/feature-flow.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Name != "feature-flow" {
		t.Errorf("name = %q, want feature-flow", f.Name)
	}
	if f.Concurrency != 4 {
		t.Errorf("concurrency = %d, want 4", f.Concurrency)
	}
	if len(f.Steps) != 4 {
		t.Fatalf("steps = %d, want 4", len(f.Steps))
	}
	if f.Steps[3].Join == nil || f.Steps[3].Join.Strategy != JoinMerge {
		t.Errorf("step 3 should be a merge join")
	}
}

func TestParseBytesRejectsUnknownKey(t *testing.T) {
	_, err := ParseBytes([]byte("name: x\nbogus: 1\nsteps: [{id: a, agent: m}]\n"))
	if err == nil {
		t.Fatal("expected strict-decode error for unknown key, got nil")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/flow/ -run TestParse`
Expected: FAIL — `Parse`/`ParseBytes` undefined.

- [ ] **Step 4: Write minimal implementation**

`internal/flow/parse.go`:
```go
package flow

import (
	"fmt"
	"os"

	"github.com/goccy/go-yaml"
)

// Parse reads and unmarshals a flow definition from a file.
func Parse(path string) (*Flow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read flow: %w", err)
	}
	return ParseBytes(data)
}

// ParseBytes unmarshals a flow with strict decoding: unknown keys and duplicate
// keys are errors, so a typo'd field fails loudly instead of being ignored.
func ParseBytes(data []byte) (*Flow, error) {
	var f Flow
	if err := yaml.UnmarshalWithOptions(data, &f, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("parse flow: %w", err)
	}
	return &f, nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/flow/ -run TestParse`
Expected: PASS (2 tests).

- [ ] **Step 6: Commit**

```bash
git add internal/flow/parse.go flows/feature-flow.yaml internal/flow/parse_test.go
git commit -m "feat(flow): add strict YAML parsing"
```

---

## Task 5: `flow.Validate` + cycle detection

**Files:**
- Create: `internal/flow/validate.go`
- Test: `internal/flow/validate_test.go`

- [ ] **Step 1: Write the failing test**

`internal/flow/validate_test.go`:
```go
package flow

import "testing"

func valid() *Flow {
	return &Flow{
		Name: "f",
		Steps: []*Step{
			{ID: "a", Agent: "m", Gate: Gate{Policy: GateManual}},
			{ID: "b", Needs: []string{"a"}, Agent: "m",
				Gate: Gate{Policy: GateAuto, Verifier: &Verifier{Command: "true"}}},
		},
	}
}

func TestValidateAcceptsGoodFlow(t *testing.T) {
	if err := Validate(valid()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejections(t *testing.T) {
	cases := map[string]func(*Flow){
		"no name":             func(f *Flow) { f.Name = "" },
		"no steps":            func(f *Flow) { f.Steps = nil },
		"dup id":              func(f *Flow) { f.Steps[1].ID = "a" },
		"unknown dep":         func(f *Flow) { f.Steps[1].Needs = []string{"ghost"} },
		"self dep":            func(f *Flow) { f.Steps[0].Needs = []string{"a"} },
		"no agent or join":    func(f *Flow) { f.Steps[0].Agent = "" },
		"agent and join":      func(f *Flow) { f.Steps[0].Join = &Join{Strategy: JoinMerge} },
		"auto without verify": func(f *Flow) { f.Steps[1].Gate.Verifier = nil },
		"bad gate policy":     func(f *Flow) { f.Steps[0].Gate.Policy = "weird" },
		"cond without expr":   func(f *Flow) { f.Steps[0].Gate.Policy = GateConditional },
		"select without agent": func(f *Flow) {
			f.Steps[1].Agent = ""
			f.Steps[1].Join = &Join{Strategy: JoinSelect}
		},
		"retry max zero": func(f *Flow) { f.Steps[0].Retry = &RetryPolicy{Max: 0} },
		"onfail retry without policy": func(f *Flow) {
			f.Steps[0].Gate.OnFail = FailRetry
		},
		"negative concurrency": func(f *Flow) { f.Concurrency = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := valid()
			mutate(f)
			if err := Validate(f); err == nil {
				t.Fatalf("%s: expected error, got nil", name)
			}
		})
	}
}

func TestValidateDetectsCycle(t *testing.T) {
	f := &Flow{Name: "f", Steps: []*Step{
		{ID: "a", Needs: []string{"b"}, Agent: "m"},
		{ID: "b", Needs: []string{"a"}, Agent: "m"},
	}}
	if err := Validate(f); err == nil {
		t.Fatal("expected cycle error, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/flow/ -run TestValidate`
Expected: FAIL — `Validate` undefined.

- [ ] **Step 3: Write minimal implementation**

`internal/flow/validate.go`:
```go
package flow

import "fmt"

// Validate enforces the invariants the engine relies on. By the time a flow
// passes Validate, the engine can treat it as total.
func Validate(f *Flow) error {
	if f.Name == "" {
		return fmt.Errorf("flow has no name")
	}
	if len(f.Steps) == 0 {
		return fmt.Errorf("flow has no steps")
	}
	if f.Concurrency < 0 {
		return fmt.Errorf("flow concurrency must be >= 0, got %d", f.Concurrency)
	}

	byID := make(map[string]*Step, len(f.Steps))
	for _, s := range f.Steps {
		if s.ID == "" {
			return fmt.Errorf("a step has no id")
		}
		if _, dup := byID[s.ID]; dup {
			return fmt.Errorf("duplicate step id %q", s.ID)
		}
		byID[s.ID] = s
	}

	for _, s := range f.Steps {
		for _, dep := range s.Needs {
			if dep == s.ID {
				return fmt.Errorf("step %q needs itself", s.ID)
			}
			if _, ok := byID[dep]; !ok {
				return fmt.Errorf("step %q needs unknown step %q", s.ID, dep)
			}
		}
		if s.Join == nil && s.Agent == "" {
			return fmt.Errorf("step %q has neither an agent nor a join", s.ID)
		}
		if s.Join != nil && s.Agent != "" {
			return fmt.Errorf("step %q has both an agent and a join (pick one)", s.ID)
		}
		if err := validateGate(s); err != nil {
			return err
		}
		if err := validateJoin(s); err != nil {
			return err
		}
		if s.Retry != nil && s.Retry.Max < 1 {
			return fmt.Errorf("step %q: retry.max must be >= 1, got %d", s.ID, s.Retry.Max)
		}
		if s.Timeout < 0 {
			return fmt.Errorf("step %q: timeout must be >= 0", s.ID)
		}
	}

	if bad := findCycle(f, byID); bad != "" {
		return fmt.Errorf("flow has a cycle involving step %q", bad)
	}
	return nil
}

func validateGate(s *Step) error {
	switch s.Gate.Policy {
	case "", GateManual:
		// default is manual
	case GateAuto:
		if s.Gate.Verifier == nil || s.Gate.Verifier.Command == "" {
			return fmt.Errorf("step %q: auto gate requires a verifier command", s.ID)
		}
	case GateConditional:
		if s.Gate.Condition == nil || s.Gate.Condition.Expr == "" {
			return fmt.Errorf("step %q: conditional gate requires a condition expr", s.ID)
		}
	default:
		return fmt.Errorf("step %q: unknown gate policy %q", s.ID, s.Gate.Policy)
	}

	switch s.Gate.OnFail {
	case "", FailAbort, FailRetry, FailEscalate:
		// ok
	default:
		return fmt.Errorf("step %q: unknown on_fail %q", s.ID, s.Gate.OnFail)
	}
	if s.Gate.OnFail == FailRetry && s.Retry == nil {
		return fmt.Errorf("step %q: on_fail=retry requires a retry policy", s.ID)
	}
	return nil
}

func validateJoin(s *Step) error {
	if s.Join == nil {
		return nil
	}
	switch s.Join.Strategy {
	case JoinMerge:
		// no arbiter needed
	case JoinSelect, JoinSynthesize:
		if s.Join.Agent == "" {
			return fmt.Errorf("step %q: %q join requires an arbiter agent", s.ID, s.Join.Strategy)
		}
	default:
		return fmt.Errorf("step %q: unknown join strategy %q", s.ID, s.Join.Strategy)
	}
	if len(s.Needs) == 0 {
		return fmt.Errorf("step %q: join step must depend on at least one step", s.ID)
	}
	return nil
}

// findCycle runs a white/gray/black DFS over the needs graph and returns a step
// that participates in a cycle, or "" if the graph is acyclic.
func findCycle(f *Flow, byID map[string]*Step) string {
	const (
		white = iota
		gray
		black
	)
	color := make(map[string]int, len(f.Steps))
	var bad string

	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = gray
		for _, dep := range byID[id].Needs {
			switch color[dep] {
			case gray:
				bad = dep
				return true
			case white:
				if visit(dep) {
					return true
				}
			}
		}
		color[id] = black
		return false
	}

	for _, s := range f.Steps {
		if color[s.ID] == white && visit(s.ID) {
			return bad
		}
	}
	return ""
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/flow/`
Expected: PASS (all flow tests).

- [ ] **Step 5: Commit**

```bash
git add internal/flow/validate.go internal/flow/validate_test.go
git commit -m "feat(flow): add validation and cycle detection"
```

---

## Task 6: `core` state enums

**Files:**
- Create: `internal/core/state.go`

- [ ] **Step 1: Write the implementation**

(Pure enum declarations; exercised by later packages.)

`internal/core/state.go`:
```go
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
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./internal/core/`
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
git add internal/core/state.go
git commit -m "feat(core): add run/step state enums"
```

---

## Task 7: `event` package (Event + Bus)

**Files:**
- Create: `internal/event/event.go`
- Create: `internal/event/bus.go`
- Test: `internal/event/bus_test.go`

- [ ] **Step 1: Write the event type**

`internal/event/event.go`:
```go
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
```

- [ ] **Step 2: Write the failing test**

`internal/event/bus_test.go`:
```go
package event

import (
	"testing"
	"time"
)

func TestBusDeliversToSubscriber(t *testing.T) {
	b := NewBus()
	ch, unsub := b.Subscribe(4)
	defer unsub()

	b.Publish(Event{RunID: "r1", Kind: RunStarted})

	select {
	case got := <-ch:
		if got.RunID != "r1" || got.Kind != RunStarted {
			t.Fatalf("got %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestBusDropsWhenSubscriberFull(t *testing.T) {
	b := NewBus()
	_, unsub := b.Subscribe(1) // buffer of 1, never drained
	defer unsub()

	// Must not block even though the subscriber never reads.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish(Event{RunID: "r1", Kind: StepDone})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	b := NewBus()
	ch, unsub := b.Subscribe(1)
	unsub()
	if _, open := <-ch; open {
		t.Fatal("channel should be closed after unsubscribe")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/event/`
Expected: FAIL — `NewBus`/`Subscribe`/`Publish` undefined.

- [ ] **Step 4: Write the bus implementation**

`internal/event/bus.go`:
```go
package event

import "sync"

// Bus is an in-process publish/subscribe hub for live observers. Publish never
// blocks: each subscriber has a buffered channel, and an event is dropped for a
// subscriber that has fallen behind. The durable record lives in the store, so a
// dropped live frame is recoverable via replay.
type Bus struct {
	mu   sync.RWMutex
	subs map[int]chan Event
	next int
}

func NewBus() *Bus {
	return &Bus{subs: make(map[int]chan Event)}
}

// Subscribe returns a receive channel and an unsubscribe func. buffer sets the
// per-subscriber backlog before events start being dropped.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Event, buffer)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
	}
}

// Publish fans an event out to every current subscriber, dropping it for any
// whose buffer is full.
func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // subscriber behind; drop (store holds the durable copy)
		}
	}
}
```

- [ ] **Step 5: Run test to verify it passes (with race detector)**

Run: `go test -race ./internal/event/`
Expected: PASS (3 tests), no race warnings.

- [ ] **Step 6: Commit**

```bash
git add internal/event/event.go internal/event/bus.go internal/event/bus_test.go
git commit -m "feat(event): add event type and in-process bus"
```

---

## Task 8: `core` ports & store interface

**Files:**
- Create: `internal/core/ports.go`
- Create: `internal/core/store.go`

- [ ] **Step 1: Write the ports**

`internal/core/ports.go`:
```go
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
```

- [ ] **Step 2: Write the store interface**

`internal/core/store.go`:
```go
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
```

- [ ] **Step 3: Verify it compiles**

Run: `go build ./internal/core/`
Expected: no output, exit 0.

- [ ] **Step 4: Commit**

```bash
git add internal/core/ports.go internal/core/store.go
git commit -m "feat(core): add ports and store interface"
```

---

## Task 9: `executor.Mock`

**Files:**
- Create: `internal/executor/mock.go`
- Test: `internal/executor/mock_test.go`

- [ ] **Step 1: Write the failing test**

`internal/executor/mock_test.go`:
```go
package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"concentus/internal/core"
)

func TestMockWritesArtifact(t *testing.T) {
	dir := t.TempDir()
	m := Mock{Name: "sonnet"}
	res, err := m.Run(context.Background(), core.Task{StepID: "impl", Role: "impl", WorkDir: dir})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(res.Artifacts))
	}
	if _, err := os.Stat(res.Artifacts[0].Path); err != nil {
		t.Fatalf("artifact not written: %v", err)
	}
	if filepath.Dir(res.Artifacts[0].Path) != dir {
		t.Errorf("artifact not in workdir")
	}
	if res.CostUSD == 0 {
		t.Errorf("expected a nonzero mock cost")
	}
}

func TestMockHonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := Mock{Name: "x", Delay: 5 /* ns; tiny */}
	if _, err := m.Run(ctx, core.Task{StepID: "s", WorkDir: t.TempDir()}); err == nil {
		t.Fatal("expected context error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/executor/`
Expected: FAIL — `Mock` undefined.

- [ ] **Step 3: Write the implementation**

`internal/executor/mock.go`:
```go
// Package executor holds implementations of core.Executor.
package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"concentus/internal/core"
)

// Mock simulates an executor so flows run end-to-end with no real CLIs or API
// keys. It writes a small artifact and reports a fixed cost.
type Mock struct {
	Name  string
	Delay time.Duration
}

func (m Mock) Run(ctx context.Context, t core.Task) (core.Result, error) {
	if m.Delay > 0 {
		select {
		case <-time.After(m.Delay):
		case <-ctx.Done():
			return core.Result{}, ctx.Err()
		}
	} else if err := ctx.Err(); err != nil {
		return core.Result{}, err
	}

	body := fmt.Sprintf("# %s\nexecutor: %s\nrole: %s\ninputs: %d\n",
		t.StepID, m.Name, t.Role, len(t.Inputs))
	outPath := filepath.Join(t.WorkDir, t.StepID+".out.md")
	if err := os.WriteFile(outPath, []byte(body), 0o644); err != nil {
		return core.Result{}, err
	}
	return core.Result{
		StepID:    t.StepID,
		Summary:   fmt.Sprintf("%s done by %s", t.StepID, m.Name),
		Artifacts: []core.Artifact{{StepID: t.StepID, Path: outPath}},
		CostUSD:   0.01,
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/executor/`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/executor/mock.go internal/executor/mock_test.go
git commit -m "feat(executor): add mock executor"
```

---

## Task 10: `workspace.Manager` (dir-based)

**Files:**
- Create: `internal/workspace/workspace.go`
- Test: `internal/workspace/workspace_test.go`

- [ ] **Step 1: Write the failing test**

`internal/workspace/workspace_test.go`:
```go
package workspace

import (
	"os"
	"testing"

	"concentus/internal/flow"
)

func TestSharedReusesRunRoot(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	d1, _, err := m.For("run1", &flow.Step{ID: "a", Workspace: flow.WSShared})
	if err != nil {
		t.Fatal(err)
	}
	d2, _, err := m.For("run1", &flow.Step{ID: "b", Workspace: flow.WSShared})
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Errorf("shared steps should share a dir: %q vs %q", d1, d2)
	}
	if _, err := os.Stat(d1); err != nil {
		t.Errorf("dir not created: %v", err)
	}
}

func TestIsolatedGetsOwnDir(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	shared, _, _ := m.For("run1", &flow.Step{ID: "a", Workspace: flow.WSShared})
	iso, cleanup, err := m.For("run1", &flow.Step{ID: "b", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatal(err)
	}
	if iso == shared {
		t.Errorf("isolated step should get its own dir")
	}
	if err := cleanup(); err != nil {
		t.Errorf("cleanup: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/workspace/`
Expected: FAIL — `Manager` undefined.

- [ ] **Step 3: Write the implementation**

`internal/workspace/workspace.go`:
```go
// Package workspace hands each step a working directory. M1 is filesystem-only:
// shared steps reuse the run root, isolated steps get their own subdir. Git
// worktrees (and real teardown) arrive in M4 behind this same interface.
package workspace

import (
	"os"
	"path/filepath"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Manager allocates working directories under Root.
type Manager struct {
	Root string
}

func (m *Manager) For(runID core.RunID, s *flow.Step) (string, func() error, error) {
	dir := filepath.Join(m.Root, string(runID))
	if s.Workspace == flow.WSIsolated {
		dir = filepath.Join(dir, s.ID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	noop := func() error { return nil } // M4 replaces this with worktree teardown
	return dir, noop, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/workspace/`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/workspace/workspace.go internal/workspace/workspace_test.go
git commit -m "feat(workspace): add dir-based workspace manager"
```

---

## Task 11: `gate` package (Approver, Verifier, Evaluator)

**Files:**
- Create: `internal/gate/approver.go`
- Create: `internal/gate/verifier.go`
- Create: `internal/gate/gate.go`
- Test: `internal/gate/gate_test.go`

- [ ] **Step 1: Write the failing test**

`internal/gate/gate_test.go`:
```go
package gate

import (
	"context"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

func TestAutoGatePassesOnZeroExit(t *testing.T) {
	e := &Evaluator{Approver: AutoApprover{}, Verifier: CommandVerifier{}}
	s := &flow.Step{ID: "a", Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}}
	ok, err := e.Evaluate(context.Background(), s, core.Result{}, t.TempDir())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want true/nil", ok, err)
	}
}

func TestAutoGateFailsOnNonZeroExit(t *testing.T) {
	e := &Evaluator{Approver: AutoApprover{}, Verifier: CommandVerifier{}}
	s := &flow.Step{ID: "a", Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "false"}}}
	ok, err := e.Evaluate(context.Background(), s, core.Result{}, t.TempDir())
	if err != nil {
		t.Fatalf("non-zero exit should be a result, not an error: %v", err)
	}
	if ok {
		t.Fatal("gate should have failed")
	}
}

func TestManualGateUsesApprover(t *testing.T) {
	e := &Evaluator{Approver: fixedApprover(false), Verifier: CommandVerifier{}}
	s := &flow.Step{ID: "a", Gate: flow.Gate{Policy: flow.GateManual}}
	ok, _ := e.Evaluate(context.Background(), s, core.Result{}, t.TempDir())
	if ok {
		t.Fatal("approver returned false; gate should fail")
	}
}

type fixedApprover bool

func (f fixedApprover) Approve(context.Context, *flow.Step, core.Result) (bool, error) {
	return bool(f), nil
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gate/`
Expected: FAIL — `Evaluator`/`AutoApprover`/`CommandVerifier` undefined.

- [ ] **Step 3: Write the approver**

`internal/gate/approver.go`:
```go
// Package gate resolves the checkpoint after each step. The key design point:
// interactive vs autonomous mode is not a branch in the engine — it is which
// Approver implementation is injected here.
package gate

import (
	"context"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Approver resolves a manual gate. The service (M3) supplies an Approver backed
// by the API approval registry; AutoApprover backs the keyless demo and tests.
type Approver interface {
	Approve(ctx context.Context, step *flow.Step, res core.Result) (bool, error)
}

// AutoApprover passes every manual gate.
type AutoApprover struct{}

func (AutoApprover) Approve(context.Context, *flow.Step, core.Result) (bool, error) {
	return true, nil
}
```

- [ ] **Step 4: Write the verifier**

`internal/gate/verifier.go`:
```go
package gate

import (
	"context"
	"errors"
	"os/exec"
)

// Verifier resolves an auto gate by running a check.
type Verifier interface {
	Verify(ctx context.Context, command, workDir string) (bool, error)
}

// CommandVerifier runs a shell command in the step's workspace; exit 0 = pass.
// Any command ("go test ./...", "tsc --noEmit", a reviewer CLI) uses this one
// path, so no per-type verifier registry is needed.
type CommandVerifier struct{}

func (CommandVerifier) Verify(ctx context.Context, command, workDir string) (bool, error) {
	if command == "" {
		return true, nil
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = workDir
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return false, nil // non-zero exit = check failed, not an infra error
		}
		return false, err
	}
	return true, nil
}
```

- [ ] **Step 5: Write the evaluator**

`internal/gate/gate.go`:
```go
package gate

import (
	"context"
	"fmt"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Evaluator resolves the gate after a step and returns whether the flow may
// proceed. It does not itself apply retry/abort — the engine owns that policy.
type Evaluator struct {
	Approver Approver
	Verifier Verifier
}

func (e *Evaluator) Evaluate(ctx context.Context, s *flow.Step, res core.Result, workDir string) (bool, error) {
	switch s.Gate.Policy {
	case "", flow.GateManual, flow.GateConditional:
		// M1: conditional falls back to manual approval (parity with the phase-1
		// prototype). The expr-lang evaluator arrives in M5.
		return e.Approver.Approve(ctx, s, res)
	case flow.GateAuto:
		ok, err := e.Verifier.Verify(ctx, s.Gate.Verifier.Command, workDir)
		if err != nil {
			return false, fmt.Errorf("verifier error: %w", err)
		}
		return ok, nil
	default:
		return false, fmt.Errorf("step %q: unknown gate policy %q", s.ID, s.Gate.Policy)
	}
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/gate/`
Expected: PASS (3 tests).

- [ ] **Step 7: Commit**

```bash
git add internal/gate/
git commit -m "feat(gate): add approver, verifier, and evaluator"
```

---

## Task 12: `join` package (Strategy, Merge, Registry)

**Files:**
- Create: `internal/join/join.go`
- Test: `internal/join/join_test.go`

- [ ] **Step 1: Write the failing test**

`internal/join/join_test.go`:
```go
package join

import (
	"context"
	"os"
	"strings"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

func TestMergeWritesManifest(t *testing.T) {
	dir := t.TempDir()
	inputs := []core.Artifact{
		{StepID: "a", Path: "/tmp/a.md"},
		{StepID: "b", Path: "/tmp/b.md"},
	}
	res, err := Merge{}.Join(context.Background(), &flow.Step{ID: "integrate"}, inputs, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("want 1 manifest artifact, got %d", len(res.Artifacts))
	}
	data, err := os.ReadFile(res.Artifacts[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a -> /tmp/a.md", "b -> /tmp/b.md"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("manifest missing %q:\n%s", want, data)
		}
	}
}

func TestDefaultRegistryHasMergeOnly(t *testing.T) {
	r := Default()
	if _, ok := r[flow.JoinMerge]; !ok {
		t.Error("merge should be registered")
	}
	if _, ok := r[flow.JoinSelect]; ok {
		t.Error("select should NOT be registered in M1")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/join/`
Expected: FAIL — `Merge`/`Default` undefined.

- [ ] **Step 3: Write the implementation**

`internal/join/join.go`:
```go
// Package join combines a fan-in step's upstream artifacts into one result.
package join

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Strategy combines a fan-in step's inputs.
type Strategy interface {
	Join(ctx context.Context, s *flow.Step, inputs []core.Artifact, workDir string) (core.Result, error)
}

// Registry maps a strategy name to its implementation.
type Registry map[flow.JoinStrategy]Strategy

// Default registers only merge. select/synthesize (which need an arbiter agent)
// arrive in M5; until then an unregistered strategy fails at runtime with a
// clear "not implemented yet" message from the engine.
func Default() Registry {
	return Registry{flow.JoinMerge: Merge{}}
}

// Merge writes a manifest listing every upstream artifact. With real worktrees
// (M4) this becomes a git merge; the manifest keeps the pipeline observable now.
type Merge struct{}

func (Merge) Join(_ context.Context, s *flow.Step, inputs []core.Artifact, workDir string) (core.Result, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# merge: %s\n", s.ID)
	for _, in := range inputs {
		fmt.Fprintf(&b, "- %s -> %s\n", in.StepID, in.Path)
	}
	manifest := filepath.Join(workDir, s.ID+".merge.md")
	if err := os.WriteFile(manifest, []byte(b.String()), 0o644); err != nil {
		return core.Result{}, err
	}
	return core.Result{
		StepID:    s.ID,
		Summary:   fmt.Sprintf("merged %d branch(es)", len(inputs)),
		Artifacts: []core.Artifact{{StepID: s.ID, Path: manifest}},
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/join/`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/join/
git commit -m "feat(join): add merge strategy and registry"
```

---

## Task 13: `store.Mem` (in-memory Store)

**Files:**
- Create: `internal/store/mem.go`
- Test: `internal/store/mem_test.go`

- [ ] **Step 1: Write the failing test**

`internal/store/mem_test.go`:
```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/`
Expected: FAIL — `NewMem` undefined.

- [ ] **Step 3: Write the implementation**

`internal/store/mem.go`:
```go
// Package store holds implementations of core.Store. Mem is the in-memory one
// used by M1 tests and the keyless demo; SQLite replaces it in M2 behind the
// same interface.
package store

import (
	"context"
	"fmt"
	"sync"

	"concentus/internal/core"
	"concentus/internal/event"
)

type Mem struct {
	mu     sync.Mutex
	runs   map[core.RunID]*core.RunState
	events map[core.RunID][]event.Event
	seq    int64
}

func NewMem() *Mem {
	return &Mem{
		runs:   make(map[core.RunID]*core.RunState),
		events: make(map[core.RunID][]event.Event),
	}
}

func (m *Mem) CreateRun(_ context.Context, r core.RunState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[r.ID]; ok {
		return fmt.Errorf("run %q already exists", r.ID)
	}
	cp := r
	m.runs[r.ID] = &cp
	return nil
}

func (m *Mem) SaveStepTransition(_ context.Context, st core.StepState, evs []event.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[st.RunID]
	if !ok {
		return fmt.Errorf("unknown run %q", st.RunID)
	}
	found := false
	for i := range r.Steps {
		if r.Steps[i].StepID == st.StepID {
			r.Steps[i] = st
			found = true
			break
		}
	}
	if !found {
		r.Steps = append(r.Steps, st)
	}
	for _, e := range evs {
		m.seq++
		e.Seq = m.seq
		m.events[st.RunID] = append(m.events[st.RunID], e)
	}
	return nil
}

func (m *Mem) SetRunStatus(_ context.Context, id core.RunID, status core.RunStatus, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return fmt.Errorf("unknown run %q", id)
	}
	r.Status = status
	r.Err = errMsg
	return nil
}

func (m *Mem) LoadIncompleteRuns(context.Context) ([]core.RunState, error) {
	// M1 is single-process and non-resuming; nothing to load. Resume lands in M2.
	return nil, nil
}

func (m *Mem) GetRun(_ context.Context, id core.RunID) (core.RunState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return core.RunState{}, fmt.Errorf("unknown run %q", id)
	}
	return *r, nil
}

func (m *Mem) ListRuns(_ context.Context, f core.Filter) ([]core.RunSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.RunSummary
	for _, r := range m.runs {
		if f.Status != "" && r.Status != f.Status {
			continue
		}
		out = append(out, core.RunSummary{ID: r.ID, Name: r.Name, Status: r.Status})
	}
	return out, nil
}

func (m *Mem) EventsSince(_ context.Context, id core.RunID, seq int64) ([]event.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []event.Event
	for _, e := range m.events[id] {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes (with race detector)**

Run: `go test -race ./internal/store/`
Expected: PASS (2 tests), no race warnings.

- [ ] **Step 5: Verify Mem satisfies core.Store (compile-time assertion)**

Append to `internal/store/mem.go` (after the imports, before `type Mem`):
```go
var _ core.Store = (*Mem)(nil)
```

Run: `go build ./internal/store/`
Expected: no output, exit 0. (If `Mem` does not satisfy `core.Store`, this line fails the build with the missing method.)

- [ ] **Step 6: Commit**

```bash
git add internal/store/
git commit -m "feat(store): add in-memory store"
```

---

## Task 14: `engine` — the DAG executor

**Files:**
- Create: `internal/engine/prompt.go`
- Create: `internal/engine/engine.go`

This task has no standalone unit test — it is exercised by the end-to-end tests in Task 15. Build verification at the end confirms it compiles and satisfies the interfaces it uses.

- [ ] **Step 1: Add the `semaphore` dependency**

Run:
```bash
go get golang.org/x/sync/semaphore@latest
```
Expected: adds `golang.org/x/sync` to `go.mod`.

- [ ] **Step 2: Write the prompt helper**

`internal/engine/prompt.go`:
```go
package engine

import (
	"fmt"
	"strings"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// promptFor builds the prompt handed to an executor. An explicit step.Prompt
// wins; otherwise a default is assembled from the role and upstream artifacts.
func promptFor(s *flow.Step, inputs []core.Artifact) string {
	if s.Prompt != "" {
		return s.Prompt
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Role: %s\nStep: %s\n", s.Role, s.ID)
	if len(inputs) > 0 {
		b.WriteString("Upstream artifacts:\n")
		for _, in := range inputs {
			fmt.Fprintf(&b, "- %s: %s\n", in.StepID, in.Path)
		}
	}
	return b.String()
}
```

- [ ] **Step 3: Write the engine**

`internal/engine/engine.go`:
```go
// Package engine executes a flow as a DAG: one goroutine per step, each blocking
// on its dependencies' channels and closing its own when it finishes. Fan-out
// and fan-in emerge from the graph — there is no explicit scheduler. Concurrency
// is bounded by a global weighted semaphore plus an optional per-run cap.
package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/flow"
	"concentus/internal/gate"
	"concentus/internal/join"
)

type Engine struct {
	Execs map[string]core.Executor // registry: "opus"→CLIAgent, …, "mock"→Mock
	WS    core.Workspace
	Gate  *gate.Evaluator
	Joins join.Registry
	Store core.Store
	Bus   core.Publisher
	Sem   *semaphore.Weighted // global concurrency cap; nil = unbounded
	Clock core.Clock
}

// Run executes one flow to completion. The first failing step cancels the run's
// context and the rest tear down. The run row must already exist in the store
// (the caller creates it); Run drives its status and all step transitions.
func (e *Engine) Run(parent context.Context, runID core.RunID, f *flow.Flow) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	_ = e.Store.SetRunStatus(ctx, runID, core.RunRunning, "")
	e.Bus.Publish(event.Event{RunID: string(runID), Kind: event.RunStarted, At: e.Clock.Now()})

	// per-run cap (0 = unlimited within the global semaphore)
	var perRun chan struct{}
	if f.Concurrency > 0 {
		perRun = make(chan struct{}, f.Concurrency)
	}

	done := make(map[string]chan struct{}, len(f.Steps))
	for _, s := range f.Steps {
		done[s.ID] = make(chan struct{})
	}

	var (
		mu       sync.Mutex
		results  = make(map[string]core.Result, len(f.Steps))
		firstErr error
		errOnce  sync.Once
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	for _, s := range f.Steps {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done[s.ID]) // always unblock dependents, even on bail-out

			// 1. wait for dependencies (holding NO concurrency token).
			for _, dep := range s.Needs {
				select {
				case <-done[dep]:
				case <-ctx.Done():
					return
				}
			}
			if ctx.Err() != nil {
				return
			}

			// 2. gather upstream artifacts.
			mu.Lock()
			var inputs []core.Artifact
			for _, dep := range s.Needs {
				inputs = append(inputs, results[dep].Artifacts...)
			}
			mu.Unlock()

			// 3. acquire concurrency tokens (per-run, then global), held only
			//    around the work — never while waiting on deps (no hold-and-wait).
			if perRun != nil {
				select {
				case perRun <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-perRun }()
			}
			if e.Sem != nil {
				if err := e.Sem.Acquire(ctx, 1); err != nil {
					return // context canceled while queued
				}
				defer e.Sem.Release(1)
			}
			if ctx.Err() != nil {
				return
			}

			// 4. run the step (execute + gate, with retries).
			res, err := e.runStep(ctx, runID, s, inputs)
			if err != nil {
				fail(fmt.Errorf("step %q: %w", s.ID, err))
				return
			}
			mu.Lock()
			results[s.ID] = res
			mu.Unlock()
		}()
	}

	wg.Wait()

	final := context.WithoutCancel(ctx)
	switch {
	case parent.Err() != nil: // external cancellation wins over any step error it caused
		_ = e.Store.SetRunStatus(final, runID, core.RunCanceled, "canceled")
		e.Bus.Publish(event.Event{RunID: string(runID), Kind: event.RunDone, Err: "canceled", At: e.Clock.Now()})
		return parent.Err()
	case firstErr != nil:
		_ = e.Store.SetRunStatus(final, runID, core.RunFailed, firstErr.Error())
		e.Bus.Publish(event.Event{RunID: string(runID), Kind: event.RunDone, Err: firstErr.Error(), At: e.Clock.Now()})
		return firstErr
	default:
		_ = e.Store.SetRunStatus(final, runID, core.RunSucceeded, "")
		e.Bus.Publish(event.Event{RunID: string(runID), Kind: event.RunDone, At: e.Clock.Now()})
		return nil
	}
}

// runStep runs one step: execute + gate, looping on the unified attempt budget.
func (e *Engine) runStep(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact) (core.Result, error) {
	workDir, cleanup, err := e.WS.For(runID, s)
	if err != nil {
		return core.Result{}, err
	}
	defer func() { _ = cleanup() }()

	maxAttempts := 1
	if s.Retry != nil {
		maxAttempts = s.Retry.Max
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			e.transition(ctx, runID, stepState(runID, s.ID, core.StepRetrying, attempt, core.Result{}, lastErr),
				event.Event{StepID: s.ID, Kind: event.StepRetrying, Attempt: attempt})
			if !e.backoff(ctx, s, attempt) {
				return core.Result{}, ctx.Err()
			}
		}

		e.transition(ctx, runID, stepState(runID, s.ID, core.StepRunning, attempt, core.Result{}, nil),
			event.Event{StepID: s.ID, Kind: event.StepStarted, Attempt: attempt})

		res, execErr := e.execute(ctx, runID, s, inputs, workDir)
		if execErr == nil {
			res.StepID = s.ID
			ok, gerr := e.Gate.Evaluate(ctx, s, res, workDir)
			switch {
			case gerr != nil:
				execErr = gerr
			case !ok:
				execErr = fmt.Errorf("gate failed (policy=%q)", gatePolicyOf(s))
			default:
				e.transition(ctx, runID, stepState(runID, s.ID, core.StepSucceeded, attempt, res, nil),
					event.Event{StepID: s.ID, Kind: event.StepDone, Summary: res.Summary, CostUSD: res.CostUSD, Attempt: attempt})
				return res, nil
			}
		}

		lastErr = execErr
		if attempt < maxAttempts && s.Retry != nil {
			continue // retry (covers both execution and gate failures)
		}
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attempt, core.Result{}, lastErr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attempt, Err: lastErr.Error()})
		return core.Result{}, lastErr
	}
	return core.Result{}, lastErr
}

// execute runs the step's work: a join strategy for fan-in steps, otherwise the
// named executor. A per-step timeout wraps the call when set.
func (e *Engine) execute(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact, workDir string) (core.Result, error) {
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(s.Timeout))
		defer cancel()
	}
	if s.Join != nil {
		strat, ok := e.Joins[s.Join.Strategy]
		if !ok {
			return core.Result{}, fmt.Errorf("join strategy %q not implemented yet", s.Join.Strategy)
		}
		return strat.Join(ctx, s, inputs, workDir)
	}
	ag, ok := e.Execs[s.Agent]
	if !ok {
		return core.Result{}, fmt.Errorf("unknown agent %q", s.Agent)
	}
	return ag.Run(ctx, core.Task{
		RunID:   runID,
		StepID:  s.ID,
		Role:    s.Role,
		Prompt:  promptFor(s, inputs),
		Inputs:  inputs,
		WorkDir: workDir,
	})
}

// backoff sleeps before a retry using the injected clock. Returns false if the
// context was canceled while waiting. Exponential; jitter arrives in M4.
func (e *Engine) backoff(ctx context.Context, s *flow.Step, attempt int) bool {
	if s.Retry == nil || s.Retry.Backoff <= 0 {
		return true
	}
	base := time.Duration(s.Retry.Backoff)
	d := base * (1 << (attempt - 2)) // attempt 2 → base, 3 → 2×base, …
	select {
	case <-e.Clock.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// transition persists a step state + its event in one store call (durable),
// then publishes the event to the live bus (persist-then-publish).
func (e *Engine) transition(ctx context.Context, runID core.RunID, st core.StepState, ev event.Event) {
	ev.RunID = string(runID)
	ev.At = e.Clock.Now()
	if err := e.Store.SaveStepTransition(context.WithoutCancel(ctx), st, []event.Event{ev}); err != nil {
		// No silent failure: surface the store error on the live stream.
		e.Bus.Publish(event.Event{RunID: string(runID), StepID: st.StepID, Kind: event.StepFailed, Err: "store: " + err.Error(), At: e.Clock.Now()})
	}
	e.Bus.Publish(ev)
}

func stepState(runID core.RunID, stepID string, status core.StepStatus, attempt int, res core.Result, err error) core.StepState {
	st := core.StepState{
		RunID:     runID,
		StepID:    stepID,
		Status:    status,
		Attempt:   attempt,
		Summary:   res.Summary,
		CostUSD:   res.CostUSD,
		Artifacts: res.Artifacts,
	}
	if err != nil {
		st.Err = err.Error()
	}
	return st
}

func gatePolicyOf(s *flow.Step) flow.GatePolicy {
	if s.Gate.Policy == "" {
		return flow.GateManual
	}
	return s.Gate.Policy
}
```

- [ ] **Step 4: Verify it compiles**

Run: `go build ./...`
Expected: no output, exit 0.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/engine/
git commit -m "feat(engine): add concurrency-capped DAG executor"
```

---

## Task 15: End-to-end engine tests

**Files:**
- Create: `internal/engine/engine_test.go`

- [ ] **Step 1: Write the tests**

`internal/engine/engine_test.go`:
```go
package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/executor"
	"concentus/internal/flow"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

// newEngine wires a fully-mock engine for tests.
func newEngine(t *testing.T, exec map[string]core.Executor, sem *semaphore.Weighted) (*Engine, *store.Mem, *event.Bus) {
	t.Helper()
	st := store.NewMem()
	bus := event.NewBus()
	eng := &Engine{
		Execs: exec,
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st,
		Bus:   bus,
		Sem:   sem,
		Clock: core.SystemClock{},
	}
	return eng, st, bus
}

func mocks() map[string]core.Executor {
	return map[string]core.Executor{
		"opus":   executor.Mock{Name: "opus"},
		"sonnet": executor.Mock{Name: "sonnet"},
		"gemini": executor.Mock{Name: "gemini"},
	}
}

func mustCreate(t *testing.T, st *store.Mem, id core.RunID, f *flow.Flow) {
	t.Helper()
	if err := st.CreateRun(context.Background(), core.RunState{ID: id, Name: f.Name, Status: core.RunPending, Concurrency: f.Concurrency}); err != nil {
		t.Fatal(err)
	}
}

func TestEngineFanOutFanIn(t *testing.T) {
	f := &flow.Flow{Name: "feat", Concurrency: 2, Steps: []*flow.Step{
		{ID: "plan", Agent: "opus", Gate: flow.Gate{Policy: flow.GateManual}},
		{ID: "api", Needs: []string{"plan"}, Agent: "sonnet", Workspace: flow.WSIsolated,
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
		{ID: "ui", Needs: []string{"plan"}, Agent: "gemini", Workspace: flow.WSIsolated,
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
		{ID: "integrate", Needs: []string{"api", "ui"},
			Join: &flow.Join{Strategy: flow.JoinMerge}, Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	if err := flow.Validate(f); err != nil {
		t.Fatalf("flow invalid: %v", err)
	}

	eng, st, bus := newEngine(t, mocks(), semaphore.NewWeighted(4))
	mustCreate(t, st, "r1", f)
	ch, unsub := bus.Subscribe(64)

	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, _ := st.GetRun(context.Background(), "r1")
	if got.Status != core.RunSucceeded {
		t.Fatalf("run status = %q, want succeeded", got.Status)
	}
	if len(got.Steps) != 4 {
		t.Fatalf("want 4 steps recorded, got %d", len(got.Steps))
	}
	for _, s := range got.Steps {
		if s.Status != core.StepSucceeded {
			t.Errorf("step %q status = %q", s.StepID, s.Status)
		}
	}

	// All events were published synchronously during Run; closing the
	// subscription lets us drain the buffer and confirm the run bookends.
	unsub()
	var sawStart, sawDone bool
	for ev := range ch {
		switch ev.Kind {
		case event.RunStarted:
			sawStart = true
		case event.RunDone:
			sawDone = true
		}
	}
	if !sawStart || !sawDone {
		t.Errorf("missing run bookends: start=%v done=%v", sawStart, sawDone)
	}
}

func TestEngineAbortsOnStepError(t *testing.T) {
	// "ghost" agent is not registered → that step errors → run fails.
	f := &flow.Flow{Name: "boom", Steps: []*flow.Step{
		{ID: "plan", Agent: "opus", Gate: flow.Gate{Policy: flow.GateManual}},
		{ID: "bad", Needs: []string{"plan"}, Agent: "ghost", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	eng, st, _ := newEngine(t, mocks(), nil)
	mustCreate(t, st, "r1", f)
	if err := eng.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected run error")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Status != core.RunFailed {
		t.Fatalf("run status = %q, want failed", got.Status)
	}
}

func TestEngineCancellation(t *testing.T) {
	// A slow plan step; cancel the context almost immediately.
	f := &flow.Flow{Name: "slow", Steps: []*flow.Step{
		{ID: "plan", Agent: "slow", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	exec := map[string]core.Executor{"slow": executor.Mock{Name: "slow", Delay: 2 * time.Second}}
	eng, st, _ := newEngine(t, exec, nil)
	mustCreate(t, st, "r1", f)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := eng.Run(ctx, "r1", f); err == nil {
		t.Fatal("expected cancellation error")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Status != core.RunCanceled {
		t.Fatalf("run status = %q, want canceled", got.Status)
	}
}

func TestEngineWideFanInNoDeadlock(t *testing.T) {
	// 20 parallel steps feeding one merge, under a global semaphore of 2 and a
	// per-run cap of 2. If tokens were held while waiting on deps, the join would
	// deadlock. It must finish well within the timeout.
	steps := []*flow.Step{{ID: "root", Agent: "opus", Gate: flow.Gate{Policy: flow.GateManual}}}
	var needs []string
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("w%d", i)
		needs = append(needs, id)
		steps = append(steps, &flow.Step{ID: id, Needs: []string{"root"}, Agent: "sonnet",
			Gate: flow.Gate{Policy: flow.GateManual}})
	}
	steps = append(steps, &flow.Step{ID: "join", Needs: needs,
		Join: &flow.Join{Strategy: flow.JoinMerge}, Gate: flow.Gate{Policy: flow.GateManual}})
	f := &flow.Flow{Name: "wide", Concurrency: 2, Steps: steps}
	if err := flow.Validate(f); err != nil {
		t.Fatalf("invalid: %v", err)
	}

	eng, st, _ := newEngine(t, mocks(), semaphore.NewWeighted(2))
	mustCreate(t, st, "r1", f)

	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background(), "r1", f) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DEADLOCK: wide fan-in did not complete")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Status != core.RunSucceeded {
		t.Fatalf("status = %q", got.Status)
	}
}

func TestEngineRetryThenSucceed(t *testing.T) {
	// Fails twice, succeeds on the third attempt. A fake clock makes backoff
	// instant and deterministic.
	flaky := &flakyExecutor{failUntil: 3}
	exec := map[string]core.Executor{"flaky": flaky}
	st := store.NewMem()
	eng := &Engine{
		Execs: exec,
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st,
		Bus:   event.NewBus(),
		Sem:   nil,
		Clock: fakeClock{},
	}
	f := &flow.Flow{Name: "retry", Steps: []*flow.Step{
		{ID: "a", Agent: "flaky", Retry: &flow.RetryPolicy{Max: 3, Backoff: flow.Duration(time.Second)},
			Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	mustCreate(t, st, "r1", f)
	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	if flaky.calls != 3 {
		t.Fatalf("executor called %d times, want 3", flaky.calls)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepSucceeded {
		t.Fatalf("step status = %q", got.Steps[0].Status)
	}
}

type flakyExecutor struct {
	mu        sync.Mutex
	calls     int
	failUntil int
}

func (f *flakyExecutor) Run(_ context.Context, t core.Task) (core.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls < f.failUntil {
		return core.Result{}, fmt.Errorf("transient failure %d", f.calls)
	}
	return core.Result{StepID: t.StepID, Summary: "ok after " + strings.Repeat("x", f.calls)}, nil
}

// fakeClock makes After fire immediately, so retry/backoff tests don't sleep.
type fakeClock struct{}

func (fakeClock) Now() time.Time { return time.Unix(0, 0) }
func (fakeClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Unix(0, 0)
	return ch
}
```

- [ ] **Step 2: Run the tests with the race detector**

Run: `go test -race ./internal/engine/`
Expected: PASS (5 tests), no race warnings. The wide-fan-in test must finish in well under its 5s guard.

- [ ] **Step 3: Run the full suite**

Run: `go test -race ./...`
Expected: PASS across all packages.

- [ ] **Step 4: Commit**

```bash
git add internal/engine/engine_test.go
git commit -m "test(engine): add end-to-end fan-out/in, abort, cancel, deadlock, retry tests"
```

---

## Task 16: Tidy & milestone gate

**Files:**
- Modify: `go.mod`, `go.sum` (via `go mod tidy`)

- [ ] **Step 1: Tidy modules**

Run: `go mod tidy`
Expected: `go.mod`/`go.sum` pruned to exactly the used deps (`goccy/go-yaml`, `golang.org/x/sync`).

- [ ] **Step 2: Vet**

Run: `go vet ./...`
Expected: no output, exit 0.

- [ ] **Step 3: Full race suite (milestone proof)**

Run: `go test -race ./...`
Expected: PASS across `flow`, `core` (build-only), `event`, `executor`, `workspace`, `gate`, `join`, `store`, `engine`.

This is the M1 proof: a mock flow runs fan-out/fan-in end-to-end under the race detector, with state and events persisted to the store — "phase-1 done right," plus a concurrency cap, an event bus, and a persistence seam.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: go mod tidy after M0+M1"
```

---

## Spec coverage check (self-review)

| Spec section | Covered by |
|---|---|
| §3 package layout & dependency rule | Tasks 2–14 (note: engine in `internal/engine`, not `core`, to avoid a cycle) |
| §3 ports (Executor/Workspace/Publisher/Store) | Task 8 |
| §4 flow schema, types, enums | Tasks 2–4 |
| §4 state machines (Run/Step enums) | Task 6 |
| §4 validation (incl. cycle detection, strict decode) | Tasks 4, 5 |
| §5 goroutine-per-step DAG, semaphore + per-run cap | Task 14 |
| §5 deadlock avoidance (token after deps) | Task 14 + Task 15 wide-fan-in test |
| §5 failure model (retry/abort; escalate deferred to M4) | Task 14 + Task 15 retry test |
| §5 cancellation | Task 14 + Task 15 cancel test |
| §6 persist-then-publish | Task 14 `transition()` |
| §11 no silent failures | Task 14 `transition()` store-error event |
| §12 testing strategy (fakes, fake clock, `-race`) | Tasks 9, 13, 15 |
| §13 M0 + M1 milestones | this whole plan |

**Deferred to later plans (by design):** SQLite store + resume (M2), HTTP/SSE API + `cm` CLI (M3), retry jitter + on_fail=escalate + real CLIAgent + git worktrees (M4), conditional `expr` gates + select/synthesize joins (M5). Each is called out inline in the code comments where its seam lives.
