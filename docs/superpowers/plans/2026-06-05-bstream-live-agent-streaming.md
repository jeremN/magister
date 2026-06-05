# Slice B-stream: Live agent streaming (stream-json milestones → SSE) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Switch `ClaudeSpec` to `--output-format stream-json` and surface live "milestone" events (which tool the agent picked up) mid-step, persisted-then-published through the existing SSE pipe — so an operator watching a run sees `agent.tool: Edit src/foo.go` lines as the agent works, instead of silence between `step.started` and `step.done`.

**Architecture:** Evolve the B1 `CLISpec` output seam from a buffer-at-end parser (`Parse([]byte)`) to a streaming consumer (`Parse(io.Reader, emit func(event.Event))`). `CLIAgent.Run` consumes stdout as a stream (`StdoutPipe` + `json.Decoder`), forwarding milestones to a nil-safe `Emit` callback on `core.Task` that the engine binds to its existing `AppendEvents`+`Publish` rails (same shape as `transition()`, minus the step-state row). Milestones are persisted (so SSE, which reads the durable events table, can replay them). One new `event.Kind` (`agent.tool`). No SSE-protocol, store-schema, migration, flow-YAML, or dependency change.

**Tech Stack:** Go 1.22, stdlib `encoding/json` (`json.Decoder` streaming — NDJSON-native, no 64KB line cap) + `os/exec` + `io`. The `claude` CLI (Claude Code) headless stream-json mode. Tests: `testing`, a `testdata` stub CLI emitting canned NDJSON, inline-fixture parser tests — no API keys, no network. The real streaming flow is a manual proof.

**Spec:** `docs/superpowers/specs/2026-06-05-bstream-live-agent-streaming-design.md`

**Risky unit (flag for opus review):** Task 2 (`CLIAgent.Run` streaming lifecycle — `StdoutPipe`/`Start`/drain/`Wait`, error precedence). Task 3 (NDJSON milestone parsing) is moderate; Tasks 1, 4 are low-risk.

**Commit convention (user CLAUDE.md):** single conventional-commit subject, NO body, NO `Co-Authored-By`, never `--no-verify`. Commit with the explicit identity:
```bash
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "<subject>"
```
**Raw test output:** the RTK hook reformats `go test`; use `rtk proxy go test ...` for per-test PASS/FAIL. **Semgrep:** the existing `#nosec G204` + `// nosemgrep` on the `exec` line stay (operator-controlled CLI, no shell).

---

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/event/event.go` | event Kinds | modify (add `AgentTool`) — T1 |
| `internal/core/ports.go` | `Task` type | modify (add `Emit` field) — T1 |
| `internal/executor/cli.go` | `CLISpec` port + `CLIAgent` runner | modify (streaming `Parse` sig; `Run` → `StdoutPipe` stream) — T2 |
| `internal/executor/claude.go` | `ClaudeSpec` (args + stream parse) | modify (`streamLine`; streaming `Parse`; T3: args flip + milestones) — T2/T3 |
| `internal/executor/claude_test.go` | parser tests | modify (adapt to reader+emit — T2; milestone tests — T3) |
| `internal/executor/cli_test.go` | runner tests | modify (streaming stub + emit assert — T3) |
| `internal/executor/testdata/fake-claude-stream` | streaming stub CLI | create — T3 |
| `internal/engine/engine.go` | `execute` builds the emit closure | modify — T4 |
| `internal/engine/engine_test.go` | engine wiring test | modify (add milestone-forwarding test) — T4 |

---

## Task 1: Scaffolding — `agent.tool` Kind + `Task.Emit` field

Additive, compiles green on its own (no behavior yet). No new test of its own — it's exercised by T3/T4; this task just lands the two declarations so later tasks reference real symbols.

**Files:**
- Modify: `internal/event/event.go`
- Modify: `internal/core/ports.go`

- [ ] **Step 1: Add the `AgentTool` Kind.** In `internal/event/event.go`, change the `const` block:

```go
const (
	RunStarted   Kind = "run.started"
	RunDone      Kind = "run.done"
	StepStarted  Kind = "step.started"
	StepDone     Kind = "step.done"
	StepFailed   Kind = "step.failed"
	StepRetrying Kind = "step.retrying"
	GateAwaiting Kind = "gate.awaiting"
)
```

to (add the last line):

```go
const (
	RunStarted   Kind = "run.started"
	RunDone      Kind = "run.done"
	StepStarted  Kind = "step.started"
	StepDone     Kind = "step.done"
	StepFailed   Kind = "step.failed"
	StepRetrying Kind = "step.retrying"
	GateAwaiting Kind = "gate.awaiting"
	AgentTool    Kind = "agent.tool" // a tool the agent picked up, mid-step (Summary = "<tool>: <input>")
)
```

- [ ] **Step 2: Add the `Emit` field to `core.Task`.** In `internal/core/ports.go`, change the `Task` struct:

```go
// Task is what the engine hands an executor for one step attempt.
type Task struct {
	RunID   RunID
	StepID  string
	Role    string
	Prompt  string
	Inputs  []Artifact
	WorkDir string
}
```

to (add the `Emit` field + doc):

```go
// Task is what the engine hands an executor for one step attempt.
type Task struct {
	RunID   RunID
	StepID  string
	Role    string
	Prompt  string
	Inputs  []Artifact
	WorkDir string
	// Emit publishes a mid-step milestone event (e.g. agent.tool). The engine binds
	// it to persist-then-publish; nil for callers that don't stream (e.g. Mock, or a
	// non-engine test) — executors must nil-check or use a no-op wrapper.
	Emit func(ev event.Event)
}
```

- [ ] **Step 3: Verify it compiles.**

Run: `rtk proxy go build ./... && rtk proxy go test ./internal/event/ ./internal/core/ 2>&1 | tail -5`
Expected: builds clean; event/core packages still pass (`core` has no test files — that's fine; `go build` proves the field type-checks and `event` already imports nothing new).

Note: `core/ports.go` already imports `concentus/internal/event` (for `Publisher`), so `event.Event` resolves with no new import.

- [ ] **Step 4: Commit.**

```bash
git add internal/event/event.go internal/core/ports.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(core): add agent.tool event kind + Task.Emit milestone callback"
```

---

## Task 2: Migrate the `CLISpec` seam to streaming (behavior-preserving)

Change `CLISpec.Parse([]byte)` → `Parse(io.Reader, emit func(event.Event))` and rewrite `ClaudeSpec.Parse` to consume the stream via `json.Decoder`, **still in `--output-format json` mode** (Args unchanged), handling only the `result` line — so behavior is identical to B1 (no milestones yet). `CLIAgent.Run` switches to `StdoutPipe` streaming with B1's error precedence preserved. This isolates the risky subprocess-lifecycle refactor and proves it against the existing B1 stubs.

**Files:**
- Modify: `internal/executor/cli.go` (`CLISpec` interface + `CLIAgent.Run`)
- Modify: `internal/executor/claude.go` (`streamLine` + streaming `Parse`; drop `claudeResult`)
- Modify: `internal/executor/claude_test.go` (adapt the 4 parser tests to reader+emit)

- [ ] **Step 1: Adapt the parser tests first (they will fail to compile).** Replace `internal/executor/claude_test.go` ENTIRELY with:

```go
package executor

import (
	"bytes"
	"slices"
	"testing"

	"concentus/internal/event"
)

// noEmit is a no-op milestone sink for parser tests that don't assert emissions.
func noEmit(event.Event) {}

func TestClaudeSpecArgs(t *testing.T) {
	got := ClaudeSpec{}.Args("opus", "do the thing")
	want := []string{"-p", "do the thing", "--model", "opus", "--output-format", "json", "--permission-mode", "acceptEdits"}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %v, want %v", got, want)
	}
}

func TestClaudeSpecParseSuccess(t *testing.T) {
	out := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"all done","total_cost_usd":0.0123,"usage":{"input_tokens":5}}`)
	summary, cost, err := ClaudeSpec{}.Parse(bytes.NewReader(out), noEmit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if summary != "all done" {
		t.Errorf("summary = %q, want %q", summary, "all done")
	}
	if cost != 0.0123 {
		t.Errorf("cost = %v, want 0.0123", cost)
	}
}

func TestClaudeSpecParseErrorResult(t *testing.T) {
	out := []byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"","total_cost_usd":0.5,"errors":["hit max turns"]}`)
	if _, _, err := (ClaudeSpec{}).Parse(bytes.NewReader(out), noEmit); err == nil {
		t.Fatal("expected error for is_error/non-success result")
	}
}

func TestClaudeSpecParseMalformed(t *testing.T) {
	if _, _, err := (ClaudeSpec{}).Parse(bytes.NewReader([]byte("not json at all")), noEmit); err == nil {
		t.Fatal("expected a parse error for non-JSON output")
	}
}
```

- [ ] **Step 2: Run the tests — expect a COMPILE failure** (the new `Parse` signature doesn't exist yet).

Run: `rtk proxy go test ./internal/executor/ -run TestClaudeSpec 2>&1 | tail -5`
Expected: build failure — `too many arguments in call to ClaudeSpec{}.Parse` / signature mismatch.

- [ ] **Step 3: Change the `CLISpec` interface.** In `internal/executor/cli.go`, change the imports to add `"concentus/internal/event"` and change the interface's `Parse` method. The import block becomes:

```go
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"

	"concentus/internal/core"
	"concentus/internal/event"
)
```

and the interface becomes:

```go
// CLISpec adapts one coding-agent CLI's invocation and output schema for CLIAgent.
// ClaudeSpec implements it now; CodexSpec/GeminiSpec arrive in a later slice. Parse
// consumes the CLI's stdout stream, emitting milestone events via emit as they
// arrive, and returns the final summary+cost. A non-nil Parse error means the agent
// ran but failed (e.g. is_error / non-success subtype / no result) — distinct from a
// process/exec failure, which CLIAgent surfaces itself. emit is never nil.
type CLISpec interface {
	Args(model, prompt string) []string
	Parse(stdout io.Reader, emit func(event.Event)) (summary string, costUSD float64, err error)
}
```

- [ ] **Step 4: Rewrite `ClaudeSpec.Parse` to stream (result-only, json mode).** In `internal/executor/claude.go`, change the imports to:

```go
import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"concentus/internal/event"
)
```

Replace the `claudeResult` struct AND the `Parse` method (lines for `claudeResult` + `Parse`) with `streamLine` + the streaming `Parse`:

```go
// streamLine is one NDJSON object from the `claude` CLI's output. Fields are the
// union of what we read across line types; unknown fields/types are ignored
// (forward-compatible). For type=="result", the flat result fields apply; the
// Message.Content blocks (type=="assistant") carry tool_use milestones (used from
// the next task on).
type streamLine struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
	Subtype      string   `json:"subtype"`
	IsError      bool     `json:"is_error"`
	Result       string   `json:"result"`
	TotalCostUSD float64  `json:"total_cost_usd"`
	Errors       []string `json:"errors"`
}

// Parse reads the claude CLI's stdout as a stream of NDJSON objects (json.Decoder
// imposes no line-size cap, so a multi-MB tool_result line cannot overflow it),
// returning the final result object's summary+cost. emit is reserved for milestone
// events (wired in from the next task); a missing result line is an error.
func (ClaudeSpec) Parse(stdout io.Reader, emit func(event.Event)) (string, float64, error) {
	dec := json.NewDecoder(stdout)
	var result *streamLine
	for dec.More() {
		var line streamLine
		if err := dec.Decode(&line); err != nil {
			return "", 0, fmt.Errorf("parse claude output: %w", err)
		}
		if line.Type == "result" {
			r := line
			result = &r
		}
	}
	if result == nil {
		return "", 0, fmt.Errorf("claude output ended with no result")
	}
	if result.IsError || (result.Subtype != "" && result.Subtype != "success") {
		msg := result.Subtype
		if len(result.Errors) > 0 {
			msg += ": " + strings.Join(result.Errors, "; ")
		}
		return "", 0, fmt.Errorf("claude agent failed (%s)", msg)
	}
	return result.Result, result.TotalCostUSD, nil
}
```

(Leave `Args`, `truncate`, and `Claude()` in `claude.go` unchanged. `emit` is intentionally unused in this task — it's part of the new signature, lit up in Task 3. Go does not flag an unused function parameter, so this compiles.)

- [ ] **Step 5: Rewrite `CLIAgent.Run` to stream stdout.** In `internal/executor/cli.go`, replace the entire `Run` method body with the streaming version (note: `exec.ErrNotFound` now comes from `Start`, not `Run`; non-zero exit comes from `Wait` and takes precedence over a parse error — preserving B1 semantics):

```go
func (a *CLIAgent) Run(ctx context.Context, t core.Task) (core.Result, error) {
	// #nosec G204 -- Bin + args are operator-controlled (daemon registry + flow YAML);
	// no shell. Running a coding-agent CLI is the intended capability.
	cmd := exec.CommandContext(ctx, a.Bin, a.Spec.Args(a.Model, t.Prompt)...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Dir = t.WorkDir
	if a.Env != nil {
		cmd.Env = a.Env
	} else {
		cmd.Env = os.Environ()
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return core.Result{}, fmt.Errorf("%s: stdout pipe: %w", a.Bin, err)
	}

	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return core.Result{}, fmt.Errorf("agent binary %q not found", a.Bin)
		}
		return core.Result{}, fmt.Errorf("%s: start: %w", a.Bin, err)
	}

	emit := t.Emit
	if emit == nil {
		emit = func(event.Event) {}
	}
	summary, cost, perr := a.Spec.Parse(stdout, emit)
	_, _ = io.Copy(io.Discard, stdout) // drain any unread tail so Wait can't block

	if werr := cmd.Wait(); werr != nil {
		// A non-zero exit (or kill via ctx) is a hard failure even if a result line
		// was parsed mid-stream — it takes precedence, surfacing trimmed stderr.
		return core.Result{}, fmt.Errorf("%s: %w: %s", a.Bin, werr, truncate(stderr.Bytes(), 500))
	}
	if perr != nil {
		return core.Result{}, perr
	}

	discover := a.Discover
	if discover == nil {
		discover = discoverGit
	}
	arts, derr := discover(t.WorkDir)
	if derr != nil {
		a.logger().Warn("artifact discovery failed", "step", t.StepID, "err", derr)
		arts = nil
	}
	for i := range arts {
		arts[i].StepID = t.StepID
	}
	return core.Result{StepID: t.StepID, Summary: summary, Artifacts: arts, CostUSD: cost}, nil
}
```

- [ ] **Step 6: Run the executor tests — all must pass (behavior unchanged).**

Run: `rtk proxy go test ./internal/executor/ -v`
Expected: PASS for the 4 `TestClaudeSpec*` (now reader-based) AND the 4 `TestCLIAgent*` runner tests (unchanged — `fake-claude-ok` still prints a single `type:"result"` line, `fake-claude-fail` still exits non-zero with stderr "boom", and not-found still triggers via `Start`). No test edits to `cli_test.go` needed in this task.
Run: `rtk proxy go test -race ./internal/executor/` → ok. `go vet ./internal/executor/` → clean.

- [ ] **Step 7: Confirm no dependency drift.** Run: `git diff -- go.mod go.sum` → empty. `grep '^go ' go.mod` → `go 1.22`.

- [ ] **Step 8: Commit.**

```bash
git add internal/executor/cli.go internal/executor/claude.go internal/executor/claude_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "refactor(executor): stream CLISpec.Parse + CLIAgent.Run via StdoutPipe"
```

---

## Task 3: Enable stream-json + `agent.tool` milestones

Flip `ClaudeSpec.Args` to `--output-format stream-json --verbose` and light up milestone emission: each complete `type:"assistant"` line's `tool_use` content blocks → one `agent.tool` event. Add the `Summary` render. New tests prove milestone emission, the no-result-line error, and the large-line (no-64KB-cap) property; a streaming stub proves end-to-end emission through `CLIAgent.Run`.

**Files:**
- Modify: `internal/executor/claude.go` (`Args`; add the `case "assistant"` + `renderTool`)
- Modify: `internal/executor/claude_test.go` (update `TestClaudeSpecArgs`; add milestone/no-result/large-line tests)
- Create: `internal/executor/testdata/fake-claude-stream`
- Modify: `internal/executor/cli_test.go` (add a streaming-stub emit-assertion test)

- [ ] **Step 1: Write the failing milestone tests.** In `internal/executor/claude_test.go`, update the `want` in `TestClaudeSpecArgs` and append three tests. First change `TestClaudeSpecArgs`'s `want` line to:

```go
	want := []string{"-p", "do the thing", "--model", "opus", "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits"}
```

Then add `"strings"` to the test file's imports and append:

```go
func TestClaudeSpecParseEmitsToolMilestones(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"working"},{"type":"tool_use","name":"Edit","input":{"file_path":"src/foo.go"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.04}`
	var got []event.Event
	emit := func(e event.Event) { got = append(got, e) }
	summary, cost, err := ClaudeSpec{}.Parse(bytes.NewReader([]byte(stream)), emit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if summary != "done" || cost != 0.04 {
		t.Errorf("summary=%q cost=%v, want done/0.04", summary, cost)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 tool milestones, got %d: %+v", len(got), got)
	}
	if got[0].Kind != event.AgentTool || got[0].Summary != "Edit: src/foo.go" {
		t.Errorf("milestone[0] = %+v, want agent.tool \"Edit: src/foo.go\"", got[0])
	}
	if got[1].Kind != event.AgentTool || got[1].Summary != "Bash: go test ./..." {
		t.Errorf("milestone[1] = %+v, want agent.tool \"Bash: go test ./...\"", got[1])
	}
}

func TestClaudeSpecParseNoResultLine(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"x"}}]}}`
	if _, _, err := (ClaudeSpec{}).Parse(bytes.NewReader([]byte(stream)), noEmit); err == nil {
		t.Fatal("expected error when the stream has no result line")
	}
}

func TestClaudeSpecParseLargeToolResultLine(t *testing.T) {
	big := strings.Repeat("x", 200_000) // > bufio.Scanner's 64KB default token cap
	stream := `{"type":"user","message":{"content":[{"type":"tool_result","content":"` + big + `"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"ok","total_cost_usd":0.01}`
	summary, _, err := ClaudeSpec{}.Parse(bytes.NewReader([]byte(stream)), noEmit)
	if err != nil {
		t.Fatalf("a >64KB NDJSON line must decode (json.Decoder has no line cap), got: %v", err)
	}
	if summary != "ok" {
		t.Errorf("summary = %q, want ok", summary)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail.**

Run: `rtk proxy go test ./internal/executor/ -run TestClaudeSpec -v`
Expected: FAIL — `TestClaudeSpecArgs` fails (Args still `json`), and `TestClaudeSpecParseEmitsToolMilestones` fails (`got` is empty — the `assistant` case isn't handled yet).

- [ ] **Step 3: Flip `Args` to stream-json.** In `internal/executor/claude.go`, change `Args` to:

```go
func (ClaudeSpec) Args(model, prompt string) []string {
	return []string{
		"-p", prompt,
		"--model", model,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "acceptEdits",
	}
}
```

Also update the `ClaudeSpec` doc comment to reflect streaming:

```go
// ClaudeSpec runs the `claude` CLI (Claude Code) headless in `--output-format
// stream-json` mode and parses its NDJSON stream: tool_use blocks become agent.tool
// milestones; the final result object yields summary+cost.
type ClaudeSpec struct{}
```

- [ ] **Step 4: Emit milestones from `assistant` lines + add `renderTool`.** In `internal/executor/claude.go`, in the `Parse` loop, add a `case` for `"assistant"` so the switch reads:

```go
		switch line.Type {
		case "assistant":
			for _, b := range line.Message.Content {
				if b.Type == "tool_use" {
					emit(event.Event{Kind: event.AgentTool, Summary: renderTool(b.Name, b.Input)})
				}
			}
		case "result":
			r := line
			result = &r
		}
```

(Replace the existing `if line.Type == "result" { ... }` with this `switch`.) Then add the `renderTool` helper at the end of `claude.go`:

```go
// renderTool produces a short human label for a tool_use block: "<name>: <salient
// input>" (file path / command / pattern), or just "<name>" when no known input
// field is present. Best-effort: unknown input shapes render as the name alone.
func renderTool(name string, input json.RawMessage) string {
	var in struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Command  string `json:"command"`
		Pattern  string `json:"pattern"`
	}
	_ = json.Unmarshal(input, &in)
	switch {
	case in.FilePath != "":
		return name + ": " + in.FilePath
	case in.Path != "":
		return name + ": " + in.Path
	case in.Command != "":
		return name + ": " + truncate([]byte(in.Command), 80)
	case in.Pattern != "":
		return name + ": " + in.Pattern
	default:
		return name
	}
}
```

- [ ] **Step 5: Run the parser tests to verify they pass.**

Run: `rtk proxy go test ./internal/executor/ -run TestClaudeSpec -v`
Expected: PASS for all `TestClaudeSpec*` (Args now stream-json; milestones emitted; no-result errors; large line decodes). `go vet ./internal/executor/` → clean.

- [ ] **Step 6: Create the streaming stub CLI.** Create `internal/executor/testdata/fake-claude-stream` with EXACTLY:

```sh
#!/bin/sh
# Fake `claude` in stream-json mode: write a file (simulating an edit), print an
# NDJSON stream with one tool_use milestone + a final result, exit 0. Used by the
# CLIAgent streaming test — no API key, no network.
echo "stub wrote this" > out.txt
printf '%s\n' '{"type":"system","subtype":"init"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"out.txt"}}]}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"stream done","total_cost_usd":0.03}'
```

Make it executable (git preserves the +x bit):
```bash
chmod +x internal/executor/testdata/fake-claude-stream
```

- [ ] **Step 7: Add the streaming runner test.** In `internal/executor/cli_test.go`, add `"concentus/internal/event"` to the imports and append:

```go
func TestCLIAgentRunStreamsMilestones(t *testing.T) {
	dir := initGitRepo(t) // from discover_test.go; skips if git absent
	var got []event.Event
	a := &CLIAgent{Bin: stubPath(t, "fake-claude-stream"), Model: "opus", Spec: ClaudeSpec{}}
	res, err := a.Run(context.Background(), core.Task{
		StepID: "s1", Prompt: "go", WorkDir: dir,
		Emit: func(e event.Event) { got = append(got, e) },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Summary != "stream done" || res.CostUSD != 0.03 {
		t.Errorf("summary=%q cost=%v, want \"stream done\"/0.03", res.Summary, res.CostUSD)
	}
	if len(got) != 1 || got[0].Kind != event.AgentTool || got[0].Summary != "Edit: out.txt" {
		t.Fatalf("milestones = %+v, want one agent.tool \"Edit: out.txt\"", got)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "s1" ||
		filepath.Base(res.Artifacts[0].Path) != "out.txt" {
		t.Errorf("artifacts = %+v, want one out.txt attributed to s1", res.Artifacts)
	}
}
```

- [ ] **Step 8: Run the executor tests + race + confirm stub mode.**

Run: `rtk proxy go test ./internal/executor/ -v`
Expected: PASS for all `TestClaudeSpec*` and all `TestCLIAgent*` (incl. the new streaming test). The pre-existing `fake-claude-ok` single-`result`-line stub still drives `TestCLIAgentRunSuccess` green (one `result` line is valid NDJSON).
Run: `rtk proxy go test -race ./internal/executor/` → ok. `go vet ./internal/executor/` → clean.
Run: `git ls-files -s internal/executor/testdata/fake-claude-stream` (after `git add`) — mode must be `100755`. (If it shows `100644`, the test fails on a fresh checkout — re-`chmod +x` and `git add` again.)

- [ ] **Step 9: Commit.**

```bash
git add internal/executor/claude.go internal/executor/claude_test.go internal/executor/cli_test.go internal/executor/testdata/fake-claude-stream
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(executor): emit agent.tool milestones from claude stream-json"
```

---

## Task 4: Wire the engine's per-attempt `Emit` closure

The engine binds `Task.Emit` to a closure that persists-then-publishes each milestone (mirroring `transition()` minus the step-state row), so milestones land in the durable events table (where SSE reads them) and wake the bus. A new engine test proves a milestone emitted during a step is both persisted and published.

**Files:**
- Modify: `internal/engine/engine.go` (`execute` signature + emit closure; its one caller in `attempt`)
- Modify: `internal/engine/engine_test.go` (add `TestEngineForwardsAgentMilestones`)

- [ ] **Step 1: Write the failing test.** In `internal/engine/engine_test.go`, append (the imports `context`, `core`, `event`, `flow`, `store`, `semaphore` are already present in this file):

```go
// emittingExec is a test executor that emits one milestone via Task.Emit, to prove
// the engine binds Emit and that the milestone is persisted + published.
type emittingExec struct{}

func (emittingExec) Run(ctx context.Context, tk core.Task) (core.Result, error) {
	if tk.Emit != nil {
		tk.Emit(event.Event{Kind: event.AgentTool, Summary: "Edit foo.go"})
	}
	return core.Result{StepID: tk.StepID, Summary: "ok", CostUSD: 0.01}, nil
}

func TestEngineForwardsAgentMilestones(t *testing.T) {
	f := &flow.Flow{Name: "feat", Concurrency: 1, Steps: []*flow.Step{
		{ID: "s1", Agent: "x", Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
	}}
	if err := flow.Validate(f); err != nil {
		t.Fatalf("flow invalid: %v", err)
	}
	eng, st, bus := newEngine(t, map[string]core.Executor{"x": emittingExec{}}, semaphore.NewWeighted(1))
	mustCreate(t, st, "r1", f)
	ch, unsub := bus.Subscribe(64)

	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Persisted: SSE reads the durable events table, so the milestone must be there.
	evs, err := st.EventsSince(context.Background(), "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var milestone *event.Event
	for i := range evs {
		if evs[i].Kind == event.AgentTool {
			milestone = &evs[i]
		}
	}
	if milestone == nil {
		t.Fatalf("no agent.tool event persisted; got %+v", evs)
	}
	if milestone.Summary != "Edit foo.go" || milestone.StepID != "s1" {
		t.Errorf("milestone = %+v, want Summary=\"Edit foo.go\" StepID=s1", *milestone)
	}

	// Published: the same milestone reached the live bus.
	unsub()
	var published bool
	for ev := range ch {
		if ev.Kind == event.AgentTool && ev.Summary == "Edit foo.go" {
			published = true
		}
	}
	if !published {
		t.Error("agent.tool milestone was not published to the bus")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails.**

Run: `rtk proxy go test ./internal/engine/ -run TestEngineForwardsAgentMilestones -v`
Expected: FAIL — no `agent.tool` event is persisted (the engine doesn't bind `Task.Emit` yet, so `emittingExec` sees `tk.Emit == nil` and emits nothing).

- [ ] **Step 3: Thread `attemptNum` into `execute`.** In `internal/engine/engine.go`, change the one call site in `attempt` (currently `res, err = e.execute(attemptCtx, runID, s, inputs, workDir)`) to pass `attemptNum`:

```go
	res, err = e.execute(attemptCtx, runID, s, inputs, attemptNum, workDir)
```

- [ ] **Step 4: Build the emit closure in `execute` and set `Task.Emit`.** In `internal/engine/engine.go`, change `execute`'s signature to accept `attemptNum int` and bind the closure into the `Task`. Replace the whole `execute` function with:

```go
// execute runs the step's work: a join strategy for fan-in steps, otherwise the
// named executor. The caller (attempt) bounds this by the step timeout. For the
// executor path it binds Task.Emit to persist-then-publish milestone events
// (mirroring transition() without a step-state row).
func (e *Engine) execute(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact, attemptNum int, workDir string) (core.Result, error) {
	if s.Join != nil {
		strat, ok := e.Joins[s.Join.Strategy]
		if !ok {
			return core.Result{}, fmt.Errorf("join strategy %q not implemented yet", s.Join.Strategy)
		}
		return strat.Join(ctx, s, inputs, workDir)
	}
	ag, ok := e.Execs[s.Agent]
	if !ok {
		return core.Result{}, fmt.Errorf("unknown agent %q", s.Agent)
	}
	emit := func(ev event.Event) {
		ev.RunID, ev.StepID, ev.Attempt, ev.At = string(runID), s.ID, attemptNum, e.Clock.Now()
		if err := e.Store.AppendEvents(context.WithoutCancel(ctx), runID, []event.Event{ev}); err != nil {
			e.logger().Error("append agent milestone", "run", runID, "step", s.ID, "err", err)
			return
		}
		e.Bus.Publish(ev) // Seq is irrelevant on the bus — sse.go re-reads the store for real seqs
	}
	return ag.Run(ctx, core.Task{
		RunID:   runID,
		StepID:  s.ID,
		Role:    s.Role,
		Prompt:  promptFor(s, inputs),
		Inputs:  inputs,
		WorkDir: workDir,
		Emit:    emit,
	})
}
```

(`engine.go` already imports `context` and `concentus/internal/event`, so no new imports. `e.Store.AppendEvents` is the M3-added method, already used for run-level events; `e.logger()` already exists.)

- [ ] **Step 5: Run the test to verify it passes.**

Run: `rtk proxy go test ./internal/engine/ -run TestEngineForwardsAgentMilestones -v`
Expected: PASS — the milestone is persisted (found via `EventsSince`) and published (seen on the bus).

- [ ] **Step 6: Full suite + vet (the whole slice).**

Run: `rtk proxy go test -race -count=1 ./...`
Expected: every package `ok` (no FAIL). The existing engine/supervisor/e2e tests use `executor.Mock` (which never calls `Emit`) and stay green; SSE/api tests are unaffected. Paste the summary.
Run: `go vet ./...` → no issues. Confirm `grep '^go ' go.mod` is still `go 1.22`, and `git diff <branch-base>..HEAD -- go.mod go.sum` is empty (no new dependency — stdlib only).

- [ ] **Step 7: Commit.**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(engine): forward agent.tool milestones to the durable event stream"
```

---

## Done criteria

- `go test -race ./...` + `go vet ./...` clean; `go.mod` still `go 1.22`; no new `require` entries (stdlib only: `encoding/json`, `os/exec`, `io`).
- `ClaudeSpec` invokes `claude … --output-format stream-json --verbose --permission-mode acceptEdits`; `CLIAgent.Run` consumes stdout as a stream via `StdoutPipe`, with B1 error precedence intact (`exec.ErrNotFound` from `Start` → friendly; non-zero exit from `Wait` → stderr-wrapped, **precedence over a parsed result**; parse/no-result error otherwise).
- Each `tool_use` block in a complete `assistant` line emits one `agent.tool` event (`Summary` = `"<tool>: <input>"`), persisted via the engine's `Task.Emit` closure (`AppendEvents`→store) and published to the bus, so SSE serves them with Last-Event-ID replay. The engine's one-Task→one-Result model, the SSE handler, and the store schema are unchanged.
- All automated tests run WITHOUT API keys or network (stub CLI + inline NDJSON fixtures). The real streaming flow is a manual proof: `ANTHROPIC_API_KEY=… cm run <opus-flow>.yaml --watch` shows live `agent.tool` lines as the agent edits files / runs tools.
- No migration, no new YAML fields, no SSE-protocol change, no engine/join/gate API change beyond the additive `Task.Emit` field + the `agent.tool` Kind.

**Plan-time spike (do FIRST, before Task 3):** run the real `claude` once in stream-json mode and capture a few lines (`ANTHROPIC_API_KEY=… claude -p "edit README" --output-format stream-json --verbose --permission-mode acceptEdits | head -40`) to confirm the exact `tool_use` input field names per tool (`file_path` for Edit/Read/Write, `command` for Bash, `pattern` for Grep) before finalizing `renderTool`. The schema was confirmed against current docs (2026-06-05) but field-level details are the one place the design depends on the live CLI. If a field differs, adjust `renderTool`'s struct tags only.
