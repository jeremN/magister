# M4 Slice B1: Real CLIAgent (`claude`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the executor real for the first CLI — a generic `CLIAgent` runner + a `ClaudeSpec` adapter that runs `claude` headless in the step's worktree, parses its JSON result for cost+summary, and discovers changed files as artifacts — wired into the daemon as `opus`/`sonnet` alongside the kept `mock`.

**Architecture:** Behind the existing `core.Executor` port: a `CLISpec` interface (per-CLI args + output parser), `ClaudeSpec` implementing it for `claude -p … --output-format json --permission-mode acceptEdits`, a `CLIAgent` that runs the subprocess + parses + discovers git changes, and a default `discoverGit`. Tested with captured-JSON (inline) for the parser and a `testdata` stub script for the runner — no API keys, no network. The "real claude flow" is a manual proof.

**Tech Stack:** Go 1.22, stdlib `encoding/json` + `os/exec` + `log/slog` (no new dependency). The `claude` CLI (Claude Code) headless mode. Tests: `testing`, `t.Skip` when `git` absent.

**Spec:** `docs/superpowers/specs/2026-06-04-m4b-cliagent-claude-design.md`

**Risky unit (flag for opus review):** Task 3 (`CLIAgent.Run` — subprocess lifecycle, error mapping, env/secrets, artifact attribution). Tasks 1, 2, 4 are lower-risk.

**Commit convention (user CLAUDE.md):** single conventional-commit subject, NO body, NO `Co-Authored-By`, never `--no-verify`. Commit with the explicit identity:
```bash
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "<subject>"
```
**Raw test output:** the RTK hook reformats `go test`; use `rtk proxy go test ...` for per-test PASS/FAIL.

---

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/executor/cli.go` | `CLISpec` port + `CLIAgent` runner | create (interface in T1; runner in T3) |
| `internal/executor/claude.go` | `ClaudeSpec` (args + parse) + `Claude()` ctor | create (T1; ctor in T3) |
| `internal/executor/discover.go` | git-status artifact discovery | create (T2) |
| `internal/executor/claude_test.go` | parser/args tests | create (T1) |
| `internal/executor/discover_test.go` | discovery test | create (T2) |
| `internal/executor/cli_test.go` | runner tests | create (T3) |
| `internal/executor/testdata/fake-claude-ok`, `fake-claude-fail` | stub CLIs | create (T3) |
| `cmd/magisterd/main.go` | daemon agent registry | modify (T4) |
| `cmd/magisterd/main_test.go` | registry test | modify/create (T4) |

---

## Task 1: `CLISpec` port + `ClaudeSpec` (args + output parser)

**Files:**
- Create: `internal/executor/cli.go` (interface only)
- Create: `internal/executor/claude.go` (`ClaudeSpec`)
- Test: `internal/executor/claude_test.go`

- [ ] **Step 1: Write the failing tests.** Create `internal/executor/claude_test.go`:

```go
package executor

import (
	"slices"
	"testing"
)

func TestClaudeSpecArgs(t *testing.T) {
	got := ClaudeSpec{}.Args("opus", "do the thing")
	want := []string{"-p", "do the thing", "--model", "opus", "--output-format", "json", "--permission-mode", "acceptEdits"}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %v, want %v", got, want)
	}
}

func TestClaudeSpecParseSuccess(t *testing.T) {
	out := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"all done","total_cost_usd":0.0123,"usage":{"input_tokens":5}}`)
	summary, cost, err := ClaudeSpec{}.Parse(out)
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
	if _, _, err := ClaudeSpec{}.Parse(out); err == nil {
		t.Fatal("expected error for is_error/non-success result")
	}
}

func TestClaudeSpecParseMalformed(t *testing.T) {
	if _, _, err := ClaudeSpec{}.Parse([]byte("not json at all")); err == nil {
		t.Fatal("expected a parse error for non-JSON output")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail.**

Run: `rtk proxy go test ./internal/executor/ -run TestClaudeSpec -v`
Expected: FAIL — `ClaudeSpec` undefined (compile error).

- [ ] **Step 3: Create the interface and the spec.**

Create `internal/executor/cli.go`:

```go
package executor

// CLISpec adapts one coding-agent CLI's invocation and output schema for CLIAgent.
// ClaudeSpec implements it now; CodexSpec/GeminiSpec arrive in a later slice. A
// non-nil Parse error means the agent ran but failed (e.g. is_error / non-success
// subtype) — distinct from a process/exec failure, which CLIAgent surfaces itself.
type CLISpec interface {
	Args(model, prompt string) []string
	Parse(stdout []byte) (summary string, costUSD float64, err error)
}
```

Create `internal/executor/claude.go`:

```go
package executor

import (
	"encoding/json"
	"fmt"
	"strings"
)

var _ CLISpec = ClaudeSpec{}

// ClaudeSpec runs the `claude` CLI (Claude Code) headless and parses its single
// `--output-format json` result object.
type ClaudeSpec struct{}

func (ClaudeSpec) Args(model, prompt string) []string {
	return []string{
		"-p", prompt,
		"--model", model,
		"--output-format", "json",
		"--permission-mode", "acceptEdits",
	}
}

// claudeResult mirrors the fields CLIAgent needs from claude's JSON result object.
// Unknown fields are ignored (forward-compatible).
type claudeResult struct {
	Subtype      string   `json:"subtype"`
	IsError      bool     `json:"is_error"`
	Result       string   `json:"result"`
	TotalCostUSD float64  `json:"total_cost_usd"`
	Errors       []string `json:"errors"`
}

func (ClaudeSpec) Parse(stdout []byte) (string, float64, error) {
	var r claudeResult
	if err := json.Unmarshal(stdout, &r); err != nil {
		return "", 0, fmt.Errorf("parse claude output: %w (got: %s)", err, truncate(stdout, 200))
	}
	if r.IsError || (r.Subtype != "" && r.Subtype != "success") {
		msg := r.Subtype
		if len(r.Errors) > 0 {
			msg += ": " + strings.Join(r.Errors, "; ")
		}
		return "", 0, fmt.Errorf("claude agent failed (%s)", msg)
	}
	return r.Result, r.TotalCostUSD, nil
}

// truncate returns a trimmed, length-capped string for error diagnostics.
func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
```

- [ ] **Step 4: Run the tests to verify they pass.**

Run: `rtk proxy go test ./internal/executor/ -v`
Expected: PASS for the four `TestClaudeSpec*` tests (and the existing `mock_test.go`).
Run: `go vet ./internal/executor/` → clean.

- [ ] **Step 5: Commit.**

```bash
git add internal/executor/cli.go internal/executor/claude.go internal/executor/claude_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(executor): add CLISpec + ClaudeSpec output parser"
```

---

## Task 2: git-status artifact discovery

**Files:**
- Create: `internal/executor/discover.go`
- Test: `internal/executor/discover_test.go`

- [ ] **Step 1: Write the failing test.** Create `internal/executor/discover_test.go`:

```go
package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initGitRepo creates a temp git repo with an empty base commit and returns its path.
func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-m", "base"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestDiscoverGitListsChangedFiles(t *testing.T) {
	dir := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	arts, err := discoverGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || arts[0].Path != filepath.Join(dir, "out.txt") {
		t.Fatalf("discoverGit = %+v, want one out.txt artifact (abs path)", arts)
	}
}

func TestDiscoverGitEmptyWhenClean(t *testing.T) {
	dir := initGitRepo(t)
	arts, err := discoverGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 0 {
		t.Fatalf("clean tree should yield no artifacts, got %+v", arts)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails.**

Run: `rtk proxy go test ./internal/executor/ -run TestDiscoverGit -v`
Expected: FAIL — `discoverGit` undefined (compile error).

- [ ] **Step 3: Implement `discoverGit`.** Create `internal/executor/discover.go`:

```go
package executor

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"concentus/internal/core"
)

// discoverGit lists files changed in workDir via `git status --porcelain`, as
// absolute-path artifacts (StepID is filled in by CLIAgent). It is CLIAgent's
// default discoverer; a non-nil error is treated as non-fatal by the caller.
func discoverGit(workDir string) ([]core.Artifact, error) {
	// #nosec G204 -- fixed git subcommand in an operator-controlled workdir; no shell.
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	var arts []core.Artifact
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 4 { // "XY <path>"
			continue
		}
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 { // rename: take the new path
			path = path[i+4:]
		}
		path = strings.Trim(path, `"`) // porcelain quotes paths with special chars
		arts = append(arts, core.Artifact{Path: filepath.Join(workDir, path)})
	}
	return arts, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass.**

Run: `rtk proxy go test ./internal/executor/ -v`
Expected: PASS for `TestDiscoverGit*` (and all prior tests).
Run: `go vet ./internal/executor/` → clean.

- [ ] **Step 5: Commit.**

```bash
git add internal/executor/discover.go internal/executor/discover_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(executor): add git-status artifact discovery"
```

---

## Task 3: `CLIAgent` runner + `Claude()` constructor

**Files:**
- Modify: `internal/executor/cli.go` (add `CLIAgent` + `Run` + helpers)
- Modify: `internal/executor/claude.go` (add `Claude()` ctor)
- Create: `internal/executor/testdata/fake-claude-ok`, `internal/executor/testdata/fake-claude-fail`
- Test: `internal/executor/cli_test.go`

- [ ] **Step 1: Create the stub CLIs.** Create `internal/executor/testdata/fake-claude-ok` with EXACTLY:

```sh
#!/bin/sh
# Fake `claude`: write a file into cwd (simulating an edit), print a success JSON
# result, exit 0. Used by CLIAgent runner tests — no API key, no network.
echo "stub wrote this" > agent-output.txt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"stub done","total_cost_usd":0.02}'
```

Create `internal/executor/testdata/fake-claude-fail` with EXACTLY:

```sh
#!/bin/sh
# Fake `claude` that fails: write to stderr, exit non-zero.
echo "boom: simulated agent failure" >&2
exit 3
```

Make both executable (git preserves the +x bit):
```bash
chmod +x internal/executor/testdata/fake-claude-ok internal/executor/testdata/fake-claude-fail
```

- [ ] **Step 2: Write the failing tests.** Create `internal/executor/cli_test.go`:

```go
package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"concentus/internal/core"
)

func stubPath(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stub %s missing: %v", name, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("stub %s is not executable — chmod +x it", name)
	}
	return abs
}

func TestCLIAgentRunSuccess(t *testing.T) {
	dir := initGitRepo(t) // from discover_test.go; skips if git absent
	a := &CLIAgent{Bin: stubPath(t, "fake-claude-ok"), Model: "opus", Spec: ClaudeSpec{}}
	res, err := a.Run(context.Background(), core.Task{StepID: "s1", Prompt: "go", WorkDir: dir})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Summary != "stub done" || res.CostUSD != 0.02 {
		t.Errorf("summary=%q cost=%v, want \"stub done\"/0.02", res.Summary, res.CostUSD)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "s1" ||
		filepath.Base(res.Artifacts[0].Path) != "agent-output.txt" {
		t.Errorf("artifacts = %+v, want one agent-output.txt attributed to s1", res.Artifacts)
	}
}

func TestCLIAgentRunNonZeroExit(t *testing.T) {
	a := &CLIAgent{Bin: stubPath(t, "fake-claude-fail"), Model: "opus", Spec: ClaudeSpec{}}
	_, err := a.Run(context.Background(), core.Task{StepID: "s1", Prompt: "go", WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should surface stderr, got: %v", err)
	}
}

func TestCLIAgentRunBinaryNotFound(t *testing.T) {
	a := &CLIAgent{Bin: "definitely-not-a-real-binary-xyz", Model: "opus", Spec: ClaudeSpec{}}
	_, err := a.Run(context.Background(), core.Task{StepID: "s1", Prompt: "go", WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected a not-found error, got: %v", err)
	}
}

func TestCLIAgentDiscoveryFailureIsNonFatal(t *testing.T) {
	// WorkDir is NOT a git repo, so discoverGit fails — but the step still succeeds
	// (the agent produced a result); artifacts are just empty.
	a := &CLIAgent{Bin: stubPath(t, "fake-claude-ok"), Model: "opus", Spec: ClaudeSpec{}}
	res, err := a.Run(context.Background(), core.Task{StepID: "s1", Prompt: "go", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("discovery failure must not fail the step, got: %v", err)
	}
	if res.Summary != "stub done" || len(res.Artifacts) != 0 {
		t.Errorf("want summary kept + no artifacts, got %+v", res)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail.**

Run: `rtk proxy go test ./internal/executor/ -run 'TestCLIAgent' -v`
Expected: FAIL — `CLIAgent` undefined (compile error).

- [ ] **Step 4: Implement `CLIAgent`.** Append to `internal/executor/cli.go` (add the imports shown):

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
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// CLIAgent is a core.Executor that runs a coding-agent CLI in the step's WorkDir,
// passes the prompt, parses cost+summary via Spec, and discovers changed files.
type CLIAgent struct {
	Bin      string                                         // e.g. "claude"
	Model    string                                         // "opus" / "sonnet"
	Spec     CLISpec                                        // ClaudeSpec{}
	Env      []string                                       // nil ⇒ os.Environ() (carries ANTHROPIC_API_KEY)
	Discover func(workDir string) ([]core.Artifact, error)  // nil ⇒ discoverGit
	Log      *slog.Logger                                   // nil ⇒ discard (non-fatal discovery errors)
}

var _ core.Executor = (*CLIAgent)(nil)

func (a *CLIAgent) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return discardLogger
}

func (a *CLIAgent) Run(ctx context.Context, t core.Task) (core.Result, error) {
	// #nosec G204 -- Bin + args are operator-controlled (daemon registry + flow YAML);
	// no shell. Running a coding-agent CLI is the intended capability.
	cmd := exec.CommandContext(ctx, a.Bin, a.Spec.Args(a.Model, t.Prompt)...)
	cmd.Dir = t.WorkDir
	if a.Env != nil {
		cmd.Env = a.Env
	} else {
		cmd.Env = os.Environ()
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return core.Result{}, fmt.Errorf("agent binary %q not found", a.Bin)
		}
		return core.Result{}, fmt.Errorf("%s: %w: %s", a.Bin, err, truncate(stderr.Bytes(), 500))
	}

	summary, cost, err := a.Spec.Parse(stdout.Bytes())
	if err != nil {
		return core.Result{}, err
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

Append to `internal/executor/claude.go`:

```go
// Claude returns a CLIAgent backed by the `claude` CLI for the given model alias
// (e.g. "opus", "sonnet"). Env defaults to os.Environ() (carries ANTHROPIC_API_KEY).
func Claude(model string) *CLIAgent {
	return &CLIAgent{Bin: "claude", Model: model, Spec: ClaudeSpec{}}
}
```

- [ ] **Step 5: Run the tests to verify they pass.**

Run: `rtk proxy go test ./internal/executor/ -v`
Expected: PASS for all `TestCLIAgent*` and every prior executor test.
Run: `rtk proxy go test -race ./internal/executor/` → ok. `go vet ./internal/executor/` → clean.

- [ ] **Step 6: Commit.**

```bash
git add internal/executor/cli.go internal/executor/claude.go internal/executor/cli_test.go internal/executor/testdata/
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(executor): add CLIAgent runner + claude constructor"
```

---

## Task 4: Register claude-backed `opus`/`sonnet` in the daemon

**Files:**
- Modify: `cmd/magisterd/main.go` (extract `agents()`, wire it)
- Test: `cmd/magisterd/main_test.go`

- [ ] **Step 1: Write the failing test.** Add to `cmd/magisterd/main_test.go` (it is `package main`; add the import `"concentus/internal/executor"` if not present):

```go
func TestAgentsRegistry(t *testing.T) {
	m := agents()
	if _, ok := m["mock"]; !ok {
		t.Error("mock agent must remain registered (keyless flows)")
	}
	opus, ok := m["opus"].(*executor.CLIAgent)
	if !ok {
		t.Fatalf("opus = %T, want *executor.CLIAgent", m["opus"])
	}
	if opus.Bin != "claude" || opus.Model != "opus" {
		t.Errorf("opus agent = {Bin:%q Model:%q}, want claude/opus", opus.Bin, opus.Model)
	}
	if sonnet, ok := m["sonnet"].(*executor.CLIAgent); !ok || sonnet.Model != "sonnet" {
		t.Errorf("sonnet agent wrong: %#v", m["sonnet"])
	}
}
```

- [ ] **Step 2: Run the test to verify it fails.**

Run: `rtk proxy go test ./cmd/magisterd/ -run TestAgentsRegistry -v`
Expected: FAIL — `agents` undefined (compile error).

- [ ] **Step 3: Extract and wire `agents()`.** In `cmd/magisterd/main.go`, add the helper (near `run`):

```go
// agents is the daemon's executor registry: the keyless mock plus claude-backed
// opus/sonnet. A flow using opus/sonnet needs `claude` on PATH + ANTHROPIC_API_KEY;
// mock flows need neither. (gemini/codex arrive in a later slice.)
func agents() map[string]core.Executor {
	return map[string]core.Executor{
		"mock":   executor.Mock{Name: "mock"},
		"opus":   executor.Claude("opus"),
		"sonnet": executor.Claude("sonnet"),
	}
}
```

Then replace the engine's `Execs` field. Change:
```go
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}}, // real CLIAgents arrive in M4
```
to:
```go
		Execs: agents(),
```

- [ ] **Step 4: Run the tests to verify they pass.**

Run: `rtk proxy go test ./cmd/magisterd/ -v`
Expected: PASS for `TestAgentsRegistry` and all existing daemon/e2e tests (they post `mock` flows, unaffected).

- [ ] **Step 5: Full suite + vet.**

Run: `rtk proxy go test -race -count=1 ./...`
Expected: every package `ok` (no FAIL). Paste the summary.
Run: `go vet ./...` → no issues. Confirm `grep '^go ' go.mod` is still `go 1.22`, and that this slice added no dependency — `go.mod`/`go.sum` should be unchanged from the branch base (the slice uses only stdlib: `encoding/json`, `os/exec`, `log/slog`, `bufio`, `bytes`). If `go test` mutated `go.sum`, investigate before committing.

- [ ] **Step 6: Commit.**

```bash
git add cmd/magisterd/main.go cmd/magisterd/main_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(magisterd): register claude-backed opus/sonnet agents"
```

---

## Done criteria

- `go test -race ./...` + `go vet ./...` clean; `go.mod` still `go 1.22`; no new `require` entries.
- The daemon registers `opus`/`sonnet` as real `claude`-backed `CLIAgent`s (verified by `TestAgentsRegistry`), `mock` retained; `claude -p … --output-format json --permission-mode acceptEdits` is the invocation; cost+summary parsed from the result object; artifacts discovered via `git status`; failures (missing binary / non-zero exit / parse error / `is_error`) surface as executor errors; secrets pass via env.
- All automated tests run WITHOUT API keys or network (stub CLI + inline JSON). The real `claude` flow is a manual proof: `ANTHROPIC_API_KEY=… cm run <flow-using-opus>.yaml --watch`.
- No migration, no new YAML fields, no executor/engine/join/gate API change beyond the new adapter.
```
