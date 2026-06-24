# `cm tui` — live terminal dashboard — Design

## Goal

Give the operator a **full-screen terminal UI** to watch magisterd runs live and
act on the interactive essentials — approve/reject gates, cancel, retry —
without context-switching between `cm` invocations. Today the only interface is
the one-shot `cm` CLI; a long-lived run with manual gates means repeatedly
polling `cm get` and remembering ids. The TUI is a single live view: runs on the
left, the selected run's steps + streaming event log on the right, contextual
key actions at the bottom.

Delivery (push/pr/ship) stays in `cm` — it has many flags and is rare; the TUI
covers what you must do *while a run is live*.

## Background — what the TUI consumes (verified against the code)

magisterd already exposes everything needed; the TUI is a pure API/SSE client,
no daemon changes.

- **Run list:** `GET /v1/runs` → `[{id,name,status}]` (`runSummary`). No global
  event stream exists, so the list is **polled**.
- **Run detail:** `GET /v1/runs/{id}` → `{id,name,status,steps:[{id,status,attempt,branch,commit,artifacts,…}]}`.
- **Live events (per run):** `GET /v1/runs/{id}/events` is **SSE**, framed
  `id: <seq>\nevent: <kind>\ndata: <json>\n\n`, resumable via the
  `Last-Event-ID` request header. Payload is `event.Event`:
  `{seq,run,step,kind,summary,cost_usd,attempt,error,at}`.
- **Event kinds:** `run.started`, `run.done`, `step.started`, `step.done`,
  `step.failed`, `step.retrying`, `gate.awaiting`, `agent.tool`.
  `gate.awaiting` is the signal that a step is blocked pending approval — the
  TUI surfaces it as the actionable item.
- **Actions:**
  - approve/reject → `POST /v1/runs/{id}/steps/{step}/approve` with
    `{approve: bool, reason: string}` (reject = `approve:false`).
  - cancel → `DELETE /v1/runs/{id}` (404 unknown, 409 terminal).
  - retry → `POST /v1/runs/{id}/retry`.
- **Auth:** optional `MAGISTER_BEARER_TOKEN`; when set, `/v1` requires
  `Authorization: Bearer <token>`. `cm` does **not** send it today; the TUI will.

## Global Constraints

- **One new direct dependency: `golang.org/x/term`** (raw mode + terminal size).
  It depends only on `golang.org/x/sys`, already in the module graph. Pin to a
  release compatible with **`go 1.22`** and verify it forces no go-version bump
  (the OTel slice's lesson). No other new deps; stdlib otherwise (`net/http`,
  `encoding/json`, `bufio`, `os/signal`). Pinned deps untouched.
- **No daemon/engine/API changes.** The TUI is a read-mostly client; all state
  lives server-side (the server remains the source of truth — the client never
  re-implements the run state machine).
- **New code is confined to `internal/tui/` + a `tui` case in `cmd/cm/main.go`.**
  No change to existing `cm` verbs.
- Commit hygiene: single conventional-commit subjects, no body, no
  `Co-Authored-By`, never `--no-verify`; `gofmt -l` clean.

## Design

### 1. Packaging

A new `cm tui` subcommand (no third binary). It reuses cm's base-URL resolution
(`MAGISTER_ADDR`, default `http://127.0.0.1:8080`). All new logic lives in
`internal/tui/`; `cmd/cm/main.go` gains a `case "tui"` that constructs the model
and runs the terminal driver.

### 2. Auth

The TUI's HTTP client reads `MAGISTER_BEARER_TOKEN` once at startup and, when
non-empty, sets `Authorization: Bearer <token>` on **every** request including
the SSE GET. Empty token = loopback, unchanged. This closes the gap `cm` has and
makes the TUI usable against an authenticated daemon.

### 3. Layout

Two panes over a status/key bar:

```
┌ RUNS ───────────────┐┌ feature-flow  a1b2  running ──────────────┐
│> a1b2 feature  RUN  ││ plan       ✅ done                         │
│  9f3c hello    DONE ││ impl-api   ⏳ running   (attempt 1)        │
│  7c1e nightly  FAIL ││ impl-ui    ✅ done                         │
│                     ││ integrate  ⏸  gate.awaiting  ◀── approve?  │
│                     │├ EVENTS ───────────────────────────────────┤
│                     ││ 12:03:21 step.done   impl-ui              │
│                     ││ 12:03:22 gate.awaiting integrate          │
└─────────────────────┘└───────────────────────────────────────────┘
 ↑↓ select · enter open · a approve · r reject · c cancel · R retry · q quit
```

- **Left:** runs list (id, name, status), newest first, selection cursor.
- **Right (top):** the focused run's steps with status + attempt; a step in
  `gate.awaiting` is highlighted as actionable.
- **Right (bottom):** a scrolling live event log for the focused run (most
  recent at the bottom), rendered from the event stream + `agent.tool` lines.
- **Bottom bar:** contextual key hints + a connection indicator (`connected` /
  `disconnected`).

Color/emphasis via raw ANSI SGR codes (small helper); status glyphs degrade to
ASCII when the terminal is narrow. No layout library.

### 4. Data flow

A single in-memory `model`, mutated only by a reducer (§6). Three message
sources feed it over one channel:

- **Poll loop:** every ~1.5s, `GET /v1/runs` → a `runsLoaded` message (keeps the
  list fresh; covers runs created elsewhere).
- **Focus subscription:** when the selection changes to run *R*, the driver
  (a) issues `GET /v1/runs/{id}` → `runSnapshot` (authoritative steps), and
  (b) opens the SSE stream for *R* (with `Last-Event-ID` once it has a seq),
  emitting `sseEvent` messages; switching focus cancels the previous stream.
  Lifecycle SSE events (`step.*`, `gate.awaiting`, `run.done`) also trigger a
  lightweight `GET /v1/runs/{id}` refresh so step statuses stay authoritative
  without the client re-deriving them. `agent.tool` events only append to the
  log.
- **Input loop:** decoded key presses → `key` messages.

### 5. Actions (contextual)

| Key | Action | Call | Guard |
|---|---|---|---|
| `a` | approve gate | `POST …/approve {approve:true}` | focused step is `gate.awaiting` |
| `r` | reject gate | prompt one-line reason → `{approve:false,reason}` | focused step is `gate.awaiting` |
| `c` | cancel run | `DELETE /v1/runs/{id}` | run is active; **y/n confirm** |
| `R` | retry run | `POST …/retry` | run is failed/canceled |
| `↑/↓`, `enter`, `q` | navigate / open / quit | — | — |

Each action runs async and reports a `actionResult` message (success refreshes
the run snapshot; failure shows the HTTP error in the status bar — e.g. a 409 on
cancelling an already-terminal run). Destructive actions (cancel) require a y/n
confirm; reject opens a one-line reason input.

### 6. Architecture (built for testability)

Hand-rolled but split into pure logic + a thin imperative shell, mirroring the
Elm/Bubble Tea split so the logic is testable with no TTY:

- **`client.go`** — typed API client over `net/http` (list, get, approve,
  cancel, retry) + the auth header. httptest-testable.
- **`sse.go`** — SSE reader: parses `id:/event:/data:` frames from a
  `bufio.Reader` into `event.Event`, tracks last seq, reconnects with
  `Last-Event-ID`. Unit-testable against a byte stream.
- **`model.go`** — the `model` struct (runs, focused run + steps, log ring
  buffer, selection, connection state, pending-confirm/reason input mode) and a
  pure `update(model, msg) (model, []cmd)` reducer. `msg` ∈ {runsLoaded,
  runSnapshot, sseEvent, key, actionResult, tick, error}. `cmd` is a side-effect
  request the driver executes (fetch, open-sse, do-action). Fully unit-tested.
- **`view.go`** — pure `view(model, width, height) string` producing the framed
  screen. Golden/snapshot-tested at fixed sizes (incl. a narrow terminal).
- **`driver.go`** — the only imperative layer: `x/term` raw mode, an input
  reader goroutine, `SIGWINCH` handling, the message loop wiring sources →
  `update` → render, and a `defer` that restores cooked mode (so a panic never
  leaves the terminal wedged). Manually verified.

### 7. Error handling

- **Daemon down / poll failure:** status bar shows `disconnected`; the poll loop
  keeps retrying with a capped backoff; no crash, no spinner lockup.
- **SSE drop:** auto-reconnect with `Last-Event-ID` so no events are missed
  across the gap; if the run is already terminal, the stream simply ends.
- **Resize:** `SIGWINCH` → re-render at the new size.
- **HTTP action errors:** surfaced in the status bar (status code + message),
  never silently swallowed.
- **Exit/panic:** `defer` restores the terminal; `q`/`Ctrl-C` exit cleanly.

### 8. Testing

- **Reducer:** a unit test per message kind — each event kind updates the right
  step/log state; each action transitions correctly; guards (approve only when
  `gate.awaiting`; retry only when terminal) hold.
- **SSE parser:** framing, multi-line data, `Last-Event-ID` resume, partial
  frames across reads.
- **API client:** each call against an `httptest.Server`, incl. the auth header
  and the 409-on-terminal-cancel path.
- **View:** golden snapshots of `view(model,w,h)` for representative states
  (empty, running run with a gate awaiting, narrow terminal).
- **Driver:** the thin terminal layer is manually verified (no TTY in CI).

## Components / files

- `internal/tui/client.go` (+ `client_test.go`)
- `internal/tui/sse.go` (+ `sse_test.go`)
- `internal/tui/model.go` (+ `model_test.go`)
- `internal/tui/view.go` (+ `view_test.go`)
- `internal/tui/driver.go`
- `cmd/cm/main.go` — add `case "tui"` + usage line
- `go.mod`/`go.sum` — add `golang.org/x/term` (go-1.22-compatible, version verified)

## Out of scope (deferred)

- **Delivery from the TUI** (push/pr/ship) and scratch gc/rm — stay in `cm`.
- **Web dashboard** — a separate future option on the same API/SSE.
- **Submitting/editing flows** from the TUI — runs are created with `cm run`.
- **Multi-daemon / multi-user** — single daemon at `MAGISTER_ADDR`.
- **A global event stream** — the list is polled; no daemon change.
