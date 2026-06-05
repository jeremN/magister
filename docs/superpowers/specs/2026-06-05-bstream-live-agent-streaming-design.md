# Design — Live agent streaming (Slice B-stream): stream-json milestones → SSE

**Written:** 2026-06-05, after M4 Slice B1 (real `claude` `CLIAgent`) merged to `main`.
**Parent specs:** `docs/superpowers/specs/2026-06-04-m4b-cliagent-claude-design.md` (B1 — the `CLISpec`/`CLIAgent`/`ClaudeSpec` this evolves); `docs/superpowers/specs/2026-06-02-orchestrator-design.md` (§6 persist-then-publish, §9 SSE API). The parent orchestrator spec's "real CLIAgent + **stream-json** cost" line is finally realized here (B1 deliberately shipped `--output-format json` first and deferred streaming behind the `CLISpec` seam).
**Status:** approved (brainstorming), ready for `writing-plans`.
**Sequence:** A self-contained refinement of B1. Independent of B2 (`gemini`/`codex`) — those adapters mirror this seam later. Multi-turn/sessions is explicitly a **separate, later slice** (see §10).

## TL;DR

Today an `opus`/`sonnet` step is a black box to observers: the SSE journal shows `step.started`, then silence, then `step.done` — the operator can't see what the agent is *doing*. This slice switches `ClaudeSpec` from one-shot `--output-format json` to `--output-format stream-json --verbose` and surfaces **milestone** events (which tool the agent picked up — `"Edit src/foo.go"`, `"Bash: go test ./..."`) live, mid-step. Milestones are **persisted-then-published** exactly like step transitions, so they ride the **existing** SSE pipe with replay on reconnect. The executor emits via a new nil-safe `Emit func(event.Event)` callback on `core.Task`, bound by the engine to its existing `AppendEvents`+`Publish` rails. **No new SSE protocol, no store-schema change, no migration, no new flow-YAML field, no new dependency** (stdlib `encoding/json`). The engine's one-Task→one-Result model is unchanged. Tested with a stub CLI emitting canned NDJSON + a recording emit — no keys, no network; the real flow is a manual proof.

## 1. Scope

**In scope**

1. `ClaudeSpec.Args` → `--output-format stream-json --verbose` (drop `--output-format json`). `--permission-mode acceptEdits` unchanged.
2. Evolve the `CLISpec` output seam from a buffer-at-end parser to a **streaming** consumer that emits milestones as lines arrive and returns the final summary+cost.
3. `ClaudeSpec` milestone filter: complete `type:"assistant"` lines → one `agent.tool` event per `tool_use` content block; the final `type:"result"` line → summary+cost (same B1 `is_error`/`subtype` failure rule).
4. `CLIAgent.Run` consumes stdout as a stream (`json.Decoder`), forwards milestones to `Task.Emit`, preserves B1 error precedence.
5. One new `event.Kind`: `agent.tool`. Engine binds the per-attempt `Emit` closure (persist-then-publish, no step-state row).
6. Tests: stub-CLI NDJSON stream + inline-fixture stream parser + engine wiring, all key/network-free.

**Non-goals (deferred)**

- **Multi-turn / sessions** — conversation continuity across invocations (`--resume <session_id>` / `session_id`). A later slice; it touches the flow schema + step state model. The `result` object's `session_id` is noted as the future hook, not consumed now.
- **Token-level text deltas** — `--include-partial-messages` (the `stream_event` / `content_block_delta` firehose). Deliberately omitted: high volume, reassembly complexity, low value for a multi-agent run dashboard. The seam can add it later.
- **`agent.tool_done` (tool completion)** — v1 emits tool *start* only; `step.done` is the terminal. Completion is an easy follow-up.
- **`gemini`/`codex` streaming** — B2 adapters mirror this seam.
- **Tool-permission tuning** (`--allowedTools`) — orthogonal to streaming; `acceptEdits` posture inherited from B1 unchanged.
- Any SSE-protocol, store-schema, engine/join/gate API change beyond the additive `Task.Emit` field + the new `agent.tool` Kind.

## 2. Architecture & data flow

```
claude -p <prompt> --model <opus|sonnet> --output-format stream-json --verbose
       --permission-mode acceptEdits        (subprocess in the step's worktree)
   │  NDJSON on stdout (one JSON object per line)
   ▼
CLIAgent.Run → Spec.Parse(stdout io.Reader, emit func(event.Event))   ← streaming
   │   assistant line, content[] tool_use → emit(Event{Kind:"agent.tool", Summary:"Edit foo.go"})
   │   result line                        → return summary, cost (B1 is_error/subtype rule)
   ▼
t.Emit  (engine-bound closure, mirrors transition() minus the step-state row):
        ev.RunID/StepID/Attempt/At filled → Store.AppendEvents(store assigns Seq) → Bus.Publish (wakeup)
   ▼
sse.go drain() wakes, re-reads Store.EventsSince → client sees live "agent.tool" lines, replayable by Last-Event-ID
```

The runner stays CLI-agnostic; all stream-json knowledge lives in `ClaudeSpec`. The engine owns the durable+bus rails; the executor only calls a function it was handed.

## 3. The `claude` invocation & milestone schema (grounded against current docs, 2026-06)

Verified against `code.claude.com/docs/en/headless.md` + `…/agent-sdk/streaming-output.md`:

- **Args:** `["-p", prompt, "--model", model, "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits"]`. `stream-json` is **NDJSON** (one complete JSON object per line). `--verbose` yields the complete `assistant`/`user`/`result` message objects on stdout. We **omit** `--include-partial-messages` (that adds the token-delta `stream_event` objects — the deferred firehose).
- **Sequence:** `type:"system"` (subtype `init`) → `type:"assistant"` (complete) → [tool executes] → `type:"user"` (tool_result) → … → `type:"result"` (final).
- **Milestone source — `type:"assistant"`:** the message carries `.message.content[]`; for each block with `type=="tool_use"`, read `.name` (e.g. `Edit`, `Bash`, `Read`) and `.input` (object). Emit one `agent.tool` with `Summary` = a short render: tool name + the most salient input (`Edit`→`file_path`, `Bash`→a truncated `command`, `Read`→`file_path`, else name only). `system`/`user`/other lines are ignored in v1.
- **Final — `type:"result"`:** unmarshal the same fields B1 already uses — `subtype`, `is_error` (bool), `result` (string→summary), `total_cost_usd` (float→cost), `errors[]`. Apply B1's rule verbatim: failed when `is_error` OR (`subtype != "" && subtype != "success"`), message includes subtype + joined errors. Extra fields (`session_id`, `usage`, `num_turns`, `duration_ms`, `uuid`) are tolerated/ignored (`session_id` is the future multi-turn hook).
- **Parsing mechanism:** `json.Decoder` over the stdout reader — `for dec.More() { dec.Decode(&line) }`. This reads successive JSON values ignoring inter-object newlines (NDJSON-native) and imposes **no line-size cap**, so a multi-MB `tool_result` line cannot overflow it (a `bufio.Scanner` would, at its 64KB default — explicitly avoided). Each decoded `line` is a single struct with `Type`, an optional `Message.Content[]`, and the flat result fields; a `switch line.Type` routes it.
- **Auth/permission unchanged from B1:** `ANTHROPIC_API_KEY` via env passthrough; `acceptEdits` runs non-interactively (auto-approves edits + safe FS ops; no permission prompts in the stream).

## 4. The seam change: `CLISpec.Parse` becomes streaming

B1's `Parse(stdout []byte) (summary, cost, error)` parsed the whole buffer at the end. Streaming requires line-by-line with mid-stream emission. The seam evolves to a single streaming consumer (no dead one-shot method left behind):

```go
type CLISpec interface {
    Args(model, prompt string) []string
    // Parse consumes the agent's stdout stream, emitting milestone events via emit as
    // they arrive, and returns the final summary+cost. A non-nil error means the agent
    // ran but failed (is_error / non-success subtype / no result line) — distinct from a
    // process/exec failure, which CLIAgent surfaces. emit is never nil (CLIAgent passes a
    // no-op when nobody is listening), so implementations call it unconditionally.
    Parse(stdout io.Reader, emit func(event.Event)) (summary string, costUSD float64, err error)
}
```

`ClaudeSpec.Parse` emits only `Kind`+`Summary` on each milestone event; the engine's closure enriches `RunID`/`StepID`/`Attempt`/`At` and the store assigns `Seq`. The `executor` package may import `event` (it imports nothing internal); `core.Task.Emit func(event.Event)` is fine — `core` already imports `event`. **B1's four `ClaudeSpec.Parse` unit tests adapt** from `[]byte` to `bytes.NewReader(...)` + a recording `emit` — an in-scope refactor within this slice.

## 5. `CLIAgent.Run` streaming + error precedence

`Run` switches from "buffer stdout, then parse" to: `cmd.StdoutPipe()`, `cmd.Start()`, `summary, cost, perr := a.Spec.Parse(pipe, emitOrNoop)` (drains the pipe to EOF), then `cmd.Wait()`. stderr is still captured into a buffer. **Error precedence is preserved from B1, exactly:**

1. `exec.ErrNotFound` (from `Start`) → `"agent binary %q not found"`.
2. non-zero exit (from `Wait`) → `"%s: %w: %s"` wrapping trimmed stderr — **takes precedence over a parsed result** (a non-zero exit is a hard failure even if a `result` line was seen mid-stream).
3. else `perr` (parse / is_error / no-result-line) → returned.
4. else success → `Spec.Parse`'s summary+cost, then artifact discovery (`discoverGit`, non-fatal) + `StepID` attribution, unchanged from B1.

`emitOrNoop` = `t.Emit` if non-nil, else a no-op — so `Mock` and any non-streaming caller are untouched. The per-step timeout (Slice A) still wraps `ctx`; a hung/killed agent surfaces via the exit/`Wait` error as a retryable executor error (the milestones already emitted stay persisted).

## 6. New event Kind + engine wiring

One new `Kind`:

```go
AgentTool Kind = "agent.tool"   // a tool the agent picked up, mid-step; Summary = "<tool>: <input>"
```

It reuses the existing `event.Event` struct — `Summary` carries the human render, `StepID`/`Attempt`/`At` locate it. **No new struct field, no migration** (the events table stores `Kind` as free text; `EventsSince`/SSE are agnostic to the value). The engine adds an additive field to `core.Task`:

```go
type Task struct { /* …existing… */ ; Emit func(ev event.Event) }
```

and binds it per attempt where it builds the `Task` (in `execute`/`attempt`), mirroring `transition()` minus the step-state row:

```go
emit := func(ev event.Event) {
    ev.RunID, ev.StepID, ev.Attempt, ev.At = string(runID), s.ID, attemptNum, e.Clock.Now()
    if err := e.Store.AppendEvents(context.WithoutCancel(ctx), runID, []event.Event{ev}); err != nil {
        e.logger().Error("append agent event", "run", runID, "step", s.ID, "err", err)
        return
    }
    e.Bus.Publish(ev) // Seq is irrelevant on the bus — sse.go re-reads the store for real seqs
}
```

`AppendEvents` is the M3-added method (same monotonic seq source as `SaveStepTransition`); `context.WithoutCancel` matches `transition`'s durability choice. Concurrency is already safe: goroutine-per-step, the single SQLite writer handle serializes appends, `Bus.Publish` is mutex-guarded — **no new concurrency surface**. Milestone seqs land monotonically between the step's `step.started` and `step.done`; parallel steps interleave in global seq order and SSE filters/orders per-run by seq (unchanged).

## 7. Failure semantics

Unchanged from B1. A failed agent (missing binary / non-zero exit / parse error / `is_error` / no `result` line) is a retryable executor error under Slice A's unified attempt budget, then abort/escalate per `on_fail`. Milestones emitted before a failure stay persisted (they happened); on retry/resume the worktree is recreated fresh (Slice C) and a new attempt re-emits. A milestone `AppendEvents` failure is logged and dropped (best-effort, like a dropped bus frame) — it never fails the step.

## 8. Testing (no API keys, no network)

- **Stream parser (pure, exhaustive)** — `ClaudeSpec.Parse(bytes.NewReader(fixture), recordingEmit)` over inline NDJSON: a `system`+`assistant`(with a `tool_use`)+`result` stream → assert the `agent.tool` milestone(s) emitted (Kind + Summary render) **and** the final summary/cost; an `is_error`/non-success `result` line → error (and milestones still emitted before it); a stream with **no** `result` line → error; an oversized `tool_result` line (>64KB) → decodes fine (guards the `json.Decoder`-not-`bufio.Scanner` choice). Mutation-resistant assertions on the rendered Summary + parsed cost.
- **Runner (stub CLI)** — `testdata/fake-claude-stream` prints a canned multi-line NDJSON stream (incl. a `tool_use`) + exits 0 → `CLIAgent.Run` with a recording `Task.Emit` asserts the milestones were emitted **and** the final result + discovered artifacts returned. The B1 non-zero-exit stub still surfaces stderr and takes precedence over any parsed line. `t.Skip` when `sh`/`git` absent.
- **Engine wiring** — the bound `Emit` closure both persists (`AppendEvents` → visible to `EventsSince`) **and** publishes (bus wakeup); assert a milestone emitted during a step is retrievable from the store (so SSE can serve it). The existing SSE handler test is unchanged.
- **Mock & B1 untouched** — `Mock` (no `Emit` use) and all engine/supervisor/e2e tests stay green; the full `-race` suite passes.
- **Manual / integration (not automated)** — `ANTHROPIC_API_KEY=… cm run <opus-flow>.yaml --watch` → the journal shows live `agent.tool` lines as the agent edits files / runs tools, replayable via reconnect. The "real streaming" proof, run by hand.

## 9. Files (rough)

- `internal/executor/cli.go` — `CLISpec.Parse` signature → streaming; `CLIAgent.Run` → `StdoutPipe` + stream consume + error precedence.
- `internal/executor/claude.go` — `ClaudeSpec.Args` (+`stream-json`/`--verbose`); `ClaudeSpec.Parse` streaming `json.Decoder` loop + milestone filter + Summary render; the `streamLine` struct.
- `internal/executor/claude_test.go`, `cli_test.go`, `testdata/fake-claude-stream` (+ adapt B1 parser tests).
- `internal/event/event.go` — add `AgentTool` Kind.
- `internal/core/ports.go` — add `Task.Emit`.
- `internal/engine/engine.go` — bind the per-attempt `emit` closure into the `Task`.

**No migration. No new YAML fields. No new dependencies. No SSE-protocol change.**

## 10. Conventions (carried from M0–M4)

- Commits: single conventional-commit subject, no body, no `Co-Authored-By`, never `--no-verify`; explicit git identity.
- Deps: `go 1.22` must not move; stdlib only (`encoding/json`, `os/exec`, `bufio`/`io`).
- RTK hook reformats `go`/`git`; use `rtk proxy` for raw PASS/FAIL.
- Semgrep: the `os/exec` of `claude` keeps B1's `#nosec G204` + `// nosemgrep` annotations.
- Engine invariants unchanged; the executor stays a leaf adapter behind `core.Executor`, now with a one-way emit callback.
- **Plan-time note:** the stream-json schema (§3) was confirmed against current docs on 2026-06-05, but the implementation plan should open with a tiny spike (run the real `claude` once, capture a stream) to pin the exact `tool_use` input field names per tool before finalizing the Summary render — the only place the design depends on field-level detail.
