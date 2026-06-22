# Runtime Log-Level Control Endpoint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator change the running daemon's log threshold without a restart, via an auth-protected `GET`/`POST /v1/loglevel` endpoint and a `cm loglevel` verb.

**Architecture:** Replace the fixed `slog.Level` handed to the log handler with one shared `*slog.LevelVar` (read atomically per record), so a single `Set` re-thresholds every logger at once. Move the strict level grammar from `package main` into `internal/config` so the startup path and the new endpoint validate identically. The endpoint rides the existing authed `v1` sub-mux, so auth/timeout/metrics-labeling come for free.

**Tech Stack:** Go 1.22, stdlib `log/slog` + `net/http` only. No new dependency, package, migration, schema, or SSE event.

## Global Constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no new external package; no migration; no schema change; no new SSE event kind.
- Default startup behavior is **byte-for-byte unchanged**: the `LevelVar` is initialized to the same level the prior fixed path produced.
- Reuse the existing strict level grammar and its exact error text (`invalid log-level %q: want debug|info|warn|error`); the runtime endpoint and the startup path validate through the same `config.ParseLogLevel`.
- The endpoint is auth-protected via registration on the existing `v1` sub-mux — no new auth wiring, no `routeLabel` change.
- `*slog.LevelVar` for the live threshold (atomic; no extra locking).
- Reuse the api package's existing `writeJSON(w, status, v)` / `writeError(w, status, msg)` helpers (in `internal/api/middleware.go`).
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.

---

## File Structure

- **Create** `internal/config/loglevel.go` — `ParseLogLevel` (moved+exported from `main`) + `LevelString` (new inverse).
- **Create** `internal/config/loglevel_test.go` — tests for both (the relocated `TestParseLogLevel` + a new `TestLevelString`).
- **Create** `internal/api/loglevel.go` — `handleGetLogLevel` / `handleSetLogLevel`.
- **Create** `internal/api/loglevel_test.go` — handler + auth + nil-guard tests.
- **Modify** `internal/api/handlers.go` — add `LogLevel *slog.LevelVar` field to `Server`.
- **Modify** `internal/api/dto.go` — add `logLevelRequest` / `logLevelResponse`.
- **Modify** `internal/api/router.go` — register the two routes on the `v1` sub-mux.
- **Modify** `cmd/magisterd/main.go` — delete local `parseLogLevel`; widen `newLogHandler` to take a `slog.Leveler`; build the `*slog.LevelVar` in `run()`; wire it into the `api.Server` literal.
- **Modify** `cmd/magisterd/main_test.go` — remove the now-relocated `TestParseLogLevel` (the `newLogHandler` test calls need no change — `slog.Level` satisfies `slog.Leveler`).
- **Modify** `cmd/cm/main.go` — add the `loglevel` dispatch case + method + usage string.
- **Modify** `cmd/cm/main_test.go` — add `cm loglevel` get/set/error tests.

**Task order is dependency-driven:** Task 1 (config) has no deps; Task 2 (api) consumes `config.ParseLogLevel`/`LevelString`; Task 3 (daemon) consumes both the config helpers and the new `Server.LogLevel` field; Task 4 (cm) is compile-independent (tests use an HTTP stub) but documents the endpoint contract, so it goes last.

---

## Task 1: Shared level grammar in `internal/config`

**Files:**
- Create: `internal/config/loglevel.go`
- Test: `internal/config/loglevel_test.go`

**Interfaces:**
- Consumes: nothing (leaf).
- Produces: `config.ParseLogLevel(s string) (slog.Level, error)` — strict lowercase parse, exact error `invalid log-level %q: want debug|info|warn|error`. `config.LevelString(l slog.Level) string` — inverse for the four canonical levels (lowercase), fallback `strings.ToLower(l.String())`.

- [ ] **Step 1: Write the failing tests**

Create `internal/config/loglevel_test.go`:

```go
package config

import (
	"log/slog"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	valid := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for s, want := range valid {
		got, err := ParseLogLevel(s)
		if err != nil {
			t.Errorf("ParseLogLevel(%q) unexpected error: %v", s, err)
		}
		if got != want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", s, got, want)
		}
	}
	for _, bad := range []string{"trace", "INFO", "info+2", ""} {
		_, err := ParseLogLevel(bad)
		if err == nil {
			t.Errorf("ParseLogLevel(%q) should return an error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "invalid log-level") {
			t.Errorf("ParseLogLevel(%q) error = %q, want it to mention invalid log-level", bad, err.Error())
		}
	}
}

func TestLevelString(t *testing.T) {
	for _, name := range []string{"debug", "info", "warn", "error"} {
		lvl, err := ParseLogLevel(name)
		if err != nil {
			t.Fatalf("ParseLogLevel(%q): %v", name, err)
		}
		if got := LevelString(lvl); got != name {
			t.Errorf("LevelString(%v) = %q, want %q", lvl, got, name)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestParseLogLevel|TestLevelString' -v`
Expected: FAIL — `undefined: ParseLogLevel` / `undefined: LevelString` (compile error).

- [ ] **Step 3: Implement the helpers**

Create `internal/config/loglevel.go`:

```go
package config

import (
	"fmt"
	"log/slog"
	"strings"
)

// ParseLogLevel maps a lowercase level name to a slog.Level. It deliberately
// rejects uppercase and slog's offset syntax (e.g. "INFO+2") so the surface is
// tight and predictable; an unknown value fails fast. Shared by the daemon's
// startup -log-level handling and the runtime POST /v1/loglevel endpoint.
func ParseLogLevel(s string) (slog.Level, error) {
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

// LevelString is the inverse of ParseLogLevel for the four canonical levels,
// rendering them as their lowercase names. Any other value (never produced by
// ParseLogLevel) falls back to slog's own lowercased label.
func LevelString(l slog.Level) string {
	switch l {
	case slog.LevelDebug:
		return "debug"
	case slog.LevelInfo:
		return "info"
	case slog.LevelWarn:
		return "warn"
	case slog.LevelError:
		return "error"
	default:
		return strings.ToLower(l.String())
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -run 'TestParseLogLevel|TestLevelString' -v`
Expected: PASS (both).

- [ ] **Step 5: Commit**

```bash
git add internal/config/loglevel.go internal/config/loglevel_test.go
git commit -m "feat(config): shared ParseLogLevel + LevelString helpers"
```

---

## Task 2: Auth-protected `GET`/`POST /v1/loglevel` endpoint

**Files:**
- Create: `internal/api/loglevel.go`
- Create: `internal/api/loglevel_test.go`
- Modify: `internal/api/handlers.go` (add `LogLevel` field to `Server`, struct at lines 26-41)
- Modify: `internal/api/dto.go` (add two DTOs near the others)
- Modify: `internal/api/router.go` (register routes on the `v1` sub-mux, after line 28)

**Interfaces:**
- Consumes: `config.ParseLogLevel`, `config.LevelString` (Task 1); `writeJSON(w, status, v)`, `writeError(w, status, msg)` (existing, `middleware.go`); the existing test helper `newServerOnly(t) (*Server, *supervisor.Supervisor, core.Store)` (in `handlers_test.go`).
- Produces: `Server.LogLevel *slog.LevelVar`; handlers `handleGetLogLevel` / `handleSetLogLevel`; DTOs `logLevelRequest{Level string}` / `logLevelResponse{Level string}`; routes `GET /v1/loglevel`, `POST /v1/loglevel`.

- [ ] **Step 1: Add the `LogLevel` field to `Server`**

In `internal/api/handlers.go`, inside the `Server` struct (after the `Metrics *metrics.Metrics` field, before the `draining atomic.Bool` field), add:

```go
	// LogLevel is the live log threshold; POST /v1/loglevel mutates it and every
	// logger built on the shared handler re-thresholds at once. nil = the endpoint
	// returns 503 (the daemon always wires it).
	LogLevel *slog.LevelVar
```

(`log/slog` is already imported in `handlers.go`.)

- [ ] **Step 2: Add the DTOs**

In `internal/api/dto.go`, after the `errorResponse` struct (lines 96-99), add:

```go
// logLevelRequest is the JSON body of POST /v1/loglevel.
type logLevelRequest struct {
	Level string `json:"level"`
}

// logLevelResponse is returned from GET and POST /v1/loglevel.
type logLevelResponse struct {
	Level string `json:"level"`
}
```

- [ ] **Step 3: Write the failing handler tests**

Create `internal/api/loglevel_test.go`:

```go
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func levelOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatalf("decode loglevel body: %v", err)
	}
	return b["level"]
}

func TestGetLogLevelReturnsCurrent(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelWarn)
	srv.LogLevel = lv
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/loglevel")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/loglevel = %d, want 200", resp.StatusCode)
	}
	if got := levelOf(t, resp); got != "warn" {
		t.Errorf("level = %q, want warn", got)
	}
}

func TestSetLogLevelChangesLevel(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelInfo)
	srv.LogLevel = lv
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	resp, err := http.Post(hs.URL+"/v1/loglevel", "application/json", strings.NewReader(`{"level":"debug"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/loglevel = %d, want 200", resp.StatusCode)
	}
	if got := levelOf(t, resp); got != "debug" {
		t.Errorf("echoed level = %q, want debug", got)
	}
	if lv.Level() != slog.LevelDebug {
		t.Errorf("LevelVar = %v, want Debug (the live threshold did not change)", lv.Level())
	}
}

func TestSetLogLevelRejectsBadValue(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	srv.LogLevel = new(slog.LevelVar)
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	resp, err := http.Post(hs.URL+"/v1/loglevel", "application/json", strings.NewReader(`{"level":"bogus"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST bad level = %d, want 400", resp.StatusCode)
	}
	var b map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&b)
	if !strings.Contains(b["error"], "invalid log-level") {
		t.Errorf("error = %q, want it to mention invalid log-level", b["error"])
	}
}

func TestSetLogLevelRejectsBadJSON(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	srv.LogLevel = new(slog.LevelVar)
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	resp, err := http.Post(hs.URL+"/v1/loglevel", "application/json", strings.NewReader(`{bad`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST malformed json = %d, want 400", resp.StatusCode)
	}
}

func TestLogLevelNilReturns503(t *testing.T) {
	srv, _, _ := newServerOnly(t) // LogLevel left nil
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()

	get, err := http.Get(hs.URL + "/v1/loglevel")
	if err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if get.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET with nil LogLevel = %d, want 503", get.StatusCode)
	}
	post, err := http.Post(hs.URL+"/v1/loglevel", "application/json", strings.NewReader(`{"level":"debug"}`))
	if err != nil {
		t.Fatal(err)
	}
	post.Body.Close()
	if post.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("POST with nil LogLevel = %d, want 503", post.StatusCode)
	}
}

func TestLogLevelBehindAuth(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	srv.LogLevel = new(slog.LevelVar)
	hs := httptest.NewServer(srv.Router("secret"))
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/loglevel")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated /v1/loglevel = %d, want 401", resp.StatusCode)
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `go test ./internal/api/ -run 'LogLevel' -v`
Expected: FAIL — routes not registered (`404`) and `handleGetLogLevel`/`handleSetLogLevel` undefined once referenced; at this point the handlers don't exist so the package may compile (routes absent) and the tests fail on status codes (404 ≠ 200). Either a compile failure or status mismatch counts as the expected red.

- [ ] **Step 5: Implement the handlers**

Create `internal/api/loglevel.go`:

```go
package api

import (
	"encoding/json"
	"net/http"

	"concentus/internal/config"
)

// handleGetLogLevel reports the daemon's current live log threshold.
func (s *Server) handleGetLogLevel(w http.ResponseWriter, r *http.Request) {
	if s.LogLevel == nil {
		writeError(w, http.StatusServiceUnavailable, "log level control unavailable")
		return
	}
	writeJSON(w, http.StatusOK, logLevelResponse{Level: config.LevelString(s.LogLevel.Level())})
}

// handleSetLogLevel changes the daemon's live log threshold. The new level takes
// effect immediately for every logger built on the shared handler.
func (s *Server) handleSetLogLevel(w http.ResponseWriter, r *http.Request) {
	if s.LogLevel == nil {
		writeError(w, http.StatusServiceUnavailable, "log level control unavailable")
		return
	}
	var req logLevelRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	lvl, err := config.ParseLogLevel(req.Level)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.LogLevel.Set(lvl)
	writeJSON(w, http.StatusOK, logLevelResponse{Level: config.LevelString(lvl)})
}
```

- [ ] **Step 6: Register the routes**

In `internal/api/router.go`, inside `Router`, after the existing `v1.HandleFunc("POST /v1/runs/{id}/ship", s.handleShip)` line (line 28), add:

```go
	v1.HandleFunc("GET /v1/loglevel", s.handleGetLogLevel)
	v1.HandleFunc("POST /v1/loglevel", s.handleSetLogLevel)
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/api/ -run 'LogLevel' -v`
Expected: PASS (all six).

- [ ] **Step 8: Run the full api package + vet/gofmt**

Run: `go test ./internal/api/ && go vet ./internal/api/ && gofmt -l internal/api/`
Expected: api tests PASS, vet silent, `gofmt -l` prints nothing.

- [ ] **Step 9: Commit**

```bash
git add internal/api/loglevel.go internal/api/loglevel_test.go internal/api/handlers.go internal/api/dto.go internal/api/router.go
git commit -m "feat(api): runtime log-level control endpoint"
```

---

## Task 3: Daemon shares the parser and drives a `*slog.LevelVar`

**Files:**
- Modify: `cmd/magisterd/main.go` (delete `parseLogLevel` lines 61-77; widen `newLogHandler` line 82; rework `run()` lines 97-106; `api.Server` literal line 137)
- Modify: `cmd/magisterd/main_test.go` (remove the relocated `TestParseLogLevel`, lines 163-189)

**Interfaces:**
- Consumes: `config.ParseLogLevel` (Task 1); `api.Server.LogLevel *slog.LevelVar` (Task 2).
- Produces: nothing new for later tasks (this wires the daemon end-to-end).

- [ ] **Step 1: Remove the relocated test from `main_test.go`**

In `cmd/magisterd/main_test.go`, delete the entire `TestParseLogLevel` function (lines 163-189). It has moved to `internal/config/loglevel_test.go` (Task 1) — this is a relocation, not a deletion of coverage. Leave `TestNewLogHandlerText`, `TestNewLogHandlerJSON`, `TestNewLogHandlerInvalid`, `TestRunRejectsBadLogFormat`, `TestNewLogHandlerAppliesLevel`, and `TestRunRejectsBadLogLevel` untouched (the `newLogHandler` calls passing `slog.LevelInfo`/`slog.LevelWarn` still compile — `slog.Level` satisfies `slog.Leveler`).

- [ ] **Step 2: Run the daemon tests to confirm the suite is green before the source change**

Run: `go test ./cmd/magisterd/`
Expected: PASS (TestParseLogLevel is gone; everything else still passes — `parseLogLevel` is still defined in `main.go` at this point).

- [ ] **Step 3: Delete the local `parseLogLevel` and widen `newLogHandler`**

In `cmd/magisterd/main.go`, delete the `parseLogLevel` function together with its doc comment (lines 61-77, the block from `// parseLogLevel maps...` through the closing `}`).

Then change the `newLogHandler` signature and doc comment (lines 79-82). Replace:

```go
// newLogHandler builds the root slog handler for the daemon. format is "text"
// (default, key=value) or "json" (one JSON object per line); any other value is
// rejected so a typo fails fast instead of silently logging in the wrong format.
func newLogHandler(format string, level slog.Level, w io.Writer) (slog.Handler, error) {
```

with:

```go
// newLogHandler builds the root slog handler for the daemon. format is "text"
// (default, key=value) or "json" (one JSON object per line); any other value is
// rejected so a typo fails fast instead of silently logging in the wrong format.
// level is the handler's minimum severity as a slog.Leveler, so a *slog.LevelVar
// can be passed to make the threshold adjustable at runtime.
func newLogHandler(format string, level slog.Leveler, w io.Writer) (slog.Handler, error) {
```

(The body — `opts := &slog.HandlerOptions{Level: level}` — is unchanged; `HandlerOptions.Level` is already a `slog.Leveler`.)

- [ ] **Step 4: Build a `*slog.LevelVar` in `run()` and wire it into the Server**

In `run()`, replace the level-parse + handler block (lines 97-106):

```go
	cfg := config.Parse(args, env)
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

with:

```go
	cfg := config.Parse(args, env)
	lvl, err := config.ParseLogLevel(cfg.LogLevel)
	if err != nil {
		return err
	}
	lvlVar := new(slog.LevelVar)
	lvlVar.Set(lvl)
	h, err := newLogHandler(cfg.LogFormat, lvlVar, os.Stderr)
	if err != nil {
		return err
	}
	log := slog.New(h)
```

Then, in the `api.Server` struct literal (line 137), add the `LogLevel` field. Replace:

```go
	srv := &api.Server{Sup: sup, Store: st, Bus: bus, Log: log, ScratchRoot: runsRoot, Metrics: mx}
```

with:

```go
	srv := &api.Server{Sup: sup, Store: st, Bus: bus, Log: log, ScratchRoot: runsRoot, Metrics: mx, LogLevel: lvlVar}
```

- [ ] **Step 5: Build + run the daemon tests**

Run: `go build ./cmd/magisterd/ && go test ./cmd/magisterd/ -v`
Expected: build succeeds; all remaining tests PASS. In particular `TestRunRejectsBadLogLevel` still passes — `run` now returns the error from `config.ParseLogLevel`, whose message still contains `invalid log-level`.

- [ ] **Step 6: Vet + gofmt**

Run: `go vet ./cmd/magisterd/ && gofmt -l cmd/magisterd/`
Expected: vet silent; `gofmt -l` prints nothing.

- [ ] **Step 7: Commit**

```bash
git add cmd/magisterd/main.go cmd/magisterd/main_test.go
git commit -m "feat(magisterd): runtime-adjustable log level via shared LevelVar"
```

---

## Task 4: `cm loglevel` client verb

**Files:**
- Modify: `cmd/cm/main.go` (usage string line 33; dispatch switch after line 69; add the `loglevel` method + body struct)
- Modify: `cmd/cm/main_test.go` (add get/set/error tests)

**Interfaces:**
- Consumes: the endpoint contract from Task 2 (`GET`/`POST /v1/loglevel`, body `{"level":"..."}`, response `{"level":"..."}`); existing client helpers `c.get(path, out)`, `printErr(resp, out)`.
- Produces: `cm loglevel [<level>]`.

- [ ] **Step 1: Write the failing tests**

In `cmd/cm/main_test.go`, add (the file already imports `bytes`, `encoding/json`, `io`, `net/http`, `net/http/httptest`, `strings`, `testing`):

```go
func TestLogLevelGetPrintsCurrent(t *testing.T) {
	var rec http.Request
	srv := fakeAPI(t, http.StatusOK, `{"level":"warn"}`, &rec)
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"loglevel"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if rec.Method != http.MethodGet || rec.URL.Path != "/v1/loglevel" {
		t.Errorf("request = %s %s, want GET /v1/loglevel", rec.Method, rec.URL.Path)
	}
	if !strings.Contains(out.String(), "warn") {
		t.Errorf("output missing level: %q", out.String())
	}
}

func TestLogLevelSetSendsJSONBody(t *testing.T) {
	var got http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeBody(w, `{"level":"debug"}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"loglevel", "debug"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v1/loglevel" {
		t.Errorf("request = %s %s, want POST /v1/loglevel", got.Method, got.URL.Path)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if sent["level"] != "debug" {
		t.Errorf("body = %v, want level=debug", sent)
	}
	if !strings.Contains(out.String(), "debug") {
		t.Errorf("output missing echoed level: %q", out.String())
	}
}

func TestLogLevelNon200PrintsError(t *testing.T) {
	var rec http.Request
	srv := fakeAPI(t, http.StatusBadRequest, `{"error":"invalid log-level \"bogus\": want debug|info|warn|error"}`, &rec)
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"loglevel", "bogus"}, srv.URL, &out); code == 0 {
		t.Fatalf("exit = 0, want non-zero; out=%s", out.String())
	}
	if !strings.Contains(out.String(), "invalid log-level") {
		t.Errorf("output missing server error: %q", out.String())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/cm/ -run 'LogLevel' -v`
Expected: FAIL — `dispatch(["loglevel"])` hits the `default` case (unknown command, exit 2), so the assertions fail.

- [ ] **Step 3: Add the dispatch case + usage string**

In `cmd/cm/main.go`, update the usage line (line 33) from:

```go
		fmt.Fprintln(out, "usage: cm <run|ls|get|watch|approve|reject|cancel|push|pr|ship> ...")
```

to:

```go
		fmt.Fprintln(out, "usage: cm <run|ls|get|watch|approve|reject|cancel|push|pr|ship|loglevel> ...")
```

Then, in the `switch args[0]` block, add a case after `case "ship":` (line 68-69), before `default:`:

```go
	case "loglevel":
		return c.loglevel(args[1:], out)
```

- [ ] **Step 4: Implement the `loglevel` method**

In `cmd/cm/main.go`, add (e.g. after the `get` method, near line 212):

```go
// logLevelBody is the JSON body cm sends to POST /v1/loglevel.
type logLevelBody struct {
	Level string `json:"level"`
}

// loglevel reports (no arg) or sets (one arg) the daemon's live log threshold.
func (c *client) loglevel(args []string, out io.Writer) int {
	if len(args) == 0 {
		return c.get("/v1/loglevel", out)
	}
	body, err := json.Marshal(logLevelBody{Level: args[0]})
	if err != nil {
		fmt.Fprintln(out, "loglevel:", err)
		return 1
	}
	resp, err := c.http.Post(c.base+"/v1/loglevel", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(out, "loglevel:", err)
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
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./cmd/cm/ -run 'LogLevel' -v`
Expected: PASS (all three).

- [ ] **Step 6: Run the full cm package + vet/gofmt**

Run: `go test ./cmd/cm/ && go vet ./cmd/cm/ && gofmt -l cmd/cm/`
Expected: cm tests PASS, vet silent, `gofmt -l` prints nothing.

- [ ] **Step 7: Commit**

```bash
git add cmd/cm/main.go cmd/cm/main_test.go
git commit -m "feat(cm): loglevel verb reads/sets the daemon log level"
```

---

## Final verification (after all tasks)

- [ ] **Whole suite, race detector:** `go test -race ./...` — all packages green.
- [ ] **Vet + format:** `go vet ./... && gofmt -l .` — vet silent, `gofmt -l` prints nothing.
- [ ] **Live smoke** (the user-facing payoff; manual, per the spec): build both binaries, start the daemon at default `-log-level info` (sandbox-disabled), run `flows/git-native-merge.yaml` → no engine Debug lines; `cm loglevel debug` (prints `debug`), `cm loglevel` (confirms `debug`); re-run the flow → the engine's Debug lines now stream mid-run; `cm loglevel info` → quiet again; `cm loglevel bogus` → non-zero exit + `400 invalid log-level "bogus": want debug|info|warn|error`. (See `.claude/skills/running-the-orchestrator/SKILL.md` for the daemon/flow recipe.)

---

## Notes for the implementer

- **Model:** every task is complete-code transcription + testing → cheap-tier implementers and reviewers are appropriate; the final whole-branch review uses the most capable model.
- **Don't touch `go.mod`.** Everything here is stdlib (`log/slog`, `net/http`, `encoding/json`, `strings`, `fmt`) plus the existing internal packages.
- **The one structural change** is `newLogHandler`'s parameter widening from `slog.Level` to `slog.Leveler`; `slog.Level` satisfies `slog.Leveler`, so the existing test call sites compile unchanged — only `run()` passes the new `*slog.LevelVar`.
- **`TestParseLogLevel` is moved, not duplicated:** Task 1 adds it under `internal/config`; Task 3 deletes it from `cmd/magisterd/main_test.go`. Both happen in this branch, so total coverage is preserved with no duplication.
```
