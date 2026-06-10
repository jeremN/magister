# M5 Slice B — select / synthesize agent-arbitrated joins (design)

**Status:** approved 2026-06-10
**Predecessors:** M0–M4 complete; M5a (conditional gates) merged. M4 Slice C built git-worktree workspaces and the `Merge` join; M5a built the conditional-gate compile/eval + extended the M4-A escalate path to conditional.
**Scope:** implement the two unregistered join strategies — `select` and `synthesize` — that arbitrate a fan-in step's upstream results via an AI agent. The YAML surface (`flow.Join{Strategy, Agent, OnConflict}`, the `select`/`synthesize` constants) and `validateJoin` (already requires an arbiter `agent` + ≥1 dependency, and validates `on_conflict`) exist; this slice fills in behavior plus ONE new seam — executor access for join strategies — and wires `on_conflict` into the engine. **The git-native merge-at-join handoff (real git branch/merge) remains a SEPARATE later slice** — M5b operates on the existing path-based artifact-handoff model.

## 1. Why this fits the existing seam

The fan-in machinery already exists; this slice only fills in the two arbitrated strategies:
- `flow.Join{Strategy, Agent, OnConflict}` is defined (`internal/flow/flow.go`); `validateJoin` (`validate.go:101`) already requires a non-empty arbiter `agent` for `select`/`synthesize`, validates `on_conflict ∈ {"", abort, retry, escalate}`, and requires ≥1 dependency. **No validation change.**
- `join.Strategy.Join(ctx, s, inputs, workDir) (core.Result, error)` and `join.Default()` exist (`internal/join/join.go`); today only `JoinMerge` is registered, so `select`/`synthesize` fail at runtime in `engine.execute()` with `join strategy %q not implemented yet` (`engine.go:322`).
- The engine already gathers a fan-in step's `inputs` from its dependencies' results (`results[dep].Artifacts`, flattened, each `core.Artifact` tagged with its source `StepID`) and dispatches `strat.Join(...)` when `s.Join != nil` (`engine.go:319-324`). M4-A built the `Escalate` block-on-approver path; M5a reused it for conditional gates.

So M5b is: register `select`/`synthesize`; give a join strategy a way to invoke its arbiter agent (the one new seam); implement the two strategies; and wire `on_conflict` into the engine's join-failure disposition. **No store/SSE/schema/YAML-surface change; no new dependency.**

**Guiding model: a join strategy that needs an agent invokes its arbiter through an injected `RunAgent` callback that reuses the engine's existing agent-execution path** (resolve the executor, build the `core.Task` with the persist-then-publish `Emit` closure, call `Run`). An arbiter therefore runs exactly like a normal step's agent — including streaming its `agent.tool` milestones over the existing SSE — and produces a normal `core.Result`.

## 2. The seam — `RunAgent` injected into `Strategy.Join`

`Strategy.Join` gains one parameter: a callback that runs a named agent with a strategy-built prompt and returns its result.

```go
// RunAgent runs the named agent with prompt in workDir and returns its result.
// The engine binds it to the same executor path a normal step uses (Task + the
// persist-then-publish Emit closure), so an arbiter streams agent.tool milestones
// and its artifacts are discovered exactly like a normal step's.
type RunAgent func(ctx context.Context, agentName, prompt, workDir string, inputs []core.Artifact) (core.Result, error)

type Strategy interface {
	Join(ctx context.Context, s *flow.Step, inputs []core.Artifact, workDir string, run RunAgent) (core.Result, error)
}
```

`Merge.Join` gains the `run RunAgent` parameter and ignores it (merge needs no agent). 

**Engine change (`execute()` refactor):** extract the existing executor branch (`engine.go:326-346` — resolve `e.Execs[name]`, build the `emit` closure, build the `core.Task`, call `ag.Run`) into a reusable method:

```go
func (e *Engine) runAgent(ctx context.Context, runID core.RunID, stepID, role, agentName, prompt, workDir string, attemptNum int, inputs []core.Artifact) (core.Result, error)
```

- A normal step calls `e.runAgent(ctx, runID, s.ID, s.Role, s.Agent, promptFor(s, inputs), workDir, attemptNum, inputs)`.
- A join step binds a closure `run := func(ctx, name, prompt, wd, in) { return e.runAgent(ctx, runID, s.ID, "arbiter", name, prompt, wd, attemptNum, in) }` and calls `strat.Join(ctx, s, inputs, workDir, run)`.
- An unknown arbiter agent surfaces as the existing `unknown agent %q` error from `runAgent` (submit-time validation already guarantees a non-empty name).

This keeps strategies free of `core.Task`/`event`/`Emit` knowledge and gives the arbiter milestone-streaming for free.

## 3. Candidate staging (shared helper)

Both arbitrated strategies must let the agent READ the candidates. Upstream `isolated` steps wrote artifacts in their own worktrees (absolute paths); to guarantee the arbiter can read them regardless of agent sandboxing, the strategy **stages** each candidate into a subdirectory of the join workdir before invoking the arbiter:

```go
// stageCandidates copies each input artifact to <workDir>/.candidates/<stepID>/<base>
// and returns a per-step listing for the prompt. Read-only context for the arbiter;
// excluded from a synthesize result (§5).
func stageCandidates(inputs []core.Artifact, workDir string) (map[string][]string, error)
```

Staging into a dedicated `.candidates/` subdir keeps the copies separable from a synthesize step's real output. (For tests the arbiter is a stub that ignores the files, so staging's file-copy correctness is unit-tested while "the agent actually reads them" is the manual proof.)

## 4. `select`

The arbiter chooses ONE candidate; the join forwards that candidate's original artifacts by reference.

1. `stageCandidates(inputs, workDir)`.
2. Build the prompt: list each candidate `<stepID>` + its staged paths; instruct *"read each candidate and choose the single best; explain why; end your reply with a line `SELECTED: <step-id>`."*
3. `res, err := run(ctx, s.Join.Agent, prompt, workDir, inputs)`; an arbiter error → strategy error (→ `on_conflict`, §6).
4. Parse `res.Summary` for the trailing `SELECTED: <step-id>` token (last match wins). The winner MUST be one of the step's dependencies (`s.Needs`). A missing/unparseable token or an unknown/out-of-set winner → strategy error (→ `on_conflict`).
5. Result = the winner's ORIGINAL artifacts (filter `inputs` by `StepID == winner`; forwarded by reference, NO copy), `Summary = res.Summary` (the arbiter's rationale incl. its choice), `CostUSD = res.CostUSD`, `StepID = s.ID`. Downstream steps consume the winner's files at their existing paths; `core.Artifact.StepID` preserves provenance.

## 5. `synthesize`

The arbiter writes a single reconciled result; the join returns it.

1. `stageCandidates(inputs, workDir)`.
2. Build the prompt: list candidates; instruct *"read all candidates and write one reconciled/merged result into the current directory."*
3. `res, err := run(ctx, s.Join.Agent, prompt, workDir, inputs)`; arbiter error → strategy error (→ `on_conflict`).
4. `runAgent` discovers the arbiter's writes via the existing `discoverGit` artifact discovery. The synthesize result = `res` with its `Artifacts` filtered to EXCLUDE anything under `.candidates/` (the staged inputs), `StepID = s.ID`. **Zero artifacts after exclusion → strategy error** (the arbiter produced no synthesis) (→ `on_conflict`).

## 6. `on_conflict` — engine disposition of a failed join

A join "fails" when its strategy returns an error (arbiter error, unparseable/invalid `select`, empty `synthesize`). For a join step, `on_conflict` GOVERNS the failure disposition (it is the join analog of a gate's `on_fail`, and for a join step it supersedes the generic `s.Retry`-driven exec-retry):

- **`abort`** (default, and `""`): fail the run immediately — `step.failed` + run aborts. No retry.
- **`retry`**: re-run the join, bounded by the step's `s.Retry.Max` budget (a `retry` `on_conflict` with no `s.Retry` set yields a single attempt — i.e. behaves as abort; configure `s.Retry` to get re-runs). Emits `step.retrying` between attempts, reusing the existing backoff.
- **`escalate`**: a NEW `escalateJoin` path (parallel to the gate `escalate` but **approve = re-run**, since a failed join has no result to accept). It emits `gate.awaiting` with the failure reason on `.Err`, blocks on `e.Gate.Escalate` (the same ApprovalRegistry/Approver): **approve → re-run the join exactly once** (a fresh `attempt`); if that re-run succeeds the step succeeds, otherwise it fails terminally (no second escalation — bounded, no human-loop recursion). **Reject → abort** (`step.failed`, reason `escalated join rejected`).

Engine integration: in `runStep`, a failed attempt of a join step (`s.Join != nil && execErr != nil`) is dispositioned by `on_conflict` rather than the generic gate/exec retry branch. `escalateJoin` is a sibling of the existing `escalate` (`engine.go:439`); the disposition reuses the existing `transition`/event plumbing (`gate.awaiting`/`step.retrying`/`step.failed`/`step.done`). **No new event kinds, no store/schema change.**

## 7. Error & evaluation semantics

- **Arbiter failure** (the agent process errors, non-zero exit, or its `core.Executor` returns an error) → strategy error → `on_conflict`.
- **`select` parse failure** (no `SELECTED:` token, or a winner not in `s.Needs`) → strategy error → `on_conflict`. The error names what was wrong (e.g. `select: no SELECTED token in arbiter output` / `select: chosen step %q is not a dependency`).
- **`synthesize` empty output** (no artifacts after excluding `.candidates/`) → strategy error → `on_conflict`.
- **Unknown arbiter agent** → `unknown agent %q` from `runAgent` (in practice unreachable — `validateJoin` requires a non-empty name at submit, though a name not in the registry still errors here).
- **Cost** in the join result is the arbiter's `core.Result.CostUSD` (0 for gemini/codex, which report no USD).
- A successful join is a normal `step.done` with the result's `Summary`/`CostUSD`; the arbiter's mid-run `agent.tool` milestones stream over the existing SSE (via the reused `Emit` closure).

## 8. Testing

All automated tests run without keys/network, using a STUB `RunAgent` (a func literal returning a canned `core.Result`) — no real agent process:
- **`join` unit tests**: `stageCandidates` copies each input under `.candidates/<stepID>/`. `select` — a stub returning `"…SELECTED: impl-api"` forwards `impl-api`'s artifacts by reference; a stub with no token / an out-of-`Needs` winner → error. `synthesize` — a stub that "writes" an artifact (returned in its result) yields that artifact, with `.candidates/` paths excluded; a stub returning zero (non-staged) artifacts → error. `Merge` still passes with the new (ignored) `run` parameter.
- **`engine` integration** (real engine, mock upstreams, stub/registered arbiter): a fan-in flow (two `mock` steps → a `select`/`synthesize` join) reaches `run.done` with `step.succeeded`. `on_conflict`: a failing join with `abort` → run fails; `retry` (+`s.Retry`) → re-runs then fails/succeeds; `escalate` → `gate.awaiting` (Err set) → approve re-runs (succeeds) / reject aborts. `runAgent` extraction leaves the existing normal-step + gate/escalate suite green.
- **Manual proof** (via the `running-the-orchestrator` skill, zero-cost mock arbiter): a fan-in flow whose join uses `select`/`synthesize` with `agent: mock` reaches `run.done` over real SSE; observe the join's `step.done` and (with a real arbiter) streamed `agent.tool` frames. Capture in the post-M5b handoff.

## 9. Out of scope (YAGNI)

Git-native merge-at-join (real branch-from-deps / `git merge`, conflict markers) — the separate later handoff; M5b forwards/synthesizes on the path-based model. Injecting upstream `Summary`s or artifact CONTENTS into the arbiter prompt (the arbiter reads staged files; summaries-in-prompt can be added later if a real flow needs it). A structured (JSON) arbiter protocol for `select` (the `SELECTED:` token convention is the v1 contract). Multi-winner `select`, ranked output, or partial synthesis. New join-specific event kinds. Persisting staged candidates beyond the run. No new dependency.

## 10. Done criteria

- `go test -race ./...` + `go vet ./...` clean; `go.mod` still `go 1.22`; no new dependency.
- `select` and `synthesize` are registered in `join.Default()` and arbitrate via an agent invoked through the injected `RunAgent`; `Merge` and every existing normal-step/gate/escalate path are unchanged in behavior (the `runAgent` extraction is behavior-preserving).
- `select` forwards the arbiter-chosen dependency's artifacts by reference (provenance preserved); `synthesize` returns the arbiter's written artifacts (staged inputs excluded); both surface the arbiter's cost/summary and stream its `agent.tool` milestones.
- A failed join is dispositioned by `on_conflict`: `abort` fails the run, `retry` re-runs within `s.Retry`, `escalate` blocks for a human whose **approval re-runs the join once** and rejection aborts.
- Manual proof shows a fan-in `select`/`synthesize` join resolving over SSE to `run.done`.
