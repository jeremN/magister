# Observability — selectable JSON log format

## Summary

Add a `-log-format text|json` flag (env `MAGISTER_LOG_FORMAT`, default `text`) that selects the daemon's root `slog` handler. `text` keeps today's `key=value` output byte-for-byte; `json` emits one JSON object per line so logs can be machine-parsed (jq, log pipelines, structured ingest). An unrecognized value fails fast: the daemon refuses to start and exits non-zero with a clear message.

The whole change is at the logger-construction seam in `cmd/magisterd/main.go` plus one config field. Log *level* (Info), *what* is logged, and the single shared logger wired into Engine/Supervisor/Server/janitor are all unchanged. Stdlib only (`slog.NewJSONHandler`) — **no new dependency, no new package, no migration**.

## Motivation

The slice that just merged made a run's logs correlatable (`run`/`step`/`agent` fields, the `run submitted` bridge line). Those fields are only as useful as the operator's ability to query them. `slog`'s `TextHandler` is greppable but not reliably machine-parseable (values with spaces get quoted ad hoc); a `JSONHandler` makes every field a first-class key for `jq '.run'`-style pivots and ingestion into structured log stores. This is the natural companion to request-scoped logging and was explicitly deferred from it.

## Design

### Config — a `LogFormat` field

`internal/config/config.go` gains one field and follows the existing `-shutdown-drain`/`MAGISTER_SHUTDOWN_DRAIN` pattern exactly:

- `Config.LogFormat string`
- Flag: `fs.StringVar(&c.LogFormat, "log-format", "text", "log output format: text or json")`
- Env override (mirrors the `MAGISTER_DB` string block — fill only when the flag was not set):
  ```go
  if v := env("MAGISTER_LOG_FORMAT"); v != "" && !flagSet(fs, "log-format") {
      c.LogFormat = v
  }
  ```

`Parse` performs **no validation** — it stays error-free, consistent with how it already ignores a malformed `MAGISTER_SCRATCH_TTL`. Validation lives at the factory (below), where the daemon can fail fast with the actual value in hand. So `cfg.LogFormat` is `"text"` (default) or whatever non-empty value the flag/env supplied.

### Logger factory — `newLogHandler`

`cmd/magisterd/main.go` replaces the hardcoded line
```go
log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
```
with a call to a new unexported factory:

```go
func newLogHandler(format string, w io.Writer) (slog.Handler, error) {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
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

In `run()`, right after `cfg := config.Parse(args, env)`:
```go
h, err := newLogHandler(cfg.LogFormat, os.Stderr)
if err != nil {
	return err
}
log := slog.New(h)
```

`run()` returning the error is the fail-fast path: `main()` already does `if err := run(...); err != nil { slog.Error("magisterd exited with error", "err", err); os.Exit(1) }`, so a bad value exits non-zero with the message. The factory takes an `io.Writer` (not hardcoding `os.Stderr`) purely so tests can capture output into a buffer; `run()` always passes `os.Stderr`.

`main.go` must add `"fmt"` and `"io"` to its import block (neither is imported today). `os` is already imported. Both handlers keep `Level: slog.LevelInfo` — identical to today.

### Behavior

| `-log-format` | result |
|---|---|
| unset / `text` | `level=INFO msg="run submitted" req=01K… run=01K…` (today's output, unchanged) |
| `json` | `{"time":"…","level":"INFO","msg":"run submitted","req":"01K…","run":"01K…"}` |
| anything else | daemon exits 1: `magisterd exited with error err="invalid log-format \"xml\": want text|json"` |

## Testing

- **config** (`internal/config/config_test.go`): default `LogFormat` is `"text"`; `-log-format json` sets `"json"`; `MAGISTER_LOG_FORMAT=json` applies when the flag is unset; an explicit `-log-format text` flag wins over `MAGISTER_LOG_FORMAT=json` (precedence). Follow the existing table/style in that file.
- **main** (`cmd/magisterd/main_test.go`): `newLogHandler("text", buf)` then logging a record produces `key=value` text (not `{`-prefixed); `newLogHandler("json", buf)` produces a line beginning with `{` that `encoding/json` can unmarshal; `newLogHandler("xml", buf)` returns a non-nil error whose message contains `invalid log-format`. Plus one end-to-end fail-fast assertion: `run([]string{"-log-format", "xml"}, …)` returns a non-nil error (it must fail before binding a listener).
- Full `go test -race ./...` green; `go vet` + `gofmt -l` clean.

## Out of scope

- A `-log-level` flag (level stays Info; selectable verbosity is a separate slice).
- Changing *what* is logged, any field names, or the access-log / bridge-line content.
- Per-component or per-stream format selection — one root daemon logger.
- Adding source-location (`AddSource`), custom time formats, or `ReplaceAttr` hooks.
- A dedicated logging package — the factory is one local switch with a single call site.

## Global constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no new package; no migration; no schema change.
- Default is `text` and produces byte-for-byte today's output; `json` uses `slog.NewJSONHandler`; both keep `Level: slog.LevelInfo`.
- `config.Parse` stays error-free; validation + fail-fast live in `newLogHandler`/`run()`. An invalid value exits non-zero with `invalid log-format "<v>": want text|json`.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.
