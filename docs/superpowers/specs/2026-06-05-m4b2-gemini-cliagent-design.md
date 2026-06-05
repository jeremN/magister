# M4 Slice B2 — Gemini CLIAgent (design)

**Status:** approved 2026-06-05
**Predecessors:** B1 (`claude` CLIAgent, merged) + B-stream (live `agent.tool` streaming, merged)
**Scope:** add a `gemini`-backed agent that streams `agent.tool` milestones, mirroring the claude path. Clear two deferred B1 nits. **codex is explicitly out of scope** (not installed on the dev machine → no real-CLI test, no manual proof; it becomes a later slice B3).

## 1. Why this fits the existing seam

B1 introduced `CLISpec` — the port that adapts one coding-agent CLI's *invocation* and *output schema* for the shared `CLIAgent` runner:

```go
type CLISpec interface {
	Args(model, prompt string) []string
	Parse(stdout io.Reader, emit func(event.Event)) (summary string, costUSD float64, err error)
}
```

Everything else in `CLIAgent.Run` is CLI-agnostic and **reused unchanged**: subprocess in the step `WorkDir`, `StdoutPipe` streaming, the `io.Copy` drain before `Wait`, the error precedence (`exec.ErrNotFound`→friendly / non-zero exit→stderr-wrapped / parse-error), nil-safe `Task.Emit`, and artifact discovery via `git status --porcelain`. **Gemini changes none of it.** Only `Args` (flags) and `Parse` (schema) differ — so B2 is a single new file `gemini.go` plus a daemon registry line, no interface or engine change.

## 2. The Gemini stream-json schema (captured live, gemini 0.41.1)

Verified by running `gemini -p "…" -m gemini-2.5-flash -o stream-json --approval-mode … --skip-trust` against the real CLI. **stdout is pure NDJSON**; the only noise (`Ripgrep is not available. Falling back to GrepTool.`) goes to **stderr**. Representative stdout:

```jsonc
{"type":"init","session_id":"…","model":"gemini-2.5-flash"}
{"type":"message","role":"user","content":"Create a file named report.txt …"}
{"type":"tool_use","tool_name":"update_topic","tool_id":"…","parameters":{"strategic_intent":"…"}}
{"type":"tool_use","tool_name":"write_file","tool_id":"…","parameters":{"file_path":"report.txt","content":"- Seven is a prime number.\n…"}}
{"type":"tool_result","tool_id":"…","status":"success","output":"…"}
{"type":"message","role":"assistant","content":"I created","delta":true}
{"type":"message","role":"assistant","content":" a file named `report.txt` … three bullet points","delta":true}
{"type":"message","role":"assistant","content":" about the number 7.","delta":true}
{"type":"result","status":"success","stats":{"total_tokens":12754,"input_tokens":12587,"output_tokens":92,"duration_ms":8386,"tool_calls":2,"models":{…}}}
```

How it differs from claude's stream-json (which is why each CLI needs its own `Parse`):

| concern | claude | gemini |
|---|---|---|
| tool_use location | nested in an `assistant` line's `message.content[]` block | **top-level** line, `type:"tool_use"` |
| tool name field | `name` (`Write`) | `tool_name` (`write_file`) |
| tool args field | `input` | `parameters` |
| final summary | `result.result` (one text field) | **concatenation** of `role:"assistant"` `message` lines; the `result` line has no text |
| assistant deltas | n/a | **incremental chunks** (`"I created"` + `" a file …"` + `" about the number 7."`) — must be appended in order, NOT "take last" |
| cost | `result.total_cost_usd` (USD) | **no USD** — only `stats` token counts |
| success flag | `result.subtype=="success"` / `is_error` | `result.status=="success"` |

## 3. `GeminiSpec.Args`

```
gemini -p <prompt> -m <model> -o stream-json --approval-mode yolo --skip-trust
```

- **`--skip-trust` is mandatory.** Without it, a headless run blocks on the interactive workspace-trust prompt (observed live).
- **`--approval-mode yolo`** auto-approves *all* tools, guaranteeing headless completion. The conservative analog of claude's `acceptEdits` is `auto_edit`, but it auto-approves only *edit* tools and would prompt (→ hang) on a non-edit tool like `run_shell_command` in headless mode. Agents already run in an isolated per-run git worktree (Slice C), so `yolo` is the right choice for autonomous orchestration.

## 4. `GeminiSpec.Parse`

`json.Decoder` over the NDJSON stream (no line-size cap; stdout is verified pure NDJSON, so decode errors are real errors, same strictness as `ClaudeSpec`). One pass, dispatch on top-level `type`:

- **`tool_use`** → `emit(event.Event{Kind: event.AgentTool, Summary: renderGeminiTool(line.ToolName, line.Parameters)})`, **except** `tool_name == "update_topic"` (Gemini's internal intent/UI tracker — not a real action; emitting it would clutter the live stream). 
- **`message`** with `role == "assistant"` → append `content` to a `strings.Builder` (delta concatenation).
- **`result`** → capture `status`.
- everything else (`init`, user `message`, `tool_result`) → ignored.

After the stream:
- no `result` line seen → `error` ("gemini output ended with no result"), mirroring claude.
- `status != "success"` → `error` ("gemini agent failed (status: <status>)").
- otherwise → return `(builder.String(), 0, nil)`.

**Cost is always `0` for Gemini** — the CLI reports token `stats` but no USD, and `core.Result.CostUSD` is the only cost field (threading token stats would be a `core.Result` schema change, out of scope). Documented on the function.

### `renderGeminiTool(toolName string, params json.RawMessage) string`

Mirrors claude's `renderTool` but reads Gemini's `parameters` field names. Salient field precedence: `file_path` (write_file/read_file/replace) → `command` (run_shell_command, truncated to 80) → `pattern` (search/grep) → bare `toolName`. Best-effort: unknown shapes render as the tool name alone. Produces e.g. `write_file: report.txt`, `run_shell_command: go test ./...`.

## 5. Daemon registration

`agents()` gains `"gemini": executor.Gemini("gemini-2.5-pro")` alongside `mock`/`opus`/`sonnet`. A flow step `agent: gemini` then resolves (needs `gemini` on PATH + its auth — OAuth cache or `GEMINI_API_KEY`, carried via the daemon's `os.Environ()` passthrough, exactly like claude's key). The bundled `flows/feature-flow.yaml`'s `gemini` step resolves as a side effect (the flow still needs the `merge` join + manual gates to run fully — those are later milestones).

## 6. Clear the two deferred B1 nits (same package)

- **(a) `ClaudeSpec.Parse` empty-failure message.** When `is_error` is true but `subtype` is `""` and `errors` is empty, the current message is `claude agent failed ()`. Fall back to a non-empty reason (e.g. `is_error`) so the message never has empty parens.
- **(b) `CLIAgent.Run` not-found asymmetry.** A bare `Bin` not on PATH yields the friendly `agent binary %q not found`, but an absolute path that doesn't exist falls through to the generic `start:` branch. Unify so both produce the friendly message (catch `fs.ErrNotExist` alongside `exec.ErrNotFound`), and add the currently-missing absolute-path test.

## 7. Testing

All automated tests run **without keys or network**, per B1:
- **Parser unit tests** (`gemini_test.go`) over inline NDJSON fixtures: delta concatenation builds the full summary; `update_topic` is skipped while `write_file` emits one milestone (`write_file: report.txt`); `run_shell_command` renders `command` truncated; cost is `0`; `status:"error"` → error; missing result line → error; `renderGeminiTool` field precedence.
- **`Args` test**: exact flag slice.
- **Runner test** via a `testdata/fake-gemini-stream` stub (chmod +x; emits the real Gemini NDJSON shape incl. a `write_file` tool_use and delta assistant messages, writes a file): asserts the milestone is forwarded through `CLIAgent.Run` via `Task.Emit` and the file is discovered as an artifact — mirrors `TestCLIAgentRunStreamsMilestones`.
- **B1-nit tests**: empty-`is_error` message is non-empty; absolute bogus `Bin` → friendly not-found.
- **Manual proof** (not automated): via the `running-the-orchestrator` skill with a one-step `agent: gemini` flow — watch a live `agent.tool` (`write_file: …`) frame over SSE.

## 8. Out of scope

codex adapter (→ B3, when installed); token→USD cost accounting; cross-CLI tool-name normalization (Gemini's `write_file` vs claude's `Write` stay native); multi-turn/session resume; `--include-partial-messages`-style token-delta streaming. No engine/SSE/store/schema/YAML change beyond the additive daemon registry line — the `agent.tool` Kind and `Task.Emit` seam from B-stream are reused as-is.

## 9. Done criteria

- `go test -race ./...` + `go vet ./...` clean; `go.mod` still `go 1.22`; no new dependency (stdlib only: `encoding/json`, `strings`, `io`).
- `GeminiSpec` invokes `gemini … -o stream-json --approval-mode yolo --skip-trust`; each non-`update_topic` `tool_use` emits one `agent.tool` milestone (`Summary` = `"<tool_name>: <param>"`); summary is the concatenated assistant deltas; cost is `0`; failures surface via `status` and the existing `CLIAgent` exit-code precedence.
- Daemon registers `gemini`; both B1 nits fixed with tests; manual proof shows a live Gemini milestone over SSE.
