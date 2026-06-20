# Selectable JSON log format Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `-log-format text|json` flag (env `MAGISTER_LOG_FORMAT`, default `text`) selecting the daemon's root `slog` handler, with an invalid value failing fast at startup.

**Architecture:** One new `Config.LogFormat` field following the existing `-shutdown-drain`/env pattern, plus a small `main.go`-local `newLogHandler(format, w) (slog.Handler, error)` factory wired into `run()` at the logger-construction seam. `text` (default) keeps today's output byte-for-byte; `json` uses `slog.NewJSONHandler`. Stdlib only.

**Tech Stack:** Go 1.22, standard library (`log/slog`, `flag`, `fmt`, `io`). Packages `internal/config` and `cmd/magisterd`.

## Global Constraints

- Go 1.22; **stdlib only, NO new dependency** (do not touch `go.mod`); no new package; no migration; no schema change.
- Default is `text` and produces byte-for-byte today's output; `json` uses `slog.NewJSONHandler`; both keep `Level: slog.LevelInfo`.
- `config.Parse` stays error-free; validation + fail-fast live in `newLogHandler`/`run()`. An invalid value makes `run()` return an error so the daemon exits non-zero with `invalid log-format "<v>": want text|json`.
- Only the root daemon logger changes; *what* is logged, field names, and log level (Info) are unchanged.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`. `gofmt -l`, `go vet`, and the relevant `go test -race` clean before each commit.

## File Structure

- `internal/config/config.go` — add `LogFormat` field + flag + env override. (Task 1)
- `cmd/magisterd/main.go` — add `newLogHandler` factory; build the root logger from it in `run()`; add `fmt`+`io` imports. (Task 2)

---

### Task 1: `Config.LogFormat` (flag + env)

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.LogFormat string` — defaults to `"text"`, set by `-log-format` or `MAGISTER_LOG_FORMAT` (flag wins). Consumed by Task 2.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go` (package `config`; imports `testing`, `time` already present — no new import needed):

```go
func TestLogFormatDefault(t *testing.T) {
	c := Parse(nil, func(string) string { return "" })
	if c.LogFormat != "text" {
		t.Errorf("default LogFormat = %q, want text", c.LogFormat)
	}
}

func TestLogFormatFlag(t *testing.T) {
	c := Parse([]string{"-log-format", "json"}, func(string) string { return "" })
	if c.LogFormat != "json" {
		t.Errorf("LogFormat flag = %q, want json", c.LogFormat)
	}
}

func TestLogFormatEnv(t *testing.T) {
	env := func(k string) string {
		if k == "MAGISTER_LOG_FORMAT" {
			return "json"
		}
		return ""
	}
	c := Parse(nil, env)
	if c.LogFormat != "json" {
		t.Errorf("LogFormat from env = %q, want json", c.LogFormat)
	}
}

func TestLogFormatFlagWinsOverEnv(t *testing.T) {
	env := func(k string) string {
		if k == "MAGISTER_LOG_FORMAT" {
			return "json"
		}
		return ""
	}
	c := Parse([]string{"-log-format", "text"}, env)
	if c.LogFormat != "text" {
		t.Errorf("explicit flag should win over env: got %q, want text", c.LogFormat)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run TestLogFormat`
Expected: compile failure — `c.LogFormat undefined (type Config has no field or method LogFormat)`.

- [ ] **Step 3: Add the field, flag, and env override**

In `internal/config/config.go`:

1. Add the field to the `Config` struct (after `ShutdownDrain time.Duration`):

```go
	LogFormat string
```

2. Register the flag, alongside the other `fs.*Var` calls (e.g. right after the `shutdown-drain` flag):

```go
	fs.StringVar(&c.LogFormat, "log-format", "text", "log output format: text or json")
```

3. Add the env override in the env-resolution block (mirroring the `MAGISTER_DB` string block — fill only when the flag was not set), e.g. after the `MAGISTER_DB` block:

```go
	if v := env("MAGISTER_LOG_FORMAT"); v != "" && !flagSet(fs, "log-format") {
		c.LogFormat = v
	}
```

(No validation here — `Parse` stays error-free; the factory in Task 2 validates.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/config/`
Expected: PASS — the four new tests plus the existing config suite.

- [ ] **Step 5: Verify formatting and vet**

Run: `gofmt -l internal/config/ && go vet ./internal/config/`
Expected: no `gofmt -l` output; `go vet` clean.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): -log-format flag and MAGISTER_LOG_FORMAT env"
```

---

### Task 2: `newLogHandler` factory + wire into `run()`

**Files:**
- Modify: `cmd/magisterd/main.go`
- Test: `cmd/magisterd/main_test.go`

**Interfaces:**
- Consumes: `Config.LogFormat` (Task 1).
- Produces: `newLogHandler(format string, w io.Writer) (slog.Handler, error)` — `"text"`→`*slog.TextHandler`, `"json"`→`*slog.JSONHandler` (both `Level: slog.LevelInfo`), else a non-nil error.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/magisterd/main_test.go` (package `main`). Add `"bytes"`, `"encoding/json"`, and `"strings"` to the import block (`io`, `log/slog`, `testing` are already imported):

```go
func TestNewLogHandlerText(t *testing.T) {
	var buf bytes.Buffer
	h, err := newLogHandler("text", &buf)
	if err != nil {
		t.Fatalf("newLogHandler(text): %v", err)
	}
	slog.New(h).Info("hi", "k", "v")
	out := buf.String()
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("text format should not be JSON: %s", out)
	}
	if !strings.Contains(out, "msg=hi") || !strings.Contains(out, "k=v") {
		t.Errorf("text output missing key=value fields: %s", out)
	}
}

func TestNewLogHandlerJSON(t *testing.T) {
	var buf bytes.Buffer
	h, err := newLogHandler("json", &buf)
	if err != nil {
		t.Fatalf("newLogHandler(json): %v", err)
	}
	slog.New(h).Info("hi", "k", "v")
	line := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(line, "{") {
		t.Fatalf("json output should be a JSON object: %s", line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("json output not parseable: %v (%s)", err, line)
	}
	if m["msg"] != "hi" || m["k"] != "v" {
		t.Errorf("json fields wrong: %v", m)
	}
}

func TestNewLogHandlerInvalid(t *testing.T) {
	_, err := newLogHandler("xml", io.Discard)
	if err == nil {
		t.Fatal("newLogHandler(xml) should return an error")
	}
	if !strings.Contains(err.Error(), "invalid log-format") {
		t.Errorf("error message = %q, want it to mention invalid log-format", err.Error())
	}
}

func TestRunRejectsBadLogFormat(t *testing.T) {
	stop := make(chan struct{})
	err := run([]string{"-log-format", "xml"}, func(string) string { return "" }, stop, nil)
	if err == nil {
		t.Fatal("run with -log-format xml should return an error")
	}
	if !strings.Contains(err.Error(), "invalid log-format") {
		t.Errorf("run error = %q, want invalid log-format", err.Error())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/magisterd/ -run 'TestNewLogHandler|TestRunRejectsBadLogFormat'`
Expected: compile failure — `undefined: newLogHandler`.

- [ ] **Step 3: Add the factory and wire it into `run()`**

In `cmd/magisterd/main.go`:

1. Add `"fmt"` and `"io"` to the import block (neither is currently imported; `os` already is).

2. Add the factory function (e.g. just above `func run(...)`):

```go
// newLogHandler builds the root slog handler for the daemon. format is "text"
// (default, key=value) or "json" (one JSON object per line); any other value is
// rejected so a typo fails fast instead of silently logging in the wrong format.
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

3. In `run()`, replace the current logger line

```go
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
```

with:

```go
	h, err := newLogHandler(cfg.LogFormat, os.Stderr)
	if err != nil {
		return err
	}
	log := slog.New(h)
```

(Place it right after `cfg := config.Parse(args, env)`, where the logger line already sits. NOTE: `run()` later declares `st, err := store.Open(...)`; since `err` is now already declared by the `newLogHandler` line, that later line stays `st, err :=` only if `st` is new — it is, so `:=` remains valid. If the compiler reports "no new variables on left side", change only that later line to use `=` for `err`; do not introduce other changes.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./cmd/magisterd/`
Expected: PASS — the four new tests plus the existing `TestRunServesHealthzAndShutsDown`.

- [ ] **Step 5: Run the whole suite + verify formatting/vet**

Run: `go test -race ./... && gofmt -l internal cmd && go vet ./...`
Expected: ALL packages PASS (report the count). No `gofmt -l` output; `go vet` clean.

- [ ] **Step 6: Commit**

```bash
git add cmd/magisterd/main.go cmd/magisterd/main_test.go
git commit -m "feat(magisterd): select text or json log handler via -log-format"
```

---

## Notes for the implementer

- The factory takes an `io.Writer` only so tests can capture output; `run()` always passes `os.Stderr`.
- Both handlers must keep `Level: slog.LevelInfo` — do not change the level or add `AddSource`/`ReplaceAttr`; the default `text` path must stay byte-for-byte identical to today.
- The fail-fast test (`TestRunRejectsBadLogFormat`) relies on `newLogHandler` being called early in `run()` (right after `Parse`), before any listener bind — so the bad-format `run()` returns immediately without needing the `stop` channel or a real DB.
- Watch the `:=`/`=` interaction in `run()` from Step 3's note — that is the one likely compile snag.
- The post-Edit hook emits a harmless path-doubling error on worktree edits; the edit still succeeds.
