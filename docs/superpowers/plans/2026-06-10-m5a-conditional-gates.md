# Conditional Gates (M5 Slice A) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `gate.policy: conditional` — compile a `condition.expr` at submit time and evaluate it synchronously against the gated step's result (replacing the M1 fallback to manual approval).

**Architecture:** `internal/flow` gains a compiled `Condition` (via `expr-lang/expr`) + a typed `GateEnv`, compiled in `Validate` so a bad expr fails submission. `internal/gate` flips the `GateConditional` branch from "ask the Approver" to "evaluate the expr" (synchronous, like an auto gate). Two `internal/engine` predicates (`gateBlocks`, `gateEscalates`) are updated so conditional behaves like auto (non-blocking, escalate-capable). No store/SSE/schema/YAML-surface change.

**Tech Stack:** Go 1.22; new dep `github.com/expr-lang/expr v1.17.8` (go-1.18, zero transitive deps, sandboxed compile-ahead). TDD throughout; all automated tests run without keys/network.

**Spec:** `docs/superpowers/specs/2026-06-10-m5a-conditional-gates-design.md`

---

## File Structure

- **Modify** `internal/flow/flow.go` — add the `expr` import, `GateEnv`/`GateResult` env types, a `prog *vm.Program` field on `Condition`, and `Condition.Compile()` / `Condition.Eval()`.
- **Create** `internal/flow/condition_test.go` — unit tests for `Compile`/`Eval`.
- **Modify** `internal/flow/validate.go:78-81` — compile the expr in the `GateConditional` case (fail submission on a bad expr).
- **Modify** `internal/flow/validate_test.go` — a bad-expr rejection case + a positive compile/eval test.
- **Modify** `internal/gate/gate.go:25-40` — split the manual/conditional branch; add `artifactPaths`.
- **Modify** `internal/gate/gate_test.go` — conditional true/false unit tests with an Approver that fails if called.
- **Modify** `internal/engine/engine.go:424-438` — `gateBlocks` drops conditional; `gateEscalates` adds it.
- **Modify** `internal/engine/engine_test.go` — three conditional integration tests (true→proceed/no-await, false→abort, false+escalate→approve).
- **Modify** `go.mod` / `go.sum` — add `expr-lang/expr v1.17.8`.

**Reused unchanged:** the engine's gate call site (`engine.go:298-310`) and `Escalate` path, `core.Result`, the SSE/store layers. Shared test helpers already exist — `baseFlow()` (`validate_test.go:6`), `fixedApprover` (`gate_test.go:41`), `mustCreate`/`fakeClock`/`rejectApprover` (`engine_test.go`). Reference them; do not redefine.

---

## Task 0: Branch + worktree + dependency + baseline

**Files:** none committed (setup only). **This task is run by the controller** (it needs one network call to fetch the dependency; the module is already in the local cache, but the controller fetches with the sandbox disabled to be safe).

- [ ] **Step 1: Create the worktree off `main`**

```bash
git worktree add .worktrees/m5a-conditional-gates -b m5a-conditional-gates
cd .worktrees/m5a-conditional-gates
```

- [ ] **Step 2: Add the dependency to go.mod (controller, network)**

```bash
go get github.com/expr-lang/expr@v1.17.8
```
Expected: `go.mod` gains `require github.com/expr-lang/expr v1.17.8` (it will show no `// indirect` once `flow.go` imports it in Task 1). `go.sum` gains its checksums. Leave these changes uncommitted — Task 1 commits them alongside the first import.

- [ ] **Step 3: Confirm a green baseline**

Run: `go test -race ./... && go vet ./...`
Expected: all packages `ok` / no vet output. `go 1.22` unchanged (`grep '^go ' go.mod`). An as-yet-unused `require` is fine for build/test/vet (do NOT run `go mod tidy` here — it would drop the unused require; Task 1 tidies once `flow.go` imports it).

---

## Task 1: `internal/flow` — compiled Condition + typed env

**Files:**
- Modify: `internal/flow/flow.go`
- Create: `internal/flow/condition_test.go`
- Modify: `go.mod` / `go.sum` (finalize the dependency)

- [ ] **Step 1: Write the failing tests**

Create `internal/flow/condition_test.go`:

```go
package flow

import "testing"

func TestConditionCompileRejectsBadExpr(t *testing.T) {
	c := &Condition{Expr: "this is not valid +++"}
	if err := c.Compile(); err == nil {
		t.Fatal("expected a compile error for a malformed expr")
	}
}

func TestConditionCompileRejectsNonBool(t *testing.T) {
	c := &Condition{Expr: "result.summary"} // a string, not a bool — AsBool must reject
	if err := c.Compile(); err == nil {
		t.Fatal("expected a compile error for a non-bool expr")
	}
}

func TestConditionCompileAcceptsGoodExpr(t *testing.T) {
	c := &Condition{Expr: `result.cost_usd < 1.0 && result.summary contains "OK"`}
	if err := c.Compile(); err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
}

func TestConditionEvalTrueFalse(t *testing.T) {
	c := &Condition{Expr: "result.cost_usd < 1.0"}
	if err := c.Compile(); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.Eval(GateEnv{Result: GateResult{CostUSD: 0.5}}); err != nil || !ok {
		t.Errorf("eval(0.5) = %v,%v want true,nil", ok, err)
	}
	if ok, err := c.Eval(GateEnv{Result: GateResult{CostUSD: 2.0}}); err != nil || ok {
		t.Errorf("eval(2.0) = %v,%v want false,nil", ok, err)
	}
}

func TestConditionEvalUncompiledErrors(t *testing.T) {
	c := &Condition{Expr: "true"}
	if _, err := c.Eval(GateEnv{}); err == nil {
		t.Fatal("expected an error evaluating an uncompiled condition")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/flow/ -run TestCondition -v`
Expected: FAIL — `undefined: GateEnv` / `c.Compile undefined` / `c.Eval undefined`.

- [ ] **Step 3: Implement in `flow.go`**

Add the import block at the top of `internal/flow/flow.go` (the file currently has none — insert directly after the `package flow` line and its doc comment block, before `type WSMode`):

```go
import (
	"fmt"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)
```

Replace the existing `Condition` type (currently `type Condition struct { Expr string \`yaml:"expr"\` }` with its M0/M1 doc comment) with:

```go
// GateEnv is the environment a conditional gate's expression evaluates against:
// the gated step's result, exposed under `result`.
type GateEnv struct {
	Result GateResult `expr:"result"`
}

// GateResult mirrors the salient fields of core.Result for expressions, e.g.
// `result.cost_usd < 1.0 && result.summary contains "OK"`.
type GateResult struct {
	Summary   string   `expr:"summary"`
	CostUSD   float64  `expr:"cost_usd"`
	Artifacts []string `expr:"artifacts"` // artifact paths
	StepID    string   `expr:"step_id"`
}

// Condition configures a conditional gate. Expr is compiled at Validate time so a
// malformed or non-bool expression fails submission, not a running step; prog holds
// the compiled program and is nil until Compile runs.
type Condition struct {
	Expr string `yaml:"expr"`
	prog *vm.Program
}

// Compile compiles Expr against GateEnv, requiring a boolean result. Called from
// Validate (submit time). A syntax error, an unknown identifier, or a non-bool
// result is returned as an error.
func (c *Condition) Compile() error {
	p, err := expr.Compile(c.Expr, expr.Env(GateEnv{}), expr.AsBool())
	if err != nil {
		return err
	}
	c.prog = p
	return nil
}

// Eval runs the compiled program against env and returns the gate decision.
// A nil prog (Compile not run) or a runtime error is returned as an error.
func (c *Condition) Eval(env GateEnv) (bool, error) {
	if c.prog == nil {
		return false, fmt.Errorf("condition not compiled")
	}
	out, err := expr.Run(c.prog, env)
	if err != nil {
		return false, err
	}
	return out.(bool), nil // AsBool guarantees a bool result
}
```

- [ ] **Step 4: Finalize the dependency and run the tests**

Run: `go mod tidy` (now that `flow.go` imports `expr`, this keeps it as a direct dependency and writes `go.sum`).
Run: `go test ./internal/flow/ -run TestCondition -v`
Expected: PASS (all five). Confirm `grep expr go.mod` shows `github.com/expr-lang/expr v1.17.8` with NO `// indirect`, and `grep '^go ' go.mod` is still `go 1.22`.

- [ ] **Step 5: Commit**

```bash
git add internal/flow/flow.go internal/flow/condition_test.go go.mod go.sum
git commit -m "feat(flow): compile+eval conditional gate expressions (expr-lang)"
```

---

## Task 2: `internal/flow/Validate` — compile at submit

**Files:**
- Modify: `internal/flow/validate.go`
- Modify: `internal/flow/validate_test.go`

- [ ] **Step 1: Write the failing tests**

In `internal/flow/validate_test.go`, add a case to the `cases` map inside `TestValidateRejections` (alongside the existing `"cond without expr"` entry):

```go
		"cond bad expr": func(f *Flow) {
			f.Steps[0].Gate = Gate{Policy: GateConditional, Condition: &Condition{Expr: "not valid +++"}}
		},
```

And append a positive test that proves Validate compiles the program (so a later Eval works without a separate Compile):

```go
func TestValidateCompilesGoodCondition(t *testing.T) {
	f := baseFlow()
	f.Steps[0].Gate = Gate{Policy: GateConditional, Condition: &Condition{Expr: "result.cost_usd < 1.0"}}
	if err := Validate(f); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ok, err := f.Steps[0].Gate.Condition.Eval(GateEnv{Result: GateResult{CostUSD: 0.1}})
	if err != nil || !ok {
		t.Errorf("post-validate eval = %v,%v want true,nil (Validate should have compiled the expr)", ok, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/flow/ -run 'TestValidateRejections/cond_bad_expr|TestValidateCompilesGoodCondition' -v`
Expected: FAIL — the bad expr is currently accepted (no compile step), and the positive test's `Eval` errors with "condition not compiled".

- [ ] **Step 3: Implement in `validate.go`**

In `validateGate`, replace the `GateConditional` case (currently lines 78-81):

```go
	case GateConditional:
		if s.Gate.Condition == nil || s.Gate.Condition.Expr == "" {
			return fmt.Errorf("step %q: conditional gate requires a condition expr", s.ID)
		}
```

with:

```go
	case GateConditional:
		if s.Gate.Condition == nil || s.Gate.Condition.Expr == "" {
			return fmt.Errorf("step %q: conditional gate requires a condition expr", s.ID)
		}
		if err := s.Gate.Condition.Compile(); err != nil {
			return fmt.Errorf("step %q: invalid condition expr: %w", s.ID, err)
		}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/flow/ -v`
Expected: PASS (the whole `flow` package, including the new cases and all existing tests).

- [ ] **Step 5: Commit**

```bash
git add internal/flow/validate.go internal/flow/validate_test.go
git commit -m "feat(flow): compile conditional gate exprs at Validate (fail-fast submit)"
```

---

## Task 3: `internal/gate` — synchronous conditional evaluation

**Files:**
- Modify: `internal/gate/gate.go`
- Modify: `internal/gate/gate_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/gate/gate_test.go`:

```go
// failIfCalledApprover fails the test if a conditional gate consults a human approver.
type failIfCalledApprover struct{ t *testing.T }

func (a failIfCalledApprover) Approve(context.Context, core.RunID, *flow.Step, core.Result) (bool, error) {
	a.t.Fatal("conditional gate must not call the Approver")
	return false, nil
}

func condStep(t *testing.T, expr string) *flow.Step {
	t.Helper()
	c := &flow.Condition{Expr: expr}
	if err := c.Compile(); err != nil {
		t.Fatalf("compile %q: %v", expr, err)
	}
	return &flow.Step{ID: "a", Gate: flow.Gate{Policy: flow.GateConditional, Condition: c}}
}

func TestConditionalGateTruePassesWithoutApprover(t *testing.T) {
	e := &Evaluator{Approver: failIfCalledApprover{t}, Verifier: CommandVerifier{}}
	s := condStep(t, "result.cost_usd < 1.0")
	ok, err := e.Evaluate(context.Background(), "r1", s, core.Result{CostUSD: 0.5}, t.TempDir())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want true/nil", ok, err)
	}
}

func TestConditionalGateFalseFails(t *testing.T) {
	e := &Evaluator{Approver: failIfCalledApprover{t}, Verifier: CommandVerifier{}}
	s := condStep(t, "result.cost_usd < 1.0")
	ok, err := e.Evaluate(context.Background(), "r1", s, core.Result{CostUSD: 2.0}, t.TempDir())
	if err != nil {
		t.Fatalf("a false condition should be a result, not an error: %v", err)
	}
	if ok {
		t.Fatal("gate should have failed (cost 2.0 is not < 1.0)")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gate/ -run TestConditionalGate -v`
Expected: FAIL — currently `GateConditional` calls `Approver.Approve`, so `failIfCalledApprover` triggers `t.Fatal`.

- [ ] **Step 3: Implement in `gate.go`**

Replace the `Evaluate` method's switch (currently `gate.go:25-40`). The `"", flow.GateManual, flow.GateConditional` case is split:

```go
func (e *Evaluator) Evaluate(ctx context.Context, runID core.RunID, s *flow.Step, res core.Result, workDir string) (bool, error) {
	switch s.Gate.Policy {
	case "", flow.GateManual:
		// manual (and the empty default) block on a human approval.
		return e.Approver.Approve(ctx, runID, s, res)
	case flow.GateConditional:
		// conditional resolves synchronously from the compiled expr (like auto).
		env := flow.GateEnv{Result: flow.GateResult{
			Summary:   res.Summary,
			CostUSD:   res.CostUSD,
			Artifacts: artifactPaths(res.Artifacts),
			StepID:    res.StepID,
		}}
		return s.Gate.Condition.Eval(env)
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

// artifactPaths extracts artifact file paths for a conditional gate's `result.artifacts`.
func artifactPaths(arts []core.Artifact) []string {
	if len(arts) == 0 {
		return nil
	}
	paths := make([]string, len(arts))
	for i, a := range arts {
		paths[i] = a.Path
	}
	return paths
}
```

(Update the now-stale comment on the old shared case — the M1 "conditional falls back to manual" note is removed by this replacement.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/gate/ -v`
Expected: PASS (the whole `gate` package — the two new tests plus all existing auto/manual/escalate tests).

- [ ] **Step 5: Commit**

```bash
git add internal/gate/gate.go internal/gate/gate_test.go
git commit -m "feat(gate): evaluate conditional gates synchronously via expr"
```

---

## Task 4: `internal/engine` — conditional behaves like auto (predicates + integration)

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/engine_test.go`

- [ ] **Step 1: Write the failing integration tests**

Append to `internal/engine/engine_test.go` (it already imports `context`, `core`, `event`, `executor`, `flow`, `gate`, `join`, `store`, `workspace`, `testing`, and defines `mustCreate`, `fakeClock`):

```go
func TestConditionalGateTrueProceedsWithoutAwaiting(t *testing.T) {
	st := store.NewMem()
	bus := event.NewBus()
	ch, unsub := bus.Subscribe(32)
	defer unsub()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: fakeClock{},
	}
	cond := &flow.Condition{Expr: "result.cost_usd < 1.0"} // mock cost 0.01 -> true
	if err := cond.Compile(); err != nil {
		t.Fatal(err)
	}
	f := &flow.Flow{Name: "cond", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateConditional, Condition: cond}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepSucceeded {
		t.Fatalf("step status = %q, want succeeded", got.Steps[0].Status)
	}
	unsub()
	for ev := range ch {
		if ev.Kind == event.GateAwaiting {
			t.Error("a conditional gate must not emit gate.awaiting (it resolves synchronously)")
		}
	}
}

func TestConditionalGateFalseAborts(t *testing.T) {
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	cond := &flow.Condition{Expr: "result.cost_usd > 1.0"} // mock cost 0.01 -> false
	if err := cond.Compile(); err != nil {
		t.Fatal(err)
	}
	f := &flow.Flow{Name: "cond", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateConditional, Condition: cond}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected the run to fail on a false conditional gate")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepFailed {
		t.Fatalf("step status = %q, want failed", got.Steps[0].Status)
	}
}

func TestConditionalGateEscalatesOnFalse(t *testing.T) {
	st := store.NewMem()
	bus := event.NewBus()
	ch, unsub := bus.Subscribe(32)
	defer unsub()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}}, // approves the escalation
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: fakeClock{},
	}
	cond := &flow.Condition{Expr: "result.cost_usd > 1.0"} // false -> escalate
	if err := cond.Compile(); err != nil {
		t.Fatal(err)
	}
	f := &flow.Flow{Name: "cond", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{
			Policy: flow.GateConditional, Condition: cond, OnFail: flow.FailEscalate}},
	}}
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("escalation approved, run should succeed: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[0].Status != core.StepSucceeded {
		t.Fatalf("step status = %q, want succeeded (escalation approved)", got.Steps[0].Status)
	}
	unsub()
	var sawEscalation bool
	for ev := range ch {
		if ev.Kind == event.GateAwaiting && ev.Err != "" {
			sawEscalation = true
		}
	}
	if !sawEscalation {
		t.Error("expected a gate.awaiting event with a failure reason (conditional escalation)")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/engine/ -run TestConditionalGate -v`
Expected: FAIL — with conditional still in `gateBlocks`, the true case emits a `gate.awaiting` frame (first test fails), and with `gateEscalates` auto-only, the escalation test's run does not escalate.

- [ ] **Step 3: Implement the two predicate changes in `engine.go`**

Replace `gateBlocks` (currently `engine.go:424-432`):

```go
// gateBlocks reports whether a step's gate can block on human approval. Auto and
// conditional gates resolve synchronously (verifier / expr) and never block.
func gateBlocks(s *flow.Step) bool {
	return gatePolicyOf(s) == flow.GateManual
}
```

Replace `gateEscalates` (currently `engine.go:434-438`):

```go
// gateEscalates reports whether a failed gate should escalate to a human. Auto and
// conditional gates can escalate (both resolve automatically); a manual gate's
// rejection is itself a human decision, so it never escalates.
func gateEscalates(s *flow.Step) bool {
	p := gatePolicyOf(s)
	return (p == flow.GateAuto || p == flow.GateConditional) && s.Gate.OnFail == flow.FailEscalate
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/engine/ -run TestConditionalGate -v && go test ./internal/engine/ -v`
Expected: PASS (the three new tests plus every existing engine test — confirm no regression in the manual/auto/escalate suite).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go
git commit -m "feat(engine): conditional gates resolve synchronously and can escalate"
```

---

## Task 5: Full verification + manual proof

**Files:** none (verification only).

- [ ] **Step 1: Full race suite + vet + dependency check**

Run: `go test -race ./... && go vet ./...`
Expected: every package `ok`; no vet output. For raw output use `rtk proxy go test -race ./internal/...`.
Run: `grep '^go ' go.mod` → `go 1.22`; `grep expr go.mod` → `github.com/expr-lang/expr v1.17.8` (direct, no `// indirect`); `git diff main -- go.mod | head` shows ONLY the expr require added.

- [ ] **Step 2: Manual proof (zero-cost, no network — uses the mock agent)**

Use the `running-the-orchestrator` skill to build `magisterd`+`cm` from this worktree and start the daemon on a throwaway db/port. Submit a one-step flow with a conditional gate over the mock agent (the mock returns `cost_usd 0.01`, so `result.cost_usd < 1.0` is true):

```yaml
name: cond-demo
concurrency: 1
steps:
  - id: greet
    agent: mock
    role: implementer
    prompt: "anything"
    gate: { policy: conditional, condition: { expr: 'result.cost_usd < 1.0' } }
```

`cm run <flow> ` then `cm watch <run>`. Confirm the event stream is `run.started → step.started → step.done → run.done` with **NO `gate.awaiting` frame** (the conditional gate resolved synchronously) and the run `succeeded`. Then flip the expr to `result.cost_usd > 1.0` and confirm the run fails at the gate; add `on_fail: escalate` and confirm a `gate.awaiting` frame appears (then `cm approve <run> greet` lets it finish). Also submit a flow with a deliberately broken expr (`condition: { expr: 'not valid +++' }`) and confirm `cm run` is **rejected at submit** (validation error), never starting a run. Capture the observed behavior in the handoff.

- [ ] **Step 3: Final state confirmation**

Run: `git log --oneline main..HEAD`
Expected: the four feature commits from Tasks 1–4. The branch is ready to merge to `main` per finishing-a-development-branch.

---

## Self-Review

**Spec coverage (spec §→task):**
- §2 dependency (`expr-lang/expr v1.17.8`, go-1.22 intact) → Task 0 Step 2 + Task 1 Step 4 + Task 5 Step 1.
- §3 `flow` GateEnv/GateResult/Condition.Compile/Eval → Task 1; compile-at-Validate → Task 2.
- §4 `gate.Evaluate` conditional synchronous + `artifactPaths` → Task 3.
- §5 engine predicates (`gateBlocks` drops conditional, `gateEscalates` adds it) → Task 4.
- §6 error semantics: bad expr → submit failure (Task 2 test); runtime/uncompiled → Eval error (Task 1 `TestConditionEvalUncompiledErrors`, Task 3 propagation).
- §7 testing: flow unit (Tasks 1–2), gate unit (Task 3), engine integration true/false/escalate (Task 4), manual proof (Task 5 Step 2).
- §9 done-criteria → Task 5.

**Placeholder scan:** No TBD/TODO; every code/command step is complete. The dependency-add is split deliberately (Task 0 fetches with network; Task 1 imports + `go mod tidy` finalizes) so subagents need no network — `go mod tidy` in Task 1 keeps the require because `flow.go` now imports it.

**Type consistency:** `GateEnv{Result GateResult}`, `GateResult{Summary,CostUSD,Artifacts,StepID}` (with `expr:"summary|cost_usd|artifacts|step_id"` tags), `Condition{Expr, prog *vm.Program}`, `Condition.Compile() error`, `Condition.Eval(GateEnv)(bool,error)`, and `artifactPaths([]core.Artifact)[]string` are used identically across Tasks 1–4. Test exprs use `result.cost_usd`/`result.summary` matching the tags. The mock-cost assumption (`0.01`) used in Task 4 matches the existing `TestEscalateApproveSucceeds` assertion (`CostUSD != 0.01`). Engine predicate names (`gateBlocks`, `gateEscalates`, `gatePolicyOf`) match the existing engine.
