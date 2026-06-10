# Select / Synthesize Agent-Arbitrated Joins (M5 Slice B) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the `select` and `synthesize` join strategies — a fan-in step whose results are arbitrated by an AI agent (pick the best candidate, or merge them) — plus the engine `on_conflict` disposition for a failed join.

**Architecture:** `internal/join` gains `Select`/`Synthesize` strategies that invoke their arbiter through a new injected `RunAgent` callback (the engine binds it to its existing executor path, so the arbiter streams `agent.tool` milestones and its artifacts are discovered like any step's). `select` parses a `SELECTED: <step-id>` token and forwards the winner's artifacts by reference; `synthesize` returns the arbiter's written output (staged inputs excluded). The engine extracts a reusable `runAgent` helper, threads a `run` closure into the join dispatch, and dispositions a failed join by `on_conflict` (abort/retry/escalate, where escalate's approval re-runs the join once).

**Tech Stack:** Go 1.22; no new dependency. TDD throughout; all automated tests run without keys/network (a stub `RunAgent` for join unit tests, mock/test executors for engine integration).

**Spec:** `docs/superpowers/specs/2026-06-10-m5b-select-synthesize-joins-design.md`

---

## File Structure

- **Modify** `internal/engine/engine.go` — extract `runAgent` (Task 1); thread a `run` closure into the join dispatch (Task 2); add the `on_conflict` disposition + `escalateJoin` (Task 6).
- **Modify** `internal/engine/engine_test.go` — fan-in integration tests (Task 5) + on_conflict tests (Task 6).
- **Modify** `internal/join/join.go` — add `RunAgent` type, change the `Strategy` interface, `Merge` ignores the new arg, add `stageCandidates` (Tasks 2–3); register `Select`/`Synthesize` in `Default()` (Tasks 3–4).
- **Modify** `internal/join/join_test.go` — update the `Merge.Join` caller for the new signature (Task 2).
- **Create** `internal/join/select.go` + `internal/join/select_test.go` (Task 3).
- **Create** `internal/join/synthesize.go` + `internal/join/synthesize_test.go` (Task 4).

**Reused unchanged:** `flow.Join`/`flow.JoinStrategy`/`validateJoin` (the YAML surface + validation already exist — NO change), `core.Result`/`core.Artifact`, the SSE/store layers, the fan-in gather (`engine.go:141-143`), the M4-A `Escalate`/ApprovalRegistry. Shared test helpers already exist — `mustCreate`, `fakeClock`, `rejectApprover` (`engine_test.go:322`), `gate.AutoApprover`. Reference them; do not redefine.

---

## Task 0: Branch + worktree + baseline

**Files:** none committed (setup only). **Run by the controller.**

- [ ] **Step 1: Create the worktree off `main`**

```bash
git worktree add .worktrees/m5b-joins -b m5b-joins
cd .worktrees/m5b-joins
```

- [ ] **Step 2: Confirm a green baseline**

Run: `go test -race ./... && go vet ./...`
Expected: every package `ok`; no vet output. `grep '^go ' go.mod` → `go 1.22`. No new dependency is needed for this slice.

---

## Task 1: `internal/engine` — extract `runAgent` (behavior-preserving refactor)

**Files:** Modify `internal/engine/engine.go`.

This isolates the executor-running logic so a join arbiter can reuse it. Pure refactor — the full existing suite must stay green.

- [ ] **Step 1: Read the current `execute`**

Read `internal/engine/engine.go:314-347` to confirm it matches the code quoted below before editing (the executor branch is `engine.go:326-346`).

- [ ] **Step 2: Add the `runAgent` method**

Add this method immediately ABOVE `execute` in `internal/engine/engine.go`:

```go
// runAgent runs the named agent with prompt in workDir, binding Task.Emit to the
// persist-then-publish milestone path. Shared by normal steps and join arbiters
// so an arbiter streams agent.tool milestones exactly like a normal step.
func (e *Engine) runAgent(ctx context.Context, runID core.RunID, stepID, role, agentName, prompt, workDir string, attemptNum int, inputs []core.Artifact) (core.Result, error) {
	ag, ok := e.Execs[agentName]
	if !ok {
		return core.Result{}, fmt.Errorf("unknown agent %q", agentName)
	}
	emit := func(ev event.Event) {
		ev.RunID, ev.StepID, ev.Attempt, ev.At = string(runID), stepID, attemptNum, e.Clock.Now()
		if err := e.Store.AppendEvents(context.WithoutCancel(ctx), runID, []event.Event{ev}); err != nil {
			e.logger().Error("append agent milestone", "run", runID, "step", stepID, "err", err)
			return
		}
		e.Bus.Publish(ev) // Seq is irrelevant on the bus — sse.go re-reads the store for real seqs
	}
	return ag.Run(ctx, core.Task{
		RunID:   runID,
		StepID:  stepID,
		Role:    role,
		Prompt:  prompt,
		Inputs:  inputs,
		WorkDir: workDir,
		Emit:    emit,
	})
}
```

- [ ] **Step 3: Replace `execute`'s executor branch with a `runAgent` call**

Replace the WHOLE body of `execute` (currently `engine.go:318-347`) with:

```go
func (e *Engine) execute(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact, attemptNum int, workDir string) (core.Result, error) {
	if s.Join != nil {
		strat, ok := e.Joins[s.Join.Strategy]
		if !ok {
			return core.Result{}, fmt.Errorf("join strategy %q not implemented yet", s.Join.Strategy)
		}
		return strat.Join(ctx, s, inputs, workDir)
	}
	return e.runAgent(ctx, runID, s.ID, s.Role, s.Agent, promptFor(s, inputs), workDir, attemptNum, inputs)
}
```

(The join branch is unchanged in this task — the `runAgent` reuse for joins arrives in Task 2.)

- [ ] **Step 4: Verify the refactor is behavior-preserving**

Run: `go test -race ./internal/engine/ && go vet ./internal/engine/`
Expected: PASS (every existing engine test — normal steps, gates, escalate, resume — green; no behavior change). If anything fails, the extraction diverged from the original — re-diff against Step 1.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/engine.go
git commit -m "refactor(engine): extract runAgent for reuse by join arbiters"
```

---

## Task 2: `internal/join` — inject the `RunAgent` seam

**Files:** Modify `internal/join/join.go`; Modify `internal/join/join_test.go`; Modify `internal/engine/engine.go`.

- [ ] **Step 1: Update the existing `Merge.Join` caller in the test first (so the change compiles incrementally)**

In `internal/join/join_test.go:19`, the call is currently:
```go
	res, err := Merge{}.Join(context.Background(), &flow.Step{ID: "integrate"}, inputs, dir)
```
Change it to pass a no-op `run` (Merge ignores it):
```go
	res, err := Merge{}.Join(context.Background(), &flow.Step{ID: "integrate"}, inputs, dir, nil)
```

- [ ] **Step 2: Add the `RunAgent` type and change the `Strategy` interface**

In `internal/join/join.go`, replace the `Strategy` interface (currently lines 15-18) with:

```go
// RunAgent runs the named agent with prompt in workDir and returns its result.
// The engine binds it to the same executor path a normal step uses (Task + the
// persist-then-publish Emit closure), so an arbiter streams agent.tool milestones
// and its artifacts are discovered exactly like a normal step's. inputs are the
// fan-in artifacts (passed through to the agent's Task).
type RunAgent func(ctx context.Context, agentName, prompt, workDir string, inputs []core.Artifact) (core.Result, error)

// Strategy combines a fan-in step's inputs. Strategies that need an arbiter agent
// (select/synthesize) invoke it via run; merge ignores run.
type Strategy interface {
	Join(ctx context.Context, s *flow.Step, inputs []core.Artifact, workDir string, run RunAgent) (core.Result, error)
}
```

- [ ] **Step 3: Update `Merge.Join` to accept (and ignore) `run`**

In `internal/join/join.go`, change the `Merge.Join` signature (currently line 34) from:
```go
func (Merge) Join(_ context.Context, s *flow.Step, inputs []core.Artifact, workDir string) (core.Result, error) {
```
to:
```go
func (Merge) Join(_ context.Context, s *flow.Step, inputs []core.Artifact, workDir string, _ RunAgent) (core.Result, error) {
```
(The body is unchanged — merge writes its manifest as before.)

- [ ] **Step 4: Thread a `run` closure into the engine's join dispatch**

In `internal/engine/engine.go`, replace the join branch of `execute` (the `if s.Join != nil { ... }` block from Task 1) with:

```go
	if s.Join != nil {
		strat, ok := e.Joins[s.Join.Strategy]
		if !ok {
			return core.Result{}, fmt.Errorf("join strategy %q not implemented yet", s.Join.Strategy)
		}
		run := func(ctx context.Context, agentName, prompt, wd string, in []core.Artifact) (core.Result, error) {
			return e.runAgent(ctx, runID, s.ID, "arbiter", agentName, prompt, wd, attemptNum, in)
		}
		return strat.Join(ctx, s, inputs, workDir, run)
	}
```

- [ ] **Step 5: Verify everything compiles and the suite is green**

Run: `go test -race ./internal/join/ ./internal/engine/ && go vet ./internal/join/ ./internal/engine/`
Expected: PASS — `Merge` still works through the new signature; the engine builds and runs (no strategy uses `run` yet, but `Merge` flows through fine).

- [ ] **Step 6: Commit**

```bash
git add internal/join/join.go internal/join/join_test.go internal/engine/engine.go
git commit -m "feat(join): inject RunAgent callback into Strategy.Join"
```

---

## Task 3: `internal/join` — `stageCandidates` + `select`

**Files:** Modify `internal/join/join.go` (add `stageCandidates` + register `Select`); Create `internal/join/select.go`; Create `internal/join/select_test.go`.

- [ ] **Step 1: Write the failing tests**

Create `internal/join/select_test.go`:

```go
package join

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// writeArtifact creates a file and returns it as a StepID-tagged artifact.
func writeArtifact(t *testing.T, dir, stepID, name, body string) core.Artifact {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return core.Artifact{StepID: stepID, Path: p}
}

// stubRun returns a fixed result, ignoring the prompt (no real agent).
func stubRun(res core.Result, err error) RunAgent {
	return func(context.Context, string, string, string, []core.Artifact) (core.Result, error) {
		return res, err
	}
}

func selectStep() *flow.Step {
	return &flow.Step{ID: "pick", Needs: []string{"a", "b"},
		Join: &flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter"}}
}

func TestSelectForwardsWinnerArtifacts(t *testing.T) {
	dir := t.TempDir()
	srcA, srcB := t.TempDir(), t.TempDir()
	inA := writeArtifact(t, srcA, "a", "a.out.md", "A")
	inB := writeArtifact(t, srcB, "b", "b.out.md", "B")
	run := stubRun(core.Result{Summary: "B is cleaner\nSELECTED: b", CostUSD: 0.02}, nil)

	res, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{inA, inB}, dir, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "b" || res.Artifacts[0].Path != inB.Path {
		t.Fatalf("result artifacts = %+v, want only b's original artifact", res.Artifacts)
	}
	if res.StepID != "pick" || res.CostUSD != 0.02 {
		t.Errorf("result = %+v, want StepID=pick cost=0.02", res)
	}
}

func TestSelectNoTokenErrors(t *testing.T) {
	dir := t.TempDir()
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A")
	run := stubRun(core.Result{Summary: "I cannot decide"}, nil)
	if _, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{in}, dir, run); err == nil {
		t.Fatal("expected an error when the arbiter emits no SELECTED token")
	}
}

func TestSelectUnknownWinnerErrors(t *testing.T) {
	dir := t.TempDir()
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A")
	run := stubRun(core.Result{Summary: "SELECTED: zzz"}, nil) // zzz is not a dependency
	if _, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{in}, dir, run); err == nil {
		t.Fatal("expected an error when the chosen step is not a dependency")
	}
}

func TestStageCandidatesCopies(t *testing.T) {
	dir := t.TempDir()
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "hello")
	staged, err := stageCandidates([]core.Artifact{in}, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, ".candidates", "a", "a.out.md")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected staged copy at %s: %v", want, err)
	}
	if len(staged["a"]) != 1 {
		t.Fatalf("staged[a] = %v, want one entry", staged["a"])
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/join/ -run 'TestSelect|TestStageCandidates' -v`
Expected: FAIL — `undefined: Select` / `undefined: stageCandidates`.

- [ ] **Step 3: Add `stageCandidates` to `join.go`**

Add to `internal/join/join.go` (it already imports `os`, `path/filepath`, `fmt`, `core`):

```go
// stageCandidates copies each input artifact into <workDir>/.candidates/<stepID>/<base>
// so the arbiter can read every candidate from within its own workspace, and returns
// the staged relative paths grouped by source step (for the prompt). select uses these
// read-only; synthesize excludes the .candidates dir from its result.
func stageCandidates(inputs []core.Artifact, workDir string) (map[string][]string, error) {
	staged := make(map[string][]string)
	for _, in := range inputs {
		destDir := filepath.Join(workDir, ".candidates", in.StepID)
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(in.Path)
		if err != nil {
			return nil, fmt.Errorf("stage candidate %s: %w", in.Path, err)
		}
		base := filepath.Base(in.Path)
		if err := os.WriteFile(filepath.Join(destDir, base), data, 0o644); err != nil {
			return nil, err
		}
		staged[in.StepID] = append(staged[in.StepID], filepath.Join(".candidates", in.StepID, base))
	}
	return staged, nil
}
```

- [ ] **Step 4: Create `select.go`**

Create `internal/join/select.go`:

```go
package join

import (
	"context"
	"fmt"
	"strings"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Select asks the arbiter agent to choose the single best candidate among the
// fan-in inputs, then forwards that candidate's artifacts (by reference).
type Select struct{}

func (Select) Join(ctx context.Context, s *flow.Step, inputs []core.Artifact, workDir string, run RunAgent) (core.Result, error) {
	staged, err := stageCandidates(inputs, workDir)
	if err != nil {
		return core.Result{}, err
	}
	res, err := run(ctx, s.Join.Agent, selectPrompt(s, staged), workDir, inputs)
	if err != nil {
		return core.Result{}, fmt.Errorf("select: arbiter failed: %w", err)
	}
	winner, ok := parseSelected(res.Summary)
	if !ok {
		return core.Result{}, fmt.Errorf("select: no SELECTED token in arbiter output")
	}
	if !isDependency(s, winner) {
		return core.Result{}, fmt.Errorf("select: chosen step %q is not a dependency", winner)
	}
	var artifacts []core.Artifact
	for _, in := range inputs {
		if in.StepID == winner {
			artifacts = append(artifacts, in)
		}
	}
	return core.Result{StepID: s.ID, Summary: res.Summary, Artifacts: artifacts, CostUSD: res.CostUSD}, nil
}

// parseSelected returns the step id from the last `SELECTED: <id>` line in text.
func parseSelected(text string) (string, bool) {
	winner, ok := "", false
	for _, line := range strings.Split(text, "\n") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(line), "SELECTED:"); found {
			if id := strings.TrimSpace(rest); id != "" {
				winner, ok = id, true
			}
		}
	}
	return winner, ok
}

func isDependency(s *flow.Step, id string) bool {
	for _, dep := range s.Needs {
		if dep == id {
			return true
		}
	}
	return false
}

// selectPrompt lists each candidate's staged files and asks for a SELECTED token.
func selectPrompt(s *flow.Step, staged map[string][]string) string {
	var b strings.Builder
	b.WriteString("You are choosing the single best candidate implementation.\n\n")
	for _, dep := range s.Needs {
		fmt.Fprintf(&b, "Candidate %s:\n", dep)
		for _, p := range staged[dep] {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	b.WriteString("\nRead each candidate's files, decide which is best, explain briefly, ")
	b.WriteString("and end your reply with a line:\nSELECTED: <step-id>\n")
	return b.String()
}
```

- [ ] **Step 5: Register `Select` in `Default()`**

In `internal/join/join.go`, update `Default()` (currently `return Registry{flow.JoinMerge: Merge{}}`) to:
```go
func Default() Registry {
	return Registry{
		flow.JoinMerge:  Merge{},
		flow.JoinSelect: Select{},
	}
}
```
(Update the doc comment above `Default` to drop "select/synthesize ... arrive in M5" — they arrive now; mention synthesize is added in the next task.)

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/join/ -v`
Expected: PASS (the whole `join` package — the new select/stage tests plus the existing Merge test).

- [ ] **Step 7: Commit**

```bash
git add internal/join/join.go internal/join/select.go internal/join/select_test.go
git commit -m "feat(join): add select strategy (arbiter picks a winner)"
```

---

## Task 4: `internal/join` — `synthesize`

**Files:** Modify `internal/join/join.go` (register `Synthesize`); Create `internal/join/synthesize.go`; Create `internal/join/synthesize_test.go`.

- [ ] **Step 1: Write the failing tests**

Create `internal/join/synthesize_test.go`:

```go
package join

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

func synthesizeStep() *flow.Step {
	return &flow.Step{ID: "merge", Needs: []string{"a", "b"},
		Join: &flow.Join{Strategy: flow.JoinSynthesize, Agent: "arbiter"}}
}

func TestSynthesizeReturnsArbiterOutput(t *testing.T) {
	dir := t.TempDir()
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A")
	out := filepath.Join(dir, "synthesis.md")
	// The arbiter "writes" synthesis.md; the stub reports it as its artifact.
	run := func(_ context.Context, _, _, wd string, _ []core.Artifact) (core.Result, error) {
		if err := os.WriteFile(out, []byte("merged"), 0o644); err != nil {
			return core.Result{}, err
		}
		return core.Result{Summary: "synthesized", Artifacts: []core.Artifact{{StepID: "merge", Path: out}}, CostUSD: 0.05}, nil
	}
	res, err := Synthesize{}.Join(context.Background(), synthesizeStep(), []core.Artifact{in}, dir, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Path != out {
		t.Fatalf("result artifacts = %+v, want only synthesis.md", res.Artifacts)
	}
	if res.StepID != "merge" || res.CostUSD != 0.05 {
		t.Errorf("result = %+v, want StepID=merge cost=0.05", res)
	}
}

func TestSynthesizeExcludesStagedCandidates(t *testing.T) {
	dir := t.TempDir()
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A")
	out := filepath.Join(dir, "synthesis.md")
	staged := filepath.Join(dir, ".candidates", "a", "a.out.md")
	// The stub reports BOTH the real output and a staged candidate path (as a
	// real agent's discoverGit would). Only the real output must survive.
	run := func(_ context.Context, _, _, wd string, _ []core.Artifact) (core.Result, error) {
		_ = os.WriteFile(out, []byte("merged"), 0o644)
		return core.Result{Summary: "ok", Artifacts: []core.Artifact{
			{StepID: "merge", Path: staged},
			{StepID: "merge", Path: out},
		}}, nil
	}
	res, err := Synthesize{}.Join(context.Background(), synthesizeStep(), []core.Artifact{in}, dir, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Path != out {
		t.Fatalf("result artifacts = %+v, want the staged .candidates path excluded", res.Artifacts)
	}
}

func TestSynthesizeEmptyOutputErrors(t *testing.T) {
	dir := t.TempDir()
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A")
	// Arbiter produced nothing outside .candidates -> error.
	run := stubRun(core.Result{Summary: "done", Artifacts: nil}, nil)
	if _, err := Synthesize{}.Join(context.Background(), synthesizeStep(), []core.Artifact{in}, dir, run); err == nil {
		t.Fatal("expected an error when the arbiter produced no synthesized output")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/join/ -run TestSynthesize -v`
Expected: FAIL — `undefined: Synthesize`.

- [ ] **Step 3: Create `synthesize.go`**

Create `internal/join/synthesize.go`:

```go
package join

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Synthesize asks the arbiter agent to read all candidates and write one
// reconciled result into the join workdir; that written output is the result.
type Synthesize struct{}

func (Synthesize) Join(ctx context.Context, s *flow.Step, inputs []core.Artifact, workDir string, run RunAgent) (core.Result, error) {
	staged, err := stageCandidates(inputs, workDir)
	if err != nil {
		return core.Result{}, err
	}
	res, err := run(ctx, s.Join.Agent, synthesizePrompt(s, staged), workDir, inputs)
	if err != nil {
		return core.Result{}, fmt.Errorf("synthesize: arbiter failed: %w", err)
	}
	var artifacts []core.Artifact
	for _, a := range res.Artifacts {
		if !underCandidates(a.Path, workDir) {
			artifacts = append(artifacts, core.Artifact{StepID: s.ID, Path: a.Path})
		}
	}
	if len(artifacts) == 0 {
		return core.Result{}, fmt.Errorf("synthesize: arbiter produced no output")
	}
	return core.Result{StepID: s.ID, Summary: res.Summary, Artifacts: artifacts, CostUSD: res.CostUSD}, nil
}

// underCandidates reports whether path is inside <workDir>/.candidates (a staged input).
func underCandidates(path, workDir string) bool {
	rel, err := filepath.Rel(filepath.Join(workDir, ".candidates"), path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// synthesizePrompt lists each candidate's staged files and asks for a merged result.
func synthesizePrompt(s *flow.Step, staged map[string][]string) string {
	var b strings.Builder
	b.WriteString("You are reconciling multiple candidate results into one.\n\n")
	for _, dep := range s.Needs {
		fmt.Fprintf(&b, "Candidate %s:\n", dep)
		for _, p := range staged[dep] {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	b.WriteString("\nRead all candidates and write a single reconciled result into the current directory.\n")
	return b.String()
}
```

- [ ] **Step 4: Register `Synthesize` in `Default()`**

In `internal/join/join.go`, add `Synthesize` to `Default()`:
```go
func Default() Registry {
	return Registry{
		flow.JoinMerge:      Merge{},
		flow.JoinSelect:     Select{},
		flow.JoinSynthesize: Synthesize{},
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/join/ -v`
Expected: PASS (whole `join` package, including all three synthesize tests).

- [ ] **Step 6: Commit**

```bash
git add internal/join/join.go internal/join/synthesize.go internal/join/synthesize_test.go
git commit -m "feat(join): add synthesize strategy (arbiter merges candidates)"
```

---

## Task 5: `internal/engine` — fan-in integration (select + synthesize happy path)

**Files:** Modify `internal/engine/engine_test.go` (test-only — proves the engine wiring: gather → `run` closure → strategy → arbiter executor → result).

- [ ] **Step 1: Write the integration tests**

Append to `internal/engine/engine_test.go` (it already imports `context`, `core`, `event`, `executor`, `flow`, `gate`, `join`, `store`, `workspace`, `testing`, and defines `mustCreate`, `fakeClock`; `path/filepath` and `os` are also imported by neighboring tests — confirm and add if missing):

```go
// pickExec is a test arbiter that always selects `pick` (emits the SELECTED token).
type pickExec struct{ pick string }

func (p pickExec) Run(_ context.Context, t core.Task) (core.Result, error) {
	return core.Result{StepID: t.StepID, Summary: "choosing\nSELECTED: " + p.pick, CostUSD: 0.02}, nil
}

// synthExec is a test arbiter that writes one merged file and returns it.
type synthExec struct{}

func (synthExec) Run(_ context.Context, t core.Task) (core.Result, error) {
	out := filepath.Join(t.WorkDir, "synthesis.md")
	if err := os.WriteFile(out, []byte("merged"), 0o644); err != nil {
		return core.Result{}, err
	}
	return core.Result{StepID: t.StepID, Summary: "synthesized", Artifacts: []core.Artifact{{StepID: t.StepID, Path: out}}, CostUSD: 0.03}, nil
}

// fanInFlow builds a two-upstream-mock fan-in into a join step `j`.
func fanInFlow(j *flow.Join) *flow.Flow {
	return &flow.Flow{Name: "fanin", Steps: []*flow.Step{
		{ID: "a", Agent: "mock"},
		{ID: "b", Agent: "mock"},
		{ID: "j", Needs: []string{"a", "b"}, Join: j},
	}}
}

func TestSelectJoinForwardsWinner(t *testing.T) {
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}, "arbiter": pickExec{pick: "a"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	f := fanInFlow(&flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter"})
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	j := got.Steps[2]
	if j.Status != core.StepSucceeded {
		t.Fatalf("join status = %q, want succeeded", j.Status)
	}
	if len(j.Artifacts) != 1 || filepath.Base(j.Artifacts[0]) != "a.out.md" {
		t.Fatalf("join artifacts = %v, want a's a.out.md (the selected winner)", j.Artifacts)
	}
}

func TestSynthesizeJoinReturnsMergedOutput(t *testing.T) {
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}, "arbiter": synthExec{}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	f := fanInFlow(&flow.Join{Strategy: flow.JoinSynthesize, Agent: "arbiter"})
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	j := got.Steps[2]
	if j.Status != core.StepSucceeded {
		t.Fatalf("join status = %q, want succeeded", j.Status)
	}
	if len(j.Artifacts) != 1 || filepath.Base(j.Artifacts[0]) != "synthesis.md" {
		t.Fatalf("join artifacts = %v, want the synthesized synthesis.md", j.Artifacts)
	}
}
```

- [ ] **Step 2: Run the tests to verify they pass**

Run: `go test ./internal/engine/ -run 'TestSelectJoin|TestSynthesizeJoin' -v`
Expected: PASS — the engine gathers `a`+`b` artifacts, builds the `run` closure, the strategy invokes the `arbiter` executor, and the join result flows back. `select` forwards `a.out.md`; `synthesize` returns `synthesis.md`.

(If the test file is missing `os`/`path/filepath` imports, add them. If a join step needs an explicit gate, none is required — an empty gate policy defaults to manual, which `AutoApprover` passes; the existing fan-in/manual tests confirm this shape.)

- [ ] **Step 3: Run the whole engine package (no regression)**

Run: `go test -race ./internal/engine/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/engine/engine_test.go
git commit -m "test(engine): cover select/synthesize fan-in joins end-to-end"
```

---

## Task 6: `internal/engine` — `on_conflict` disposition + `escalateJoin`

**Files:** Modify `internal/engine/engine.go`; Modify `internal/engine/engine_test.go`.

- [ ] **Step 1: Write the failing integration tests**

Append to `internal/engine/engine_test.go`:

```go
// flakyPick fails the select once (no token), then selects `pick` on re-run.
type flakyPick struct {
	pick  string
	calls *int
}

func (f flakyPick) Run(_ context.Context, t core.Task) (core.Result, error) {
	*f.calls++
	if *f.calls == 1 {
		return core.Result{StepID: t.StepID, Summary: "undecided"}, nil // no SELECTED -> select errors
	}
	return core.Result{StepID: t.StepID, Summary: "SELECTED: " + f.pick, CostUSD: 0.02}, nil
}

// noPick never emits a SELECTED token, so the select join always fails.
type noPick struct{}

func (noPick) Run(_ context.Context, t core.Task) (core.Result, error) {
	return core.Result{StepID: t.StepID, Summary: "undecided"}, nil
}

func TestJoinConflictAbortFailsRun(t *testing.T) {
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}, "arbiter": noPick{}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	f := fanInFlow(&flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter", OnConflict: flow.FailAbort})
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected the run to fail on a join conflict with on_conflict=abort")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[2].Status != core.StepFailed {
		t.Fatalf("join status = %q, want failed", got.Steps[2].Status)
	}
}

func TestJoinConflictEscalateApproveReRuns(t *testing.T) {
	st := store.NewMem()
	bus := event.NewBus()
	ch, unsub := bus.Subscribe(64)
	defer unsub()
	calls := 0
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}, "arbiter": flakyPick{pick: "a", calls: &calls}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}}, // approves the escalation
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: fakeClock{},
	}
	f := fanInFlow(&flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter", OnConflict: flow.FailEscalate})
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("escalation approved, run should succeed: %v", err)
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[2].Status != core.StepSucceeded {
		t.Fatalf("join status = %q, want succeeded (approve re-ran the join)", got.Steps[2].Status)
	}
	unsub()
	var sawAwaiting bool
	for ev := range ch {
		if ev.Kind == event.GateAwaiting && ev.Err != "" {
			sawAwaiting = true
		}
	}
	if !sawAwaiting {
		t.Error("expected a gate.awaiting event with the join failure reason")
	}
}

func TestJoinConflictEscalateRejectAborts(t *testing.T) {
	st := store.NewMem()
	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}, "arbiter": noPick{}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: rejectApprover{}, Verifier: gate.CommandVerifier{}}, // rejects the escalation
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: fakeClock{},
	}
	f := fanInFlow(&flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter", OnConflict: flow.FailEscalate})
	mustCreate(t, st, "r1", f)
	if err := e.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected the run to fail when the escalated join is rejected")
	}
	got, _ := st.GetRun(context.Background(), "r1")
	if got.Steps[2].Status != core.StepFailed {
		t.Fatalf("join status = %q, want failed (escalation rejected)", got.Steps[2].Status)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/engine/ -run TestJoinConflict -v`
Expected: FAIL — `on_conflict` is not yet interpreted, so a failed join with `escalate` does not emit `gate.awaiting`/re-run, and `abort` may not behave as specified.

- [ ] **Step 3: Add `escalateJoin` to `engine.go`**

Add this method immediately AFTER the existing `escalate` method (`engine.go:439-459`):

```go
// escalateJoin handles a failed join with on_conflict=escalate. Unlike a gate
// escalate (which approves an existing result), a failed join has no result, so
// approval RE-RUNS the join exactly once: approve -> one fresh attempt; reject ->
// abort. The failure reason rides on the gate.awaiting event's Err.
func (e *Engine) escalateJoin(ctx context.Context, runID core.RunID, s *flow.Step, inputs []core.Artifact, workDir string, joinErr error, attemptNum int) (core.Result, error) {
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepAwaitingGate, attemptNum, workDir, core.Result{}, joinErr),
		event.Event{StepID: s.ID, Kind: event.GateAwaiting, Attempt: attemptNum, Err: joinErr.Error()})

	ok, err := e.Gate.Escalate(ctx, runID, s, core.Result{})
	if err != nil {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attemptNum, workDir, core.Result{}, err),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attemptNum, Err: err.Error()})
		return core.Result{}, err
	}
	if !ok {
		rej := fmt.Errorf("escalated join rejected")
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attemptNum, workDir, core.Result{}, rej),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attemptNum, Err: rej.Error()})
		return core.Result{}, rej
	}
	// approved: re-run the join exactly once.
	res, _, execErr := e.attempt(ctx, runID, s, inputs, attemptNum+1, workDir)
	if execErr != nil {
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attemptNum+1, workDir, core.Result{}, execErr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attemptNum + 1, Err: execErr.Error()})
		return core.Result{}, execErr
	}
	e.transition(ctx, runID, stepState(runID, s.ID, core.StepSucceeded, attemptNum+1, workDir, res, nil),
		event.Event{StepID: s.ID, Kind: event.StepDone, Summary: res.Summary, CostUSD: res.CostUSD, Attempt: attemptNum + 1})
	return res, nil
}
```

- [ ] **Step 4: Wire `on_conflict` into `runStep`**

In `internal/engine/engine.go`, in `runStep`'s attempt loop, replace the retry-continue line and the terminal disposition (currently `engine.go:257-267`):

```go
		if attempt < maxAttempts && s.Retry != nil {
			continue // retry (covers both execution and gate failures)
		}
		// budget spent — terminal disposition. A failed auto/conditional gate with on_fail=escalate
		// becomes a human approval; everything else fails the run.
		if gateFailed && gateEscalates(s) {
			return e.escalate(ctx, runID, s, res, workDir, lastErr, attempt)
		}
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attempt, workDir, core.Result{}, lastErr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attempt, Err: lastErr.Error()})
		return core.Result{}, lastErr
```

with:

```go
		// A join step's failure disposition is governed by on_conflict (only `retry`
		// re-runs via the generic budget); a normal/gate failure retries on s.Retry.
		canRetry := attempt < maxAttempts && s.Retry != nil
		if s.Join != nil {
			canRetry = canRetry && s.Join.OnConflict == flow.FailRetry
		}
		if canRetry {
			continue // retry (covers execution, gate, and on_conflict=retry join failures)
		}
		// budget spent — terminal disposition.
		if s.Join != nil && s.Join.OnConflict == flow.FailEscalate {
			return e.escalateJoin(ctx, runID, s, inputs, workDir, lastErr, attempt)
		}
		// A failed auto/conditional gate with on_fail=escalate becomes a human approval.
		if gateFailed && gateEscalates(s) {
			return e.escalate(ctx, runID, s, res, workDir, lastErr, attempt)
		}
		e.transition(ctx, runID, stepState(runID, s.ID, core.StepFailed, attempt, workDir, core.Result{}, lastErr),
			event.Event{StepID: s.ID, Kind: event.StepFailed, Attempt: attempt, Err: lastErr.Error()})
		return core.Result{}, lastErr
```

(Confirm `runStep` has `inputs` in scope — it is a parameter of `runStep`; `s.Join.OnConflict`, `flow.FailRetry`, `flow.FailEscalate` are the existing flow constants.)

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/engine/ -run TestJoinConflict -v && go test -race ./internal/engine/`
Expected: PASS — abort fails the run; escalate+approve re-runs the join (flaky arbiter succeeds on call 2) and emits `gate.awaiting`; escalate+reject aborts. Every existing engine test (normal/gate/escalate/resume/fan-in) stays green — the `s.Join != nil` guards leave non-join behavior unchanged.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go
git commit -m "feat(engine): on_conflict disposition for failed joins (abort/retry/escalate)"
```

---

## Task 7: Full verification + manual proof

**Files:** none (verification only).

- [ ] **Step 1: Full race suite + vet + dependency check**

Run: `go test -race ./... && go vet ./...`
Expected: every package `ok`; no vet output. `grep '^go ' go.mod` → `go 1.22`; `git diff main -- go.mod` is EMPTY (no new dependency this slice).

- [ ] **Step 2: Manual proof (zero-cost, no network — uses the mock arbiter)**

Use the `running-the-orchestrator` skill to build `magisterd`+`cm` from this worktree and start the daemon on a throwaway db/port. The mock agent's summary is `"<stepID> done by mock"` — it does NOT contain a `SELECTED:` token, so a `select` over a `mock` arbiter will hit `on_conflict`. Use `select` with `on_conflict: escalate` to prove the awaiting→approve re-run is observable, and `synthesize` to prove the merged-output happy path (the mock arbiter writes its `<stepID>.out.md`, which synthesize returns).

Synthesize happy-path flow:
```yaml
name: synth-demo
concurrency: 2
steps:
  - id: a
    agent: mock
    role: implementer
  - id: b
    agent: mock
    role: implementer
  - id: merge
    needs: [a, b]
    join: { strategy: synthesize, agent: mock }
    gate: { policy: auto, verifier: { command: "true" } }
```
`cm run synth-demo.yaml` then `cm watch <run>`: confirm `a`/`b` run, then the `merge` join's `step.started → step.done → run.done`, run `succeeded`, and `cm get <run>` shows the join step's artifact (the mock arbiter's `merge.out.md`, since synthesize excludes the staged `.candidates/` copies).

Select-conflict-escalate flow (swap the join):
```yaml
  - id: pick
    needs: [a, b]
    join: { strategy: select, agent: mock, on_conflict: escalate }
    gate: { policy: auto, verifier: { command: "true" } }
```
Confirm the `pick` join emits a `gate.awaiting` frame (the mock arbiter emits no `SELECTED:` token → conflict → escalate); `cm approve <run> pick` triggers one re-run (which fails again the same way → terminal fail) — observe the awaiting frame and the re-run. (A real arbiter agent, e.g. `sonnet`, instructed to end with `SELECTED: a`, completes a true select; note this as the real-agent path, not run here to stay zero-cost.) Capture the observed behavior in the post-M5b handoff.

- [ ] **Step 3: Final state confirmation**

Run: `git log --oneline main..HEAD`
Expected: the six commits from Tasks 1–6 (refactor + 4 feat + 1 test). The branch is ready to merge to `main` per finishing-a-development-branch.

---

## Self-Review

**Spec coverage (spec §→task):**
- §2 the `RunAgent` seam (engine `runAgent` extraction + `Strategy.Join` signature + dispatch closure) → Tasks 1–2.
- §3 `stageCandidates` → Task 3 (Step 3) + tested in Task 3 (`TestStageCandidatesCopies`).
- §4 `select` (stage → prompt → `SELECTED:` parse → forward winner by reference) → Task 3.
- §5 `synthesize` (stage → prompt → discover-minus-`.candidates`) → Task 4.
- §6 `on_conflict` disposition (abort/retry/escalate; `escalateJoin` approve=re-run) → Task 6.
- §7 error semantics (arbiter error, parse failure, empty synthesis, unknown agent) → Tasks 3/4 unit tests + Task 6 integration.
- §8 testing (join unit w/ stub `RunAgent`, engine integration w/ mock/test executors, manual proof) → Tasks 3–7.
- §10 done-criteria → Task 7.

**Placeholder scan:** No TBD/TODO; every code/command step is complete. No new dependency (no `go get`).

**Type consistency:** `RunAgent func(ctx, agentName, prompt, workDir string, inputs []core.Artifact) (core.Result, error)`, `Strategy.Join(ctx, s, inputs, workDir, run)`, `stageCandidates(inputs, workDir) (map[string][]string, error)`, `parseSelected(string) (string, bool)`, `isDependency(*flow.Step, string) bool`, `underCandidates(path, workDir string) bool`, and `escalateJoin(ctx, runID, s, inputs, workDir, joinErr, attemptNum)` are used identically across tasks. `Merge.Join` gains the `_ RunAgent` arg (Task 2) consistent with the interface. Test executors (`pickExec`, `synthExec`, `flakyPick`, `noPick`) and `fanInFlow` are defined in Task 5/6 before use. The mock-cost assumption (`0.01`) and the mock artifact name (`<stepID>.out.md`) match `executor.Mock` (verified). `flow.FailAbort`/`FailRetry`/`FailEscalate`, `flow.JoinSelect`/`JoinSynthesize`, and `core.StepSucceeded`/`StepFailed` are existing constants.
