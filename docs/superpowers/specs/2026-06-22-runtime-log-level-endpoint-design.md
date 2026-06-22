# Observability — runtime log-level control endpoint

## Summary

Make the daemon's log threshold adjustable **at runtime**, without a restart, via an auth-protected HTTP control endpoint (`GET`/`POST /v1/loglevel`) and a matching `cm loglevel` client verb. Today `-log-level` is fixed at startup; an operator who wants to see the engine's Debug lines (shipped in the engine-instrumentation slice) on a *running* daemon — to diagnose an in-flight run — must stop, re-flag, and restart it, losing the very run they wanted to observe.

The mechanism is a single shared `*slog.LevelVar` (read atomically on every log record) substituted for the fixed `slog.Level` currently handed to the log handler. One `LevelVar.Set(...)` re-thresholds **every** logger built on that handler (engine, supervisor, server, janitor) live and lock-free. Stdlib `slog` only — **no new dependency, package, migration, schema, or SSE event**. Go 1.22.

## Motivation

The prior two slices made the daemon's logging genuinely useful: `-log-level debug|info|warn|error` selects a threshold, and the engine now emits seven Debug/Warn lines describing its internal decisions. But the threshold is frozen at process start. The real operational need is *reactive*: a run is behaving oddly **now**, and the operator wants to raise verbosity to `debug` to watch it, then drop back to `info` — all against the long-lived daemon, which routinely outlives and resumes individual runs. A restart-to-reconfigure both kills in-flight runs and discards the live state worth observing. A runtime knob closes that loop; it is the natural payoff of pairing the level slice with the engine-debug-lines slice.

## Design

### Mechanism: one shared `*slog.LevelVar`

`slog.HandlerOptions.Level` is typed `slog.Leveler`, not `slog.Level`. A `*slog.LevelVar` satisfies `Leveler` and is consulted atomically on every record, so a single shared `LevelVar` makes the threshold mutable for every logger derived from the handler at once — no re-wiring, no locks.

In `cmd/magisterd/main.go`'s `run()`:

1. Parse the startup level (now via `config.ParseLogLevel` — see below) → `startLvl slog.Level`. Bad value still returns an error before any side-effecting init (fail-fast preserved).
2. `lvlVar := new(slog.LevelVar); lvlVar.Set(startLvl)`.
3. Pass `lvlVar` to `newLogHandler` (param widened from `slog.Level` to `slog.Leveler`; the `&slog.HandlerOptions{Level: level}` line is unchanged because `Level` already takes a `Leveler`).
4. Wire `lvlVar` into the `api.Server` struct literal as the new `LogLevel` field.

Because `slog.Level` itself satisfies `slog.Leveler` (it has a `Level()` method), the existing `newLogHandler` test calls that pass `slog.LevelInfo` keep compiling unchanged after the signature widening — only the production call in `run()` passes the `*slog.LevelVar`.

**Default behavior is byte-for-byte unchanged:** the `LevelVar` is initialized to exactly the level `-log-level`/`MAGISTER_LOG_LEVEL` would have produced, so a daemon nobody touches logs identically to today.

### Shared level parsing (small refactor)

`parseLogLevel` currently lives in `package main` (`cmd/magisterd/main.go`), unreachable from `internal/api`. Move it to `internal/config` (which already owns the `LogLevel`/`LogFormat` config strings) and export it, so both the startup path and the runtime endpoint validate against one strict surface and cannot drift:

- `func ParseLogLevel(s string) (slog.Level, error)` — the existing strict, exact-match lowercase switch (`debug`/`info`/`warn`/`error` → the four `slog.Level` constants), else `fmt.Errorf("invalid log-level %q: want debug|info|warn|error", s)`. Deliberately rejects uppercase `INFO`, offsets like `info+2`, and `""` — identical behavior and error text to today's `parseLogLevel`.
- `func LevelString(l slog.Level) string` — the reverse map: the four constants → their lowercase canonical names; default `strings.ToLower(l.String())` (a defensive fallback never hit in practice, since the only values ever `Set` come from `ParseLogLevel`).

`internal/config` gains a `log/slog` import (plus `fmt`/`strings`). No import cycle: `config` imports nothing project-internal that would cycle, and both `main` and `api` already depend on `config` (one-way). `config.Parse` itself stays error-free — these are standalone helpers, not part of flag parsing.

`run()` switches its call from the local `parseLogLevel` to `config.ParseLogLevel`; the local function is deleted. Its unit tests move to `internal/config`.

### Endpoint (`GET`/`POST /v1/loglevel`, on the authed `v1` sub-mux)

Registered on the existing `v1` sub-mux in `Router`, so it inherits — with zero special-casing — `authMiddleware` (auth-protected: respects the bearer token when one is configured; open when `token == ""`, exactly like every other `/v1/*` route), `timeoutMiddleware` (30s; the handler is instant), and `metricsMiddleware` route-label resolution (`route="/v1/loglevel"`, a bounded template via the sub-mux's `Handler(r)`). No change to `routeLabel`, which only special-cases the outer-mux paths.

- **`GET /v1/loglevel`** → `200 {"level":"<current>"}`, where `<current>` = `config.LevelString(s.LogLevel.Level())`.
- **`POST /v1/loglevel`** with body `{"level":"debug"}` → decode → `config.ParseLogLevel(req.Level)` → `s.LogLevel.Set(lvl)` → `200 {"level":"<canonical>"}` (`config.LevelString(lvl)`, echoing the normalized level just applied).
  - Malformed JSON body → `400`.
  - Invalid level value → `400` carrying the strict `invalid log-level "...": want debug|info|warn|error` message.
- If `s.LogLevel == nil` (endpoint unconfigured — e.g. a test server, or a future caller that omits the field) → `503` for both verbs. The daemon always wires it, so production never returns 503; this is a safety nil-guard mirroring how `Metrics` is handled.

POST (not PUT) is used for the mutation, consistent with every other mutating route in this codebase (all POST/DELETE; no PUT exists). The handlers live in a new focused file `internal/api/loglevel.go`; the request/response DTOs join the others in `dto.go`. The handlers reuse the package's existing response helpers `writeJSON(w, status, v)` and `writeError(w, status, msg)` (in `middleware.go`).

New `api.Server` field: `LogLevel *slog.LevelVar` (optional, nil-guarded). `internal/api` gains a `log/slog` import and an `internal/config` import (for `ParseLogLevel`/`LevelString`); neither creates a cycle.

DTOs:

```go
type logLevelRequest struct {
    Level string `json:"level"`
}
type logLevelResponse struct {
    Level string `json:"level"`
}
```

### `cm loglevel` client verb

Add `case "loglevel":` to the dispatch switch in `cmd/cm/main.go`:

- `cm loglevel` (no argument) → `GET /v1/loglevel`, print the current level.
- `cm loglevel <level>` → `POST /v1/loglevel` with `{"level":"<level>"}`, print the level the server echoes back.
- A non-2xx response prints the server's error body and exits non-zero, consistent with the other `cm` commands.

`cm` sends no `Authorization` header today (it relies on the local daemon running token-less, like every existing verb), so `cm loglevel` needs no new token plumbing and is consistent with the rest of the client surface. Against a token-protected daemon, an operator uses `curl` with the header — the same pre-existing limitation that applies to `cm run`/`push`/etc., not introduced here.

### Concurrency & persistence

`*slog.LevelVar` is goroutine-safe (atomic load/store), so the GET/POST handlers are safe against concurrent logging from engine goroutines with no additional locking. The runtime override is **in-memory only**: a restart reverts to the `-log-level`/`MAGISTER_LOG_LEVEL` startup value. This is the expected semantics for a live override (persisting it would need a store write and is explicitly out of scope).

## Testing

- **`internal/config`** (`config_test.go`): `ParseLogLevel` table — the four valid lowercase values map to the right constants; uppercase (`INFO`), an offset (`info+2`), and `""` are rejected with the exact message. `LevelString` — the four constants render to their lowercase names and round-trip through `ParseLogLevel`. (These are the relocated `parseLogLevel` tests plus the new reverse-map tests.)
- **`internal/api`** (new `loglevel_test.go`): with a `Server` whose `LogLevel` is a `*slog.LevelVar`:
  - `GET` returns the current level (set the `LevelVar` to `warn` → body `{"level":"warn"}`).
  - `POST {"level":"debug"}` → `200`, body `{"level":"debug"}`, **and** `srv.LogLevel.Level() == slog.LevelDebug` (the live effect, not just the echo).
  - `POST` with an invalid level → `400` with the strict message; `POST` with malformed JSON → `400`.
  - `GET` and `POST` against a `Server` with `LogLevel == nil` → `503`.
  - `/v1/loglevel` is behind auth: a request without the token, to a router built with a non-empty token, → `401` (confirms registration on the authed `v1` mux).
- **`cmd/cm`** (`main_test.go`): `cm loglevel` against an `httptest` stub → GETs and prints the level; `cm loglevel debug` → POSTs `{"level":"debug"}` and prints the echoed level; a stub returning `400` → `cm` exits non-zero and surfaces the body.
- **`cmd/magisterd`**: the `newLogHandler` signature widening is compile-checked; existing `newLogHandler` tests pass `slog.LevelInfo` unchanged (it satisfies `slog.Leveler`). Full `run()` wiring is covered by the live smoke below.
- Full `go test -race ./...` green; `go vet` and `gofmt -l` clean.

### Live smoke (manual proof)

Real branch binary, the zero-cost mock flow `flows/git-native-merge.yaml`, daemon started at default `-log-level info` (sandbox-disabled):

1. Run the flow → stderr shows **no** engine Debug lines (info threshold).
2. `cm loglevel debug` → prints `debug`; `cm loglevel` → confirms `debug`.
3. Run the flow again → the engine's Debug lines (`agent finished`, `step slot acquired`, `gate evaluated`, `join starting/finished`) now stream **mid-run** — proving the live flip re-thresholds the running engine's loggers.
4. `cm loglevel info` → prints `info`; a third run is quiet again.
5. `cm loglevel bogus` → non-zero exit, `400` with `invalid log-level "bogus": want debug|info|warn|error`.

This is the user-facing payoff and the only thing exercising the full `endpoint → shared LevelVar → live engine loggers` path against a daemon that never restarts.

## Out of scope

- Persisting the runtime override across restarts (would need a store write).
- Per-component / per-logger levels (one shared root `LevelVar`).
- Changing `-log-format`, the startup `-log-level` flag/env, or any existing log line or its fields.
- Adding new engine/supervisor/executor log lines (the engine-instrumentation slice already did).
- `cm` bearer-token support (a pre-existing client-wide gap, orthogonal to this slice).
- OTel tracing.

## Global constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no new external package; no migration; no schema change; no new SSE event kind.
- Default startup behavior is **byte-for-byte unchanged**: the `LevelVar` is initialized to the same level the prior fixed path produced.
- Reuse the existing strict level grammar and its exact error text; the runtime endpoint and the startup path validate through the same `config.ParseLogLevel`.
- The endpoint is auth-protected via registration on the existing `v1` sub-mux — no new auth wiring, no `routeLabel` change.
- `*slog.LevelVar` for the live threshold (atomic; no extra locking).
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.
