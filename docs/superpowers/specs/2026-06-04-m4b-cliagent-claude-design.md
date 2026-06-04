# Design — M4 Slice B (B1): Real CLIAgent — `claude`

**Written:** 2026-06-04, after M4 Slices A & C merged to `main`.
**Parent spec:** `docs/superpowers/specs/2026-06-02-orchestrator-design.md` (line 78 `executor/` adapter = CLIAgent + stream-json + MockAgent; line 229 `execs` registry `"opus"→CLIAgent`; M4 row "real CLIAgent + stream-json cost").
**Status:** approved (brainstorming), ready for `writing-plans`.
**Sequence:** Slice B is the LAST M4 piece (A & C already merged). Built **after** C so real agents run inside C's git worktrees. This is **B1** — the framework + the `claude` CLI only; `gemini`/`codex` and the external-repo/merge handoff are later slices.

## TL;DR

Make the executor real for the first CLI. Today the daemon registers only `executor.Mock`. B1 adds a generic **`CLIAgent`** (subprocess runner, `core.Executor`) plus a per-CLI **`CLISpec`** adapter, with **`ClaudeSpec`** implementing the `claude` CLI: run `claude -p "<prompt>" --model <opus|sonnet> --output-format json --permission-mode acceptEdits` in the step's worktree, parse the result object for cost + summary, discover artifacts via `git status`. The daemon registers `opus`/`sonnet` (both `claude`, different `--model`) alongside the kept `mock`. Tested with a **fake-CLI stub + JSON fixtures** — no API keys, no network; the real flow is a manual proof. **No DB migration, no new YAML fields, no new dependencies** (`encoding/json` + `os/exec`, stdlib).

## 1. Scope

**In scope**

1. `CLIAgent` — generic `core.Executor`: builds args, runs the binary in `WorkDir` with the daemon env, captures output, parses cost+summary, discovers artifacts, maps failures to errors.
2. `CLISpec` — the per-CLI seam (args builder + output parser). One impl now: `ClaudeSpec`.
3. Git-based artifact discovery (default; injectable).
4. Secrets via env passthrough; `os/exec` no-shell invocation.
5. Daemon wiring: `opus` + `sonnet` (claude), keeping `mock`.
6. Tests with a stub CLI + captured-JSON fixtures.

**Non-goals (deferred)**

- `gemini` + `codex` adapters (B2 — additional `CLISpec`s).
- External target repo (real codebase) + git-native branch-from-deps / merge-at-join handoff (a later slice; B1 is greenfield in C's scratch worktree).
- Streaming agent events live to the SSE bus (would use `--output-format stream-json`; B1 uses `--output-format json`).
- Any engine / join / gate / config-schema change. Agent-key→CLI mapping is hardcoded in the daemon for B1.

## 2. Architecture

Two units behind the existing `core.Executor` port (`Run(ctx, Task) (Result, error)`), in `internal/executor`:

```go
// CLISpec adapts one CLI's invocation + output schema. ClaudeSpec now; CodexSpec/
// GeminiSpec later. Parse returns a non-nil error when the agent itself failed
// (e.g. is_error / non-success subtype), distinct from a process/exec failure.
type CLISpec interface {
	Args(model, prompt string) []string
	Parse(stdout []byte) (summary string, costUSD float64, err error)
}

// CLIAgent is a core.Executor that runs a coding-agent CLI in the step's WorkDir,
// passes the prompt, parses cost+summary, and discovers the files it changed.
type CLIAgent struct {
	Bin      string                                          // "claude"
	Model    string                                          // "opus" / "sonnet"
	Spec     CLISpec                                         // ClaudeSpec{}
	Env      []string                                        // nil ⇒ os.Environ() (carries ANTHROPIC_API_KEY)
	Discover func(workDir string) ([]core.Artifact, error)  // nil ⇒ git-status discovery
	Log      *slog.Logger                                    // nil ⇒ discard (non-fatal discovery errors)
}
```

`CLIAgent.Run(ctx, t)`:
1. `args := Spec.Args(Model, t.Prompt)`.
2. `cmd := exec.CommandContext(ctx, Bin, args...)`; `cmd.Dir = t.WorkDir`; `cmd.Env = Env` (or `os.Environ()`); capture stdout + stderr separately.
3. Run. On `exec.ErrNotFound` → `"agent binary %q not found"`. On non-zero exit → error wrapping trimmed stderr. The engine's per-step timeout (Slice A) wraps `ctx`, so a hung agent is killed and surfaces as a (retryable) error.
4. `summary, cost, perr := Spec.Parse(stdout)`; on `perr` → error (the agent ran but failed/aborted).
5. `arts, derr := Discover(t.WorkDir)` (default git-status); a non-nil `derr` is **logged via the nil-safe `Log`** and treated as no artifacts — the step still succeeds (the agent produced its result).
6. return `core.Result{StepID: t.StepID, Summary: summary, Artifacts: arts, CostUSD: cost}`.

The runner is CLI-agnostic; all per-CLI knowledge lives in the `CLISpec`.

## 3. The `claude` invocation (`ClaudeSpec`)

Grounded in the current `claude` CLI (Claude Code headless mode):

- **Args:** `["-p", prompt, "--model", model, "--output-format", "json", "--permission-mode", "acceptEdits"]`. The prompt is a positional arg after `-p`; `model` is the alias (`opus`/`sonnet`). Runs against the cwd (`cmd.Dir`).
- **Output format = `--output-format json`** (a single result object), NOT `stream-json`. For B1's need (cost + summary) the single object is equivalent and far less fragile to parse; `stream-json` (JSON-lines, needs `--verbose`) only earns its keep when streaming live agent events to SSE — deferred. The `CLISpec.Parse` seam keeps that a later swap. (Deliberate, documented deviation from the parent spec's "stream-json" wording.)
- **Permission mode = `acceptEdits`** (auto-approves file edits + safe filesystem ops, not arbitrary bash) rather than `--dangerously-skip-permissions` (full bypass). The agent runs autonomously (no prompts) inside an isolated worktree on a loopback-only daemon; `acceptEdits` is the least-privilege default that still lets the agent write code.
- **Parse:** unmarshal the result object; read `total_cost_usd` (float) → `costUSD`, `result` (string) → `summary`. Treat the run as **failed** (return a non-nil error) when `is_error == true` OR `subtype != "success"` (e.g. `error_max_turns`, `error_during_execution`); include `subtype` + any `errors[]` in the message. Tolerate unknown/extra fields (forward-compatible JSON).

## 4. Artifact discovery

Default `Discover` runs **`git status --porcelain` in `WorkDir`** and turns each changed path into a `core.Artifact{StepID, Path: <abs path>}`. With Slice C's isolated worktree (empty base commit) this is exactly the set of files the agent created. For `shared` steps (the base tree accumulates changes across steps) it reports all uncommitted changes — imprecise but acceptable for v1 (shared steps are already documented as "may overlap"); a per-step baseline diff is a later refinement. `Discover` is an injectable field so the runner is testable without git, and so a future external-repo slice can swap in a `git diff <base>` discoverer. A discovery failure is non-fatal: log and return no artifacts (the step still succeeded). Git is available because the daemon wires `GitManager` (Slice C); a `CLIAgent` used without a git workdir simply discovers nothing.

## 5. Error handling & failure semantics

A failed agent is an **executor error**, which the engine treats as a retryable failure under the unified attempt budget (Slice A) and then aborts/escalates per `on_fail`. Failure cases, each surfaced as a clear error:

- `claude` not on PATH → `"agent binary %q not found"` (no retry helps, but the unified loop still applies; the run fails fast with a clear message).
- non-zero exit → error wrapping trimmed stderr (truncated to a sane cap).
- unparseable stdout → parse error (include a short prefix of stdout).
- agent-reported failure (`is_error` / non-success `subtype`) → error from `Parse`.
- timeout/cancel → `ctx` kills the subprocess (Slice A per-step timeout); `exec` returns the ctx error.

Partial edits from a failed/killed attempt are contained by the worktree and recreated fresh on retry/resume (Slice C's `freshWorktree`).

## 6. Secrets & trust boundary

`Env == nil ⇒ os.Environ()` passes `ANTHROPIC_API_KEY` (and any provider vars: Bedrock/Vertex/etc.) to the subprocess. Secrets live only in the daemon's environment — never in flow YAML, run state, args, or logs. The prompt comes from operator-controlled flow YAML (a trusted config boundary, same as `gate.Verifier.Command`); the agent runs with the daemon's privileges inside the worktree — the intended capability of an agent orchestrator. `exec.Command` uses an argument slice (no shell), annotated `#nosec G204` with the trust-boundary rationale (mirroring `gate/verifier.go` and `workspace/gitmanager.go`). The loopback-only bind (config default `127.0.0.1`) keeps the submit surface local.

## 7. Daemon wiring

A small constructor (e.g. `executor.Claude(model string) *CLIAgent`) builds a claude-backed agent; the daemon's registry becomes:

```go
Execs: map[string]core.Executor{
	"mock":   executor.Mock{Name: "mock"},
	"opus":   executor.Claude("opus"),
	"sonnet": executor.Claude("sonnet"),
},
```

**`mock` stays** — keyless flows and every existing engine/supervisor/e2e test keep working with no `claude`/key/network requirement. A flow that uses `opus`/`sonnet` now requires `claude` on PATH and `ANTHROPIC_API_KEY`; a flow using only `mock` requires neither. (B2 adds `gemini`/`codex`; a config-driven registry is a later refinement.)

## 8. Testing

The key constraint: **no API keys, no network in automated tests.** Achieved by splitting the pure parser from the subprocess runner.

- **Parser (pure, exhaustive)** — `ClaudeSpec.Parse` against captured-JSON fixtures in `testdata/`: a success object (assert `costUSD`, `summary`), an `is_error`/non-success-`subtype` object (assert error), malformed JSON (assert error), and an object with unknown extra fields (assert tolerated). No subprocess.
- **Runner (stub CLI)** — `CLIAgent.Run` with `Bin` pointed at a `testdata` stub script (`fake-claude`) that writes a file into cwd + prints a canned JSON result + exits 0; assert the parsed cost/summary is returned and the written file is discovered as an artifact (in a `git init`'d tempdir). A second stub exits non-zero → assert error (stderr surfaced). `Bin: "definitely-not-a-real-binary"` → assert the not-found error. `t.Skip` when `sh`/`git` are absent.
- **Discovery (focused)** — the default git-status discoverer over a tempdir with known changes → assert the changed paths. `t.Skip` without git.
- **Mock untouched** — all existing engine/supervisor/e2e tests stay green (they register `mock`); confirm the full `-race` suite still passes after wiring.
- **Manual / integration (not automated)** — documented: `ANTHROPIC_API_KEY=… cm run <flow-with-opus.yaml> --watch` against the daemon → a real agent edits the worktree, cost is recorded, the gate proceeds. The "real claude flow" proof, run by hand (like M3's runnable proof).

## 9. Files (rough)

- `internal/executor/cli.go` — `CLIAgent` + `CLISpec` + default git-status `Discover`.
- `internal/executor/claude.go` — `ClaudeSpec` (Args + Parse of the result object) + `Claude(model)` constructor.
- `internal/executor/cli_test.go`, `claude_test.go`, `testdata/` (fixtures + `fake-claude` stub).
- `cmd/magisterd/main.go` — register `opus`/`sonnet` (keep `mock`).

**No migration. No new YAML fields. No new dependencies.**

## 10. Conventions (carried from M0–M4 A & C)

- Commits: single conventional-commit subject, no body, no `Co-Authored-By`, never `--no-verify`; explicit git identity (M4 kickoff handoff §5).
- Deps: `go 1.22` must not move; no `go get` expected (`encoding/json`, `os/exec` are stdlib).
- RTK hook reformats `go`/`git`; use `rtk proxy` for raw PASS/FAIL.
- Semgrep hook: `os/exec` of an operator-controlled CLI is the intended capability — annotate `#nosec G204` with the rationale (precedent: `gate/verifier.go`, `workspace/gitmanager.go`).
- Engine invariants unchanged; the executor is a leaf adapter behind `core.Executor`.
