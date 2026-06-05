# Handoff — after M4 Slice B2 (2026-06-05)

**Pick up next week.** This captures where `concentus-magister` stands after merging Slice B2 (gemini CLIAgent), and the menu of next work with the prerequisites/gotchas for each.

## TL;DR state

- `main` is at **`2425b3e`**, clean. `go test -race ./...` and `go vet ./...` are green; `go.mod` is `go 1.22`, no new deps.
- **M0–M3 are done. M4 is complete except B3 (codex) and the external-repo / git-native-merge handoff.** Then M5.
- This session shipped: **Slice B-stream** (live `agent.tool` streaming for claude) and **Slice B2** (gemini CLIAgent + cleared two B1 nits), plus a committed run-the-app project skill.

## What shipped this session

### Slice B-stream (live agent streaming) — merged `5f36df8`
`claude` now runs in `--output-format stream-json --verbose`; `CLISpec.Parse(stdout io.Reader, emit func(event.Event)) (summary, costUSD, err)` streams NDJSON via `json.Decoder`; each `tool_use` emits a persisted `agent.tool` milestone through a nil-safe `core.Task.Emit`, which the engine binds to a persist-then-publish closure (`AppendEvents`→`Bus.Publish`, `context.WithoutCancel`), so milestones stream over the **existing** SSE with `Last-Event-ID` replay. `CLIAgent.Run` streams via `StdoutPipe` + an `io.Copy` drain before `Wait` (deadlock-pinned by a regression test). **Verified live** end-to-end against the real `claude`.

### Slice B2 (gemini CLIAgent) — merged `2425b3e` (5 commits)
- `cde284b` — fix: claude failure errors never render empty parens.
- `5b2243c` — fix: an absent **absolute** agent path reports the friendly "not found" (catches `os.ErrNotExist`). *(These two were the deferred B1 nits — now cleared.)*
- `466d1cc` — `GeminiSpec` (`internal/executor/gemini.go`): implements the **same** `CLISpec` with zero engine/SSE change. `Args` = `gemini -p <prompt> -m <model> -o stream-json --approval-mode yolo --skip-trust`. `Parse` handles Gemini's NDJSON, whose schema **differs from claude's** (verified live, gemini 0.41.1): top-level `tool_use` lines (`tool_name`/`parameters`, snake_case) → one `agent.tool` milestone each via `renderGeminiTool` (param precedence `file_path`/`command`(trunc 80)/`pattern`), **skipping** gemini's internal `update_topic`; `role:"assistant"` `message` deltas are **concatenated** into the summary (deltas are incremental, not cumulative); the `result` line carries `status` and **no USD → cost is always 0**.
- `60f515b` — e2e test of the gemini path through the shared runner (`testdata/fake-gemini-stream` stub).
- `2425b3e` — daemon `agents()` registers `"gemini" → Gemini("gemini-2.5-pro")`.

Spec: `docs/superpowers/specs/2026-06-05-m4b2-gemini-cliagent-design.md`. Plan: `docs/superpowers/plans/2026-06-05-m4b2-gemini-cliagent.md`.

**One thing NOT done for B2:** the live manual proof (running a real `gemini` flow over SSE). The automated e2e test covers the wiring; the live proof is trivial via the run skill but was skipped for time. Worth doing as a 2-minute warm-up next week.

### Run-the-app skill — `.claude/skills/running-the-orchestrator/SKILL.md` (`ed3ceb2`)
A verified recipe (build `magisterd`+`cm` → start daemon on a throwaway db/port → minimal one-step flow → `cm run --watch` → verify SSE/persistence/`Last-Event-ID` → SIGTERM teardown), auto-discovered by the `run` skill. **Gotcha captured there:** inside the Claude Code sandbox, launch the daemon with the sandbox disabled so its CLI child reaches the network; gemini needs `--skip-trust` or a headless run hangs on the workspace-trust prompt.

## Next work — pick one (each is its own brainstorm→spec→plan→subagent loop)

1. **B3 — codex adapter.** Mirrors B1/B2: a `CodexSpec` (`internal/executor/codex.go`) implementing `CLISpec`, plus daemon registration. **Blocked on a prerequisite:** the OpenAI Codex CLI is **not installed** on this machine, so we can't sample its real output schema, test against it, or do a manual proof — which breaks the rigor bar we've held (every CLI slice was validated against the real binary). **Do first:** `npm i -g @openai/codex` (or equivalent), then `codex exec --help` / sample its `--json` output to learn its tool-event + result schema before writing the spec. Codex's `codex exec --json` emits its own event stream; the parser will differ again (this is exactly what `CLISpec` is for).

2. **External-repo + git-native handoff.** The larger deferred M4 piece. Today handoff between steps is path-based and runs happen in a per-run scratch git repo (Slice C's `GitManager`). This slice would let a flow target an **external** repo and do git-native branch-from-deps / merge-at-join instead of path passing. Design-heavy — start with a brainstorm.

3. **M5 — expr conditional gates + `select`/`synthesize` joins.** The next milestone proper: `flow.Gate.Condition` (an `expr` the engine already has a field for) and real join strategies beyond the stubbed `merge`. Skips ahead of remaining M4 adapter work.

My suggestion: **B3 if you can install codex** (keeps Slice B symmetric and is low-risk now that the pattern is proven twice), otherwise **M5** (most user-visible feature value). The external-repo handoff is the heaviest and least urgent.

## Open carried follow-ups (small, non-blocking)

- Dead `Server.BearerToken` / `ShutdownTimeout` fields + an auth footgun in the API server.
- Nil-safe `sse.go` logger.
- Canceled-step status handling.
- 2× shutdown-timeout interaction with a live SSE stream.
- *(Optional, additive)* gemini parser parity tests: a malformed-JSON `Parse` error test + a `renderGeminiTool` `pattern`-branch assertion (claude has the analogues). The final reviewer ruled these accept-as-is — structurally identical to tested claude code.

## How to resume

- Read this file + the project memory (loaded automatically). The run skill (`/run` or the `running-the-orchestrator` skill) launches the app.
- Conventions: specs/plans/handoffs commit to `main`; implementation on a branch via an isolated worktree (`git worktree add .worktrees/<name>` — native `EnterWorktree` is broken in this repo). Commit messages = single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. Use `rtk proxy go test …` for raw test output. Build via brainstorm → writing-plans → subagent-driven-development (fresh implementer per task; spec + quality review each; opus for the riskiest unit and the final holistic review).
