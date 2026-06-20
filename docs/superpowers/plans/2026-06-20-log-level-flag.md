# Selectable log level Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `-log-level debug|info|warn|error` flag (env `MAGISTER_LOG_LEVEL`, default `info`) that sets the daemon's root `slog` handler threshold, with an invalid value failing fast at startup.

**Architecture:** One new `Config.LogLevel` field (following the `-log-format`/`-shutdown-drain` pattern), a strict `parseLogLevel(s) (slog.Level, error)` validator in `main.go`, and a new `level slog.Level` parameter on the existing `newLogHandler` factory; `run()` parses the level (fail-fast) then builds the handler with it. `info` (default) is byte-for-byte today's behavior. Stdlib only.

**Tech Stack:** Go 1.22, standard library (`log/slog`, `flag`, `fmt`, `io`). Packages `internal/config` and `cmd/magisterd`.

## Global Constraints

- Go 1.22; **stdlib only, NO new dependency** (do not touch `go.mod`); no new package; no migration; no schema change.
- Default is `info` and produces byte-for-byte today's behavior; the value maps to `slog.HandlerOptions.Level`.
- Parsing is strict lowercase (`debug|info|warn|error`); uppercase and offset syntax (`INFO`, `info+2`) are rejected. `config.Parse` stays error-free; validation + fail-fast live in `parseLogLevel`/`run()`. An invalid value makes `run()` return an error so the daemon exits non-zero with `invalid log-level "<v>": want debug|info|warn|error`.
- Only the root daemon logger's threshold changes; *what* is logged and the format flag are unchanged.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`. `gofmt -l`, `go vet`, and the relevant `go test -race` clean before each commit.

## File Structure

- `internal/config/config.go` — add `LogLevel` field + flag + env override. (Task 1)
- `cmd/magisterd/main.go` — add `parseLogLevel`; add a `level slog.Level` param to `newLogHandler`; parse+wire in `run()`. (Task 2)

---

### Task 1: `Config.LogLevel` (flag + env)

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.LogLevel string` — defaults to `"info"`, set by `-log-level` or `MAGISTER_LOG_LEVEL` (flag wins). Consumed by Task 2.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go` (package `config`; `testing`/`time` already imported — no new import):

```go
func TestLogLevelDefault(t *testing.T) {
	c := Parse(nil, func(string) string { return "" })
	if c.LogLevel != "info" {
		t.Errorf("default LogLevel = %q, want info", c.LogLevel)
	}
}

func TestLogLevelFlag(t *testing.T) {
	c := Parse([]string{"-log-level", "debug"}, func(string) string { return "" })
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel flag = %q, want debug", c.LogLevel)
	}
}

func TestLogLevelEnv(t *testing.T) {
	env := func(k string) string {
		if k == "MAGISTER_LOG_LEVEL" {
			return "warn"
		}
		return ""
	}
	c := Parse(nil, env)
	if c.LogLevel != "warn" {
		t.Errorf("LogLevel from env = %q, want warn", c.LogLevel)
	}
}

func TestLogLevelFlagWinsOverEnv(t *testing.T) {
	env := func(k string) string {
		if k == "MAGISTER_LOG_LEVEL" {
			return "warn"
		}
		return ""
	}
	c := Parse([]string{"-log-level", "info"}, env)
	if c.LogLevel != "info" {
		t.Errorf("explicit flag should win over env: got %q, want info", c.LogLevel)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run TestLogLevel`
Expected: compile failure — `c.LogLevel undefined (type Config has no field or method LogLevel)`.

- [ ] **Step 3: Add the field, flag, and env override**

In `internal/config/config.go`:

1. Add the field to the `Config` struct (after `LogFormat string`):

```go
	LogLevel string
```

2. Register the flag alongside the others (e.g. right after the `log-format` flag):

```go
	fs.StringVar(&c.LogLevel, "log-level", "info", "log level: debug, info, warn, or error")
```

3. Add the env override in the env-resolution block (after the `MAGISTER_LOG_FORMAT` block):

```go
	if v := env("MAGISTER_LOG_LEVEL"); v != "" && !flagSet(fs, "log-level") {
		c.LogLevel = v
	}
```

(No validation here — `Parse` stays error-free; the validator in Task 2 validates.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/config/`
Expected: PASS — the four new tests plus the existing config suite.

- [ ] **Step 5: Verify formatting and vet**

Run: `gofmt -l internal/config/ && go vet ./internal/config/`
Expected: no `gofmt -l` output; `go vet` clean.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): -log-level flag and MAGISTER_LOG_LEVEL env"
```

---

### Task 2: `parseLogLevel` + level-aware `newLogHandler` + wire `run()`

**Files:**
- Modify: `cmd/magisterd/main.go`
- Test: `cmd/magisterd/main_test.go`

**Interfaces:**
- Consumes: `Config.LogLevel` (Task 1); `Config.LogFormat` (existing).
- Produces: `parseLogLevel(s string) (slog.Level, error)` (strict lowercase) and the updated `newLogHandler(format string, level slog.Level, w io.Writer) (slog.Handler, error)`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/magisterd/main_test.go` (package `main`). No new imports — `bytes`, `log/slog`, `strings`, `io`, `testing` are already imported.

```go
func TestParseLogLevel(t *testing.T) {
	valid := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for s, want := range valid {
		got, err := parseLogLevel(s)
		if err != nil {
			t.Errorf("parseLogLevel(%q) unexpected error: %v", s, err)
		}
		if got != want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", s, got, want)
		}
	}
	for _, bad := range []string{"trace", "INFO", "info+2", ""} {
		_, err := parseLogLevel(bad)
		if err == nil {
			t.Errorf("parseLogLevel(%q) should return an error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "invalid log-level") {
			t.Errorf("parseLogLevel(%q) error = %q, want it to mention invalid log-level", bad, err.Error())
		}
	}
}

func TestNewLogHandlerAppliesLevel(t *testing.T) {
	var buf bytes.Buffer
	h, err := newLogHandler("text", slog.LevelWarn, &buf)
	if err != nil {
		t.Fatalf("newLogHandler: %v", err)
	}
	log := slog.New(h)
	log.Info("below-threshold")
	if buf.Len() != 0 {
		t.Errorf("Info should be suppressed at Warn level, got: %s", buf.String())
	}
	log.Warn("above-threshold")
	if !strings.Contains(buf.String(), "above-threshold") {
		t.Errorf("Warn should be emitted at Warn level, got: %s", buf.String())
	}
}

func TestRunRejectsBadLogLevel(t *testing.T) {
	stop := make(chan struct{})
	err := run([]string{"-log-level", "trace"}, func(string) string { return "" }, stop, nil)
	if err == nil {
		t.Fatal("run with -log-level trace should return an error")
	}
	if !strings.Contains(err.Error(), "invalid log-level") {
		t.Errorf("run error = %q, want invalid log-level", err.Error())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/magisterd/ -run 'TestParseLogLevel|TestNewLogHandlerAppliesLevel|TestRunRejectsBadLogLevel'`
Expected: compile failure — `undefined: parseLogLevel` and `too many arguments in call to newLogHandler` (the new threshold test passes 3 args).

- [ ] **Step 3: Add `parseLogLevel`, update `newLogHandler`, fix existing call sites, wire `run()`**

In `cmd/magisterd/main.go`:

1. Add the validator (e.g. just above `newLogHandler`):

```go
// parseLogLevel maps a lowercase level name to a slog.Level. It deliberately
// rejects uppercase and slog's offset syntax (e.g. "INFO+2") so the surface is
// tight and predictable; an unknown value fails fast.
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

2. Change `newLogHandler` to take the level and use it (replace the function header and the `opts` line):

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

3. Update the THREE existing `newLogHandler` calls in `cmd/magisterd/main_test.go` to pass `slog.LevelInfo` as the new second argument:
   - line ~108: `newLogHandler("text", &buf)` → `newLogHandler("text", slog.LevelInfo, &buf)`
   - line ~124: `newLogHandler("json", &buf)` → `newLogHandler("json", slog.LevelInfo, &buf)`
   - line ~143: `newLogHandler("xml", io.Discard)` → `newLogHandler("xml", slog.LevelInfo, io.Discard)`

4. In `run()`, replace the current
```go
	h, err := newLogHandler(cfg.LogFormat, os.Stderr)
	if err != nil {
		return err
	}
	log := slog.New(h)
```
with:
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
(`lvl, err :=` then `h, err :=` is valid because `lvl`/`h` are new each time; the later `st, err := store.Open(...)` stays `:=` because `st` is new. No imports change — `fmt`, `io`, `slog`, `os` are all already imported.)

- [ ] **Step 4: Run the package tests to verify they pass**

Run: `go test -race ./cmd/magisterd/`
Expected: PASS — the three new tests, the three updated `newLogHandler` tests, and the existing `TestRunServesHealthzAndShutsDown`.

- [ ] **Step 5: Run the whole suite + verify formatting/vet**

Run: `go test -race ./... && gofmt -l internal cmd && go vet ./...`
Expected: ALL packages PASS (report the count). No `gofmt -l` output; `go vet` clean.

- [ ] **Step 6: Commit**

```bash
git add cmd/magisterd/main.go cmd/magisterd/main_test.go
git commit -m "feat(magisterd): -log-level sets the slog handler threshold"
```

---

## Notes for the implementer

- `parseLogLevel` returns `0` on error; the caller checks the error and returns before using the level, so the zero value is never observed (mirrors `newLogHandler` returning a nil handler on its error path).
- `newLogHandler` keeps its existing `-log-format` validation and error wording unchanged — this task only adds the level parameter; do not alter the format switch.
- Default `-log-level info` must keep the daemon's output identical to today (`slog.LevelInfo`). Do not change `-log-format` behavior.
- The fail-fast test relies on `parseLogLevel` being called early in `run()` (right after `Parse`, before any listener bind) — keep it before `newLogHandler` and `store.Open`.
- The post-Edit hook emits a harmless path-doubling error on worktree edits; the edit still succeeds.
