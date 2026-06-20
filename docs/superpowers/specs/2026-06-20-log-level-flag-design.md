# Observability — selectable log level

## Summary

Add a `-log-level debug|info|warn|error` flag (env `MAGISTER_LOG_LEVEL`, default `info`) that sets the threshold of the daemon's root `slog` handler. `info` (the default) keeps today's behavior byte-for-byte; `debug` surfaces lower-severity lines, `warn`/`error` quiet the logs. Parsing is strict: an unrecognized value fails fast — the daemon refuses to start and exits non-zero with a clear message.

This is the companion knob to the just-merged `-log-format text|json` slice and reuses the same seam: a `Config` field, a small `main.go`-local validator/factory, and the single logger-construction point in `run()`. Stdlib only (`slog.HandlerOptions.Level`) — **no new dependency, no new package, no migration**.

## Motivation

The handler today is pinned to `slog.LevelInfo`. The engine, supervisor, and executor have no Debug/Warn lines *yet*, but operators still want the standard verbosity control: quiet a noisy daemon to `warn`, or (as future Debug lines land) raise it to `debug` without a recompile. Pairing a level knob with the existing format knob completes the conventional logging-configuration surface; both are deferred-but-expected from the request-scoped-logging work.

## Design

### Config — a `LogLevel` field

`internal/config/config.go` gains one field, following the existing `-log-format`/`-shutdown-drain` pattern exactly:

- `Config.LogLevel string`
- Flag: `fs.StringVar(&c.LogLevel, "log-level", "info", "log level: debug, info, warn, or error")`
- Env override (fill only when the flag was not set):
  ```go
  if v := env("MAGISTER_LOG_LEVEL"); v != "" && !flagSet(fs, "log-level") {
      c.LogLevel = v
  }
  ```

`Parse` performs **no validation** — it stays error-free; the validator (below) fails fast with the value in hand. So `cfg.LogLevel` is `"info"` (default) or whatever non-empty value the flag/env supplied.

### Parser — `parseLogLevel`

A strict, lowercase-only mapping in `cmd/magisterd/main.go` (matching `newLogHandler`'s switch style and error wording — deliberately rejecting uppercase and `slog`'s offset syntax like `INFO+2` for a tight, predictable surface):

```go
func parseLogLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log-level %q: want debug|info|warn|error", s)
	}
}
```

### Factory — `newLogHandler` takes the level

The existing factory currently hardcodes `Level: slog.LevelInfo`. It gains a `level slog.Level` parameter:

```go
func newLogHandler(format string, level slog.Level, w io.Writer) (slog.Handler, error) {
	opts := &slog.HandlerOptions{Level: level}
	switch format {
	case "text":
		return slog.NewTextHandler(w, opts), nil
	case "json":
		return slog.NewJSONHandler(w, opts), nil
	default:
		return nil, fmt.Errorf("invalid log-format %q: want text|json", format)
	}
}
```

The three existing `newLogHandler` tests (`…Text`, `…JSON`, `…Invalid`) gain a `slog.LevelInfo` argument; their behavior is otherwise unchanged.

### `run()` wiring

In `run()`, right after `cfg := config.Parse(args, env)`, parse the level (fail-fast) then build the handler:

```go
lvl, err := parseLogLevel(cfg.LogLevel)
if err != nil {
	return err
}
h, err := newLogHandler(cfg.LogFormat, lvl, os.Stderr)
if err != nil {
	return err
}
log := slog.New(h)
```

`parseLogLevel` runs before `newLogHandler`, both before any listener bind or DB open, so a bad value returns the error early and `main()`'s existing `os.Exit(1)` makes it a non-zero exit with nothing half-initialized. Default `info` → `slog.LevelInfo`, identical to today. `lvl, err :=` then `h, err :=` is valid (`h`/`lvl` are new each time); the later `st, err := store.Open(...)` stays `:=` because `st` is new.

### Behavior

| `-log-level` | result |
|---|---|
| unset / `info` | `Level: slog.LevelInfo` — today's behavior, unchanged |
| `debug` | `Level: slog.LevelDebug` — Debug+Info+Warn+Error emitted |
| `warn` | `Level: slog.LevelWarn` — Info and below dropped |
| `error` | `Level: slog.LevelError` — only Error |
| anything else (`trace`, `INFO`, `info+2`, `""`) | daemon exits 1: `invalid log-level "<v>": want debug|info|warn|error` |

## Testing

- **config** (`internal/config/config_test.go`): default `LogLevel` is `"info"`; `-log-level debug` sets `"debug"`; `MAGISTER_LOG_LEVEL=warn` applies when the flag is unset; an explicit `-log-level info` flag wins over `MAGISTER_LOG_LEVEL=warn`.
- **main** (`cmd/magisterd/main_test.go`):
  - `parseLogLevel` returns the correct `slog.Level` for each of `debug`/`info`/`warn`/`error` (no error), and a non-nil error containing `invalid log-level` for `trace` (and confirm `INFO` uppercase is rejected, locking in the strict surface).
  - A handler built via `newLogHandler("text", slog.LevelWarn, buf)`: logging an `Info` record produces no output; logging a `Warn` record produces output — proving the level threshold is actually applied.
  - End-to-end fail-fast: `run([]string{"-log-level", "trace"}, …)` returns a non-nil error (before binding a listener).
  - Update the three existing `newLogHandler` tests to pass `slog.LevelInfo`.
- Full `go test -race ./...` green; `go vet` + `gofmt -l` clean.

## Out of scope

- Runtime-adjustable level (`slog.LevelVar` / a control endpoint) — startup-only here.
- Per-component or per-logger levels — one root daemon logger.
- Adding new Debug/Warn log lines to the engine/supervisor/executor — this slice only makes the threshold configurable.
- Any change to `-log-format`, source location, or custom time/attr handling.

## Global constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no new package; no migration; no schema change.
- Default is `info` and produces byte-for-byte today's behavior; the value maps to `slog.HandlerOptions.Level`.
- Parsing is strict lowercase (`debug|info|warn|error`); uppercase and offset syntax are rejected. `config.Parse` stays error-free; validation + fail-fast live in `parseLogLevel`/`run()`. An invalid value exits non-zero with `invalid log-level "<v>": want debug|info|warn|error`.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.
