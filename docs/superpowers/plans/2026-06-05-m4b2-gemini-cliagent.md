# M4 Slice B2 — Gemini CLIAgent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `gemini`-backed agent that streams `agent.tool` milestones (mirroring the merged claude path), and clear two deferred B1 nits.

**Architecture:** Implement the existing `CLISpec` port with a new `GeminiSpec` (its own `Args` + `Parse` for Gemini's distinct stream-json schema); the shared `CLIAgent` runner, engine `Task.Emit` wiring, SSE path, and `agent.tool` Kind are reused unchanged. Register `gemini` in the daemon. codex is out of scope (later B3).

**Tech Stack:** Go 1.22, stdlib only (`encoding/json`, `strings`, `io`, `os/exec`). No new dependencies, no migration, no YAML/engine/SSE change.

**Spec:** `docs/superpowers/specs/2026-06-05-m4b2-gemini-cliagent-design.md`

---

## File structure

- **Create** `internal/executor/gemini.go` — `GeminiSpec` (`Args`, `Parse`, `renderGeminiTool`), `Gemini(model)` ctor. One focused unit, mirrors `claude.go`.
- **Create** `internal/executor/gemini_test.go` — parser unit tests.
- **Create** `internal/executor/testdata/fake-gemini-stream` — executable stub emitting the real Gemini NDJSON shape.
- **Modify** `internal/executor/claude.go` — B1 nit (a): non-empty failure reason.
- **Modify** `internal/executor/claude_test.go` — test for nit (a).
- **Modify** `internal/executor/cli.go` — B1 nit (b): absolute-path not-found.
- **Modify** `internal/executor/cli_test.go` — test for nit (b) + the Gemini runner integration test.
- **Modify** `cmd/magisterd/main.go` — register `gemini`.
- **Modify** `cmd/magisterd/main_test.go` — assert `gemini` is registered.

Convention reminders: raw test output via `rtk proxy go test ...` (a hook reformats bare `go test`). Commit with explicit identity, single conventional-commit subject, no body, no `Co-Authored-By`, never `--no-verify`:
`git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "<subject>"`. If the Semgrep pre-commit hook flags a line with a rule ID that is a false positive, add a trailing `// nosemgrep: <ruleID>` mirroring the existing `engine.go` precedent — never bypass the hook.

---

## Task 1: B1 nit (a) — claude failure errors never have empty parens

When `is_error` is true but `subtype==""` and `errors` is empty, `ClaudeSpec.Parse` currently returns `claude agent failed ()`. Give it a non-empty reason.

**Files:**
- Modify: `internal/executor/claude.go:77-83`
- Test: `internal/executor/claude_test.go`

- [ ] **Step 1: Write the failing test.** Append to `internal/executor/claude_test.go` (imports `bytes`, `strings`, `event` are already present):

```go
func TestClaudeSpecParseErrorMessageNeverEmpty(t *testing.T) {
	out := []byte(`{"type":"result","is_error":true,"subtype":"","result":"","total_cost_usd":0}`)
	_, _, err := (ClaudeSpec{}).Parse(bytes.NewReader(out), noEmit)
	if err == nil {
		t.Fatal("expected an error for an is_error result")
	}
	if strings.Contains(err.Error(), "()") {
		t.Errorf("failure message has empty parens: %v", err)
	}
	if !strings.Contains(err.Error(), "is_error") {
		t.Errorf("want a non-empty reason in the message, got: %v", err)
	}
}
```

- [ ] **Step 2: Run it, verify it fails.**

Run: `rtk proxy go test ./internal/executor/ -run TestClaudeSpecParseErrorMessageNeverEmpty -v`
Expected: FAIL — current message is `claude agent failed ()` (contains `()`, lacks `is_error`).

- [ ] **Step 3: Fix the message.** In `internal/executor/claude.go`, replace lines 77-83:

```go
	if result.IsError || (result.Subtype != "" && result.Subtype != "success") {
		msg := result.Subtype
		if len(result.Errors) > 0 {
			msg += ": " + strings.Join(result.Errors, "; ")
		}
		return "", 0, fmt.Errorf("claude agent failed (%s)", msg)
	}
```

with:

```go
	if result.IsError || (result.Subtype != "" && result.Subtype != "success") {
		msg := result.Subtype
		if len(result.Errors) > 0 {
			if msg != "" {
				msg += ": "
			}
			msg += strings.Join(result.Errors, "; ")
		}
		if msg == "" {
			msg = "is_error"
		}
		return "", 0, fmt.Errorf("claude agent failed (%s)", msg)
	}
```

- [ ] **Step 4: Run it, verify it passes.**

Run: `rtk proxy go test ./internal/executor/ -run TestClaudeSpecParse -v`
Expected: PASS (the new test and the existing `TestClaudeSpecParseErrorResult` both green — the `error_max_turns`/`hit max turns` message is unchanged).

- [ ] **Step 5: Commit.**

```bash
git add internal/executor/claude.go internal/executor/claude_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "fix(executor): give claude failure errors a non-empty reason"
```

---

## Task 2: B1 nit (b) — an absent absolute agent path reports "not found"

A bare `Bin` not on PATH yields the friendly `agent binary %q not found`, but an absolute path that doesn't exist falls through to the generic `start:` branch. Unify them.

**Files:**
- Modify: `internal/executor/cli.go:67-72`
- Test: `internal/executor/cli_test.go`

- [ ] **Step 1: Write the failing test.** Append to `internal/executor/cli_test.go` (imports `context`, `core`, `strings` are already present):

```go
func TestCLIAgentRunAbsentAbsolutePathNotFound(t *testing.T) {
	// A bare name not on PATH already reports "not found" (exec.ErrNotFound); an
	// absolute path that doesn't exist must report it the same friendly way.
	a := &CLIAgent{Bin: "/nonexistent/definitely-not-a-real-binary-xyz", Model: "opus", Spec: ClaudeSpec{}}
	_, err := a.Run(context.Background(), core.Task{StepID: "s1", Prompt: "go", WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected a friendly not-found error for an absolute bad path, got: %v", err)
	}
}
```

- [ ] **Step 2: Run it, verify it fails.**

Run: `rtk proxy go test ./internal/executor/ -run TestCLIAgentRunAbsentAbsolutePathNotFound -v`
Expected: FAIL — the error is `…: start: fork/exec /nonexistent/…: no such file or directory` (no substring "not found").

- [ ] **Step 3: Unify the not-found branch.** In `internal/executor/cli.go`, replace lines 67-72:

```go
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return core.Result{}, fmt.Errorf("agent binary %q not found", a.Bin)
		}
		return core.Result{}, fmt.Errorf("%s: start: %w", a.Bin, err)
	}
```

with (`os` is already imported; `os.ErrNotExist` is the `fs.ErrNotExist` sentinel, which a fork/exec ENOENT matches):

```go
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return core.Result{}, fmt.Errorf("agent binary %q not found", a.Bin)
		}
		return core.Result{}, fmt.Errorf("%s: start: %w", a.Bin, err)
	}
```

- [ ] **Step 4: Run it, verify it passes.**

Run: `rtk proxy go test ./internal/executor/ -run 'TestCLIAgentRun' -v`
Expected: PASS — the new test plus the existing `TestCLIAgentRunBinaryNotFound` (bare name) both green.

- [ ] **Step 5: Commit.**

```bash
git add internal/executor/cli.go internal/executor/cli_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "fix(executor): treat an absent absolute agent path as not-found"
```

---

## Task 3: `GeminiSpec` — Args + stream-json Parse + renderGeminiTool

The core unit. Gemini's stream-json (verified live, gemini 0.41.1): top-level `tool_use` lines (`tool_name`/`parameters`), `role:"assistant"` `message` deltas concatenated into the summary, a `result` line with `status` and no USD. See spec §2.

**Files:**
- Create: `internal/executor/gemini.go`
- Test: `internal/executor/gemini_test.go`

- [ ] **Step 1: Write the failing tests.** Create `internal/executor/gemini_test.go` (`noEmit` is defined in `claude_test.go`, same package):

```go
package executor

import (
	"bytes"
	"slices"
	"testing"

	"concentus/internal/event"
)

func TestGeminiSpecArgs(t *testing.T) {
	got := GeminiSpec{}.Args("gemini-2.5-pro", "do the thing")
	want := []string{"-p", "do the thing", "-m", "gemini-2.5-pro", "-o", "stream-json", "--approval-mode", "yolo", "--skip-trust"}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %v, want %v", got, want)
	}
}

func TestGeminiSpecParseConcatenatesDeltasAndEmitsMilestones(t *testing.T) {
	stream := `{"type":"init","model":"gemini-2.5-flash"}
{"type":"message","role":"user","content":"go"}
{"type":"tool_use","tool_name":"update_topic","parameters":{"strategic_intent":"x"}}
{"type":"tool_use","tool_name":"write_file","parameters":{"file_path":"report.txt","content":"hi"}}
{"type":"tool_result","tool_id":"x","status":"success"}
{"type":"message","role":"assistant","content":"I created","delta":true}
{"type":"message","role":"assistant","content":" report.txt.","delta":true}
{"type":"result","status":"success","stats":{"total_tokens":10}}`
	var got []event.Event
	emit := func(e event.Event) { got = append(got, e) }
	summary, cost, err := GeminiSpec{}.Parse(bytes.NewReader([]byte(stream)), emit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if summary != "I created report.txt." {
		t.Errorf("summary = %q, want %q (deltas concatenated)", summary, "I created report.txt.")
	}
	if cost != 0 {
		t.Errorf("cost = %v, want 0 (gemini reports no USD)", cost)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 milestone (update_topic skipped), got %d: %+v", len(got), got)
	}
	if got[0].Kind != event.AgentTool || got[0].Summary != "write_file: report.txt" {
		t.Errorf("milestone = %+v, want agent.tool \"write_file: report.txt\"", got[0])
	}
}

func TestGeminiSpecParseRendersCommandTool(t *testing.T) {
	stream := `{"type":"tool_use","tool_name":"run_shell_command","parameters":{"command":"go test ./..."}}
{"type":"result","status":"success"}`
	var got []event.Event
	_, _, err := GeminiSpec{}.Parse(bytes.NewReader([]byte(stream)), func(e event.Event) { got = append(got, e) })
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 || got[0].Summary != "run_shell_command: go test ./..." {
		t.Fatalf("milestone = %+v, want \"run_shell_command: go test ./...\"", got)
	}
}

func TestGeminiSpecParseErrorStatus(t *testing.T) {
	stream := `{"type":"result","status":"error"}`
	if _, _, err := (GeminiSpec{}).Parse(bytes.NewReader([]byte(stream)), noEmit); err == nil {
		t.Fatal("expected an error for a non-success result status")
	}
}

func TestGeminiSpecParseNoResultLine(t *testing.T) {
	stream := `{"type":"message","role":"assistant","content":"hi","delta":true}`
	if _, _, err := (GeminiSpec{}).Parse(bytes.NewReader([]byte(stream)), noEmit); err == nil {
		t.Fatal("expected an error when the stream has no result line")
	}
}

func TestRenderGeminiToolFallsBackToName(t *testing.T) {
	if got := renderGeminiTool("search_web", []byte(`{"query":"golang"}`)); got != "search_web" {
		t.Errorf("renderGeminiTool = %q, want bare \"search_web\"", got)
	}
}
```

- [ ] **Step 2: Run them, verify they fail (do not compile).**

Run: `rtk proxy go test ./internal/executor/ -run Gemini -v`
Expected: FAIL — build error, `undefined: GeminiSpec` / `undefined: renderGeminiTool`.

- [ ] **Step 3: Implement `gemini.go`.** Create `internal/executor/gemini.go` (reuses the package-local `truncate` from `claude.go`):

```go
package executor

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"concentus/internal/event"
)

var _ CLISpec = GeminiSpec{}

// GeminiSpec runs the `gemini` CLI (Google Gemini CLI) headless in `-o stream-json`
// mode and parses its NDJSON stream: top-level tool_use lines become agent.tool
// milestones; role:"assistant" message deltas are concatenated into the summary; the
// final result line carries status. Gemini reports token stats but no USD, so cost is
// always 0.
type GeminiSpec struct{}

func (GeminiSpec) Args(model, prompt string) []string {
	return []string{
		"-p", prompt,
		"-m", model,
		"-o", "stream-json",
		"--approval-mode", "yolo", // auto-approve all tools; headless can't prompt
		"--skip-trust",            // else a workspace-trust prompt hangs the headless run
	}
}

// geminiLine is one NDJSON object from the `gemini` CLI. Fields are the union of what
// we read across line types; unknown fields/types are ignored (forward-compatible).
// tool_use lines are top-level (unlike claude's nested content blocks); role-tagged
// message lines carry assistant text as incremental deltas; the result line carries
// status (no summary text, no USD).
type geminiLine struct {
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolName   string          `json:"tool_name"`
	Parameters json.RawMessage `json:"parameters"`
	Status     string          `json:"status"`
}

// Parse reads the gemini CLI's stdout as a stream of NDJSON objects (json.Decoder
// imposes no line-size cap). It emits one agent.tool milestone per tool_use line
// (except gemini's internal update_topic tracker), concatenates the assistant message
// deltas into the summary, and fails on a missing or non-success result line. cost is
// always 0 — gemini reports token stats but no USD. emit is never nil.
func (GeminiSpec) Parse(stdout io.Reader, emit func(event.Event)) (string, float64, error) {
	dec := json.NewDecoder(stdout)
	var summary strings.Builder
	sawResult := false
	status := ""
	for dec.More() {
		var line geminiLine
		if err := dec.Decode(&line); err != nil {
			return "", 0, fmt.Errorf("parse gemini output: %w", err)
		}
		switch line.Type {
		case "tool_use":
			if line.ToolName == "update_topic" {
				continue // gemini's internal intent/UI tracker, not a real action
			}
			emit(event.Event{Kind: event.AgentTool, Summary: renderGeminiTool(line.ToolName, line.Parameters)})
		case "message":
			if line.Role == "assistant" {
				summary.WriteString(line.Content) // deltas are incremental chunks
			}
		case "result":
			sawResult = true
			status = line.Status
		}
	}
	if !sawResult {
		return "", 0, fmt.Errorf("gemini output ended with no result")
	}
	if status != "success" {
		return "", 0, fmt.Errorf("gemini agent failed (status: %s)", status)
	}
	return summary.String(), 0, nil
}

// renderGeminiTool produces a short human label for a gemini tool_use line:
// "<tool_name>: <salient parameter>" (file path / command / pattern), or just the
// tool name when no known parameter is present. Best-effort: unknown shapes render as
// the name alone.
func renderGeminiTool(toolName string, params json.RawMessage) string {
	var p struct {
		FilePath string `json:"file_path"`
		Command  string `json:"command"`
		Pattern  string `json:"pattern"`
	}
	_ = json.Unmarshal(params, &p)
	switch {
	case p.FilePath != "":
		return toolName + ": " + p.FilePath
	case p.Command != "":
		return toolName + ": " + truncate([]byte(p.Command), 80)
	case p.Pattern != "":
		return toolName + ": " + p.Pattern
	default:
		return toolName
	}
}

// Gemini returns a CLIAgent backed by the `gemini` CLI for the given model
// (e.g. "gemini-2.5-pro"). Env defaults to os.Environ() (carries gemini's auth).
func Gemini(model string) *CLIAgent {
	return &CLIAgent{Bin: "gemini", Model: model, Spec: GeminiSpec{}}
}
```

- [ ] **Step 4: Run them, verify they pass.**

Run: `rtk proxy go test ./internal/executor/ -run Gemini -v`
Expected: PASS (all six Gemini tests).

- [ ] **Step 5: Commit.**

```bash
git add internal/executor/gemini.go internal/executor/gemini_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(executor): add GeminiSpec stream-json parser"
```

---

## Task 4: Prove the Gemini path streams through `CLIAgent`

No production code — an end-to-end test through the shared runner (which already accepts any `CLISpec`): the milestone is forwarded via `Task.Emit`, the summary is the concatenated deltas, cost is 0, and the agent's file is discovered as an artifact. Mirrors `TestCLIAgentRunStreamsMilestones`.

**Files:**
- Create: `internal/executor/testdata/fake-gemini-stream`
- Test: `internal/executor/cli_test.go`

- [ ] **Step 1: Create the stub.** Write `internal/executor/testdata/fake-gemini-stream`:

```sh
#!/bin/sh
# Fake `gemini` in stream-json mode: write a file (simulating write_file), print the
# Gemini NDJSON shape (init, update_topic + write_file tool_use, assistant deltas,
# result success), exit 0. Used by the CLIAgent gemini streaming test — no API key,
# no network.
echo "stub gemini wrote this" > out.txt
printf '%s\n' '{"type":"init","model":"gemini-2.5-pro"}'
printf '%s\n' '{"type":"tool_use","tool_name":"update_topic","parameters":{"strategic_intent":"x"}}'
printf '%s\n' '{"type":"tool_use","tool_name":"write_file","parameters":{"file_path":"out.txt","content":"hi"}}'
printf '%s\n' '{"type":"message","role":"assistant","content":"Wrote ","delta":true}'
printf '%s\n' '{"type":"message","role":"assistant","content":"out.txt.","delta":true}'
printf '%s\n' '{"type":"result","status":"success","stats":{"total_tokens":5}}'
```

- [ ] **Step 2: Make it executable.**

Run: `chmod +x internal/executor/testdata/fake-gemini-stream && ls -l internal/executor/testdata/fake-gemini-stream`
Expected: mode shows `-rwxr-xr-x` (the `stubPath` helper fails the test if it is not executable).

- [ ] **Step 3: Write the failing test.** Append to `internal/executor/cli_test.go` (all imports already present):

```go
func TestCLIAgentRunStreamsGeminiMilestones(t *testing.T) {
	dir := initGitRepo(t) // from discover_test.go; skips if git absent
	var got []event.Event
	a := &CLIAgent{Bin: stubPath(t, "fake-gemini-stream"), Model: "gemini-2.5-pro", Spec: GeminiSpec{}}
	res, err := a.Run(context.Background(), core.Task{
		StepID: "s1", Prompt: "go", WorkDir: dir,
		Emit: func(e event.Event) { got = append(got, e) },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Summary != "Wrote out.txt." || res.CostUSD != 0 {
		t.Errorf("summary=%q cost=%v, want \"Wrote out.txt.\"/0", res.Summary, res.CostUSD)
	}
	if len(got) != 1 || got[0].Kind != event.AgentTool || got[0].Summary != "write_file: out.txt" {
		t.Fatalf("milestones = %+v, want one agent.tool \"write_file: out.txt\"", got)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "s1" ||
		filepath.Base(res.Artifacts[0].Path) != "out.txt" {
		t.Errorf("artifacts = %+v, want one out.txt attributed to s1", res.Artifacts)
	}
}
```

- [ ] **Step 4: Run it, verify it passes** (the stub + existing runner already satisfy it; this confirms the wiring, not new code).

Run: `rtk proxy go test ./internal/executor/ -run TestCLIAgentRunStreamsGeminiMilestones -v`
Expected: PASS.

- [ ] **Step 5: Mutation check (confirm the test is real).** Temporarily change the stub's `write_file` line `tool_name` to `update_topic`, run the test, confirm it FAILS (`want one agent.tool`, got 0 — both real tool_uses skipped), then revert the stub and re-run to PASS.

Run: `rtk proxy go test ./internal/executor/ -run TestCLIAgentRunStreamsGeminiMilestones -v` (after revert)
Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add internal/executor/cli_test.go internal/executor/testdata/fake-gemini-stream
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "test(executor): cover gemini milestone streaming via CLIAgent"
```

---

## Task 5: Register the `gemini` agent in the daemon

**Files:**
- Modify: `cmd/magisterd/main.go:46-52` (`agents()`)
- Test: `cmd/magisterd/main_test.go:45-60` (`TestAgentsRegistry`)

- [ ] **Step 1: Extend the registry test.** In `cmd/magisterd/main_test.go`, insert before the closing `}` of `TestAgentsRegistry` (after line 59):

```go
	gem, ok := m["gemini"].(*executor.CLIAgent)
	if !ok {
		t.Fatalf("gemini = %T, want *executor.CLIAgent", m["gemini"])
	}
	if gem.Bin != "gemini" || gem.Model != "gemini-2.5-pro" {
		t.Errorf("gemini agent = {Bin:%q Model:%q}, want gemini/gemini-2.5-pro", gem.Bin, gem.Model)
	}
```

- [ ] **Step 2: Run it, verify it fails.**

Run: `rtk proxy go test ./cmd/magisterd/ -run TestAgentsRegistry -v`
Expected: FAIL — `m["gemini"]` is nil, so the type assertion `ok` is false → `gemini = <nil>, want *executor.CLIAgent`.

- [ ] **Step 3: Register the agent.** In `cmd/magisterd/main.go`, change the `agents()` map literal (lines 47-51) from:

```go
	return map[string]core.Executor{
		"mock":   executor.Mock{Name: "mock"},
		"opus":   executor.Claude("opus"),
		"sonnet": executor.Claude("sonnet"),
	}
```

to:

```go
	return map[string]core.Executor{
		"mock":   executor.Mock{Name: "mock"},
		"opus":   executor.Claude("opus"),
		"sonnet": executor.Claude("sonnet"),
		"gemini": executor.Gemini("gemini-2.5-pro"),
	}
```

Also update the `agents` doc comment (line 43-45) to mention gemini, e.g. change `the keyless mock plus claude-backed opus/sonnet.` to `the keyless mock, claude-backed opus/sonnet, and gemini-backed gemini.` and `(gemini/codex arrive in a later slice.)` to `(codex arrives in a later slice.)`.

- [ ] **Step 4: Run it, verify it passes.**

Run: `rtk proxy go test ./cmd/magisterd/ -run TestAgentsRegistry -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add cmd/magisterd/main.go cmd/magisterd/main_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(magisterd): register gemini-backed agent"
```

---

## Final verification

- [ ] **Full suite + vet + race.**

Run: `rtk proxy go test -race -count=1 ./...`
Expected: every package `ok`, no FAIL.
Run: `rtk proxy go vet ./...`
Expected: no output.
Confirm `grep '^go ' go.mod` is still `go 1.22` and `git diff <slice-base>..HEAD -- go.mod go.sum` is empty (stdlib only — no new dependency).

- [ ] **Manual proof (not automated — needs gemini auth + network).** Using the `running-the-orchestrator` skill, run a one-step flow with `agent: gemini` (a prompt that writes a file) and `--watch`; confirm a live `event: agent.tool` frame with `summary: "write_file: …"` arrives mid-step, then `step.done` with `cost_usd: 0`, then `run.done`. (Inside the Claude Code sandbox, launch the daemon with the sandbox disabled so its `gemini` child reaches the network.)

## Done criteria (from spec §9)

- `go test -race ./...` + `go vet ./...` clean; `go.mod` still `go 1.22`; no new dependency.
- `GeminiSpec` invokes `gemini … -o stream-json --approval-mode yolo --skip-trust`; each non-`update_topic` `tool_use` emits one `agent.tool` milestone; summary is the concatenated assistant deltas; cost is `0`.
- Daemon registers `gemini`; both B1 nits fixed with tests; manual proof shows a live Gemini milestone over SSE.
