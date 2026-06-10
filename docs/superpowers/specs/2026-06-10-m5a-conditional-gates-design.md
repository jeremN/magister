# M5 Slice A — Conditional Gates (design)

**Status:** approved 2026-06-10
**Predecessors:** M0–M4 complete (all CLI adapters + git-worktree workspaces merged). M4 Slice A already built the gate/escalate machinery this slice extends.
**Scope:** implement `gate.policy: conditional` — compile a `condition.expr` at submit time and evaluate it synchronously against the gated step's result, instead of the current M1 fallback to manual approval. **M5 Slice B (`select`/`synthesize` agent-arbitrated joins) is a SEPARATE later slice** — M5 was split because gates and joins share zero code.

## 1. Why this fits the existing seam

The YAML surface and the gate machinery already exist; this slice only fills in the behavior:
- `flow.Gate{Policy, Verifier, Condition, OnFail}` and `flow.Condition{Expr}` are defined (`internal/flow/flow.go`); `flow.Validate` already requires a non-empty `condition.expr` for a conditional gate (`validate.go:78-81`).
- `gate.Evaluator.Evaluate` (`internal/gate/gate.go`) already dispatches on `Gate.Policy`; today `GateConditional` shares the manual branch (`return e.Approver.Approve(...)`) — an explicit M1 placeholder ("the expr-lang evaluator arrives in M5").
- The engine already classifies gates with `gateBlocks` / `gateEscalates` (`engine.go:424/436`) and owns the abort/retry/escalate policy. M4 Slice A built `Escalate` (a failed AUTO gate → human approval) — this slice reuses it for conditional.

So M5a is: add the expr compile/eval to `flow`, flip the `GateConditional` branch in `gate.Evaluate` to synchronous expr evaluation, and update two engine predicates. No new YAML fields, no store/SSE/schema change.

**Guiding model: a conditional gate is an AUTO gate whose pass/fail comes from an expression instead of a shell command's exit code.** Everything true of auto (synchronous, non-blocking, `on_fail: escalate` supported) becomes true of conditional.

## 2. Dependency: `expr-lang/expr`

Add `github.com/expr-lang/expr` pinned at **`v1.17.8`** (latest). Verified: its `go.mod` declares **`go 1.18`** — within the project's go-1.22 ceiling — and it has **zero transitive dependencies** (a single self-contained, sandboxed, compile-ahead expression engine). This is the project's first third-party *logic* dependency; it was the original design's explicit choice (`docs/superpowers/specs/2026-06-02-orchestrator-design.md` §"Condition language"). `go.mod` moves to listing it under `require`; `go.sum` gains its checksums. The go-1.22 line and all existing pins are unchanged.

## 3. `internal/flow` changes — compile-ahead + typed env

`flow` gains the expr import and owns compilation + evaluation so the engine/gate layers stay decoupled from the expr library. `flow` does NOT import `core` (the `core.Result → GateEnv` mapping lives in `gate`, §4), keeping the schema package core-free.

```go
// GateEnv is the environment a conditional gate's expression evaluates against:
// the gated step's result, exposed under `result`.
type GateEnv struct {
	Result GateResult `expr:"result"`
}

// GateResult mirrors the salient fields of core.Result for expressions
// (e.g. `result.cost_usd < 1.0 && result.summary contains "OK"`).
type GateResult struct {
	Summary   string   `expr:"summary"`
	CostUSD   float64  `expr:"cost_usd"`
	Artifacts []string `expr:"artifacts"` // artifact paths
	StepID    string   `expr:"step_id"`
}

// Condition configures a conditional gate. Expr is compiled at Validate time
// (fail-fast at submit); prog is the compiled program (nil until Compile runs).
type Condition struct {
	Expr string `yaml:"expr"`
	prog *vm.Program
}

// Compile compiles Expr against GateEnv, requiring a boolean result. Called from
// Validate so a malformed/non-bool expression fails submission, not a running step.
func (c *Condition) Compile() error {
	p, err := expr.Compile(c.Expr, expr.Env(GateEnv{}), expr.AsBool())
	if err != nil {
		return err
	}
	c.prog = p
	return nil
}

// Eval runs the compiled program against env. Returns the boolean gate decision.
// A nil prog (Compile not run) or a runtime error is returned as an error.
func (c *Condition) Eval(env GateEnv) (bool, error) {
	if c.prog == nil {
		return false, fmt.Errorf("condition not compiled")
	}
	out, err := expr.Run(c.prog, env)
	if err != nil {
		return false, err
	}
	return out.(bool), nil // AsBool guarantees a bool
}
```

`validate.go`'s `GateConditional` case gains the compile call:
```go
case GateConditional:
	if s.Gate.Condition == nil || s.Gate.Condition.Expr == "" {
		return fmt.Errorf("step %q: conditional gate requires a condition expr", s.ID)
	}
	if err := s.Gate.Condition.Compile(); err != nil {
		return fmt.Errorf("step %q: invalid condition expr: %w", s.ID, err)
	}
```

The compiled `*vm.Program` is in-memory only (never serialized). On resume the flow YAML is re-parsed → re-validated → re-compiled (`supervisor.go:136`), so it's idempotent and persistence-free.

## 4. `internal/gate` change — synchronous conditional evaluation

Split the shared manual/conditional branch in `Evaluate`:
```go
switch s.Gate.Policy {
case "", flow.GateManual:
	return e.Approver.Approve(ctx, runID, s, res)
case flow.GateConditional:
	env := flow.GateEnv{Result: flow.GateResult{
		Summary:   res.Summary,
		CostUSD:   res.CostUSD,
		Artifacts: artifactPaths(res.Artifacts),
		StepID:    res.StepID,
	}}
	return s.Gate.Condition.Eval(env)
case flow.GateAuto:
	// unchanged verifier path
}
```
`artifactPaths` is a small local helper mapping `[]core.Artifact` → `[]string` of `.Path`. A `Condition.Eval` error propagates as the `Evaluate` error (the engine fails the attempt with it). No Approver, no blocking — conditional now resolves synchronously like auto.

## 5. `internal/engine` changes — two predicates

- **`gateBlocks`** (`engine.go:424`): drop `flow.GateConditional` so conditional no longer takes the block-on-human path (it resolves synchronously). Becomes `case flow.GateManual: return true`.
- **`gateEscalates`** (`engine.go:436`): broaden from auto-only to auto-OR-conditional:
  `return (p == flow.GateAuto || p == flow.GateConditional) && s.Gate.OnFail == flow.FailEscalate`
  so a false condition with `on_fail: escalate` converts to a human approval via the existing `Escalate` path (approve → the step's result stands; reject → abort). The failure reason on the `gate.awaiting` event's `Err` is the synthesized `gate failed (policy=conditional)` already produced at the call site.

No other engine change: a conditional gate returning `(false, nil)` flows through the existing `case !ok:` → `gate failed` → abort/retry/escalate exactly as an auto gate does.

## 6. Error & evaluation semantics

- **Bad expression** (syntax, unknown identifier, non-bool result) → caught at **submit** by `Compile` (`expr.AsBool()` + the typed `expr.Env(GateEnv{})`), surfaced as a validation error on `POST /v1/runs` (and on resume). A running step never sees it.
- **Runtime eval error** (rare given the typed env — e.g. a future env extension introducing a nilable field) → `Eval` returns an error → `Evaluate` returns it → the engine fails the attempt with `verifier error`-style propagation (a gate *error*, distinct from a gate *false*).
- **Cost in the env** is the step's `core.Result.CostUSD` (0 for gemini/codex agents, which report no USD — expressions over `cost_usd` are still valid, just compare against 0).

## 7. Testing

All automated, no keys/network:
- **`flow` unit tests**: `Condition.Compile` rejects a syntactically bad expr and a non-bool expr (`result.summary` alone); accepts a valid bool expr. `Validate` fails a conditional gate with a bad expr (compile error surfaced) and passes a good one. `Eval` returns the expected bool for true/false cases over a constructed `GateEnv`, and errors when `prog` is nil.
- **`gate` unit tests**: `Evaluate` on a conditional gate returns the expr's bool (true→pass, false→fail) synchronously without touching the Approver (use a stub Approver that fails the test if called); a runtime eval error propagates.
- **`engine` integration**: a conditional gate whose expr is false → the run aborts (default `on_fail`); false + `on_fail: escalate` → the run blocks awaiting human approval (reuses the M4-A escalate test shape); true → the run proceeds. `gateBlocks`/`gateEscalates` table tests include the conditional cases.
- **Manual proof**: a one-step `agent: mock` (or sonnet) flow with `gate: { policy: conditional, condition: { expr: 'result.cost_usd < 1.0' } }` over the run skill — the gate resolves synchronously (no `gate.awaiting` frame) and the run reaches `run.done`; a false expr + escalate shows a `gate.awaiting` frame.

## 8. Out of scope (YAGNI)

`select`/`synthesize` joins (M5 Slice B); exposing sibling steps' results or run-level metadata in the env (only the gated step's `result.*` — extend later if a real flow needs it); routing/`skipped` status from a condition (the design reserves `skipped`; v1 conditional only gates, it does not skip-and-continue); custom expr functions/operators beyond expr-lang defaults; persisting the compiled program. No store/SSE/migration/YAML-schema change.

## 9. Done criteria

- `go test -race ./...` + `go vet ./...` clean; `go.mod` still `go 1.22`; the only new dependency is `expr-lang/expr v1.17.8` (go-1.18, zero transitive deps).
- A conditional gate compiles its expr at submit (bad expr → submission error) and evaluates it synchronously at gate time against `result.{summary,cost_usd,artifacts,step_id}`; true → proceed, false → abort/retry/escalate per `on_fail`; `on_fail: escalate` on a false condition blocks for human approval.
- `GateConditional` no longer falls back to manual approval; `gateBlocks` excludes conditional; `gateEscalates` includes it. Manual proof shows a conditional gate resolving over SSE with no `gate.awaiting` on a true expr.
