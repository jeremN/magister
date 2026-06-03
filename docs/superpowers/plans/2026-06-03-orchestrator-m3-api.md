# Orchestrator M3 (HTTP/SSE API + `cm` client + daemon) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the engine+store into a running service — a `magisterd` daemon exposing an HTTP/JSON + SSE API (with `Last-Event-ID` replay), a `cm` CLI client, real blocking manual-gate approvals (Supervisor + ApprovalRegistry), and resume-on-startup — so `cm run flows/feature-flow.yaml --watch` drives a flow to completion against the daemon, a manual gate blocks until `cm approve`, and killing+restarting the daemon resumes incomplete runs.

**Architecture:** Adapters over the unchanged engine/store. A new `internal/supervisor` owns active runs (cancelation), the `ApprovalRegistry`, and the `RegistryApprover` (the API-backed `gate.Approver` that blocks a manual gate until resolved); it drives `engine.Run`/`engine.Resume` in goroutines. A new `internal/api` is stdlib `net/http` (Go 1.22 `ServeMux` patterns) — handlers, a middleware chain (`slog` logging, recovery, bearer auth, timeouts), and the SSE hub whose **content always comes from `store.EventsSince` (real seqs)** while the in-memory `event.Bus` is only a wakeup. `cmd/magisterd` wires it all (open SQLite store, build engine with a real `slog.Logger`, resume incomplete runs, graceful shutdown on SIGTERM); `cmd/cm` is a thin HTTP client. The engine gains a small change: before a **blocking** gate (manual/conditional) it persists `StepAwaitingGate` + emits a `gate.awaiting` event, so the wait is observable and survives restart (resume re-runs the awaiting step under the existing at-least-once model — no special resume path needed).

**Tech Stack:** Go 1.22, stdlib `net/http`/`log/slog`/`os/signal`/`crypto/subtle`/`flag`. New dep: `github.com/oklog/ulid/v2 v2.1.1` (sortable run IDs; declares go 1.15, so no pin gymnastics). Tests use `net/http/httptest` and run under `-race`.

**Spec:** `docs/superpowers/specs/2026-06-02-orchestrator-design.md` — §3 (two binaries, runtime flow), §5 (Supervisor/ApprovalRegistry), §9 (API + security), §10 (CLI), §11 (errors/slog).

---

## Conventions for the implementer (read once)

- **Module is `concentus`, `go 1.22`.** Do NOT raise the `go` directive. `oklog/ulid/v2` is go-1.15 — safe. Existing pins (`modernc.org/sqlite v1.36.1`, `pressly/goose/v3 v3.24.1`) must not move.
- **Commits (user CLAUDE.md — strict):** single conventional-commit subject line, **no body**, **never** a `Co-Authored-By` trailer, **never** `--no-verify`. Git identity isn't global:
  ```bash
  git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "<subject>"
  ```
- **RTK hook** reformats `go`/`git` output (e.g. "Go test: N passed"). If a result looks summarized, re-run with `-v -count=1` (or `rtk proxy go test ...`) for raw lines.
- **Semgrep hook** runs. It will flag `fmt.Fprintf(w, ...)` to an `http.ResponseWriter` in the SSE handler as CWE-79 (XSS). **This is an accepted false positive** — SSE streams `text/event-stream` (not HTML), the content is server-generated event JSON, and the trust boundary is loopback (§9). Document it in-code with a short comment (like the accepted `sh -c` in `gate/verifier.go`); do not switch to `html/template` (wrong tool for SSE). It may also flag the bearer-token compare — we use `crypto/subtle.ConstantTimeCompare`, which is correct; note it if flagged.
- **Security stance (§9):** trust boundary = the loopback interface. Default bind `127.0.0.1`. Optional static **bearer token** (constant-time compare) for non-loopback binds. No cookies → no CSRF surface. `http.MaxBytesReader` on bodies; server read/write/idle timeouts; graceful shutdown; sane security headers even without a browser surface.
- This is **TDD**. Run `go test -race ./...` green before every commit. Run `go vet ./...` clean.

## Dependency rule (preserved + extended)

`flow`/`event` import nothing internal; `core` imports only `flow`+`event`; adapters import `core`/`flow`/`event`; `engine` additionally imports `gate`+`join`. **New:** `supervisor` imports `engine`+`gate`+`store`?-no (store only via the `core.Store` interface)+`core`+`flow`+`event`+`ulid`; `api` imports `supervisor`+`core`+`flow`+`event` (and `store` only for its concrete type at wiring time — handlers use the `core.Store` interface); `config` imports nothing internal; `cmd/magisterd` wires everything; `cmd/cm` imports only stdlib + `core`/`event` for DTO shapes if needed. **No package imports `engine` except `supervisor` and the daemon.** No cycles.

---

## File structure

```
concentus-magister/
├── go.mod / go.sum                     # MODIFY: + oklog/ulid/v2 (Task 3)
├── internal/
│   ├── event/event.go                  # MODIFY: + GateAwaiting kind (Task 1)
│   ├── gate/
│   │   ├── approver.go                 # MODIFY: Approve gains runID (Task 1)
│   │   ├── gate.go                     # MODIFY: Evaluate gains runID (Task 1)
│   │   └── gate_test.go                # MODIFY: fixedApprover + Evaluate calls (Task 1)
│   ├── engine/
│   │   ├── engine.go                   # MODIFY: emit awaiting_gate; pass runID to Evaluate (Task 2)
│   │   └── engine_test.go              # MODIFY: blocking-gate test (Task 2)
│   ├── supervisor/
│   │   ├── approval.go                 # CREATE: ApprovalRegistry + Decision (Task 4)
│   │   ├── approver.go                 # CREATE: RegistryApprover impl of gate.Approver (Task 5)
│   │   ├── supervisor.go               # CREATE: Supervisor + ulid run IDs (Task 6)
│   │   └── *_test.go
│   ├── config/
│   │   ├── config.go                   # CREATE: flags/env (Task 7)
│   │   └── config_test.go
│   └── api/
│       ├── dto.go                      # CREATE: request/response shapes (Task 8)
│       ├── middleware.go               # CREATE: requestID/log/recover/auth/timeout (Task 9)
│       ├── handlers.go                 # CREATE: Server + run/approve/health handlers (Task 10)
│       ├── sse.go                      # CREATE: SSE replay handler (Task 11)
│       ├── router.go                   # CREATE: ServeMux wiring (Task 12)
│       └── *_test.go
├── cmd/
│   ├── magisterd/main.go               # CREATE: wiring + resume + shutdown (Task 13)
│   └── cm/main.go                      # CREATE: HTTP client + subcommands (Task 14)
└── (e2e test lives in internal/api or a new internal/integration — Task 15)
```

---

## Task 0: branch off `main`

- [ ] **Step 1: Confirm clean tree on `main`**

Run: `git status --short && git branch --show-current`
Expected: clean, `main`.

- [ ] **Step 2: Create and switch to the feature branch**

Run: `git switch -c feat/m3-api`
Expected: `Switched to a new branch 'feat/m3-api'`. All task commits land here.

---

## Task 1: thread `runID` through the gate; add `gate.awaiting` event kind

The API-backed approver must key its pending-approval registry by `(runID, stepID)`, but `gate.Approver.Approve` has no `runID`. Thread it through `Evaluate`→`Approve` (the `gate` package is an adapter, not frozen). Also add the `GateAwaiting` event kind the engine emits in Task 2.

**Files:**
- Modify: `internal/event/event.go`, `internal/gate/approver.go`, `internal/gate/gate.go`, `internal/gate/gate_test.go`, `internal/engine/engine.go`

- [ ] **Step 1: Add the event kind**

In `internal/event/event.go`, add to the `const ( ... )` kind block (after `StepRetrying`):
```go
	GateAwaiting Kind = "gate.awaiting"
```

- [ ] **Step 2: Update the `Approver` interface + `AutoApprover`**

In `internal/gate/approver.go`, change the interface and the `AutoApprover` method to take a `core.RunID`:
```go
// Approver resolves a manual gate. The service (M3) supplies an Approver backed
// by the API approval registry; AutoApprover backs the keyless demo and tests.
type Approver interface {
	Approve(ctx context.Context, runID core.RunID, step *flow.Step, res core.Result) (bool, error)
}

// AutoApprover passes every manual gate.
type AutoApprover struct{}

func (AutoApprover) Approve(context.Context, core.RunID, *flow.Step, core.Result) (bool, error) {
	return true, nil
}
```

- [ ] **Step 3: Update `Evaluator.Evaluate` to pass `runID`**

In `internal/gate/gate.go`, change the signature and the `Approve` call:
```go
func (e *Evaluator) Evaluate(ctx context.Context, runID core.RunID, s *flow.Step, res core.Result, workDir string) (bool, error) {
	switch s.Gate.Policy {
	case "", flow.GateManual, flow.GateConditional:
		// M1: conditional falls back to manual approval. The expr-lang evaluator arrives in M5.
		return e.Approver.Approve(ctx, runID, s, res)
	case flow.GateAuto:
		ok, err := e.Verifier.Verify(ctx, s.Gate.Verifier.Command, workDir)
		if err != nil {
			return false, fmt.Errorf("verifier error: %w", err)
		}
		return ok, nil
	default:
		return false, fmt.Errorf("step %q: unknown gate policy %q", s.ID, s.Gate.Policy)
	}
}
```

- [ ] **Step 4: Update the engine's `Evaluate` call site**

In `internal/engine/engine.go`, in `runStep`, change the gate call to pass `runID`:
```go
		ok, gerr := e.Gate.Evaluate(ctx, runID, s, res, workDir)
```
(This is the only `Evaluate` call in the engine; it's inside the `if execErr == nil {` block.)

- [ ] **Step 5: Update `gate_test.go` to the new signatures**

In `internal/gate/gate_test.go`, update the three `e.Evaluate(...)` calls to pass a run ID, and update the `fixedApprover` method signature:
```go
// in each test, the Evaluate call becomes (note the new "r1" arg):
	ok, err := e.Evaluate(context.Background(), "r1", s, core.Result{}, t.TempDir())
```
```go
func (f fixedApprover) Approve(context.Context, core.RunID, *flow.Step, core.Result) (bool, error) {
	return bool(f), nil
}
```

- [ ] **Step 6: Run to verify everything compiles + passes**

Run: `go build ./... && go test -race -count=1 ./internal/gate/ ./internal/engine/ -v 2>&1 | tail -20`
Expected: PASS — gate tests and all engine tests stay green (the engine behavior is unchanged in this task; only the signature threads through).

- [ ] **Step 7: Commit**

```bash
git add internal/event/event.go internal/gate/ internal/engine/engine.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "refactor(gate): thread runID through approver; add gate.awaiting kind"
```

---

## Task 2: engine emits `awaiting_gate` + `gate.awaiting` before a blocking gate

Before a **blocking** gate (manual/conditional — auto gates never block), the engine persists `StepAwaitingGate` (with the provisional result, so a snapshot/SSE shows the wait and resume has the artifacts) and emits a `gate.awaiting` event, then calls the (possibly-blocking) `Evaluate`. Resume needs no change: an `awaiting_gate` step is non-`succeeded`, so the existing `runDAG` re-runs it (re-execute + re-block) under at-least-once (spec §7).

**Files:**
- Modify: `internal/engine/engine.go`
- Test: `internal/engine/engine_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/engine/engine_test.go`:
```go
// blockingApprover blocks Approve until the test sends a decision, so we can
// assert the step is observably awaiting_gate before it resolves.
type blockingApprover struct {
	gate  chan bool // test sends the approve/reject decision
	await chan struct{}
}

func (b *blockingApprover) Approve(ctx context.Context, _ core.RunID, _ *flow.Step, _ core.Result) (bool, error) {
	close(b.await) // signal that we've entered the gate
	select {
	case ok := <-b.gate:
		return ok, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestEngineEmitsAwaitingGateAndBlocks(t *testing.T) {
	st := store.NewMem()
	bus := event.NewBus()
	ch, unsub := bus.Subscribe(32)
	defer unsub()
	ba := &blockingApprover{gate: make(chan bool, 1), await: make(chan struct{})}

	e := &Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: ba, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: core.SystemClock{},
	}
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	if err := st.CreateRun(context.Background(), core.RunState{ID: "r1", Name: "f", FlowYAML: "x", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- e.Run(context.Background(), "r1", f) }()

	<-ba.await // the step has entered the gate
	// the step must be persisted as awaiting_gate while blocked
	got, err := st.GetRun(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Steps) != 1 || got.Steps[0].Status != core.StepAwaitingGate {
		t.Fatalf("step should be awaiting_gate while blocked, got %+v", got.Steps)
	}

	ba.gate <- true // approve
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	final, _ := st.GetRun(context.Background(), "r1")
	if final.Steps[0].Status != core.StepSucceeded {
		t.Errorf("approved step should be succeeded, got %s", final.Steps[0].Status)
	}

	// a gate.awaiting frame must have been published
	unsub()
	var sawAwaiting bool
	for ev := range ch {
		if ev.Kind == event.GateAwaiting {
			sawAwaiting = true
		}
	}
	if !sawAwaiting {
		t.Error("expected a gate.awaiting event")
	}
}
```
(Imports `executor`, `gate`, `join`, `workspace` are already imported by `engine_test.go`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestEngineEmitsAwaitingGateAndBlocks -v`
Expected: FAIL — the step is persisted as `running` (not `awaiting_gate`) and no `gate.awaiting` event is emitted.

- [ ] **Step 3: Add the awaiting-gate emission**

In `internal/engine/engine.go`, add a helper near `gatePolicyOf`:
```go
// gateBlocks reports whether a step's gate can block on human approval. Auto
// gates resolve synchronously via the verifier and never block.
func gateBlocks(s *flow.Step) bool {
	switch gatePolicyOf(s) {
	case flow.GateManual, flow.GateConditional:
		return true
	default:
		return false
	}
}
```
Then in `runStep`, inside the `if execErr == nil {` block, **before** the `Evaluate` call, emit the awaiting transition for blocking gates:
```go
		res, execErr := e.execute(ctx, runID, s, inputs, workDir)
		if execErr == nil {
			res.StepID = s.ID
			if gateBlocks(s) {
				e.transition(ctx, runID, stepState(runID, s.ID, core.StepAwaitingGate, attempt, workDir, res, nil),
					event.Event{StepID: s.ID, Kind: event.GateAwaiting, Attempt: attempt})
			}
			ok, gerr := e.Gate.Evaluate(ctx, runID, s, res, workDir)
			switch {
			case gerr != nil:
				execErr = gerr
			case !ok:
				execErr = fmt.Errorf("gate failed (policy=%q)", gatePolicyOf(s))
			default:
				e.transition(ctx, runID, stepState(runID, s.ID, core.StepSucceeded, attempt, workDir, res, nil),
					event.Event{StepID: s.ID, Kind: event.StepDone, Summary: res.Summary, CostUSD: res.CostUSD, Attempt: attempt})
				return res, nil
			}
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -count=1 ./internal/engine/ -v 2>&1 | tail -25`
Expected: PASS — the new blocking-gate test plus all existing engine tests (the existing ones only assert run/step status + bookend events, so the extra `gate.awaiting` frames don't break them).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(engine): persist awaiting_gate and emit gate.awaiting"
```

---

## Task 3: add `oklog/ulid/v2` (sortable run IDs)

**Files:** Modify `go.mod`, `go.sum`.

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/oklog/ulid/v2@v2.1.1`
Expected: `go.mod` gains it; the `go 1.22` line is unchanged.

- [ ] **Step 2: Verify the go directive didn't move**

Run: `grep '^go ' go.mod && go list -m -f '{{.Path}} {{.GoVersion}}' all | awk '$2 != "" { split($2,a,"."); if (a[1]>1 || (a[1]==1 && a[2]>22)) print "TOO NEW:", $0 }'`
Expected: `go 1.22`, and the awk prints nothing.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "chore(deps): add oklog/ulid for run IDs"
```

---

## Task 4: `supervisor.ApprovalRegistry`

A daemon-level registry of pending manual-gate approvals, keyed by `(runID, stepID)`. The blocking approver (Task 5) `Await`s a decision; the approve endpoint (Task 10) `Resolve`s it.

**Files:**
- Create: `internal/supervisor/approval.go`
- Test: `internal/supervisor/approval_test.go`

- [ ] **Step 1: Write the failing test**

`internal/supervisor/approval_test.go`:
```go
package supervisor

import "testing"

func TestApprovalRegistryResolveDeliversDecision(t *testing.T) {
	r := NewApprovalRegistry()
	ch := r.Await("run1", "stepA")

	if !r.Resolve("run1", "stepA", Decision{Approved: true, Reason: "ok"}) {
		t.Fatal("Resolve should find the pending approval")
	}
	d := <-ch
	if !d.Approved || d.Reason != "ok" {
		t.Fatalf("wrong decision: %+v", d)
	}
	// resolving again finds nothing (it was consumed)
	if r.Resolve("run1", "stepA", Decision{Approved: true}) {
		t.Error("second Resolve should report no pending approval")
	}
}

func TestApprovalRegistryResolveUnknownReturnsFalse(t *testing.T) {
	r := NewApprovalRegistry()
	if r.Resolve("nope", "nope", Decision{Approved: true}) {
		t.Error("Resolve of an unregistered key must return false")
	}
}

func TestApprovalRegistryCancelRemoves(t *testing.T) {
	r := NewApprovalRegistry()
	_ = r.Await("r", "s")
	r.Cancel("r", "s")
	if r.Resolve("r", "s", Decision{Approved: true}) {
		t.Error("a canceled approval must not resolve")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/supervisor/ -run TestApprovalRegistry -v`
Expected: FAIL — `NewApprovalRegistry`/`Decision` undefined.

- [ ] **Step 3: Write the implementation**

`internal/supervisor/approval.go`:
```go
// Package supervisor owns the daemon's active runs, the pending-approval
// registry for blocking manual gates, and drives engine.Run/Resume.
package supervisor

import (
	"sync"

	"concentus/internal/core"
)

// Decision is the outcome of a human gate approval.
type Decision struct {
	Approved bool
	Reason   string
}

// ApprovalRegistry tracks manual gates blocked awaiting a human decision,
// keyed by (run, step). The blocking approver Awaits; the API Resolves.
type ApprovalRegistry struct {
	mu      sync.Mutex
	pending map[string]chan Decision
}

func NewApprovalRegistry() *ApprovalRegistry {
	return &ApprovalRegistry{pending: make(map[string]chan Decision)}
}

func approvalKey(runID core.RunID, stepID string) string {
	return string(runID) + "\x00" + stepID
}

// Await registers a pending approval and returns the channel its decision will
// arrive on (buffered, so Resolve never blocks). One waiter per (run,step).
func (r *ApprovalRegistry) Await(runID core.RunID, stepID string) <-chan Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan Decision, 1)
	r.pending[approvalKey(runID, stepID)] = ch
	return ch
}

// Resolve delivers a decision to a waiting gate. Returns false if no gate is
// awaiting (unknown run/step, or already resolved/canceled).
func (r *ApprovalRegistry) Resolve(runID core.RunID, stepID string, d Decision) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := approvalKey(runID, stepID)
	ch, ok := r.pending[k]
	if !ok {
		return false
	}
	delete(r.pending, k)
	ch <- d
	return true
}

// Cancel drops a pending approval without resolving it (run canceled/shutdown).
func (r *ApprovalRegistry) Cancel(runID core.RunID, stepID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, approvalKey(runID, stepID))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -count=1 ./internal/supervisor/ -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/supervisor/approval.go internal/supervisor/approval_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(supervisor): add approval registry"
```

---

## Task 5: `supervisor.RegistryApprover` (the blocking `gate.Approver`)

Implements `gate.Approver` by registering a pending approval and blocking until it's resolved or the context is canceled.

**Files:**
- Create: `internal/supervisor/approver.go`
- Test: `internal/supervisor/approver_test.go`

- [ ] **Step 1: Write the failing test**

`internal/supervisor/approver_test.go`:
```go
package supervisor

import (
	"context"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/flow"
	"concentus/internal/gate"
)

var _ gate.Approver = (*RegistryApprover)(nil)

func TestRegistryApproverBlocksUntilResolved(t *testing.T) {
	reg := NewApprovalRegistry()
	a := &RegistryApprover{Reg: reg}
	step := &flow.Step{ID: "s"}

	res := make(chan bool, 1)
	go func() {
		ok, err := a.Approve(context.Background(), "r1", step, core.Result{})
		if err != nil {
			t.Errorf("approve: %v", err)
		}
		res <- ok
	}()

	// give the goroutine time to register, then resolve
	waitFor(t, func() bool { return reg.Resolve("r1", "s", Decision{Approved: true}) })
	select {
	case ok := <-res:
		if !ok {
			t.Error("expected approval")
		}
	case <-time.After(time.Second):
		t.Fatal("Approve did not return after Resolve")
	}
}

func TestRegistryApproverContextCancelUnblocks(t *testing.T) {
	a := &RegistryApprover{Reg: NewApprovalRegistry()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Approve(ctx, "r1", &flow.Step{ID: "s"}, core.Result{}); err == nil {
		t.Error("expected context error when canceled")
	}
}

// waitFor retries fn until true or a timeout (fn has a side effect we want once).
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/supervisor/ -run TestRegistryApprover -v`
Expected: FAIL — `RegistryApprover` undefined.

- [ ] **Step 3: Write the implementation**

`internal/supervisor/approver.go`:
```go
package supervisor

import (
	"context"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// RegistryApprover is the API-backed gate.Approver: a manual gate blocks here
// until a human resolves it via the API (Resolve on the registry) or the run's
// context is canceled.
type RegistryApprover struct {
	Reg *ApprovalRegistry
}

func (a *RegistryApprover) Approve(ctx context.Context, runID core.RunID, step *flow.Step, _ core.Result) (bool, error) {
	ch := a.Reg.Await(runID, step.ID)
	select {
	case d := <-ch:
		return d.Approved, nil
	case <-ctx.Done():
		a.Reg.Cancel(runID, step.ID)
		return false, ctx.Err()
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -count=1 ./internal/supervisor/ -v`
Expected: PASS (all supervisor tests).

- [ ] **Step 5: Commit**

```bash
git add internal/supervisor/approver.go internal/supervisor/approver_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(supervisor): add blocking registry approver"
```

---

## Task 6: `supervisor.Supervisor` (runs, submit, cancel, approve, resume, shutdown)

Owns active runs (for cancellation), generates ULID run IDs, persists+starts new runs, drives `engine.Run`/`engine.Resume` in goroutines, and shuts them down gracefully.

**Files:**
- Create: `internal/supervisor/supervisor.go`
- Test: `internal/supervisor/supervisor_test.go`

- [ ] **Step 1: Write the failing test**

`internal/supervisor/supervisor_test.go`:
```go
package supervisor

import (
	"context"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/engine"
	"concentus/internal/event"
	"concentus/internal/executor"
	"concentus/internal/flow"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

func testEngine(t *testing.T, st core.Store, reg *ApprovalRegistry) *engine.Engine {
	t.Helper()
	return &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: &RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: event.NewBus(), Clock: core.SystemClock{},
	}
}

func TestSupervisorSubmitRunsToCompletion(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg), st, reg)

	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	id, err := sup.Submit(context.Background(), f, "name: f\n")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected a run ID")
	}

	// the manual gate blocks; approve it, then the run completes
	waitFor(t, func() bool { return sup.Approve(id, "a", true, "") })
	waitForStatus(t, st, id, core.RunSucceeded)
	sup.Shutdown(time.Second)
}

func TestSupervisorCancelStopsRun(t *testing.T) {
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg), st, reg)
	f := &flow.Flow{Name: "f", Steps: []*flow.Step{
		{ID: "a", Agent: "mock", Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	id, _ := sup.Submit(context.Background(), f, "x")
	// the gate is blocking; cancel the run
	waitFor(t, func() bool { return sup.Cancel(id) })
	waitForStatus(t, st, id, core.RunCanceled)
	sup.Shutdown(time.Second)
}

func waitForStatus(t *testing.T, st core.Store, id core.RunID, want core.RunStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := st.GetRun(context.Background(), id); err == nil && r.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never reached status %s", id, want)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/supervisor/ -run TestSupervisor -v`
Expected: FAIL — `New`/`Supervisor` undefined.

- [ ] **Step 3: Write the implementation**

`internal/supervisor/supervisor.go`:
```go
package supervisor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"concentus/internal/core"
	"concentus/internal/engine"
	"concentus/internal/flow"
)

// Supervisor owns all active runs: it persists+starts new ones, cancels them,
// routes gate approvals, resumes incomplete runs on startup, and drains on
// shutdown. The engine is stateless config shared across runs.
type Supervisor struct {
	engine *engine.Engine
	store  core.Store
	reg    *ApprovalRegistry

	mu   sync.Mutex
	runs map[core.RunID]context.CancelFunc
	wg   sync.WaitGroup
}

func New(eng *engine.Engine, store core.Store, reg *ApprovalRegistry) *Supervisor {
	return &Supervisor{
		engine: eng, store: store, reg: reg,
		runs: make(map[core.RunID]context.CancelFunc),
	}
}

// NewRunID returns a fresh sortable run ID.
func NewRunID() core.RunID { return core.RunID(ulid.Make().String()) }

// Submit validates is the caller's job; this persists a pending run and starts it.
func (s *Supervisor) Submit(ctx context.Context, f *flow.Flow, flowYAML string) (core.RunID, error) {
	id := NewRunID()
	if err := s.store.CreateRun(ctx, core.RunState{
		ID: id, Name: f.Name, FlowYAML: flowYAML, Status: core.RunPending, Concurrency: f.Concurrency,
	}); err != nil {
		return "", fmt.Errorf("create run: %w", err)
	}
	s.start(id, func(runCtx context.Context) error { return s.engine.Run(runCtx, id, f) })
	return id, nil
}

// start launches a run goroutine under a cancelable context registered for
// cancellation and shutdown.
func (s *Supervisor) start(id core.RunID, run func(context.Context) error) {
	runCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.runs[id] = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.runs, id)
			s.mu.Unlock()
			cancel()
		}()
		_ = run(runCtx) // terminal status is persisted by the engine
	}()
}

// Cancel cancels an active run. Returns false if the run isn't active.
func (s *Supervisor) Cancel(id core.RunID) bool {
	s.mu.Lock()
	cancel, ok := s.runs[id]
	s.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// Approve resolves a pending manual gate. Returns false if nothing is awaiting.
func (s *Supervisor) Approve(id core.RunID, stepID string, approved bool, reason string) bool {
	return s.reg.Resolve(id, stepID, Decision{Approved: approved, Reason: reason})
}

// ResumeAll loads incomplete runs from the store and resumes each (startup).
func (s *Supervisor) ResumeAll(ctx context.Context) error {
	runs, err := s.store.LoadIncompleteRuns(ctx)
	if err != nil {
		return fmt.Errorf("load incomplete runs: %w", err)
	}
	for _, rs := range runs {
		f, err := flow.ParseBytes([]byte(rs.FlowYAML))
		if err != nil {
			return fmt.Errorf("resume run %s: parse: %w", rs.ID, err)
		}
		if err := flow.Validate(f); err != nil {
			return fmt.Errorf("resume run %s: validate: %w", rs.ID, err)
		}
		rs := rs
		s.start(rs.ID, func(runCtx context.Context) error { return s.engine.Resume(runCtx, rs, f) })
	}
	return nil
}

// Shutdown cancels all active runs and waits for them to unwind, up to timeout.
func (s *Supervisor) Shutdown(timeout time.Duration) {
	s.mu.Lock()
	for _, cancel := range s.runs {
		cancel()
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -count=1 ./internal/supervisor/ -v 2>&1 | tail -20`
Expected: PASS — submit-runs-to-completion (gate blocks, approve, succeeds) and cancel-stops-run.

- [ ] **Step 5: Commit**

```bash
git add internal/supervisor/supervisor.go internal/supervisor/supervisor_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(supervisor): add run supervisor with resume and shutdown"
```

---

## Task 7: `internal/config`

Flag/env config for the daemon. Loopback default; optional bearer token.

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

`internal/config/config_test.go`:
```go
package config

import (
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c := Parse(nil, func(string) string { return "" })
	if c.Addr != "127.0.0.1:8080" {
		t.Errorf("default addr = %q, want loopback", c.Addr)
	}
	if c.DBPath != "magister.db" {
		t.Errorf("default db = %q", c.DBPath)
	}
	if c.BearerToken != "" {
		t.Errorf("bearer should be empty by default")
	}
	if c.ShutdownTimeout != 10*time.Second {
		t.Errorf("shutdown timeout = %v", c.ShutdownTimeout)
	}
}

func TestFlagsOverrideDefaults(t *testing.T) {
	c := Parse([]string{"-addr", ":9999", "-db", "/tmp/x.db"}, func(string) string { return "" })
	if c.Addr != ":9999" || c.DBPath != "/tmp/x.db" {
		t.Errorf("flags not applied: %+v", c)
	}
}

func TestEnvSuppliesBearer(t *testing.T) {
	env := func(k string) string {
		if k == "MAGISTER_BEARER_TOKEN" {
			return "secret"
		}
		return ""
	}
	c := Parse(nil, env)
	if c.BearerToken != "secret" {
		t.Errorf("bearer from env not applied: %q", c.BearerToken)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -v`
Expected: FAIL — `Parse`/`Config` undefined.

- [ ] **Step 3: Write the implementation**

`internal/config/config.go`:
```go
// Package config loads daemon configuration from flags and environment. The
// trust boundary is the loopback interface (§9): the default bind is
// 127.0.0.1, and a bearer token is optional (recommended for non-loopback binds).
package config

import (
	"flag"
	"io"
	"time"
)

type Config struct {
	Addr            string
	DBPath          string
	BearerToken     string
	ShutdownTimeout time.Duration
}

// Parse builds a Config from args (nil = none) and an env lookup (e.g. os.Getenv).
// Env supplies secrets (the bearer token) so they don't appear in process args.
func Parse(args []string, env func(string) string) Config {
	fs := flag.NewFlagSet("magisterd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var c Config
	fs.StringVar(&c.Addr, "addr", "127.0.0.1:8080", "listen address (loopback by default)")
	fs.StringVar(&c.DBPath, "db", "magister.db", "SQLite database path")
	fs.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", 10*time.Second, "graceful shutdown deadline")
	_ = fs.Parse(args)

	c.BearerToken = env("MAGISTER_BEARER_TOKEN")
	if v := env("MAGISTER_ADDR"); v != "" && !flagSet(fs, "addr") {
		c.Addr = v
	}
	if v := env("MAGISTER_DB"); v != "" && !flagSet(fs, "db") {
		c.DBPath = v
	}
	return c
}

func flagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -count=1 ./internal/config/ -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(config): add daemon config from flags and env"
```

---

## Task 8: `api` DTOs

Request/response shapes for the JSON API. Kept separate so handlers stay thin.

**Files:**
- Create: `internal/api/dto.go`

- [ ] **Step 1: Write the implementation**

(Pure type declarations; exercised by the handler tests in Tasks 10–11. No standalone test.)

`internal/api/dto.go`:
```go
// Package api is the HTTP/SSE adapter: stdlib net/http handlers, a middleware
// chain, and the SSE hub. Trust boundary is the loopback interface (§9).
package api

import "concentus/internal/core"

// runResponse is returned from POST /v1/runs.
type runResponse struct {
	ID core.RunID `json:"id"`
}

// stepDTO is one step in a run snapshot.
type stepDTO struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	Attempt   int     `json:"attempt"`
	Summary   string  `json:"summary,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
	WorkDir   string  `json:"workdir,omitempty"`
	Error     string  `json:"error,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}

// runSnapshot is returned from GET /v1/runs/{id}.
type runSnapshot struct {
	ID          core.RunID `json:"id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Concurrency int        `json:"concurrency"`
	Error       string     `json:"error,omitempty"`
	Steps       []stepDTO  `json:"steps"`
}

// runSummaryDTO is one row in GET /v1/runs.
type runSummaryDTO struct {
	ID     core.RunID `json:"id"`
	Name   string     `json:"name"`
	Status string     `json:"status"`
}

// approveRequest is the body of POST .../approve.
type approveRequest struct {
	Approve bool   `json:"approve"`
	Reason  string `json:"reason,omitempty"`
}

// errorResponse is the uniform error envelope.
type errorResponse struct {
	Error string `json:"error"`
}

func snapshotFromState(rs core.RunState) runSnapshot {
	out := runSnapshot{
		ID: rs.ID, Name: rs.Name, Status: string(rs.Status),
		Concurrency: rs.Concurrency, Error: rs.Err,
		Steps: make([]stepDTO, 0, len(rs.Steps)),
	}
	for _, st := range rs.Steps {
		d := stepDTO{
			ID: st.StepID, Status: string(st.Status), Attempt: st.Attempt,
			Summary: st.Summary, CostUSD: st.CostUSD, WorkDir: st.WorkDir, Error: st.Err,
		}
		for _, a := range st.Artifacts {
			d.Artifacts = append(d.Artifacts, a.Path)
		}
		out.Steps = append(out.Steps, d)
	}
	return out
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./internal/api/`
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
git add internal/api/dto.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(api): add request/response DTOs"
```

---

## Task 9: `api` middleware chain

Request ID → `slog` logging → panic recovery → bearer auth → per-request timeout, plus a `writeJSON`/`writeError` helper and security headers.

**Files:**
- Create: `internal/api/middleware.go`
- Test: `internal/api/middleware_test.go`

- [ ] **Step 1: Write the failing test**

`internal/api/middleware_test.go`:
```go
package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestAuthRejectsMissingBearer(t *testing.T) {
	h := authMiddleware("secret")(okHandler())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/runs", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("missing bearer = %d, want 401", rr.Code)
	}
}

func TestAuthAcceptsCorrectBearer(t *testing.T) {
	h := authMiddleware("secret")(okHandler())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("correct bearer = %d, want 200", rr.Code)
	}
}

func TestAuthDisabledWhenNoToken(t *testing.T) {
	h := authMiddleware("")(okHandler()) // empty token => auth disabled (loopback)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/runs", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("no-token mode should allow, got %d", rr.Code)
	}
}

func TestRecoverTurnsPanicInto500(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := recoverMiddleware(log)(panicky)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("panic should map to 500, got %d", rr.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run 'TestAuth|TestRecover' -v`
Expected: FAIL — `authMiddleware`/`recoverMiddleware` undefined.

- [ ] **Step 3: Write the implementation**

`internal/api/middleware.go`:
```go
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

type ctxKey int

const requestIDKey ctxKey = 0

// chain composes middlewares so the first listed runs outermost.
func chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := ulid.Make().String()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// statusRecorder captures the status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func loggingMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			id, _ := r.Context().Value(requestIDKey).(string)
			log.Info("request",
				"id", id, "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "dur_ms", time.Since(start).Milliseconds())
		})
	}
}

func recoverMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic", "path", r.URL.Path, "value", v)
					writeError(w, http.StatusInternalServerError, "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// authMiddleware enforces a static bearer token via constant-time compare. An
// empty token disables auth (loopback trust boundary, §9).
func authMiddleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if token == "" {
			return next
		}
		want := []byte("Bearer " + token)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := []byte(r.Header.Get("Authorization"))
			if subtle.ConstantTimeCompare(got, want) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func timeoutMiddleware(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// SSE streams must not be force-timed-out; they manage their own lifetime.
			if strings.HasSuffix(r.URL.Path, "/events") {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -count=1 ./internal/api/ -run 'TestAuth|TestRecover' -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/api/middleware.go internal/api/middleware_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(api): add middleware chain"
```

---

> **Tasks 10–12 are one API cluster.** They build the `internal/api` package together: Task 10 writes the handlers + their `httptest` tests (which call `srv.Router("")`), Task 11 adds the SSE handler + tests, and **Task 12 creates `Router` — the integration point that wires every route and is where the package's tests first run green.** So the Task 10/11 tests are written test-first but are *verified at Task 12* (their "Expected: PASS" is gated on the router existing). When executing, treat 10→11→12 as a unit: commit each task's files, and run `go test ./internal/api/` to green at the end of Task 12. This is the one place the plan can't make a single task's tests pass in isolation (handlers and router are mutually referential); everywhere else is strict per-task TDD.

## Task 10: `api` handlers (runs CRUD, approve, health) + `Server`

The `Server` holds the supervisor, store, bus, and logger. Handlers are thin: validate, call supervisor/store, encode JSON.

**Files:**
- Create: `internal/api/handlers.go`
- Test: `internal/api/handlers_test.go`

- [ ] **Step 1: Write the failing test**

`internal/api/handlers_test.go`:
```go
package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/engine"
	"concentus/internal/event"
	"concentus/internal/executor"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/store"
	"concentus/internal/supervisor"
	"concentus/internal/workspace"
)

func testServer(t *testing.T) (*httptest.Server, *supervisor.Supervisor, core.Store) {
	t.Helper()
	st := store.NewMem()
	reg := supervisor.NewApprovalRegistry()
	bus := event.NewBus()
	eng := &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: &supervisor.RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: core.SystemClock{},
	}
	sup := supervisor.New(eng, st, reg)
	srv := &Server{Sup: sup, Store: st, Bus: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ShutdownTimeout: time.Second}
	hs := httptest.NewServer(srv.Router(""))
	t.Cleanup(func() { hs.Close(); sup.Shutdown(time.Second) })
	return hs, sup, st
}

const oneStepFlow = "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"true\" } }\n"

func TestPostRunCreatesAndCompletes(t *testing.T) {
	hs, _, st := testServer(t)

	resp, err := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString(oneStepFlow))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v1/runs = %d: %s", resp.StatusCode, b)
	}
	var rr runResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatal(err)
	}
	if rr.ID == "" {
		t.Fatal("no run ID returned")
	}

	// auto gate → completes without approval
	waitForStatus(t, st, rr.ID, core.RunSucceeded)

	// GET snapshot reflects it
	gresp, _ := http.Get(hs.URL + "/v1/runs/" + string(rr.ID))
	defer gresp.Body.Close()
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("GET snapshot = %d", gresp.StatusCode)
	}
	var snap runSnapshot
	json.NewDecoder(gresp.Body).Decode(&snap)
	if snap.Status != "succeeded" || len(snap.Steps) != 1 {
		t.Errorf("snapshot wrong: %+v", snap)
	}
}

func TestPostRunRejectsInvalidFlow(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, _ := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString("name: \nsteps: []\n"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid flow = %d, want 400", resp.StatusCode)
	}
}

func TestGetUnknownRun404(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, _ := http.Get(hs.URL + "/v1/runs/nope")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown run = %d, want 404", resp.StatusCode)
	}
}

func TestApproveReleasesManualGate(t *testing.T) {
	hs, _, st := testServer(t)
	manualFlow := "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: manual }\n"
	resp, _ := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString(manualFlow))
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()

	// wait until the step is awaiting_gate
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s, _ := st.GetRun(nil, rr.ID) // nil ctx ok for Mem
		if len(s.Steps) == 1 && s.Steps[0].Status == core.StepAwaitingGate {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	body, _ := json.Marshal(approveRequest{Approve: true})
	areq, _ := http.NewRequest(http.MethodPost, hs.URL+"/v1/runs/"+string(rr.ID)+"/steps/a/approve", bytes.NewReader(body))
	aresp, err := http.DefaultClient.Do(areq)
	if err != nil {
		t.Fatal(err)
	}
	aresp.Body.Close()
	if aresp.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d", aresp.StatusCode)
	}
	waitForStatus(t, st, rr.ID, core.RunSucceeded)
}

func TestHealthz(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, _ := http.Get(hs.URL + "/healthz")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}
}

func waitForStatus(t *testing.T, st core.Store, id core.RunID, want core.RunStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := st.GetRun(nil, id); err == nil && r.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %s", id, want)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run 'TestPostRun|TestGet|TestApprove|TestHealthz' -v`
Expected: FAIL — `Server`/`Router` undefined.

- [ ] **Step 3: Write the implementation**

`internal/api/handlers.go`:
```go
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/flow"
	"concentus/internal/supervisor"
)

const maxBodyBytes = 1 << 20 // 1 MiB flow uploads

// Server holds the dependencies the HTTP handlers need.
type Server struct {
	Sup             *supervisor.Supervisor
	Store           core.Store
	Bus             *event.Bus
	Log             *slog.Logger
	BearerToken     string
	ShutdownTimeout time.Duration
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "flow too large")
		return
	}
	f, err := flow.ParseBytes(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "parse flow: "+err.Error())
		return
	}
	if err := flow.Validate(f); err != nil {
		writeError(w, http.StatusBadRequest, "invalid flow: "+err.Error())
		return
	}
	id, err := s.Sup.Submit(r.Context(), f, string(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, runResponse{ID: id})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	f := core.Filter{Status: core.RunStatus(r.URL.Query().Get("status"))}
	rows, err := s.Store.ListRuns(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]runSummaryDTO, 0, len(rows))
	for _, rw := range rows {
		out = append(out, runSummaryDTO{ID: rw.ID, Name: rw.Name, Status: string(rw.Status)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	rs, err := s.Store.GetRun(r.Context(), core.RunID(r.PathValue("id")))
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown run")
		return
	}
	writeJSON(w, http.StatusOK, snapshotFromState(rs))
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if !s.Sup.Cancel(core.RunID(r.PathValue("id"))) {
		writeError(w, http.StatusNotFound, "run not active")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req approveRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := core.RunID(r.PathValue("id"))
	step := r.PathValue("step")
	if !s.Sup.Approve(id, step, req.Approve, req.Reason) {
		writeError(w, http.StatusConflict, "no gate awaiting approval for this step")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return errors.New("body too large")
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./internal/api/`
Expected: the handler code compiles. The handler tests call `srv.Router("")`, which is created in Task 12 — these tests go **green at Task 12** (see the API-cluster note above). Commit the handler code now.

- [ ] **Step 5: Commit**

```bash
git add internal/api/handlers.go internal/api/handlers_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(api): add run and approval handlers"
```

---

## Task 11: `api` SSE handler (`Last-Event-ID` replay)

The validated pattern: content always comes from `store.EventsSince` (real seqs); the `event.Bus` is a wakeup only; a ticker backstops dropped wakeups; the stream ends after the `run.done` frame or on client disconnect.

**Files:**
- Create: `internal/api/sse.go`
- Test: `internal/api/sse_test.go`

- [ ] **Step 1: Write the failing test**

`internal/api/sse_test.go`:
```go
package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"concentus/internal/core"
)

func TestSSEStreamsRunToCompletion(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString(oneStepFlow))
	if err != nil {
		t.Fatal(err)
	}
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()

	// stream events; the handler closes after run.done
	ereq, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/runs/"+string(rr.ID)+"/events", nil)
	eresp, err := http.DefaultClient.Do(ereq)
	if err != nil {
		t.Fatal(err)
	}
	defer eresp.Body.Close()

	var kinds []string
	var lastSeq int64
	sc := bufio.NewScanner(eresp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			lastSeq, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		}
		if strings.HasPrefix(line, "event: ") {
			kinds = append(kinds, strings.TrimPrefix(line, "event: "))
		}
	}
	if len(kinds) == 0 || kinds[0] != "run.started" || kinds[len(kinds)-1] != "run.done" {
		t.Fatalf("expected run.started ... run.done, got %v", kinds)
	}
	if lastSeq == 0 {
		t.Error("expected non-zero final seq for Last-Event-ID")
	}
}

func TestSSEReplayWithLastEventID(t *testing.T) {
	hs, _, st := testServer(t)
	resp, _ := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString(oneStepFlow))
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()
	waitForStatus(t, st, rr.ID, core.RunSucceeded)

	// reconnect asking for events after seq 1 → must not include seq 1
	ereq, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/runs/"+string(rr.ID)+"/events", nil)
	ereq.Header.Set("Last-Event-ID", "1")
	eresp, _ := http.DefaultClient.Do(ereq)
	defer eresp.Body.Close()
	sc := bufio.NewScanner(eresp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			seq, _ := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			if seq <= 1 {
				t.Errorf("replay returned seq %d, want only > 1", seq)
			}
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestSSE -v`
Expected: FAIL — the events route / handler isn't wired.

- [ ] **Step 3: Write the implementation**

`internal/api/sse.go`:
```go
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"concentus/internal/core"
	"concentus/internal/event"
)

// handleEvents streams a run's journal as SSE. The durable events table is the
// source of truth (real seqs); the in-memory bus is only a "re-query now"
// wakeup, with a ticker backstop for dropped (lossy) wakeups. Last-Event-ID
// (or ?since=) resumes a reconnecting client from where it left off.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	id := core.RunID(r.PathValue("id"))
	if _, err := s.Store.GetRun(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "unknown run")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	since := parseSinceCursor(r)
	sub, unsub := s.Bus.Subscribe(64)
	defer unsub()

	// drain writes all events after `since`; returns false once run.done is sent.
	drain := func() bool {
		evs, err := s.Store.EventsSince(r.Context(), id, since)
		if err != nil {
			s.Log.Error("sse events read", "run", id, "err", err)
			return false
		}
		for _, e := range evs {
			// SSE is text/event-stream, not HTML; data is server-generated event
			// JSON on a loopback trust boundary — Fprintf here is intentional.
			data, _ := json.Marshal(e)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Kind, data)
			since = e.Seq
			if e.Kind == event.RunDone {
				flusher.Flush()
				return false
			}
		}
		flusher.Flush()
		return true
	}

	if !drain() {
		return
	}
	tick := time.NewTicker(time.Second) // backstop for dropped wakeups
	defer tick.Stop()
	for {
		select {
		case <-sub: // some event happened (maybe for another run) — re-query
			if !drain() {
				return
			}
		case <-tick.C:
			if !drain() {
				return
			}
		case <-r.Context().Done(): // client disconnected
			return
		}
	}
}

func parseSinceCursor(r *http.Request) int64 {
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		if v, err := strconv.ParseInt(h, 10, 64); err == nil {
			return v
		}
	}
	if q := r.URL.Query().Get("since"); q != "" {
		if v, err := strconv.ParseInt(q, 10, 64); err == nil {
			return v
		}
	}
	return 0
}
```

- [ ] **Step 4: Verify it compiles (tests go green at Task 12)**

Run: `go build ./internal/api/`
Expected: compiles. The SSE tests call `srv.Router(...)` (created in Task 12) and exercise `GET /v1/runs/{id}/events`, so they pass at Task 12 (API-cluster note above). Commit the SSE code now.

- [ ] **Step 5: Commit**

```bash
git add internal/api/sse.go internal/api/sse_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(api): add SSE event stream with replay"
```

---

## Task 12: `api` router (`ServeMux` wiring + middleware)

Wires all routes with Go 1.22 method+wildcard patterns and applies the middleware chain. This is what makes Tasks 10–11 tests pass.

**Files:**
- Create: `internal/api/router.go`
- Test: `internal/api/router_test.go`

- [ ] **Step 1: Write the failing test**

`internal/api/router_test.go`:
```go
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouterMethodNotAllowed(t *testing.T) {
	hs, _, _ := testServer(t)
	// DELETE on /v1/runs (no {id}) isn't a route → 404/405 from ServeMux
	req, _ := http.NewRequest(http.MethodPut, hs.URL+"/v1/runs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("PUT /v1/runs should not be 200")
	}
}

func TestRouterAuthAppliesToV1(t *testing.T) {
	// a server with a bearer token rejects unauthenticated /v1 calls
	srv, _, _ := newServerOnly(t)
	srv.BearerToken = "secret"
	hs := httptest.NewServer(srv.Router(srv.BearerToken))
	defer hs.Close()
	resp, _ := http.Get(hs.URL + "/v1/runs")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated /v1/runs = %d, want 401", resp.StatusCode)
	}
	// healthz is exempt
	hresp, _ := http.Get(hs.URL + "/healthz")
	hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		t.Errorf("healthz should be exempt from auth, got %d", hresp.StatusCode)
	}
}
```
And add a `newServerOnly` helper to `handlers_test.go` (so the router test can build a `*Server` without starting an `httptest.Server`):
```go
func newServerOnly(t *testing.T) (*Server, *supervisor.Supervisor, core.Store) {
	t.Helper()
	st := store.NewMem()
	reg := supervisor.NewApprovalRegistry()
	bus := event.NewBus()
	eng := &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: &supervisor.RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: core.SystemClock{},
	}
	sup := supervisor.New(eng, st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	return &Server{Sup: sup, Store: st, Bus: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ShutdownTimeout: time.Second}, sup, st
}
```
(And change `testServer` to call `newServerOnly` then wrap with `httptest.NewServer(srv.Router(""))` — DRY.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestRouter -v`
Expected: FAIL — `Router` undefined.

- [ ] **Step 3: Write the implementation**

`internal/api/router.go`:
```go
package api

import (
	"net/http"
	"time"
)

// Router builds the HTTP handler: stdlib ServeMux with Go 1.22 method+wildcard
// patterns, wrapped in the middleware chain. /healthz is exempt from auth so
// liveness probes work without a token. token == "" disables auth (loopback).
func (s *Server) Router(token string) http.Handler {
	mux := http.NewServeMux()

	// health is mounted outside the authed group
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	v1 := http.NewServeMux()
	v1.HandleFunc("POST /v1/runs", s.handleCreateRun)
	v1.HandleFunc("GET /v1/runs", s.handleListRuns)
	v1.HandleFunc("GET /v1/runs/{id}", s.handleGetRun)
	v1.HandleFunc("DELETE /v1/runs/{id}", s.handleCancelRun)
	v1.HandleFunc("GET /v1/runs/{id}/events", s.handleEvents)
	v1.HandleFunc("POST /v1/runs/{id}/steps/{step}/approve", s.handleApprove)

	authed := chain(v1,
		authMiddleware(token),
		timeoutMiddleware(30*time.Second), // SSE is exempted inside the middleware
	)
	mux.Handle("/v1/", authed)

	return chain(mux,
		requestIDMiddleware,
		loggingMiddleware(s.Log),
		recoverMiddleware(s.Log),
		securityHeaders,
	)
}
```

- [ ] **Step 4: Run the full api suite to verify it passes**

Run: `go test -race -count=1 ./internal/api/ -v 2>&1 | tail -30`
Expected: PASS — handlers (Task 10), SSE (Task 11), middleware (Task 9), and router tests all green.

- [ ] **Step 5: Commit**

```bash
git add internal/api/router.go internal/api/router_test.go internal/api/handlers_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(api): wire router and middleware"
```

---

## Task 13: `cmd/magisterd` (daemon wiring, resume, graceful shutdown)

Wires config → SQLite store → engine (with a real `slog` logger + the registry approver) → supervisor → HTTP server; resumes incomplete runs on startup; shuts down gracefully on SIGINT/SIGTERM.

**Files:**
- Create: `cmd/magisterd/main.go`
- Test: `cmd/magisterd/main_test.go`

- [ ] **Step 1: Write the failing test**

`cmd/magisterd/main_test.go`:
```go
package main

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestRunServesHealthzAndShutsDown(t *testing.T) {
	db := filepath.Join(t.TempDir(), "m.db")
	stop := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		errc <- run([]string{"-addr", "127.0.0.1:0", "-db", db}, func(string) string { return "" }, stop, func(addr string) {
			// addr callback: hit healthz, then signal stop
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				resp, err := http.Get("http://" + addr + "/healthz")
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						close(stop)
						return
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Error("healthz never became reachable")
			close(stop)
		})
	}()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/magisterd/ -v`
Expected: FAIL — `run` undefined.

- [ ] **Step 3: Write the implementation**

`cmd/magisterd/main.go`:
```go
// Command magisterd is the orchestrator daemon: it owns the engine, the SQLite
// store, the supervisor, and the HTTP/SSE API. It resumes incomplete runs on
// startup and shuts down gracefully on SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"concentus/internal/api"
	"concentus/internal/config"
	"concentus/internal/core"
	"concentus/internal/engine"
	"concentus/internal/event"
	"concentus/internal/executor"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/store"
	"concentus/internal/supervisor"
	"concentus/internal/workspace"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopCh := make(chan struct{})
	go func() { <-ctx.Done(); close(stopCh) }()

	if err := run(os.Args[1:], os.Getenv, stopCh, nil); err != nil {
		slog.Error("magisterd exited with error", "err", err)
		os.Exit(1)
	}
}

// run is the testable daemon body. It serves until stopCh closes, then drains.
// onListen (optional) is called with the bound address once serving begins.
func run(args []string, env func(string) string, stopCh <-chan struct{}, onListen func(addr string)) error {
	cfg := config.Parse(args, env)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	reg := supervisor.NewApprovalRegistry()
	bus := event.NewBus()
	eng := &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}}, // real CLIAgents arrive in M4
		WS:    &workspace.Manager{Root: filepath.Join(filepath.Dir(cfg.DBPath), "runs")},
		Gate:  &gate.Evaluator{Approver: &supervisor.RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: core.SystemClock{}, Log: log,
	}
	sup := supervisor.New(eng, st, reg)

	if err := sup.ResumeAll(context.Background()); err != nil {
		log.Error("resume incomplete runs", "err", err)
	}

	srv := &api.Server{Sup: sup, Store: st, Bus: bus, Log: log, BearerToken: cfg.BearerToken, ShutdownTimeout: cfg.ShutdownTimeout}
	httpSrv := &http.Server{
		Handler:      srv.Router(cfg.BearerToken),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // SSE streams are long-lived
		IdleTimeout:  60 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	log.Info("listening", "addr", ln.Addr().String())
	if onListen != nil {
		go onListen(ln.Addr().String())
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		return err
	case <-stopCh:
	}

	log.Info("shutting down")
	sup.Shutdown(cfg.ShutdownTimeout) // cancel active runs first
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
```
> Run artifacts land beside the DB: `filepath.Join(filepath.Dir(cfg.DBPath), "runs")`. Real CLIAgents + isolated git-worktree workspaces arrive in M4; M3 registers only the `mock` executor so the keyless loop works end-to-end.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -count=1 ./cmd/magisterd/ -v`
Expected: PASS — the daemon serves `/healthz` then shuts down cleanly when `stopCh` closes.

- [ ] **Step 5: Commit**

```bash
git add cmd/magisterd/
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(magisterd): add daemon with resume and graceful shutdown"
```

---

## Task 14: `cmd/cm` (thin HTTP client)

Stdlib subcommand dispatch. Agent-aware: `--json` and meaningful exit codes. Subcommands: `run <flow.yaml> [--watch]`, `ls`, `get <run>`, `watch <run>`, `approve|reject <run> <step>`, `cancel <run>`.

**Files:**
- Create: `cmd/cm/main.go`
- Test: `cmd/cm/main_test.go`

- [ ] **Step 1: Write the failing test**

`cmd/cm/main_test.go`:
```go
package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeAPI is a minimal server that records the last request and returns canned JSON.
func fakeAPI(t *testing.T, status int, body string, record *http.Request) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*record = *r
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

func TestRunSubmitsFlowAndPrintsID(t *testing.T) {
	var got http.Request
	srv := fakeAPI(t, http.StatusCreated, `{"id":"01ABC"}`, &got)
	defer srv.Close()

	dir := t.TempDir()
	flowPath := dir + "/f.yaml"
	if err := os.WriteFile(flowPath, []byte("name: f\nsteps: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code := dispatch([]string{"run", flowPath}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit code = %d, out=%s", code, out.String())
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v1/runs" {
		t.Errorf("wrong request: %s %s", got.Method, got.URL.Path)
	}
	if !strings.Contains(out.String(), "01ABC") {
		t.Errorf("output missing run ID: %q", out.String())
	}
}

func TestApproveSendsApproveTrue(t *testing.T) {
	var got http.Request
	srv := fakeAPI(t, http.StatusOK, `{"status":"resolved"}`, &got)
	defer srv.Close()
	var out bytes.Buffer
	code := dispatch([]string{"approve", "01ABC", "stepA"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if got.URL.Path != "/v1/runs/01ABC/steps/stepA/approve" {
		t.Errorf("wrong path: %s", got.URL.Path)
	}
}

func TestUnknownCommandExitsNonZero(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"frobnicate"}, "http://x", &out); code == 0 {
		t.Error("unknown command should exit non-zero")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/cm/ -v`
Expected: FAIL — `dispatch` undefined.

- [ ] **Step 3: Write the implementation**

`cmd/cm/main.go`:
```go
// Command cm is the thin CLI client for magisterd: pure HTTP calls, no
// orchestration logic. Every subcommand is scriptable (--json, exit codes).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	base := os.Getenv("MAGISTER_ADDR")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	os.Exit(dispatch(os.Args[1:], base, os.Stdout))
}

func dispatch(args []string, base string, out io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(out, "usage: cm <run|ls|get|watch|approve|reject|cancel> ...")
		return 2
	}
	c := &client{base: base, http: &http.Client{Timeout: 0}}
	switch args[0] {
	case "run":
		return c.run(args[1:], out)
	case "ls":
		return c.get("/v1/runs", out)
	case "get":
		if len(args) < 2 {
			fmt.Fprintln(out, "usage: cm get <run>")
			return 2
		}
		return c.get("/v1/runs/"+args[1], out)
	case "watch":
		if len(args) < 2 {
			fmt.Fprintln(out, "usage: cm watch <run>")
			return 2
		}
		return c.watch(args[1], out)
	case "approve":
		return c.approve(args[1:], true, out)
	case "reject":
		return c.approve(args[1:], false, out)
	case "cancel":
		if len(args) < 2 {
			fmt.Fprintln(out, "usage: cm cancel <run>")
			return 2
		}
		return c.delete("/v1/runs/"+args[1], out)
	default:
		fmt.Fprintf(out, "unknown command %q\n", args[0])
		return 2
	}
}

type client struct {
	base string
	http *http.Client
}

func (c *client) run(args []string, out io.Writer) int {
	watch := false
	var path string
	for _, a := range args {
		if a == "--watch" {
			watch = true
		} else {
			path = a
		}
	}
	if path == "" {
		fmt.Fprintln(out, "usage: cm run <flow.yaml> [--watch]")
		return 2
	}
	body, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(out, "read flow:", err)
		return 1
	}
	resp, err := c.http.Post(c.base+"/v1/runs", "application/x-yaml", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(out, "submit:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return printErr(resp, out)
	}
	var rr struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&rr)
	fmt.Fprintln(out, rr.ID)
	if watch {
		return c.watch(rr.ID, out)
	}
	return 0
}

func (c *client) watch(id string, out io.Writer) int {
	resp, err := c.http.Get(c.base + "/v1/runs/" + id + "/events")
	if err != nil {
		fmt.Fprintln(out, "watch:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	_, _ = io.Copy(out, resp.Body) // stream SSE frames verbatim until run.done closes it
	return 0
}

func (c *client) approve(args []string, approve bool, out io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(out, "usage: cm approve|reject <run> <step> [reason]")
		return 2
	}
	reason := ""
	if len(args) >= 3 {
		reason = args[2]
	}
	body, _ := json.Marshal(map[string]any{"approve": approve, "reason": reason})
	url := c.base + "/v1/runs/" + args[0] + "/steps/" + args[1] + "/approve"
	resp, err := c.http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(out, "approve:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	fmt.Fprintln(out, "ok")
	return 0
}

func (c *client) get(path string, out io.Writer) int {
	resp, err := c.http.Get(c.base + path)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	_, _ = io.Copy(out, resp.Body)
	fmt.Fprintln(out)
	return 0
}

func (c *client) delete(path string, out io.Writer) int {
	req, _ := http.NewRequest(http.MethodDelete, c.base+path, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return printErr(resp, out)
	}
	fmt.Fprintln(out, "canceled")
	return 0
}

func printErr(resp *http.Response, out io.Writer) int {
	b, _ := io.ReadAll(resp.Body)
	fmt.Fprintf(out, "error (%d): %s\n", resp.StatusCode, bytes.TrimSpace(b))
	return 1
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -count=1 ./cmd/cm/ -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add cmd/cm/
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "feat(cm): add CLI client"
```

---

## Task 15: end-to-end test (submit → watch → complete; manual gate → approve; kill → resume)

Drive the full stack over HTTP against a real SQLite-backed daemon: a flow runs to completion streamed via SSE; a manual gate blocks and `cm`-style approval releases it; killing and restarting the daemon resumes an incomplete run.

**Files:**
- Create: `cmd/magisterd/e2e_test.go`

- [ ] **Step 1: Write the test**

`cmd/magisterd/e2e_test.go`:
```go
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startDaemon runs the daemon on an ephemeral port and returns its base URL + a stop func.
func startDaemon(t *testing.T, db string) (string, func()) {
	t.Helper()
	stop := make(chan struct{})
	addrCh := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run([]string{"-addr", "127.0.0.1:0", "-db", db}, func(string) string { return "" }, stop, func(addr string) { addrCh <- addr })
	}()
	var addr string
	select {
	case addr = <-addrCh:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon never reported its address")
	}
	return "http://" + addr, func() { close(stop); <-done }
}

func postFlow(t *testing.T, base, yaml string) string {
	t.Helper()
	resp, err := http.Post(base+"/v1/runs", "application/x-yaml", bytes.NewBufferString(yaml))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("submit = %d", resp.StatusCode)
	}
	var rr struct{ ID string `json:"id"` }
	json.NewDecoder(resp.Body).Decode(&rr)
	return rr.ID
}

func runStatus(t *testing.T, base, id string) string {
	resp, _ := http.Get(base + "/v1/runs/" + id)
	defer resp.Body.Close()
	var snap struct{ Status string `json:"status"` }
	json.NewDecoder(resp.Body).Decode(&snap)
	return snap.Status
}

func waitStatus(t *testing.T, base, id, want string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if runStatus(t, base, id) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %s (last=%s)", id, want, runStatus(t, base, id))
}

func TestE2EAutoFlowStreamsToCompletion(t *testing.T) {
	base, stop := startDaemon(t, filepath.Join(t.TempDir(), "e2e.db"))
	defer stop()
	id := postFlow(t, base, "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"true\" } }\n")

	resp, _ := http.Get(base + "/v1/runs/" + id + "/events")
	defer resp.Body.Close()
	var kinds []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event: ") {
			kinds = append(kinds, strings.TrimPrefix(sc.Text(), "event: "))
		}
	}
	if len(kinds) == 0 || kinds[len(kinds)-1] != "run.done" {
		t.Fatalf("stream did not end on run.done: %v", kinds)
	}
	waitStatus(t, base, id, "succeeded")
}

func TestE2EManualGateBlocksThenApprove(t *testing.T) {
	base, stop := startDaemon(t, filepath.Join(t.TempDir(), "gate.db"))
	defer stop()
	id := postFlow(t, base, "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: manual }\n")

	waitStatus(t, base, id, "running") // run is running while the step awaits the gate
	// wait until step a is awaiting_gate
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, _ := http.Get(base + "/v1/runs/" + id)
		var snap struct {
			Steps []struct{ ID, Status string } `json:"steps"`
		}
		json.NewDecoder(resp.Body).Decode(&snap)
		resp.Body.Close()
		if len(snap.Steps) == 1 && snap.Steps[0].Status == "awaiting_gate" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, _ := json.Marshal(map[string]any{"approve": true})
	resp, err := http.Post(base+"/v1/runs/"+id+"/steps/a/approve", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d", resp.StatusCode)
	}
	waitStatus(t, base, id, "succeeded")
}

func TestE2EKillAndResume(t *testing.T) {
	db := filepath.Join(t.TempDir(), "resume.db")
	base, stop := startDaemon(t, db)
	// a two-step chain; block at the manual gate on step a, then kill the daemon
	id := postFlow(t, base, "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: manual }\n  - id: b\n    needs: [a]\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"true\" } }\n")
	waitStatus(t, base, id, "running")
	stop() // "crash" the daemon while step a is awaiting the gate (run row stays running)

	// restart against the same DB → resume
	base2, stop2 := startDaemon(t, db)
	defer stop2()
	waitStatus(t, base2, id, "running") // resumed and awaiting the gate again
	// approve the re-blocked gate → run completes
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, _ := http.Get(base2 + "/v1/runs/" + id)
		var snap struct {
			Steps []struct{ ID, Status string } `json:"steps"`
		}
		json.NewDecoder(resp.Body).Decode(&snap)
		resp.Body.Close()
		awaiting := false
		for _, s := range snap.Steps {
			if s.ID == "a" && s.Status == "awaiting_gate" {
				awaiting = true
			}
		}
		if awaiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, _ := json.Marshal(map[string]any{"approve": true})
	http.Post(base2+"/v1/runs/"+id+"/steps/a/approve", "application/json", bytes.NewReader(body))
	waitStatus(t, base2, id, "succeeded")
}
```

- [ ] **Step 2: Run the e2e tests**

Run: `go test -race -count=1 ./cmd/magisterd/ -v 2>&1 | tail -25`
Expected: PASS — auto flow streams to `run.done`; manual gate blocks then completes on approve; kill+restart resumes and completes after re-approving.
> If `TestE2EKillAndResume` is flaky because the gate hadn't reached `awaiting_gate` before `stop()`, add a short poll for `awaiting_gate` (like the other tests) **before** `stop()`. The persisted run status is `running`, which is what `LoadIncompleteRuns` requires.

- [ ] **Step 3: Commit**

```bash
git add cmd/magisterd/e2e_test.go
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "test(e2e): add end-to-end api, approval, and resume tests"
```

---

## Task 16: final verification & milestone close

- [ ] **Step 1: Full race suite + vet**

Run:
```bash
go vet ./...
go test -race -count=1 ./... 2>&1 | tail -30
```
Expected: `go vet` silent; all packages PASS, including `cmd/magisterd`, `cmd/cm`, `internal/api`, `internal/supervisor`, `internal/config`.

- [ ] **Step 2: Confirm the go directive and frozen interface**

Run:
```bash
grep '^go ' go.mod
git diff main -- internal/core/store.go
```
Expected: `go 1.22`; `core.Store` is unchanged (M3 reads through it, doesn't alter it).

- [ ] **Step 3: Smoke test the binaries by hand (optional but recommended)**

```bash
go build -o /tmp/magisterd ./cmd/magisterd && go build -o /tmp/cm ./cmd/cm
/tmp/magisterd -db /tmp/smoke.db &   # starts on 127.0.0.1:8080
MAGISTER_ADDR=http://127.0.0.1:8080 /tmp/cm run flows/feature-flow.yaml --watch
# the feature-flow has manual gates; in another shell: /tmp/cm approve <run> plan ; etc.
kill %1
```
Expected: `cm run --watch` prints a run ID then streams the journal; manual gates block until `cm approve`.

- [ ] **Step 4: Milestone close**

Invoke **`superpowers:finishing-a-development-branch`** to integrate `feat/m3-api`. Then update the project memory and write the M4 kickoff handoff (retries/backoff, on_fail escalate, timeouts, real CLIAgent + stream-json cost parsing, git-worktree workspace + teardown + re-run safety).

---

## Self-review (spec coverage)

| Spec requirement | Task |
|---|---|
| §9 `POST /v1/runs` (validate, persist, return id) | 10 |
| §9 `GET /v1/runs` (list, filter by status) | 10 |
| §9 `GET /v1/runs/{id}` (snapshot: run + steps) | 10 |
| §9 `DELETE /v1/runs/{id}` (cancel) | 6 (Cancel) + 10 |
| §9 `GET /v1/runs/{id}/events` SSE + `Last-Event-ID` replay | 11 |
| §9 `POST .../approve` ({approve, reason}) | 10 |
| §9 `GET /healthz` | 10 |
| §9 stdlib `ServeMux` patterns, no router dep | 12 |
| §9 middleware: request ID → slog → recovery → auth → timeouts; MaxBytesReader; security headers; graceful shutdown | 9, 12, 13 |
| §9 loopback default + optional bearer (constant-time) | 7, 9, 13 |
| §5 Supervisor (active runs, cancel) + ApprovalRegistry | 4, 6 |
| §5/§3 manual gate emits gate.awaiting + blocks on approval channel | 1, 2, 5 |
| §3 two binaries: `magisterd` (wiring, lifecycle, shutdown) + `cm` (thin client) | 13, 14 |
| §3/§7 resume on startup (LoadIncompleteRuns → Resume) | 6 (ResumeAll), 13 |
| §10 `cm run/ls/get/watch/approve/reject/cancel`, `--json`/exit codes | 14 |
| §11 slog logging w/ run_id/step_id; no silent failures | 9, 13 |
| §14 `oklog/ulid` run IDs | 3, 6 |
| Runnable proof: `cm run --watch` + manual gate + kill/resume | 15 |

**Deferred (with reason):** `on_fail: escalate` (escalating a *failed* auto gate to a human) → M4 (the milestone table puts it there); M3 blocks only true `manual`/`conditional` gates. Real CLIAgents (`opus`/`sonnet`/`gemini`) → M4 — `magisterd` registers only the `mock` executor for now, so the keyless demo loop works end-to-end. `expr`-evaluated conditional gates and select/synthesize joins → M5 (conditional still falls back to manual blocking in M3). `--json` structured output is wired as a flag seam in `cm` but most subcommands already emit the API's JSON verbatim; richer formatting is additive.
