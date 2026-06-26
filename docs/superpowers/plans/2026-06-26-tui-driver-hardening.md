# `cm tui` Driver Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop a terminal resize from faking the "connected" indicator, and stop `streamLoop` from hammer-reconnecting when the events endpoint deliberately refuses (404/5xx).

**Architecture:** Two isolated changes in `internal/tui/`. (#1) A dedicated `redrawMsg` the reducer no-ops, replacing the `connMsg(true)` the SIGWINCH handler abused purely to force a frame. (#2) `StreamEvents` checks the HTTP status before parsing and returns a typed `*streamStatusError` on non-2xx; `streamLoop` stops reconnecting on it via `errors.As`, while transport errors and clean EOF keep the existing 500ms backoff-reconnect.

**Tech Stack:** Go 1.22, stdlib only (`net/http`, `net/http/httptest`, `errors`, `sync/atomic`), the in-repo `concentus/internal/event` package.

## Global Constraints

- `internal/tui/` only — NO daemon changes, NO new dependencies.
- `go.mod` stays `go 1.22`.
- Spec: `docs/superpowers/specs/2026-06-26-tui-driver-hardening-design.md`.
- Stop policy is **any non-2xx** (404 and 5xx both stop). Transport errors and clean EOF stay retryable — do not change those paths.
- Daemon fact relied upon: `internal/api/sse.go` closes the stream on `run.done`, so a terminal run ends with a *clean EOF*, never a non-2xx. Do not add terminal-status signalling (explicitly out of scope — it would regress `R`-retry rewatch).
- `gofmt` clean; run `go test -race ./internal/tui/...` and `go build ./...` before each commit.
- Commit subjects: single conventional-commits line, no `Co-Authored-By` trailer, no `--no-verify`.

---

### Task 1: Redraw message — resize no longer fakes "connected"

**Files:**
- Modify: `internal/tui/model.go` (messages block ~line 7-13; reducer `update` ~line 97-143)
- Modify: `internal/tui/driver.go:126` (SIGWINCH goroutine)
- Test: `internal/tui/model_test.go` (append)

**Interfaces:**
- Produces: `type redrawMsg struct{}` — a driver→reducer message that forces a render without mutating state. Reducer handles it as `return m, nil`.

- [ ] **Step 1: Write the failing test**

Append to `internal/tui/model_test.go`:

```go
// A redraw (e.g. on terminal resize) must force a render without touching the
// connection indicator — only the poll loop owns conn. Regression guard for the
// SIGWINCH-sends-connMsg(true) false-"connected" flip.
func TestRedrawMsgPreservesConnAndEmitsNoCommands(t *testing.T) {
	m := model{conn: false}
	got, cmds := update(m, redrawMsg{})
	if got.conn {
		t.Fatalf("redrawMsg changed conn to true, want false")
	}
	if cmds != nil {
		t.Fatalf("redrawMsg emitted commands %v, want none", cmds)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestRedrawMsgPreservesConnAndEmitsNoCommands`
Expected: FAIL — compile error `undefined: redrawMsg`.

- [ ] **Step 3: Add the message type**

In `internal/tui/model.go`, in the `// ---- messages (driver -> reducer) ----` block (just after `type connMsg bool` on line 13), add:

```go
type redrawMsg struct{} // force a render without mutating state (e.g. on resize)
```

- [ ] **Step 4: Add the reducer case**

In `internal/tui/model.go`, in `update`, add a case alongside the others (e.g. just after the `connMsg` case, ~line 130):

```go
	case redrawMsg:
		return m, nil
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/tui/ -run TestRedrawMsgPreservesConnAndEmitsNoCommands`
Expected: PASS.

- [ ] **Step 6: Wire the driver to send redrawMsg on resize**

In `internal/tui/driver.go`, the SIGWINCH goroutine currently has (line ~126):

```go
			case <-winch:
				trySend(msgs, connMsg(true)) // benign message to trigger a re-render
```

Replace that line with:

```go
			case <-winch:
				trySend(msgs, redrawMsg{}) // force a re-render without touching conn
```

- [ ] **Step 7: Verify the package builds, formats, and the suite is green**

Run: `gofmt -l internal/tui/ && go build ./... && go test -race ./internal/tui/`
Expected: no `gofmt` output, build succeeds, all tests PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/tui/model.go internal/tui/driver.go internal/tui/model_test.go
git commit -m "fix(tui): use a dedicated redraw message on resize, not connMsg(true)"
```

---

### Task 2: Bounded SSE reconnect — stop hammering a refusing endpoint

**Files:**
- Modify: `internal/tui/sse.go` (add error type; status check in `StreamEvents` ~line 47-62)
- Modify: `internal/tui/driver.go` (`streamLoop` ~line 158-173; imports)
- Test: `internal/tui/sse_test.go` (append)
- Test: `internal/tui/driver_test.go` (append)

**Interfaces:**
- Consumes: `Client.StreamEvents(ctx, id, lastSeq, emit) error`, `event.Event`, `event.RunDone` (unchanged signatures).
- Produces: `type streamStatusError struct{ Status int }` with `func (e *streamStatusError) Error() string`. `StreamEvents` returns `*streamStatusError` on a non-2xx response (and does not call `emit` in that path). `streamLoop` returns (stops reconnecting) when `StreamEvents` returns a `*streamStatusError`.

- [ ] **Step 1: Write the failing `StreamEvents` test**

Append to `internal/tui/sse_test.go` (the file already imports `context`, `net/http`, `net/http/httptest`, `strings`, `testing`, and `concentus/internal/event`; add `"errors"` to its import block):

```go
func TestStreamEventsNon2xxReturnsTypedErrorWithoutEmitting(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"nope"}`, code)
		}))
		var emitted int
		err := NewClient(srv.URL, "").StreamEvents(context.Background(), "a1", 0, func(event.Event) { emitted++ })
		srv.Close()

		var se *streamStatusError
		if !errors.As(err, &se) {
			t.Fatalf("code %d: err = %v, want *streamStatusError", code, err)
		}
		if se.Status != code {
			t.Fatalf("Status = %d, want %d", se.Status, code)
		}
		if emitted != 0 {
			t.Fatalf("emit called %d times on a non-2xx, want 0", emitted)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestStreamEventsNon2xxReturnsTypedErrorWithoutEmitting`
Expected: FAIL — compile error `undefined: streamStatusError`.

- [ ] **Step 3: Add the typed error and the status check**

In `internal/tui/sse.go` (it already imports `strconv`), add the type below the imports, above `parseEvents`:

```go
// streamStatusError signals a non-2xx HTTP response from the events endpoint —
// the server is up but deliberately refusing this stream (404 gone, 5xx broken).
// streamLoop treats it as permanent and stops reconnecting, unlike a transport
// error or a clean EOF, which remain retryable.
type streamStatusError struct{ Status int }

func (e *streamStatusError) Error() string {
	return "events stream: HTTP " + strconv.Itoa(e.Status)
}
```

Then in `StreamEvents`, after the `resp, err := c.hc.Do(req)` / error check / `defer resp.Body.Close()` (currently lines ~56-60), insert the status check *before* `return parseEvents(resp.Body, emit)`:

```go
	if resp.StatusCode/100 != 2 {
		return &streamStatusError{Status: resp.StatusCode}
	}
	return parseEvents(resp.Body, emit)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run TestStreamEventsNon2xxReturnsTypedErrorWithoutEmitting`
Expected: PASS.

- [ ] **Step 5: Write the failing `streamLoop` test**

Append to `internal/tui/driver_test.go` (it currently imports `context`, `testing`, `time`; add `"net/http"`, `"net/http/httptest"`, and `"sync/atomic"`):

```go
// streamLoop must stop reconnecting once the endpoint returns a non-2xx (the run
// is gone / the server is refusing) — otherwise it hammer-reconnects at ~2/s.
func TestStreamLoopStopsOnNon2xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	// Generous safety ctx so a regression hangs here rather than wedging the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		streamLoop(ctx, NewClient(srv.URL, ""), "a1", make(chan any, 1))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("streamLoop did not return promptly on a 404 — it kept reconnecting")
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("server hit %d times, want exactly 1 (no reconnect)", n)
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestStreamLoopStopsOnNon2xx`
Expected: FAIL — before the streamLoop change, `StreamEvents` now returns `*streamStatusError` but `streamLoop` ignores it (`_ = c.StreamEvents(...)`) and reconnects every 500ms, so the `time.After(1s)` arm fires with `t.Fatal("...kept reconnecting")`.

- [ ] **Step 7: Add the `errors` import to driver.go**

In `internal/tui/driver.go`, add `"errors"` to the import block (alphabetically, above `"os"`):

```go
import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"concentus/internal/event"
)
```

- [ ] **Step 8: Stop streamLoop on a non-2xx**

In `internal/tui/driver.go`, replace the body of `streamLoop` (currently lines ~158-173) so it inspects the returned error:

```go
// streamLoop keeps the per-run SSE stream open, reconnecting until ctx ends.
// A non-2xx response (run gone / server refusing) is permanent — stop, don't
// hammer-reconnect. Transport errors and clean EOF remain retryable.
func streamLoop(ctx context.Context, c *Client, id string, msgs chan any) {
	var last int64
	for ctx.Err() == nil {
		err := c.StreamEvents(ctx, id, last, func(e event.Event) {
			if e.Seq > last {
				last = e.Seq
			}
			trySend(msgs, sseEvent(e))
		})
		var se *streamStatusError
		if errors.As(err, &se) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond): // backoff before reconnect
		}
	}
}
```

- [ ] **Step 9: Run test to verify it passes**

Run: `go test ./internal/tui/ -run TestStreamLoopStopsOnNon2xx`
Expected: PASS — `done` closes promptly, `hits == 1`.

- [ ] **Step 10: Verify the package builds, formats, and the full suite is green under -race**

Run: `gofmt -l internal/tui/ && go build ./... && go test -race ./internal/tui/`
Expected: no `gofmt` output, build succeeds, all tests PASS (including the unchanged `TestStreamEventsSendsLastEventIDAndAuth` 200 path).

- [ ] **Step 11: Commit**

```bash
git add internal/tui/sse.go internal/tui/driver.go internal/tui/sse_test.go internal/tui/driver_test.go
git commit -m "fix(tui): stop SSE reconnect storm on a non-2xx events response"
```

---

## Self-Review

**1. Spec coverage:**
- Spec #1 (redraw message, SIGWINCH wiring, reducer no-op, test) → Task 1. ✓
- Spec #2 (`streamStatusError`, status check in `StreamEvents`, `streamLoop` stop-on-non-2xx, sse 404/500 test, driver stops-once test) → Task 2. ✓
- Spec "any non-2xx" stop policy → Task 2 Step 8 (`errors.As` with no status discrimination). ✓
- Spec out-of-scope (terminal signalling, cmdRetry rewatch, daemon changes) → not built; called out in Global Constraints. ✓

**2. Placeholder scan:** No TBD/TODO/"handle edge cases"/"similar to". Every code step shows full code. ✓

**3. Type consistency:** `redrawMsg` (Task 1) used identically in test, model.go, driver.go. `streamStatusError{Status int}` + `Error()` (Task 2 Step 3) consumed by the sse test (Step 1) and `streamLoop` (Step 8) with the same field name `Status`. `StreamEvents`/`streamLoop`/`event.RunDone` signatures match the current source. ✓

## Verification After Both Tasks

Run the whole repo suite once on the branch to confirm no cross-package regression:

```
go test -race ./...
```

Expected: all packages PASS (the cm-tui baseline was 480 passed / 20 packages; this slice adds 3 tests in `internal/tui` and changes no other package).
