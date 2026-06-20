# Observability — run-scoped logging + request→run bridge

## Summary

Make a run's logs correlatable end to end, in two small pieces:

- **Run-scoped logger into agent runs.** A new `internal/logctx` helper carries a `*slog.Logger` on a `context.Context`. The engine's `runAgent` seam stashes a logger pre-tagged with `run`/`step`/`agent` into the context it already passes to the executor, so the agent layer's currently-**discarded** logs (e.g. `CLIAgent`'s artifact-discovery warning) now emit, correlated by run-id.
- **Request→run bridge line.** `handleCreateRun` emits one `run submitted` log carrying both the HTTP **request-id** (the ULID `requestIDMiddleware` already stamps) and the new **run-id**, so an operator can pivot from an access-log line to every log/event for the resulting run.

Correlation key is the **run-id**, not the request-id: runs outlive the HTTP request that submitted them, and resumed runs have no request at all. The request-id correlates the HTTP request; the bridge line is the one place the two ids meet.

Stdlib only, **no new dependency**, no DB migration, no schema change, no `event.Event` change. Log output stays `TextHandler` (already greppable `key=value`).

## Motivation

Today three facts block correlation (confirmed by exploration):
- The HTTP request context is **deliberately severed** at `supervisor.start()` (`context.WithCancel(context.Background())`, comment: *"a run outlives the HTTP request that submitted it"*), so the request-id cannot flow into the engine — and shouldn't be the engine's correlation key.
- The engine and supervisor already tag every log line with `run` (and step lines with `step`), but **nothing links a request-id to the run-id it created** — you cannot pivot from an access log to a run.
- `executor.CLIAgent.Log` is **never wired** in `main`, so agent-level warnings (artifact-discovery failures, cli.go) are silently discarded; the agent layer has no correlated logging at all.

This slice fixes the two real gaps (the request→run link and the dead agent logs) without disturbing the parts that already correlate.

## Design

### `internal/logctx` (new package)

A minimal context-logger carrier, stdlib only:

```go
package logctx

// With returns a context carrying log, retrievable by From.
func With(ctx context.Context, log *slog.Logger) context.Context

// From returns the logger stored by With, or a no-op (io.Discard) logger if
// none is set. Never returns nil.
func From(ctx context.Context) *slog.Logger
```

One unexported context-key type (`type ctxKey struct{}`). `From` on a bare context returns a package-level discard logger (`slog.New(slog.NewTextHandler(io.Discard, nil))`) so callers never nil-check.

### Engine — inject at the `runAgent` seam

`runAgent` (engine.go:368) already has `runID`, `stepID`, `agentName` and passes `ctx` to `ag.Run(ctx, task)`. Immediately before that call, enrich the context:

```go
ctx = logctx.With(ctx, e.logger().With("run", string(runID), "step", stepID, "agent", agentName))
```

`e.logger()` is the engine's existing nil-safe logger accessor (unchanged). This is the propagation point: the run-scoped logger now reaches the executor. No other engine change — the engine's existing Error logs already carry `run`/`step`.

### Executor — read the context logger

`CLIAgent`'s nil-guard `logger()` helper gains a `ctx` parameter and resolves in this order: the explicitly-set `a.Log` if non-nil, else `logctx.From(ctx)` (which the engine populated), else the discard logger. The artifact-discovery `Warn` (cli.go:96) now logs with `run`+`step`+`agent`. Call sites pass the `ctx` already in scope in `Run`. Only `CLIAgent` logs; Mock and other executors are untouched.

### API — the request→run bridge line

`handleCreateRun` (handlers.go) runs in `package api`, so it can read the request-id directly from the existing unexported `requestIDKey`. After a successful `Submit` returns the new run-id, emit:

```go
s.Log.Info("run submitted", "req", reqID, "run", string(id))
```

where `reqID` is `r.Context().Value(requestIDKey).(string)` (the same value `loggingMiddleware` logs as `id`, and that the response returns as `X-Request-ID`). This single line is the bridge: `req` ties to the access log, `run` ties to every engine/agent log and every event for that run.

## Deliberate non-changes

- `event.Event` is untouched — events already carry `RunID`/`StepID`/`Seq` and correlate by run-id.
- The `supervisor.start()` severance is preserved — the run-id is the engine correlation key, which also works for resumed runs (no request).
- The root handler stays `TextHandler` — logs are already structured `key=value` and greppable; JSON format is a separate future concern.
- The engine's existing `e.logger().Error(...)` sites are not refactored — they already carry `run`/`step`.

## Testing

- **logctx** (`internal/logctx`): `With`/`From` round-trip returns the same logger; `From` on a bare `context.Background()` returns a usable, non-nil logger (writing to it does not panic).
- **executor** (`internal/executor`): `CLIAgent.logger(ctx)` returns the context logger when `a.Log` is nil; with a `bytes.Buffer`-backed capturing handler set via `logctx.With`, a log call carries the injected `run`/`step`/`agent` fields. Prefer driving the real artifact-discovery `Warn` path if cheap; otherwise a focused test of the resolved logger.
- **api** (`internal/api`): build a `Server` whose `Log` writes to a `bytes.Buffer`; `POST /v1/runs` with a valid one-step flow; assert the buffer contains `run submitted` with `run=<the returned run-id>` and a non-empty `req=` matching the response's `X-Request-ID`.
- Full `go test -race ./...` green; `go vet` + `gofmt -l` clean.

## Out of scope

- JSON log format / a `-log-format` flag.
- Converting the engine's existing Error logs to the context logger.
- Forcing the request-id into the engine (the severance is intentional).
- Adding agent token/Debug-level logs or new event kinds.
- Wiring loggers into Mock or non-CLIAgent executors (they don't log).

## Global constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no DB migration; no schema change; `event.Event` unchanged.
- Correlation key is the run-id; the request-id appears only in the access log and the one `run submitted` bridge line.
- `logctx.From` never returns nil.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.
