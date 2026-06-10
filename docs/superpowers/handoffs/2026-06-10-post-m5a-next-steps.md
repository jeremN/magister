# Handoff — post-M5a (conditional gates DONE; next: M5b joins or git-native merge) (2026-06-10)

**M5 Slice A (conditional gates) is COMPLETE and merged to `main`.** This handoff records the finished state, the manual-proof evidence, and the next-step menu.

## State

- `main` at `c6844ce`, clean. M5a merged via fast-forward (4 feature commits over the kickoff commit `1ed66d2`). Worktree removed, branch `m5a-conditional-gates` deleted.
- `go test -race ./...` → **159 passed / 15 packages**; `go vet ./...` clean; `go 1.22` unchanged.
- New dep: `github.com/expr-lang/expr v1.17.8` (go-1.18, zero transitive deps), a **direct** require.
- The four feature commits:
  - `41e03fc` feat(flow): compile+eval conditional gate expressions (expr-lang)
  - `e04c294` feat(flow): compile conditional gate exprs at Validate (fail-fast submit)
  - `e0d9140` feat(gate): evaluate conditional gates synchronously via expr
  - `c6844ce` feat(engine): conditional gates resolve synchronously and can escalate

## What M5a built

`gate.policy: conditional` now compiles a `condition.expr` (via `expr-lang/expr`) at **submit time** and evaluates it **synchronously** against the gated step's result — replacing the old M1 fallback to manual approval. Guiding model held throughout: **a conditional gate is an auto gate whose pass/fail comes from an expr instead of a shell exit code** (synchronous, non-blocking, `on_fail: escalate`-capable). No store/SSE/schema/YAML-surface change.

Data flow (one conditional gate): YAML → `flow.Validate` compiles the expr (bad expr → submit 400) → engine `attempt` → `gate.Evaluate` maps `core.Result → flow.GateEnv` → `Condition.Eval`. Architecture:
- **`internal/flow`** owns `GateEnv{Result GateResult}` / `GateResult{Summary,CostUSD,Artifacts,StepID}` (expr tags `summary/cost_usd/artifacts/step_id`), `Condition{Expr, prog *vm.Program}`, `Condition.Compile()` (`expr.Compile` + `expr.Env(GateEnv{})` + `expr.AsBool()` → rejects malformed/non-bool at compile), `Condition.Eval(GateEnv)(bool,error)`. **`flow` does NOT import `core`** (the schema package stays core-free); `prog` is unexported/untagged → never serialized, re-compiled on resume (idempotent).
- **`internal/flow/validate.go`** compiles the expr in the `GateConditional` case → fail-fast at `POST /v1/runs` and resume re-validation.
- **`internal/gate/gate.go`** splits the conditional branch out of the manual case: builds the env via the `core.Result → flow.GateEnv` mapping (`artifactPaths([]core.Artifact)→[]string` of `.Path`) and returns `Condition.Eval(env)` — no Approver, synchronous.
- **`internal/engine/engine.go`** two predicates: `gateBlocks` → manual-only (conditional resolves inline, no `gate.awaiting`); `gateEscalates` → `(auto || conditional) && OnFail==escalate` (a false condition with `on_fail: escalate` reuses the M4-A `Escalate` path).

**Error semantics (3 distinct paths, spec §6):** bad expr → submit failure (Validate); runtime Eval error → `Evaluate` returns it → `attempt` returns `gateFailed=false` → run fails **without** escalating (error ≠ false); false condition → `gateFailed=true` → abort/retry/escalate per `on_fail`.

User-approved design decisions honored: **expr-lang/expr v1.17.8**; **escalate extends to conditional**.

## Manual proof (Task 5 Step 2) — observed, zero-cost (mock agent, no network/keys)

Built `magisterd`+`cm` from the worktree, ran a one-step `agent: mock` flow (mock returns `cost_usd 0.01`) over the live daemon + SSE on a throwaway db/port:

| Scenario | Config | Observed SSE / result |
|---|---|---|
| **true** | `result.cost_usd < 1.0` | `run.started → step.started → step.done → run.done`, **no `gate.awaiting`**, `succeeded` (synchronous pass) |
| **false** | `result.cost_usd > 1.0` | `step.failed` (`gate failed (policy="conditional")`) → run `failed`, no awaiting (synchronous abort) |
| **false + escalate** | `> 1.0`, `on_fail: escalate` | `gate.awaiting` (Err=`gate failed (policy="conditional")`) → `cm approve <run> greet` → `step.done → run.done`, `succeeded` |
| **bad expr** | `not valid +++` | `cm run` → `400: invalid condition expr: unexpected token EOF (1:13)` (rich expr-lang diagnostic), **no run created** (submit-time reject) |

All four pass. The `gate.awaiting.Err` carries the synthesized failure reason; the true case emits no awaiting frame (proves synchronous resolve over real SSE).

## Notes / blessed deviations

- **go.mod has TWO changes, not one.** Beyond adding `expr` (direct), Task 1's mandated `go mod tidy` also moved `github.com/oklog/ulid/v2` from the `// indirect` block to the **direct** require block. That package is genuinely a direct import (`internal/api/middleware.go`, `internal/supervisor/supervisor.go`) but `main`'s go.mod had wrongly marked it indirect — i.e. `main` was already untidy and tidy correctly fixed it. **The plan's "go.mod diff shows ONLY the expr require" done-criterion is relaxed to `{expr added direct; oklog/ulid indirect→direct}`.** This is correct, not over-reach; reverting it would knowingly re-ship an untidy go.mod.
- **Two hardening tests added during review** (beyond the plan): `flow.TestConditionEvalRuntimeNonBoolErrors` (pins that a compile-OK-but-runtime-non-bool expr surfaces as a `Run` error, protecting the `out.(bool)` assertion against a future expr-lang change) and `engine.TestConditionalGateEvalErrorFailsWithoutEscalating` (pins error ≠ false at the engine seam — an eval error fails the run without escalating even under `on_fail: escalate`). Also `gate.TestConditionalGateMapsResultFields` locks the `core.Result → GateEnv` field wiring (`.Path`/`step_id`).
- **Four escalate-path comments broadened** auto→auto/conditional (`flow.go` FailPolicy doc, `gate.go` Escalate doc, `engine.go` x2) — the feature's `gateEscalates` broadening had left neighboring docs stale (a cross-commit seam the per-task reviews missed; caught by the final holistic review).

## After M5a — next-step menu (both remain)

1. **M5b — `select`/`synthesize` agent-arbitrated joins.** The other half of the M5 split (gates and joins share zero code). M5b is the one that **leads into the git-native merge-at-join handoff** (joining multiple step results across worktrees). No design doc yet — would start with brainstorming → spec → plan, same as M5a.
2. **External-repo / git-native merge-at-join handoff.** The remaining M4 carry-over (M4 was COMPLETE except this). Could be tackled independently or folded into M5b's join work.

Recommended: **M5b**, since it naturally produces the git-native merge requirement rather than tackling it abstractly. Spec/plan live under `docs/superpowers/{specs,plans}/`; this slice's are `2026-06-10-m5a-conditional-gates-{design,…}.md`.
