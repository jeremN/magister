# M4 Slice B3 — Codex CLIAgent (design)

**Status:** approved 2026-06-09
**Predecessors:** B1 (`claude` CLIAgent, merged) + B-stream (live `agent.tool` streaming, merged) + B2 (`gemini` CLIAgent, merged)
**Scope:** add a `codex`-backed agent that streams `agent.tool` milestones, mirroring the claude/gemini path. This unblocks the last deferred M4 adapter: the OpenAI Codex CLI is now installed (`codex-cli 0.138.0`) and its output schema was sampled live, so the rigor bar (parser validated against the real binary) is met.

## 1. Why this fits the existing seam

B1 introduced `CLISpec` — the port that adapts one coding-agent CLI's *invocation* and *output schema* for the shared `CLIAgent` runner:

```go
type CLISpec interface {
	Args(model, prompt string) []string
	Parse(stdout io.Reader, emit func(event.Event)) (summary string, costUSD float64, err error)
}
```

Everything else in `CLIAgent.Run` is CLI-agnostic and **reused unchanged**: subprocess in the step `WorkDir`, `StdoutPipe` streaming, the `io.Copy` drain before `Wait`, the error precedence (`exec.ErrNotFound`/`os.ErrNotExist`→friendly / non-zero exit→stderr-wrapped / parse-error), nil-safe `Task.Emit`, and artifact discovery via `git status --porcelain`. **Codex changes none of it.** Only `Args` (flags) and `Parse` (schema) differ — so B3 is a single new file `codex.go` plus a daemon registry line, no interface or engine change.

One verified non-issue worth recording: `CLIAgent.Run` leaves `cmd.Stdin` nil, which Go wires to `/dev/null`. `codex exec` reads that as immediate EOF and proceeds; the `Reading additional input from stdin...` line it prints goes to **stderr** (surfaced only on failure). No stdin handling is needed. (A naive interactive invocation that leaves stdin attached to a TTY/pipe *will* hang on that read — but the runner never does.)

## 2. The Codex `--json` schema (captured live, codex-cli 0.138.0, ChatGPT-account auth)

Verified by running `codex exec --json -s workspace-write --skip-git-repo-check -C <dir> '<prompt>'` against the real CLI in a throwaway dir. **stdout is pure JSONL**; the `Reading additional input from stdin...` noise goes to **stderr**. Representative stdout (a shell-list + file-read turn, then a file-create turn):

```jsonc
{"type":"thread.started","thread_id":"019eadec-…"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"I'll inspect the current directory and then read `a.txt`."}}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/zsh -lc ls","aggregated_output":"","exit_code":null,"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"/bin/zsh -lc ls","aggregated_output":"a.txt\nb.txt\n","exit_code":0,"status":"completed"}}
{"type":"item.started","item":{"id":"item_2","type":"file_change","changes":[{"path":"/…/hello.txt","kind":"add"}],"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_2","type":"file_change","changes":[{"path":"/…/hello.txt","kind":"add"}],"status":"completed"}}
{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":23893,"cached_input_tokens":20736,"output_tokens":171,"reasoning_output_tokens":0}}
```

Failure path (captured live by forcing an invalid `-m`): the process **exits non-zero** and the stream carries

```jsonc
{"type":"error","message":"{\"type\":\"error\",\"status\":400,\"error\":{…\"message\":\"The '…' model is not supported…\"}}"}
{"type":"turn.failed","error":{"message":"{…same…}"}}
```

How it differs from claude's and gemini's streams (which is why each CLI needs its own `Parse`) — codex is a **third dialect**, an `item.*` envelope with a `started`→`completed` lifecycle rather than claude's content blocks or gemini's flat top-level lines:

| concern | claude | gemini | **codex** |
|---|---|---|---|
| event envelope | `assistant`/`result` lines, nested content blocks | flat top-level lines keyed by `type` | `item.started`/`item.completed` wrapping a typed `item`; plus `thread.*`/`turn.*` lifecycle |
| tool call signal | `tool_use` content block | top-level `type:"tool_use"` | `item` with `type:"command_execution"` or `"file_change"` |
| tool identity | `name` + `input` | `tool_name` + `parameters` | per-item payload: `command` (shell) / `changes:[{path,kind}]` (edit) |
| assistant text | `result.result` (one field) | concatenated `role:"assistant"` `message` **deltas** | one or more **complete** `agent_message` items (`text`) — a preamble *and* a final answer (the final is sometimes just `"done"`) |
| final/success | `result.subtype=="success"` / `is_error` | `result.status=="success"` | `turn.completed` present (no status field) |
| failure | `is_error` | `result.status!="success"` | `turn.failed` / top-level `error` (and non-zero exit) |
| cost | `result.total_cost_usd` (USD) | **no USD** — token `stats` only | **no USD** — `turn.completed.usage` token counts only |
| session id | `session_id` | `session_id` | `thread.started.thread_id` (unused; multi-turn deferred) |

## 3. `CodexSpec.Args`

```
codex exec --json -s workspace-write --skip-git-repo-check -m <model> <prompt>
```

- **`exec`** is codex's non-interactive subcommand (the prompt is a positional arg, not a `-p` flag).
- **`--json`** prints the JSONL event stream to stdout.
- **`-s workspace-write`** is the chosen sandbox posture (per design decision): codex sandboxes model-generated commands to writes within the workspace. `codex exec` is non-interactive and does **not** prompt for approval under this mode (verified headless: file created, exit 0, no hang) — so no approval flag is needed. Note `codex exec` **rejects** `-a/--ask-for-approval` (interactive-only; `exec` errors with "unexpected argument '-a'"), which is why approval is left at the exec default rather than set explicitly. The conceptual analog of gemini's `--approval-mode yolo` would be `--dangerously-bypass-approvals-and-sandbox`; we deliberately chose the **more conservative** sandboxed-writes posture since each step already runs in an isolated per-run git worktree (Slice C).
- **`--skip-git-repo-check`** lets codex run when the `WorkDir` is not a git repo (the codex analog of gemini's `--skip-trust` guard-skip); harmless when it is one. `CLIAgent.Run` already sets `cmd.Dir = WorkDir`, so no `-C` is passed.
- **`-m <model>` is omitted entirely when `model == ""`.** Codex validates `-m` against the account's allowed set (e.g. `gpt-5-codex` was rejected for the ChatGPT account at sample time) and resolves a working default when `-m` is absent (`codex doctor` reports `model <default>`). The daemon therefore registers codex with the empty/account-default model (§5). This is the one place B3 deviates from B2's always-pass-`-m`, justified by codex's account-gated model validation.

## 4. `CodexSpec.Parse`

`json.Decoder` over the JSONL stream (no line-size cap; stdout is verified pure JSONL, so decode errors are real errors, same strictness as `ClaudeSpec`/`GeminiSpec`). One pass, dispatch on top-level `type`:

- **`item.started`** → if `item.type ∈ {command_execution, file_change}`, `emit(event.Event{Kind: event.AgentTool, Summary: renderCodexItem(item)})`. Emitting on `started` (not `completed`) gives the earliest live signal and fires exactly once per tool item. Other item types on `started` are ignored.
- **`item.completed`** → if `item.type == agent_message`, append `item.text` to a `strings.Builder`, newline-joined (**concatenate all** agent messages, per design decision — robust against a terse final `"done"` whose substance lived in the preamble). Tool items on `completed` are ignored (already emitted on `started`); other types ignored.
- **`turn.completed`** → mark success seen.
- **`turn.failed`** → capture `error.message`. **`error`** (top-level) → capture `message`.
- everything else (`thread.started`, `turn.started`) → ignored.

After the stream:
- a `turn.failed`/`error` message was captured → `error` ("codex agent failed: <message>").
- no `turn.completed` and no failure seen → `error` ("codex output ended with no turn.completed"), mirroring gemini's no-result guard.
- otherwise → return `(builder.String(), 0, nil)`.

`turn.failed`/`error` detection is defensive belt-and-suspenders: codex also exits non-zero on these, and `CLIAgent.Run`'s exit-code precedence already surfaces the failure with stderr. The `Parse` check matters only if codex ever emits a failure with a zero exit, and it produces a cleaner message either way. **Cost is always `0`** — codex reports token `usage` but no USD, and `core.Result.CostUSD` is the only cost field (threading token stats would be a `core.Result` schema change, out of scope). Documented on the function.

### `renderCodexItem(item) string`

Best-effort short human label, dispatched on `item.type`:
- **`command_execution`** → `"command_execution: " + truncate(command, 80)` (the `command` is the shell invocation, e.g. `/bin/zsh -lc ls`).
- **`file_change`** → `"file_change: " + kind + " " + path` of the first entry in `changes` (e.g. `file_change: add hello.txt`).
- unknown/empty → bare `item.type` (forward-compatible fallback, mirroring gemini's bare-name fallback).

Produces e.g. `command_execution: /bin/zsh -lc ls`, `file_change: add hello.txt`.

## 5. Daemon registration

`agents()` gains `"codex": executor.Codex("")` alongside `mock`/`opus`/`sonnet`/`gemini`, with the comment updated (drop the "codex arrives in a later slice" note). The empty model means `Args` omits `-m` and codex resolves its account default. A flow step `agent: codex` then resolves (needs `codex` on PATH + a codex login — ChatGPT OAuth cache or `OPENAI_API_KEY`, carried via the daemon's `os.Environ()` passthrough, exactly like claude's/gemini's auth).

## 6. Testing

All automated tests run **without keys or network**, per B1/B2:
- **Parser unit tests** (`codex_test.go`) over inline JSONL fixtures: multiple `agent_message` items concatenate (preamble + `"done"`) newline-joined into the summary; a `command_execution` `item.started` emits one milestone (`command_execution: …`, truncated) and its `item.completed` does **not** double-emit; a `file_change` `item.started` emits `file_change: add <path>`; cost is `0`; a `turn.failed` line → error; a stream with no `turn.completed` → error; `renderCodexItem` fallback to bare type for an unknown item.
- **`Args` test**: exact flag slice for a non-empty model, and the `-m`-omitted slice for `model == ""`.
- **Runner test** via a `testdata/fake-codex-stream` stub (chmod +x, `sh`; emits the real codex envelope incl. a `file_change` started/completed and two `agent_message` items, writes a file): asserts the milestone is forwarded through `CLIAgent.Run` via `Task.Emit` and the file is discovered as an artifact — mirrors `TestCLIAgentRunStreamsGeminiMilestones`, added as `TestCLIAgentRunStreamsCodexMilestones` in `cli_test.go`.
- **Manual proof** (not automated): via the `running-the-orchestrator` skill with a one-step `agent: codex` flow — watch a live `agent.tool` (`command_execution: …` / `file_change: …`) frame over SSE.

## 7. Out of scope

token→USD cost accounting; cross-CLI tool-name normalization (codex's `command_execution`/`file_change` stay native, not mapped to claude's `Bash`/`Write`); multi-turn / `thread_id` resume; token-delta streaming; rendering codex item types beyond `command_execution`/`file_change` (e.g. `mcp_tool_call`, web search, reasoning) — added as they're observed against the real binary. No engine/SSE/store/schema/YAML change beyond the additive daemon registry line — the `agent.tool` Kind and `Task.Emit` seam from B-stream are reused as-is.

## 8. Done criteria

- `go test -race ./...` + `go vet ./...` clean; `go.mod` still `go 1.22`; no new dependency (stdlib only: `encoding/json`, `strings`, `io`).
- `CodexSpec` invokes `codex exec --json -s workspace-write --skip-git-repo-check [-m <model>] <prompt>`; each `command_execution`/`file_change` `item.started` emits one `agent.tool` milestone (`Summary` = `"<item.type>: <salient>"`); summary is the concatenated `agent_message` texts; cost is `0`; failures surface via `turn.failed`/`error` and the existing `CLIAgent` exit-code precedence.
- Daemon registers `codex` (account-default model); manual proof shows a live codex milestone over SSE.
