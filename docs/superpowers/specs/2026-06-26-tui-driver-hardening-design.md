# `cm tui` driver hardening — design

**Date:** 2026-06-26
**Status:** Approved, ready for plan
**Scope:** `internal/tui/` only. No daemon changes. No new dependencies.

Closes two non-blocking follow-ups recorded in the post-cm-tui handoff
(`docs/superpowers/handoffs/2026-06-24-post-cm-tui-next-steps.md`, items #1 and #2).

## Problem

### #1 — A terminal resize briefly fakes "connected"

The SIGWINCH goroutine in `driver.go` sends `connMsg(true)` purely to force a
re-render on resize. The reducer reads `connMsg` as the daemon-reachability
signal, so a resize while the daemon is unreachable flips the connection
indicator from "disconnected" back to "connected" until the next 1.5s poll
corrects it. Cosmetic, but wrong: the resize event carries no connectivity
information and must not mutate `conn`.

### #2 — `StreamEvents` cannot tell a refusal from a clean stream end

`StreamEvents` calls `hc.Do(req)` and immediately hands `resp.Body` to
`parseEvents` without inspecting `resp.StatusCode`. On a non-2xx response
(`404` run-gone, `5xx` broken) the body is a short JSON error, not an event
stream; `parseEvents` finds zero frames and returns `nil` — indistinguishable
from a clean EOF. `streamLoop` then reconnects unconditionally after a 500ms
backoff, so a deliberately-refusing endpoint is hammer-reconnected at ~2/s
until the focus context is cancelled.

## Daemon behavior this design relies on (verified, not assumed)

`internal/api/sse.go` closes the SSE response when it writes `run.done`
(`drain()` returns `false` on `event.RunDone`, the handler returns, the
connection closes). Consequences traced against the client:

- A **terminal run** ends the stream with a *clean EOF*, not a non-2xx.
  `streamLoop` reconnects once; the daemon returns `200` and the handler then
  **blocks idle** (events-since-`run.done` is empty). That single idle
  held-open connection is **load-bearing**: it is how pressing `R` (retry) on a
  focused run streams the resumed run's new events live, because retry reuses
  the same run id and the daemon drains the new higher-seq events onto the
  still-open connection.
- Therefore terminal runs do **not** storm. The storm is exclusively a
  *refusing* response (`404`/persistent `5xx`), where `StreamEvents` returns
  `nil` fast and repeatedly.

This corrects the handoff's framing of #2 ("a focused terminal/deleted run
reconnects ~2/s") and rules out the "reducer→driver terminal-status
signalling" fix it proposed — see Out of scope.

## Design

### #1 — Dedicated redraw message

- Add `type redrawMsg struct{}` to `model.go`.
- Add `case redrawMsg: return m, nil` to the `update` reducer — a pure no-op.
  It still forces a frame because `runLoop` renders on every message.
- Change the SIGWINCH goroutine in `driver.go` to send `redrawMsg{}` instead of
  `connMsg(true)`.

After this, the connection indicator reflects only its real source of truth:
the poll loop's `connMsg(true|false)`.

### #2 — Typed non-2xx error + bounded reconnect

- Add a typed error to `sse.go`:

  ```go
  // streamStatusError signals a non-2xx HTTP response from the events endpoint
  // — the server is up but deliberately refusing this stream (404 gone, 5xx
  // broken). streamLoop treats it as permanent and stops reconnecting, unlike a
  // transport error or a clean EOF, which remain retryable.
  type streamStatusError struct{ Status int }

  func (e *streamStatusError) Error() string {
      return "events stream: HTTP " + strconv.Itoa(e.Status)
  }
  ```

- In `StreamEvents`, after `hc.Do` succeeds, check `resp.StatusCode/100 != 2`
  *before* parsing. On a non-2xx: close the body (drain a bounded prefix is
  unnecessary — we discard it) and return `&streamStatusError{resp.StatusCode}`.
  `emit` is never called in this path.
- In `streamLoop`, inspect the returned error:

  ```go
  err := c.StreamEvents(ctx, id, last, emit)
  var se *streamStatusError
  if errors.As(err, &se) {
      return // any non-2xx: server is refusing — stop, don't hammer-reconnect
  }
  // transport error or clean EOF -> existing 500ms backoff + reconnect
  ```

**Stop policy: any non-2xx.** A `404` (run gone) and a persistent `5xx` both
stop reconnecting, because retrying an identical request against a server that
is up and deliberately refusing only storms. Transport errors (`hc.Do` failed —
daemon down/restarting) and clean EOF remain retryable, preserving daemon-down
recovery and the load-bearing terminal/retry-rewatch path. The cost of this
choice: a *transient* daemon `5xx` stops the focused run's live event stream
until the user re-enters the run (the run stays visible and usable in the polled
list; the connection indicator stays honest via the poll loop). Accepted in
exchange for a hard no-storm guarantee.

## Testing (TDD, red → green)

- `model_test.go`: `update(model{conn:false}, redrawMsg{})` returns `conn:false`
  and no commands — the regression guard for the false-"connected" flip.
- `sse_test.go`: `StreamEvents` against a `404` httptest server returns a
  `*streamStatusError{Status:404}` and never calls `emit`; same shape for `500`.
- `driver_test.go`: `streamLoop` against a handler that always returns `404`
  returns promptly and hits the server **exactly once** (proves it stops rather
  than hammer-reconnecting), guarded by a context timeout so a regression fails
  loudly instead of hanging.

The existing `TestStreamEventsSendsLastEventIDAndAuth` (a 200 stream) and the
`runLoop`/reducer suites continue to pass unchanged.

## Out of scope (recorded so it is not re-litigated)

- **Reducer→driver terminal-status signalling** (handoff #2c). The trace above
  shows terminal runs do not storm, and the idle held-open stream they leave is
  load-bearing for `R`-retry rewatch; closing it on terminal would regress that.
  Not built.
- **`cmdRetry` reopening the SSE stream** — orthogonal pre-existing behavior;
  retry-while-focused already works via the held-open stream. Not touched.
- Any daemon-side change to `/v1/runs/{id}/events`.

## Process

Two commits — #1 then #2 — each TDD red→green, on a
`.claude/worktrees/tui-driver-hardening` worktree, with an Opus whole-branch
review before merge, matching the project's per-slice arc. Pinned deps
unchanged; `go.mod` stays `go 1.22`.
