# Codex CLIAgent (M4 Slice B3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `codex`-backed coding agent that streams `agent.tool` milestones over the existing SSE seam, mirroring the merged claude (B1) and gemini (B2) adapters.

**Architecture:** Implement the existing `CLISpec` port in a single new file `internal/executor/codex.go` — `CodexSpec.Args` (codex's `exec --json` flags) + `CodexSpec.Parse` (codex's `item.*` JSONL dialect) + a `renderCodexItem` label helper + a `Codex(model)` constructor. `CLIAgent.Run` and the engine/SSE/store layers are reused unchanged. Register `codex` in the daemon's `agents()` map. Validated live against `codex-cli 0.138.0`.

**Tech Stack:** Go 1.22, stdlib only (`encoding/json`, `strings`, `io`, `path/filepath`). No new dependencies. TDD throughout; tests run with no keys and no network.

**Spec:** `docs/superpowers/specs/2026-06-09-m4b3-codex-cliagent-design.md`

---

## File Structure

- **Create** `internal/executor/codex.go` — `CodexSpec` (`Args`, `Parse`), the `codexEvent`/`codexItem`/`codexChange`/`codexError` decode structs, `renderCodexItem`, and the `Codex(model)` constructor. Mirrors `gemini.go`.
- **Create** `internal/executor/codex_test.go` — unit tests for `Args` (with and without model), `Parse` (concatenation + milestones + cost), `Parse` failure paths, and `renderCodexItem` fallback. Mirrors `gemini_test.go`.
- **Create** `internal/executor/testdata/fake-codex-stream` — an executable `sh` stub emitting the real codex JSONL envelope + writing a file. Mirrors `testdata/fake-gemini-stream`.
- **Modify** `internal/executor/cli_test.go` — add `TestCLIAgentRunStreamsCodexMilestones`, mirroring `TestCLIAgentRunStreamsGeminiMilestones`.
- **Modify** `cmd/magisterd/main.go:47-54` — register `"codex": executor.Codex("")` in `agents()` and update the comment.

**Reused unchanged (do not edit):** `internal/executor/cli.go` (`CLISpec`, `CLIAgent.Run`), `internal/event`, the engine, SSE, and store. The shared helpers `truncate(b []byte, n int) string` (`claude.go:94`), `noEmit` (`claude_test.go:13`), `initGitRepo(t)` (`discover_test.go:11`), and `stubPath(t, name)` (`cli_test.go:15`) already exist — reference them, do not redefine.

---

## Task 0: Branch + isolated worktree

**Files:** none (git setup only).

- [ ] **Step 1: Create a feature branch in an isolated worktree**

Native `EnterWorktree` is broken in this repo (per project convention) — use git directly. From the repo root:

```bash
git worktree add .worktrees/m4b3-codex -b m4b3-codex
cd .worktrees/m4b3-codex
```

- [ ] **Step 2: Confirm a clean green baseline before any change**

Run: `go test -race ./... && go vet ./...`
Expected: all packages `ok` / no vet output. (If `go test ./internal/executor/` skips git-dependent tests because `git` is absent, that is acceptable — but `git` is present in this environment.)

---

## Task 1: `Codex(model)` constructor + `CodexSpec.Args`

**Files:**
- Create: `internal/executor/codex.go`
- Create (this task's tests): `internal/executor/codex_test.go`

- [ ] **Step 1: Write the failing Args tests**

Create `internal/executor/codex_test.go` (only `slices`/`testing` are needed now; Task 2 adds `bytes` and `concentus/internal/event` when the Parse tests arrive):

```go
package executor

import (
	"slices"
	"testing"
)

func TestCodexSpecArgs(t *testing.T) {
	got := CodexSpec{}.Args("gpt-5-codex", "do the thing")
	want := []string{"exec", "--json", "-s", "workspace-write", "--skip-git-repo-check", "-m", "gpt-5-codex", "do the thing"}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %v, want %v", got, want)
	}
}

func TestCodexSpecArgsOmitsEmptyModel(t *testing.T) {
	got := CodexSpec{}.Args("", "do the thing")
	want := []string{"exec", "--json", "-s", "workspace-write", "--skip-git-repo-check", "do the thing"}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %v, want %v (no -m when model empty)", got, want)
	}
}
```

(The `bytes`/`event` imports are unused until Task 2; if `go vet`/compile complains about unused imports at this step, temporarily drop them and re-add in Task 2. They are listed now so the file's final import block is correct.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/executor/ -run TestCodexSpecArgs -v`
Expected: FAIL — `undefined: CodexSpec`.

- [ ] **Step 3: Create `codex.go` with the constructor and Args**

Create `internal/executor/codex.go`:

`Args` and the constructor need no imports (only builtins and in-package types), so this file compiles on its own. The full import block and the `var _ CLISpec = CodexSpec{}` interface assertion are added in Task 2 once `Parse` exists.

```go
package executor

// CodexSpec runs the OpenAI `codex` CLI headless (`codex exec --json`) and parses its
// JSONL event stream. Codex wraps tool calls in item.started/item.completed lifecycle
// events (a third dialect, distinct from claude's content blocks and gemini's flat
// lines): command_execution and file_change items become agent.tool milestones; the
// one-or-more agent_message items are concatenated into the summary. Codex reports
// token usage but no USD, so cost is always 0.
type CodexSpec struct{}

// Args builds `codex exec --json -s workspace-write --skip-git-repo-check [-m <model>]
// <prompt>`. The sandbox is workspace-write (codex exec is non-interactive and does not
// prompt under it); -m is omitted when model is empty so codex resolves its
// account-default model (an explicit model is validated by codex against the account's
// allowed set). The prompt is a positional arg, not a flag.
func (CodexSpec) Args(model, prompt string) []string {
	args := []string{"exec", "--json", "-s", "workspace-write", "--skip-git-repo-check"}
	if model != "" {
		args = append(args, "-m", model)
	}
	return append(args, prompt)
}

// Codex returns a CLIAgent backed by the `codex` CLI. An empty model uses codex's
// account-default. Env defaults to os.Environ() (carries codex's login / OPENAI_API_KEY).
func Codex(model string) *CLIAgent {
	return &CLIAgent{Bin: "codex", Model: model, Spec: CodexSpec{}}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/executor/ -run TestCodexSpecArgs -v`
Expected: PASS (both `TestCodexSpecArgs` and `TestCodexSpecArgsOmitsEmptyModel`).

- [ ] **Step 5: Commit**

```bash
git add internal/executor/codex.go internal/executor/codex_test.go
git commit -m "feat(executor): add CodexSpec.Args + Codex constructor"
```

---

## Task 2: `CodexSpec.Parse` happy path + `renderCodexItem`

**Files:**
- Modify: `internal/executor/codex.go`
- Modify: `internal/executor/codex_test.go`

- [ ] **Step 1: Write the failing Parse + render tests**

First extend the import block of `internal/executor/codex_test.go` to add `"bytes"` and `"concentus/internal/event"` (final block: `bytes`, `slices`, `testing`, `concentus/internal/event`). Then append:

```go
func TestCodexSpecParseConcatenatesMessagesAndEmitsMilestones(t *testing.T) {
	stream := `{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"I'll create hello.txt."}}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/zsh -lc ls","status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"/bin/zsh -lc ls","aggregated_output":"hello.txt\n","exit_code":0,"status":"completed"}}
{"type":"item.started","item":{"id":"item_2","type":"file_change","changes":[{"path":"/tmp/x/hello.txt","kind":"add"}],"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_2","type":"file_change","changes":[{"path":"/tmp/x/hello.txt","kind":"add"}],"status":"completed"}}
{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}`
	var got []event.Event
	emit := func(e event.Event) { got = append(got, e) }
	summary, cost, err := CodexSpec{}.Parse(bytes.NewReader([]byte(stream)), emit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if summary != "I'll create hello.txt.\ndone" {
		t.Errorf("summary = %q, want %q (messages concatenated newline-joined)", summary, "I'll create hello.txt.\ndone")
	}
	if cost != 0 {
		t.Errorf("cost = %v, want 0 (codex reports no USD)", cost)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 milestones (one per tool item.started, no double-emit on completed), got %d: %+v", len(got), got)
	}
	if got[0].Kind != event.AgentTool || got[0].Summary != "command_execution: /bin/zsh -lc ls" {
		t.Errorf("milestone[0] = %+v, want agent.tool \"command_execution: /bin/zsh -lc ls\"", got[0])
	}
	if got[1].Kind != event.AgentTool || got[1].Summary != "file_change: add hello.txt" {
		t.Errorf("milestone[1] = %+v, want agent.tool \"file_change: add hello.txt\"", got[1])
	}
}

func TestRenderCodexItemFallsBackToType(t *testing.T) {
	if got := renderCodexItem(&codexItem{Type: "web_search"}); got != "web_search" {
		t.Errorf("renderCodexItem = %q, want bare \"web_search\"", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/executor/ -run 'TestCodexSpecParse|TestRenderCodexItem' -v`
Expected: FAIL — `CodexSpec has no field or method Parse` / `undefined: renderCodexItem` / `undefined: codexItem`.

- [ ] **Step 3: Implement Parse, the decode structs, and renderCodexItem**

First add the import block and the interface assertion to `internal/executor/codex.go` — directly under `package executor`, insert:

```go
import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"concentus/internal/event"
)

var _ CLISpec = CodexSpec{}
```

Then append the structs, `Parse`, and `renderCodexItem`:

```go
// codexEvent is one top-level JSONL object from `codex exec --json`. Fields are the
// union read across event types; unknown fields/types are ignored (forward-compatible).
// The item.* lifecycle nests a codexItem; turn.failed nests an error; a top-level error
// event carries message directly.
type codexEvent struct {
	Type    string      `json:"type"`
	Item    *codexItem  `json:"item"`
	Message string      `json:"message"` // top-level "error" event
	Error   *codexError `json:"error"`   // "turn.failed" event
}

type codexItem struct {
	Type    string        `json:"type"`
	Text    string        `json:"text"`    // agent_message
	Command string        `json:"command"` // command_execution
	Changes []codexChange `json:"changes"` // file_change
}

type codexChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type codexError struct {
	Message string `json:"message"`
}

// Parse reads codex's stdout as a stream of JSONL events (json.Decoder imposes no
// line-size cap). It emits one agent.tool milestone per command_execution/file_change
// item.started (the earliest live signal; item.completed for tool items does not
// re-emit), concatenates the agent_message texts (newline-joined) into the summary, and
// fails on a turn.failed/error event or a stream that never reaches turn.completed. cost
// is always 0 — codex reports token usage but no USD. emit is never nil.
func (CodexSpec) Parse(stdout io.Reader, emit func(event.Event)) (string, float64, error) {
	dec := json.NewDecoder(stdout)
	var summary strings.Builder
	sawCompleted := false
	failMsg := ""
	for dec.More() {
		var ev codexEvent
		if err := dec.Decode(&ev); err != nil {
			return "", 0, fmt.Errorf("parse codex output: %w", err)
		}
		switch ev.Type {
		case "item.started":
			if ev.Item != nil && (ev.Item.Type == "command_execution" || ev.Item.Type == "file_change") {
				emit(event.Event{Kind: event.AgentTool, Summary: renderCodexItem(ev.Item)})
			}
		case "item.completed":
			if ev.Item != nil && ev.Item.Type == "agent_message" {
				if summary.Len() > 0 {
					summary.WriteByte('\n')
				}
				summary.WriteString(ev.Item.Text)
			}
		case "turn.completed":
			sawCompleted = true
		case "turn.failed":
			if ev.Error != nil {
				failMsg = ev.Error.Message
			}
		case "error":
			failMsg = ev.Message
		}
	}
	if failMsg != "" {
		return "", 0, fmt.Errorf("codex agent failed: %s", failMsg)
	}
	if !sawCompleted {
		return "", 0, fmt.Errorf("codex output ended with no turn.completed")
	}
	return summary.String(), 0, nil
}

// renderCodexItem produces a short human label for a tool item:
// "command_execution: <command truncated to 80>" or "file_change: <kind> <basename>".
// Unknown/empty item types render as the bare type (forward-compatible fallback).
func renderCodexItem(item *codexItem) string {
	switch item.Type {
	case "command_execution":
		return "command_execution: " + truncate([]byte(item.Command), 80)
	case "file_change":
		if len(item.Changes) > 0 {
			return "file_change: " + item.Changes[0].Kind + " " + filepath.Base(item.Changes[0].Path)
		}
		return "file_change"
	default:
		return item.Type
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/executor/ -run 'TestCodexSpecParse|TestRenderCodexItem|TestCodexSpecArgs' -v`
Expected: PASS for all four.

- [ ] **Step 5: Commit**

```bash
git add internal/executor/codex.go internal/executor/codex_test.go
git commit -m "feat(executor): add CodexSpec.Parse stream-json parser"
```

---

## Task 3: `CodexSpec.Parse` failure handling

**Files:**
- Modify: `internal/executor/codex_test.go`

(The Parse implementation from Task 2 already handles failures; these tests lock that behavior in. If a test fails, fix Parse in `codex.go`.)

- [ ] **Step 1: Write the failing failure-path tests**

Append to `internal/executor/codex_test.go`:

```go
func TestCodexSpecParseTurnFailed(t *testing.T) {
	stream := `{"type":"turn.started"}
{"type":"error","message":"bad model"}
{"type":"turn.failed","error":{"message":"bad model"}}`
	if _, _, err := (CodexSpec{}).Parse(bytes.NewReader([]byte(stream)), noEmit); err == nil {
		t.Fatal("expected an error when the stream reports turn.failed")
	}
}

func TestCodexSpecParseNoTurnCompleted(t *testing.T) {
	stream := `{"type":"item.completed","item":{"type":"agent_message","text":"hi"}}`
	if _, _, err := (CodexSpec{}).Parse(bytes.NewReader([]byte(stream)), noEmit); err == nil {
		t.Fatal("expected an error when the stream has no turn.completed")
	}
}
```

- [ ] **Step 2: Run the tests**

Run: `go test ./internal/executor/ -run TestCodexSpecParse -v`
Expected: PASS (Parse from Task 2 already returns errors for both cases). If either fails, correct the failure logic in `codex.go` Parse, then re-run.

- [ ] **Step 3: Commit**

```bash
git add internal/executor/codex_test.go
git commit -m "test(executor): cover codex Parse failure paths"
```

---

## Task 4: End-to-end runner test + `fake-codex-stream` stub

**Files:**
- Create: `internal/executor/testdata/fake-codex-stream`
- Modify: `internal/executor/cli_test.go`

- [ ] **Step 1: Write the failing e2e test**

Append to `internal/executor/cli_test.go` (the file already imports `context`, `filepath`, `testing`, `concentus/internal/core`, `concentus/internal/event`):

```go
func TestCLIAgentRunStreamsCodexMilestones(t *testing.T) {
	dir := initGitRepo(t) // from discover_test.go; skips if git absent
	var got []event.Event
	a := &CLIAgent{Bin: stubPath(t, "fake-codex-stream"), Model: "", Spec: CodexSpec{}}
	res, err := a.Run(context.Background(), core.Task{
		StepID: "s1", Prompt: "go", WorkDir: dir,
		Emit: func(e event.Event) { got = append(got, e) },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Summary != "Creating out.txt.\ndone" || res.CostUSD != 0 {
		t.Errorf("summary=%q cost=%v, want \"Creating out.txt.\\ndone\"/0", res.Summary, res.CostUSD)
	}
	if len(got) != 1 || got[0].Kind != event.AgentTool || got[0].Summary != "file_change: add out.txt" {
		t.Fatalf("milestones = %+v, want one agent.tool \"file_change: add out.txt\"", got)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "s1" ||
		filepath.Base(res.Artifacts[0].Path) != "out.txt" {
		t.Errorf("artifacts = %+v, want one out.txt attributed to s1", res.Artifacts)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/executor/ -run TestCLIAgentRunStreamsCodexMilestones -v`
Expected: FAIL — `stub fake-codex-stream missing` (the stub doesn't exist yet).

- [ ] **Step 3: Create the stub**

Create `internal/executor/testdata/fake-codex-stream`:

```sh
#!/bin/sh
# Fake `codex exec --json`: write a file (simulating a file_change), print the codex
# JSONL envelope (thread.started, turn.started, an agent_message preamble, a file_change
# started+completed pair, an agent_message final, turn.completed), exit 0. Used by the
# CLIAgent codex streaming test — no API key, no network.
echo "stub codex wrote this" > out.txt
printf '%s\n' '{"type":"thread.started","thread_id":"t1"}'
printf '%s\n' '{"type":"turn.started"}'
printf '%s\n' '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"Creating out.txt."}}'
printf '%s\n' '{"type":"item.started","item":{"id":"item_1","type":"file_change","changes":[{"path":"out.txt","kind":"add"}],"status":"in_progress"}}'
printf '%s\n' '{"type":"item.completed","item":{"id":"item_1","type":"file_change","changes":[{"path":"out.txt","kind":"add"}],"status":"completed"}}'
printf '%s\n' '{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"done"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":2}}'
```

- [ ] **Step 4: Make the stub executable**

Run: `chmod +x internal/executor/testdata/fake-codex-stream`
Expected: no output. (`stubPath` fails the test if the stub is not `+x`.)

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/executor/ -run TestCLIAgentRunStreamsCodexMilestones -v`
Expected: PASS. (Summary is the two concatenated `agent_message` texts; one `file_change` milestone with the basename; `out.txt` discovered as an artifact via `git status --porcelain` in the git repo.)

- [ ] **Step 6: Commit**

```bash
git add internal/executor/cli_test.go internal/executor/testdata/fake-codex-stream
git commit -m "test(executor): cover codex milestone streaming via CLIAgent"
```

---

## Task 5: Register the codex agent in the daemon

**Files:**
- Modify: `cmd/magisterd/main.go:47-54`

- [ ] **Step 1: Update the `agents()` registry and its comment**

In `cmd/magisterd/main.go`, replace the `agents` doc comment + function body (currently lines ~44-54) with:

```go
// agents is the daemon's executor registry: the keyless mock, claude-backed
// opus/sonnet, gemini-backed gemini, and codex-backed codex. A flow using opus/sonnet
// needs `claude` on PATH + ANTHROPIC_API_KEY, gemini needs `gemini` on PATH + its auth,
// codex needs `codex` on PATH + a codex login (ChatGPT OAuth or OPENAI_API_KEY); mock
// flows need none. codex("") uses codex's account-default model.
func agents() map[string]core.Executor {
	return map[string]core.Executor{
		"mock":   executor.Mock{Name: "mock"},
		"opus":   executor.Claude("opus"),
		"sonnet": executor.Claude("sonnet"),
		"gemini": executor.Gemini("gemini-2.5-pro"),
		"codex":  executor.Codex(""),
	}
}
```

- [ ] **Step 2: Verify the daemon still builds and its tests pass**

Run: `go build ./cmd/magisterd/ && go test ./cmd/magisterd/ -v`
Expected: build succeeds; daemon tests PASS. (If a test asserts the exact set of registered agent keys, update it to include `"codex"`.)

- [ ] **Step 3: Commit**

```bash
git add cmd/magisterd/main.go
git commit -m "feat(magisterd): register codex-backed agent"
```

---

## Task 6: Full verification + manual proof

**Files:** none (verification only).

- [ ] **Step 1: Full race suite + vet**

Run: `go test -race ./... && go vet ./...`
Expected: every package `ok`; no vet output. For raw (untruncated) test output use `rtk proxy go test -race ./internal/executor/`.

- [ ] **Step 2: Confirm no new dependency and Go version unchanged**

Run: `git diff main -- go.mod go.sum`
Expected: empty diff (still `go 1.22`, stdlib only).

- [ ] **Step 3: Manual proof against the real codex CLI (closes the slice)**

Use the `running-the-orchestrator` skill (`.claude/skills/running-the-orchestrator/SKILL.md`) to build `magisterd`+`cm`, start the daemon on a throwaway db/port (with the Claude Code sandbox disabled so the codex child reaches the network), run a one-step flow whose single step is `agent: codex` (e.g. a prompt like "create a file notes.txt with one line, then say done"), and `cm run --watch`. Confirm a live `agent.tool` frame (`command_execution: …` or `file_change: …`) streams over SSE and the run reaches a terminal success. Capture the observed milestone line in the handoff.

- [ ] **Step 4: Final state confirmation**

Run: `git log --oneline main..HEAD`
Expected: the five feature/test commits from Tasks 1–5, in order. The branch is ready to merge to `main` (or open a PR), per the project's finishing-a-development-branch flow.

---

## Self-Review

**Spec coverage (spec §→task):**
- §3 Args (`exec --json -s workspace-write --skip-git-repo-check [-m] <prompt>`, empty-model omits `-m`) → Task 1.
- §4 Parse: emit on `item.started` for `command_execution`/`file_change`; concatenate `agent_message`; cost 0 → Task 2. Failure (`turn.failed`/`error`/no `turn.completed`) → Task 3.
- §4 `renderCodexItem` (command trunc-80; file_change kind+basename; bare-type fallback) → Task 2.
- §5 daemon registration (`codex` → `Codex("")`, comment) → Task 5.
- §6 testing: parser unit tests → Tasks 2–3; Args test (both forms) → Task 1; runner test + `fake-codex-stream` stub → Task 4; manual proof → Task 6 Step 3.
- §8 done criteria: race+vet clean / `go 1.22` / no new dep → Task 6 Steps 1–2; milestone+summary+cost behavior → Tasks 2/4; daemon registers codex → Task 5.

**Placeholder scan:** No TBD/TODO; every code and command step shows complete content. Imports are staged deliberately: Task 1's `codex.go`/`codex_test.go` compile with a minimal import set, and Task 2 adds the full blocks (`encoding/json`/`fmt`/`io`/`path/filepath`/`strings`/`event` in `codex.go`; `bytes`/`event` in the test) alongside the code that uses them, so each task compiles green on its own.

**Type consistency:** `CodexSpec`, `codexEvent`, `codexItem` (`Type`/`Text`/`Command`/`Changes`), `codexChange` (`Path`/`Kind`), `codexError` (`Message`), `renderCodexItem(*codexItem) string`, and `Codex(model) *CLIAgent` are used identically across Tasks 1, 2, 3, 4. Milestone strings (`"command_execution: …"`, `"file_change: add out.txt"`) and the concatenated summary (`"…\ndone"`) match between the unit test (Task 2), the stub + e2e test (Task 4), and `renderCodexItem`/`Parse` (Task 2 impl). Helper signatures (`truncate`, `noEmit`, `initGitRepo`, `stubPath`) match the verified existing definitions.
