# Run-scoped logging + request→run bridge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Carry a run-scoped `*slog.Logger` on the context so the agent layer's currently-discarded logs emit correlated by run-id, and log one `run submitted` line bridging the HTTP request-id to the new run-id.

**Architecture:** A tiny `internal/logctx` package stashes a logger on a `context.Context`. The engine's `runAgent` seam injects a `run`/`step`/`agent`-tagged logger into the context it already passes to the executor; `CLIAgent` reads it. Separately, `handleCreateRun` emits the request→run bridge line. Correlation key is the run-id (runs outlive their request and can resume without one).

**Tech Stack:** Go 1.22, standard library only (`log/slog`, `context`). Packages `internal/logctx` (new), `internal/engine`, `internal/executor`, `internal/api`.

## Global Constraints

- Go 1.22; **stdlib only, NO new dependency** (do not touch `go.mod`); no DB migration; no schema change; `event.Event` unchanged.
- Correlation key is the run-id; the request-id appears only in the access log and the one `run submitted` bridge line.
- `logctx.From` never returns nil.
- The engine's existing `e.logger().Error(...)` sites and the `supervisor.start()` context severance are NOT changed; the root handler stays `TextHandler`.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`. `gofmt -l`, `go vet`, and the relevant `go test -race` clean before each commit.

## File Structure

- `internal/logctx/logctx.go` (new) — `With`/`From` context-logger carrier. (Task 1)
- `internal/executor/cli.go` — `CLIAgent.logger(ctx)` reads `logctx.From`; remove dead `discardLogger`. (Task 2)
- `internal/engine/engine.go` — `runAgent` injects the run-scoped logger into the agent context. (Task 3)
- `internal/api/handlers.go` — `handleCreateRun` emits the `run submitted` bridge line. (Task 4)

---

### Task 1: `internal/logctx` package

**Files:**
- Create: `internal/logctx/logctx.go`
- Test: `internal/logctx/logctx_test.go`

**Interfaces:**
- Produces: `logctx.With(ctx context.Context, log *slog.Logger) context.Context` and `logctx.From(ctx context.Context) *slog.Logger` (never nil) — consumed by Tasks 2 and 3.

- [ ] **Step 1: Write the failing tests**

Create `internal/logctx/logctx_test.go`:

```go
package logctx

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

func TestWithFromRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := slog.New(slog.NewTextHandler(&buf, nil))
	if got := From(With(context.Background(), want)); got != want {
		t.Fatal("From did not return the logger stored by With")
	}
}

func TestFromBareContextIsUsableNotNil(t *testing.T) {
	got := From(context.Background())
	if got == nil {
		t.Fatal("From(bare ctx) returned nil; must return a usable discard logger")
	}
	got.Info("must not panic") // writes to the discard handler
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/logctx/`
Expected: compile failure — `undefined: With` / `undefined: From` (the package does not exist yet).

- [ ] **Step 3: Implement the package**

Create `internal/logctx/logctx.go`:

```go
// Package logctx carries a *slog.Logger on a context so deep callers (the engine's
// agent seam, the executor) can log under a run-scoped logger without threading it
// through every function signature.
package logctx

import (
	"context"
	"io"
	"log/slog"
)

type ctxKey struct{}

// discard is returned by From when no logger is set, so callers never nil-check.
var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// With returns a context carrying log, retrievable by From.
func With(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, log)
}

// From returns the logger stored by With, or a no-op discard logger if none is
// set. It never returns nil.
func From(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && log != nil {
		return log
	}
	return discard
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/logctx/`
Expected: PASS (both tests).

- [ ] **Step 5: Verify formatting and vet**

Run: `gofmt -l internal/logctx/ && go vet ./internal/logctx/`
Expected: no `gofmt -l` output; `go vet` clean.

- [ ] **Step 6: Commit**

```bash
git add internal/logctx/logctx.go internal/logctx/logctx_test.go
git commit -m "feat(logctx): context-carried slog.Logger"
```

---

### Task 2: `CLIAgent` reads the context logger

**Files:**
- Modify: `internal/executor/cli.go`
- Test: `internal/executor/cli_test.go`

**Interfaces:**
- Consumes: `logctx.From` (Task 1).
- Produces: `(*CLIAgent).logger(ctx context.Context) *slog.Logger` (internal; resolves explicit `Log` → context logger → discard).

- [ ] **Step 1: Write the failing tests**

In `internal/executor/cli_test.go` (package `executor`), add `"bytes"`, `"log/slog"`, and `"concentus/internal/logctx"` to the import block (`context`, `strings`, `testing` are already imported), then add:

```go
func TestCLIAgentLoggerPrefersExplicitLog(t *testing.T) {
	var buf bytes.Buffer
	a := &CLIAgent{Log: slog.New(slog.NewTextHandler(&buf, nil))}
	a.logger(context.Background()).Info("hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Fatal("logger should use the explicit Log when set")
	}
}

func TestCLIAgentLoggerFallsBackToContext(t *testing.T) {
	var buf bytes.Buffer
	ctx := logctx.With(context.Background(), slog.New(slog.NewTextHandler(&buf, nil)))
	a := &CLIAgent{} // Log nil
	a.logger(ctx).Info("hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Fatal("logger should fall back to the context logger when Log is nil")
	}
}

func TestCLIAgentLoggerNeverNil(t *testing.T) {
	a := &CLIAgent{}
	a.logger(context.Background()).Info("noop") // discard logger; must not panic
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/executor/ -run TestCLIAgentLogger`
Expected: compile failure — `too many arguments in call to a.logger` (the method currently takes no parameter).

- [ ] **Step 3: Change `logger` to read the context logger and drop the dead `discardLogger`**

In `internal/executor/cli.go`:

1. Add `"concentus/internal/logctx"` to the import block.
2. Delete the package-level line `var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))` (it becomes unused; `io` stays imported because `CLISpec.Parse` uses `io.Reader`).
3. Replace the `logger` helper:

```go
func (a *CLIAgent) logger(ctx context.Context) *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return logctx.From(ctx)
}
```

4. Update the one call site (the artifact-discovery warning) from `a.logger().Warn(...)` to `a.logger(ctx).Warn(...)`:

```go
		a.logger(ctx).Warn("artifact discovery failed", "step", t.StepID, "err", derr)
```

(`ctx` is the parameter of `CLIAgent.Run`, in scope at that line.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/executor/`
Expected: PASS — the three new tests plus the existing executor suite (stub-CLI tests unaffected).

- [ ] **Step 5: Verify formatting and vet**

Run: `gofmt -l internal/executor/ && go vet ./internal/executor/`
Expected: no `gofmt -l` output; `go vet` clean.

- [ ] **Step 6: Commit**

```bash
git add internal/executor/cli.go internal/executor/cli_test.go
git commit -m "feat(executor): CLIAgent logs via context logger"
```

---

### Task 3: engine injects the run-scoped logger at the `runAgent` seam

**Files:**
- Modify: `internal/engine/engine.go` (`runAgent`)
- Test: `internal/engine/logctx_inject_test.go` (new)

**Interfaces:**
- Consumes: `logctx.With` (Task 1). The injected logger carries fields `run`, `step`, `agent`.

- [ ] **Step 1: Write the failing test**

Create `internal/engine/logctx_inject_test.go` (package `engine`):

```go
package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"concentus/internal/core"
	"concentus/internal/logctx"
)

// ctxLogExec logs via the context logger so the test can assert the engine
// injected a run-scoped logger before invoking the agent.
type ctxLogExec struct{}

func (ctxLogExec) Run(ctx context.Context, t core.Task) (core.Result, error) {
	logctx.From(ctx).Info("agent ran")
	return core.Result{StepID: t.StepID, Summary: "ok"}, nil
}

func TestRunAgentInjectsScopedLogger(t *testing.T) {
	var buf bytes.Buffer
	// Metrics is left nil — runAgent's ObserveAgentRun/AddCost both nil-guard the
	// receiver; Bus/Store are unused because ctxLogExec never emits.
	e := &Engine{
		Execs: map[string]core.Executor{"mock": ctxLogExec{}},
		Log:   slog.New(slog.NewTextHandler(&buf, nil)),
		Clock: core.SystemClock{},
	}
	if _, err := e.runAgent(context.Background(), "run-1", "step-a", "impl", "mock", "prompt", t.TempDir(), 1, nil); err != nil {
		t.Fatalf("runAgent: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"agent ran", "run=run-1", "step=step-a", "agent=mock"} {
		if !strings.Contains(out, want) {
			t.Errorf("agent log missing %q; got: %s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/engine/ -run TestRunAgentInjectsScopedLogger`
Expected: FAIL — `runAgent` passes the un-enriched `ctx` to `ag.Run`, so `logctx.From(ctx)` in the stub is the discard logger; `"agent ran"` (and the `run=`/`step=`/`agent=` fields) never reach `buf`, so the assertions fail.

- [ ] **Step 3: Inject the run-scoped logger in `runAgent`**

In `internal/engine/engine.go`:

1. Add `"concentus/internal/logctx"` to the import block.
2. In `runAgent`, immediately before `agentStart := e.Clock.Now()`, add the enriched context, and change the `ag.Run` call to use it (leaving the `emit` closure's capture of the original `ctx` untouched):

```go
	agentCtx := logctx.With(ctx, e.logger().With("run", string(runID), "step", stepID, "agent", agentName))
	agentStart := e.Clock.Now()
	res, err := ag.Run(agentCtx, core.Task{
		RunID:   runID,
		StepID:  stepID,
		Role:    role,
		Prompt:  prompt,
		Inputs:  inputs,
		WorkDir: workDir,
		Emit:    emit,
```

(Only the `ag.Run(ctx, …)` argument changes to `agentCtx`; the rest of the `core.Task{…}` literal and the lines after are unchanged.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -race ./internal/engine/ -run TestRunAgentInjectsScopedLogger`
Expected: PASS — `agentCtx` carries `e.logger().With("run","run-1","step","step-a","agent","mock")`; the stub logs through it to `buf`.

- [ ] **Step 5: Run the whole engine package + verify formatting/vet**

Run: `go test -race ./internal/engine/ && gofmt -l internal/engine/ && go vet ./internal/engine/`
Expected: all engine tests PASS (the new one plus existing); no `gofmt -l` output; `go vet` clean.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/engine.go internal/engine/logctx_inject_test.go
git commit -m "feat(engine): inject run-scoped logger at the agent seam"
```

---

### Task 4: api request→run bridge line

**Files:**
- Modify: `internal/api/handlers.go` (`handleCreateRun`)
- Test: `internal/api/bridge_test.go` (new)

**Interfaces:**
- Consumes: the existing package-local `requestIDKey` (middleware.go) and `s.Sup.Submit`’s returned run-id. (Independent of Tasks 1–3.)

- [ ] **Step 1: Write the failing test**

Create `internal/api/bridge_test.go` (package `api`):

```go
package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// syncBuf is a mutex-guarded sink so the request goroutine's access log and the
// test's read don't race under -race.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestRunSubmittedBridgeLog(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	sb := &syncBuf{}
	srv.Log = slog.New(slog.NewTextHandler(sb, nil))
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	resp, err := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString(oneStepFlow))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/runs = %d, want 201", resp.StatusCode)
	}
	reqID := resp.Header.Get("X-Request-ID")
	if reqID == "" {
		t.Fatal("response missing X-Request-ID")
	}

	out := sb.String()
	if !strings.Contains(out, "run submitted") {
		t.Errorf("missing 'run submitted' bridge log; got: %s", out)
	}
	if !strings.Contains(out, "req="+reqID) {
		t.Errorf("bridge log missing req=%s; got: %s", reqID, out)
	}
	if !strings.Contains(out, "run=") {
		t.Errorf("bridge log missing run=<id>; got: %s", out)
	}
}
```

(`newServerOnly` and the `oneStepFlow` const live in `handlers_test.go`, same package. The bridge line is written synchronously inside the handler, so it is present in `sb` by the time `http.Post` returns; the async mock run logs to the engine's own discard logger, not `srv.Log`.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api/ -run TestRunSubmittedBridgeLog`
Expected: FAIL — no `run submitted` line is logged yet (`out` lacks the substring).

- [ ] **Step 3: Emit the bridge line in `handleCreateRun`**

In `internal/api/handlers.go`, in `handleCreateRun`, after the successful `Submit` (the `id, err := s.Sup.Submit(...)` block) and before `writeJSON(w, http.StatusCreated, runResponse{ID: id})`, add:

```go
	reqID, _ := r.Context().Value(requestIDKey).(string)
	s.Log.Info("run submitted", "req", reqID, "run", string(id))
```

(`requestIDKey` is defined in `middleware.go`, same `package api`. `s.Log` is used directly elsewhere, e.g. `sse.go`.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -race ./internal/api/ -run TestRunSubmittedBridgeLog`
Expected: PASS — `out` contains `run submitted` with `req=<X-Request-ID>` and `run=<id>`.

- [ ] **Step 5: Run the whole suite + verify formatting/vet**

Run: `go test -race ./... && gofmt -l internal cmd && go vet ./...`
Expected: ALL packages PASS (report the count). No `gofmt -l` output; `go vet` clean.

- [ ] **Step 6: Commit**

```bash
git add internal/api/handlers.go internal/api/bridge_test.go
git commit -m "feat(api): log run-submitted bridging request-id and run-id"
```

---

## Notes for the implementer

- `logctx.From` must never return nil — callers (`CLIAgent.logger`, the engine stub) rely on it being directly callable.
- In Task 3, inject into a NEW `agentCtx` variable passed only to `ag.Run`; do not reassign `ctx`, because the `emit` closure (defined just above) captures `ctx` and uses `context.WithoutCancel(ctx)` — leaving it untouched keeps event persistence byte-for-byte unchanged.
- In Task 2, after deleting `discardLogger`, confirm `io` is still imported (it is — `CLISpec.Parse(stdout io.Reader, …)` uses it). Do not remove the `io` import.
- Do NOT change the engine's existing `e.logger().Error(...)` sites, `supervisor.start()`, the root `TextHandler`, or `event.Event` — all out of scope.
- The post-Edit hook emits a harmless path-doubling error on worktree edits; the edit still succeeds.
