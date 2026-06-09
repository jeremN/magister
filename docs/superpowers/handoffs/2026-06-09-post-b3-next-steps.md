# Handoff — after M4 Slice B3 (2026-06-09)

**Pick up next.** This captures where `concentus-magister` stands after merging Slice B3 (codex CLIAgent) — the last M4 adapter — and the remaining work.

## TL;DR state

- `main` is at **`40f33c2`**, clean. `go test -race ./...` (144 tests) and `go vet ./...` are green; `go.mod` is `go 1.22`, no new deps.
- **M0–M3 done. M4 is now COMPLETE except the external-repo / git-native-merge handoff.** All three real CLI adapters (claude, gemini, codex) ship and stream live `agent.tool` milestones over SSE.
- This session shipped **Slice B3** (codex CLIAgent) end-to-end: installed the CLI → sampled its real schema → brainstorm → spec → plan → subagent-driven implementation (6 commits) → merged → **live SSE manual proof against the real `codex`**.

## What shipped this session

### Slice B3 (codex CLIAgent) — merged `40f33c2` (6 commits)
Installed `@openai/codex` (`codex-cli 0.138.0`, ChatGPT-account auth) and **sampled its real `--json` output live before writing the spec** (the rigor bar that kept B3 deferred until codex was installable).

- `d173d91` — `CodexSpec.Args` + `Codex(model)` constructor.
- `b3e4152` — `CodexSpec.Parse` (the codex JSONL parser) + decode structs + `renderCodexItem`. **Failure handling hardened in review:** a `failed` bool (set on `turn.failed`/top-level `error`, independent of message extraction, `"codex reported a failure"` fallback) that `turn.completed` resets — last-terminal-event-wins.
- `c2b51d5` — Parse failure-path tests.
- `eafab6f` — e2e test of the codex path through the shared runner (`testdata/fake-codex-stream` stub, committed `100755`).
- `83bb51a` — daemon `agents()` registers `"codex" → Codex("")`.
- `40f33c2` — fixed the now-stale `cli.go` `CLISpec` doc comment to list all three implementers.

**Key design points (all validated against the real binary):**
- `Args` = `codex exec --json -s workspace-write --skip-git-repo-check [-m <model>] <prompt>`. **Sandboxed-writes** posture (chosen over full `--dangerously-bypass-approvals-and-sandbox` yolo). `codex exec` is non-interactive and **rejects `-a`**, so no approval flag is passed. **`-m` is omitted when `model == ""`** — the one deliberate deviation from B1/B2's always-pass-`-m`, because codex validates `-m` against the account's allowed set (`gpt-5-codex` was rejected for the ChatGPT account) and resolves a working default when absent. The daemon registers `Codex("")` (account-default).
- codex's stream is a **third dialect**: an `item.started`/`item.completed` lifecycle envelope (vs claude content-blocks, vs gemini flat lines). Milestones emit on `command_execution`/`file_change` `item.started` (once each, no double-emit on completed); `agent_message` `item.completed` texts are concatenated newline-joined into the summary. **Cost is always 0** (codex reports token `usage`, no USD).
- `CLIAgent.Run` leaves stdin nil → `/dev/null`, so codex's `Reading additional input…` (stderr) is harmless. Zero engine/SSE/store change — the `CLISpec` + `Task.Emit` seam absorbed it, as designed.

Spec: `docs/superpowers/specs/2026-06-09-m4b3-codex-cliagent-design.md`. Plan: `docs/superpowers/plans/2026-06-09-m4b3-codex-cliagent.md`.

**Manual proof DONE** (the thing B2 deferred): a one-step `agent: codex` flow streamed a live `agent.tool` `file_change: add notes.txt` frame mid-step over real SSE — run succeeded, `notes.txt` written with the requested content, `Last-Event-ID` resume verified.

## Next work — pick one (each is its own brainstorm→spec→plan→subagent loop)

1. **External-repo + git-native handoff.** The last deferred M4 piece. Today handoff between steps is path-based and runs happen in a per-run scratch git repo (Slice C's `GitManager`). This slice lets a flow target an **external** repo and do git-native branch-from-deps / merge-at-join instead of path passing. Design-heavy — start with a brainstorm.

2. **M5 — expr conditional gates + `select`/`synthesize` joins.** The next milestone proper: `flow.Gate.Condition` (an `expr` the engine already has a field for) and real join strategies beyond the stubbed `merge`. Most user-visible feature value; the bundled `flows/feature-flow.yaml` needs the `merge` join to run fully.

My suggestion: **M5** — it unblocks the bundled flow and delivers the most visible value, and the external-repo handoff is the heaviest, least urgent piece.

## Open carried follow-ups (small, non-blocking)

- Dead `Server.BearerToken` / `ShutdownTimeout` fields + an auth footgun in the API server.
- Nil-safe `sse.go` logger.
- Canceled-step status handling.
- 2× shutdown-timeout interaction with a live SSE stream.
- *(Optional, additive)* codex parser parity tests beyond the current set (e.g. a malformed-JSON `Parse` error test, a multi-entry `file_change` render assertion) — structurally identical to tested claude/gemini code; the final reviewer ruled the current coverage accept-as-is.

## How to resume

- Read this file + the project memory (loaded automatically). The run skill (`/run` or the `running-the-orchestrator` skill) launches the app; a one-step `agent: codex` / `gemini` / `sonnet` flow proves a live agent over SSE.
- Conventions: specs/plans/handoffs commit to `main`; implementation on a branch via an isolated worktree (`git worktree add .worktrees/<name>` — native `EnterWorktree` is broken in this repo). Commit messages = single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. Build via brainstorm → writing-plans → subagent-driven-development (fresh implementer per task; spec + quality review each; opus for the riskiest unit and the final holistic review).
