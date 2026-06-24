# `cm tui` Terminal Dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A hand-rolled full-screen terminal dashboard (`cm tui`) to watch magisterd runs live and approve/reject/cancel/retry them, built as a pure client over the existing `/v1` REST + SSE API.

**Architecture:** New `internal/tui/` package split into a pure core (API client, SSE parser, model+reducer, view) and a thin imperative terminal driver — the Elm split, hand-rolled, so all logic is unit/golden-tested without a TTY. `cmd/cm/main.go` gains a `tui` subcommand. The daemon is unchanged; the server stays the source of truth (poll the run list; snapshot + SSE per focused run).

**Tech Stack:** Go 1.22, stdlib (`net/http`, `encoding/json`, `bufio`, `os/signal`), the existing `concentus/internal/event` type, and **one** new dep `golang.org/x/term` (raw mode + size).

## Global Constraints

- One new direct dependency only: `golang.org/x/term`, pinned to a release compatible with **`go 1.22`**; `go.mod` must still read `go 1.22` after adding it (verify — do not let it bump the go directive). No other new deps. Pinned deps untouched (modernc.org/sqlite v1.36.1, goose/v3 v3.24.1, oklog/ulid/v2 v2.1.1, expr-lang/expr v1.17.8, OTel v1.32.0).
- No daemon/engine/API changes. All new code lives in `internal/tui/` and a `case "tui"` in `cmd/cm/main.go`. No existing `cm` verb changes.
- Reuse `concentus/internal/event.Event` for the live event payload — do not redefine it.
- Commit hygiene: single conventional-commit subjects, no body, no `Co-Authored-By`, never `--no-verify`. `gofmt -l` clean.
- The TUI sends `Authorization: Bearer <MAGISTER_BEARER_TOKEN>` on every request (incl. SSE) when the env var is non-empty; otherwise no auth header.
- Tests are pure Go (httptest / byte streams / golden) — no real TTY in CI. The terminal driver (`driver.go`, `term.go`) is build- and manually-verified; the plan notes this explicitly per task.

## File Structure

| File | Responsibility |
|---|---|
| `internal/tui/term.go` | thin `golang.org/x/term` wrappers: terminal size, raw-mode enter/restore |
| `internal/tui/client.go` | typed REST client (list/get/approve/cancel/retry) + auth header |
| `internal/tui/sse.go` | SSE reader: parse `id:/event:/data:` frames → `event.Event`, resume by `Last-Event-ID` |
| `internal/tui/model.go` | `model` state + pure `update(model,msg)→(model,[]cmd)` reducer + msg/cmd types |
| `internal/tui/view.go` | pure `view(model,w,h)→string` screen renderer |
| `internal/tui/driver.go` | imperative shell: input loop, SIGWINCH, message loop, terminal restore |
| `cmd/cm/main.go` | add `case "tui"` + usage line |

---

## Task 1: `x/term` preflight + terminal wrappers

De-risks the single dependency unknown first, and provides the terminal primitives the driver needs.

**Files:**
- Create: `internal/tui/term.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Produces: `func size(fd int) (w, h int, err error)`; `func enterRaw(fd int) (restore func() error, err error)`. (`fd` is typically `int(os.Stdin.Fd())`.)

- [ ] **Step 1: Add the dependency and verify the go directive is unchanged.**

```bash
go get golang.org/x/term@v0.27.0
grep '^go ' go.mod    # MUST still print: go 1.22
go mod tidy
grep '^go ' go.mod    # MUST still print: go 1.22  (if it bumped, STOP — pick an older x/term)
```
Expected: `go 1.22` both times. If `go.mod` shows a higher go version, revert (`git checkout go.mod go.sum`) and retry with an older tag (`v0.24.0`, `v0.22.0`), recording which one stays on `go 1.22`.

- [ ] **Step 2: Write the wrappers.**

```go
// Package tui implements `cm tui`, a hand-rolled terminal dashboard for magisterd.
package tui

import "golang.org/x/term"

// size returns the terminal width and height in cells for the given fd.
func size(fd int) (w, h int, err error) {
	return term.GetSize(fd)
}

// enterRaw puts the terminal in raw mode and returns a restore func that puts it
// back. The caller MUST defer restore (including on panic) so the terminal is
// never left wedged.
func enterRaw(fd int) (restore func() error, err error) {
	st, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() error { return term.Restore(fd, st) }, nil
}
```

- [ ] **Step 3: Verify it builds.**

Run: `go build ./internal/tui/`
Expected: builds clean (no test — these are thin OS wrappers, build- and manually-verified; the reducer/view/client tasks carry the unit tests).

- [ ] **Step 4: Commit.**

```bash
gofmt -w internal/tui/term.go
git add go.mod go.sum internal/tui/term.go
git commit -m "feat(tui): add golang.org/x/term preflight and terminal wrappers"
```

---

## Task 2: REST API client

**Files:**
- Create: `internal/tui/client.go`, `internal/tui/client_test.go`

**Interfaces:**
- Produces:
  - `type RunSummary struct { ID, Name, Status string }`
  - `type StepView struct { ID, Status string; Attempt int }`
  - `type RunDetail struct { ID, Name, Status string; Steps []StepView }`
  - `type Client struct { ... }`; `func NewClient(base, token string) *Client`
  - `func (c *Client) ListRuns(ctx context.Context) ([]RunSummary, error)`
  - `func (c *Client) GetRun(ctx context.Context, id string) (RunDetail, error)`
  - `func (c *Client) Approve(ctx context.Context, id, step string, approve bool, reason string) error`
  - `func (c *Client) Cancel(ctx context.Context, id string) error`
  - `func (c *Client) Retry(ctx context.Context, id string) error`
- Consumes: the daemon DTOs — `GET /v1/runs` → `[{"id","name","status"}]`; `GET /v1/runs/{id}` → `{"id","name","status","steps":[{"id","status","attempt"}]}`; `POST /v1/runs/{id}/steps/{step}/approve` body `{"approve":bool,"reason":string}` (must match `api.approveRequest` field tags — verify in `internal/api/handlers.go`); `DELETE /v1/runs/{id}`; `POST /v1/runs/{id}/retry`. JSON decoding ignores extra fields, so the client structs deliberately carry only the rendered subset.

- [ ] **Step 1: Write the failing tests.**

```go
package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListRunsParsesAndSendsAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "a1", "name": "feature", "status": "running"},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sekret")
	runs, err := c.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != "a1" || runs[0].Status != "running" {
		t.Fatalf("got %+v", runs)
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("auth header = %q, want %q", gotAuth, "Bearer sekret")
	}
}

func TestNoAuthHeaderWhenTokenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Fatalf("unexpected Authorization header")
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{})
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "").ListRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGetRunParsesSteps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/a1" {
			t.Fatalf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"a1","name":"feature","status":"running","steps":[{"id":"plan","status":"succeeded","attempt":1}]}`))
	}))
	defer srv.Close()
	rd, err := NewClient(srv.URL, "").GetRun(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if rd.Name != "feature" || len(rd.Steps) != 1 || rd.Steps[0].ID != "plan" {
		t.Fatalf("got %+v", rd)
	}
}

func TestApproveSendsBody(t *testing.T) {
	var body struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs/a1/steps/plan/approve" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL, "").Approve(context.Background(), "a1", "plan", false, "nope"); err != nil {
		t.Fatal(err)
	}
	if body.Approve != false || body.Reason != "nope" {
		t.Fatalf("got %+v", body)
	}
}

func TestCancelSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method %s", r.Method)
		}
		http.Error(w, "run not active", http.StatusConflict)
	}))
	defer srv.Close()
	err := NewClient(srv.URL, "").Cancel(context.Background(), "a1")
	if err == nil {
		t.Fatal("want error on 409, got nil")
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`undefined: NewClient`).

Run: `go test ./internal/tui/ -run 'TestListRuns|TestNoAuth|TestGetRun|TestApprove|TestCancel' -count=1`

- [ ] **Step 3: Implement `client.go`.**

```go
package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type RunSummary struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type StepView struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Attempt int    `json:"attempt"`
}

type RunDetail struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Status string     `json:"status"`
	Steps  []StepView `json:"steps"`
}

// Client is a thin REST client for magisterd's /v1 API.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

func NewClient(base, token string) *Client {
	return &Client{base: base, token: token, hc: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) newReq(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// do executes req and, on a non-2xx, returns an error carrying the status.
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) ListRuns(ctx context.Context) ([]RunSummary, error) {
	req, err := c.newReq(ctx, http.MethodGet, "/v1/runs", nil)
	if err != nil {
		return nil, err
	}
	var out []RunSummary
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) GetRun(ctx context.Context, id string) (RunDetail, error) {
	req, err := c.newReq(ctx, http.MethodGet, "/v1/runs/"+id, nil)
	if err != nil {
		return RunDetail{}, err
	}
	var out RunDetail
	if err := c.do(req, &out); err != nil {
		return RunDetail{}, err
	}
	return out, nil
}

func (c *Client) Approve(ctx context.Context, id, step string, approve bool, reason string) error {
	body, _ := json.Marshal(map[string]any{"approve": approve, "reason": reason})
	req, err := c.newReq(ctx, http.MethodPost, "/v1/runs/"+id+"/steps/"+step+"/approve", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, nil)
}

func (c *Client) Cancel(ctx context.Context, id string) error {
	req, err := c.newReq(ctx, http.MethodDelete, "/v1/runs/"+id, nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

func (c *Client) Retry(ctx context.Context, id string) error {
	req, err := c.newReq(ctx, http.MethodPost, "/v1/runs/"+id+"/retry", nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}
```

- [ ] **Step 4: Run — expect PASS.** `go test ./internal/tui/ -count=1`

- [ ] **Step 5: Verify the approve body tags against the daemon.** Read `internal/api/handlers.go` for `approveRequest`; confirm its JSON field names are `approve` and `reason`. If they differ, fix the `map[string]any` keys in `Approve` and the test. (Do not change the daemon.)

- [ ] **Step 6: Commit.**

```bash
gofmt -w internal/tui/client.go internal/tui/client_test.go
git add internal/tui/client.go internal/tui/client_test.go
git commit -m "feat(tui): REST client for runs, approve, cancel, retry"
```

---

## Task 3: SSE reader

**Files:**
- Create: `internal/tui/sse.go`, `internal/tui/sse_test.go`

**Interfaces:**
- Consumes: `concentus/internal/event.Event` (`{seq,run,step,kind,summary,cost_usd,attempt,error,at}`); the SSE wire format `id: <seq>\nevent: <kind>\ndata: <json>\n\n`; the `Last-Event-ID` request header.
- Produces:
  - `func parseEvents(r io.Reader, emit func(event.Event)) error` — parses the framed stream, calling `emit` per event; returns when the stream ends/errors.
  - `func (c *Client) StreamEvents(ctx context.Context, id string, lastSeq int64, emit func(event.Event)) error` — opens `GET /v1/runs/{id}/events` (auth header + `Last-Event-ID: <lastSeq>` when `lastSeq>0`) and feeds `parseEvents`.

- [ ] **Step 1: Write the failing tests.**

```go
package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"concentus/internal/event"
)

func TestParseEventsFrames(t *testing.T) {
	stream := "id: 1\nevent: step.started\ndata: {\"seq\":1,\"run\":\"a1\",\"step\":\"plan\",\"kind\":\"step.started\"}\n\n" +
		"id: 2\nevent: gate.awaiting\ndata: {\"seq\":2,\"run\":\"a1\",\"step\":\"plan\",\"kind\":\"gate.awaiting\"}\n\n"
	var got []event.Event
	if err := parseEvents(strings.NewReader(stream), func(e event.Event) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 1 || got[1].Kind != "gate.awaiting" || got[1].StepID != "plan" {
		t.Fatalf("got %+v", got)
	}
}

func TestStreamEventsSendsLastEventIDAndAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/a1/events" {
			t.Fatalf("path %s", r.URL.Path)
		}
		if r.Header.Get("Last-Event-ID") != "5" {
			t.Fatalf("Last-Event-ID = %q", r.Header.Get("Last-Event-ID"))
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Fatalf("auth = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte("id: 6\nevent: run.done\ndata: {\"seq\":6,\"run\":\"a1\",\"kind\":\"run.done\"}\n\n"))
	}))
	defer srv.Close()
	var got []event.Event
	err := NewClient(srv.URL, "tok").StreamEvents(context.Background(), "a1", 5, func(e event.Event) { got = append(got, e) })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "run.done" {
		t.Fatalf("got %+v", got)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`undefined: parseEvents`).

- [ ] **Step 3: Implement `sse.go`.**

```go
package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"concentus/internal/event"
)

// parseEvents reads the SSE framing (`id:`/`event:`/`data:` lines, blank line
// terminates a frame) and calls emit for each event whose data decodes. It
// returns nil at clean EOF.
func parseEvents(r io.Reader, emit func(event.Event)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "": // frame boundary
			if data != "" {
				var e event.Event
				if json.Unmarshal([]byte(data), &e) == nil {
					emit(e)
				}
				data = ""
			}
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		default:
			// id: / event: lines are redundant with the JSON payload; ignore.
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// StreamEvents opens the per-run SSE stream and feeds parseEvents until the
// stream ends or ctx is cancelled. lastSeq>0 is sent as Last-Event-ID to resume.
func (c *Client) StreamEvents(ctx context.Context, id string, lastSeq int64, emit func(event.Event)) error {
	req, err := c.newReq(ctx, http.MethodGet, "/v1/runs/"+id+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastSeq > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(lastSeq, 10))
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return parseEvents(resp.Body, emit)
}
```

Note: `StreamEvents` uses `c.hc` (15s timeout) — acceptable because the driver runs it in a goroutine and reconnects; if a long-lived stream is cut by the timeout, the driver reopens with `Last-Event-ID`. (A dedicated no-timeout client is an optional later refinement; not needed for correctness.)

- [ ] **Step 4: Run — expect PASS.** `go test ./internal/tui/ -count=1`

- [ ] **Step 5: Commit.**

```bash
gofmt -w internal/tui/sse.go internal/tui/sse_test.go
git add internal/tui/sse.go internal/tui/sse_test.go
git commit -m "feat(tui): SSE reader with Last-Event-ID resume"
```

---

## Task 4: Model + reducer (pure core)

**Files:**
- Create: `internal/tui/model.go`, `internal/tui/model_test.go`

**Interfaces:**
- Consumes: `RunSummary`, `RunDetail`, `StepView` (Task 2); `event.Event` (Task 3).
- Produces:
  - message types: `runsLoaded []RunSummary`; `runSnapshot RunDetail`; `sseEvent event.Event`; `keyMsg rune`; `actionResult struct{ Err error }`; `connMsg bool`.
  - command types (side-effect requests the driver executes): `cmdFocus string` (snapshot + open SSE for run id, on enter); `cmdRefresh string` (snapshot-only refresh, no SSE change, on a lifecycle event); `cmdApprove struct{ ID, Step string; OK bool; Reason string }`; `cmdCancel string`; `cmdRetry string`; `cmdQuit`. Run-list fetching is done directly by the driver's poll loop (no command).
  - `type model struct { ... }`; `func initialModel() model`; `func update(m model, ms any) (model, []any)`.
  - helpers: `func (m model) focusedRun() (RunDetail, bool)`; `func (m model) gateStep() (StepView, bool)` (the focused run's `gate.awaiting`/`awaiting` step, if any).

- [ ] **Step 1: Write the failing tests.**

```go
package tui

import (
	"testing"

	"concentus/internal/event"
)

func hasCmd[T any](cmds []any) bool {
	for _, c := range cmds {
		if _, ok := c.(T); ok {
			return true
		}
	}
	return false
}

func TestRunsLoadedPopulatesListAndKeepsSelection(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Name: "f", Status: "running"}, {ID: "b2", Name: "g", Status: "done"}})
	if len(m.runs) != 2 || m.sel != 0 {
		t.Fatalf("runs=%d sel=%d", len(m.runs), m.sel)
	}
}

func TestEnterFocusesSelectedRun(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, cmds := update(m, keyMsg('\r')) // enter
	if m.focus != "a1" {
		t.Fatalf("focus=%q", m.focus)
	}
	if !hasCmd[cmdFocus](cmds) {
		t.Fatalf("want a cmdFocus, got %T", cmds)
	}
}

func TestGateAwaitingEnablesApprove(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running", Steps: []StepView{{ID: "plan", Status: "awaiting"}}})
	step, ok := m.gateStep()
	if !ok || step.ID != "plan" {
		t.Fatalf("gateStep=%+v ok=%v", step, ok)
	}
	// 'a' approve -> a cmdApprove for the gate step
	_, cmds := update(m, keyMsg('a'))
	if !hasCmd[cmdApprove](cmds) {
		t.Fatalf("want cmdApprove, got %#v", cmds)
	}
}

func TestApproveIgnoredWhenNoGate(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running", Steps: []StepView{{ID: "plan", Status: "running"}}})
	_, cmds := update(m, keyMsg('a'))
	if hasCmd[cmdApprove](cmds) {
		t.Fatal("approve must be a no-op when no gate is awaiting")
	}
}

func TestRetryOnlyWhenTerminal(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running"})
	if _, cmds := update(m, keyMsg('R')); hasCmd[cmdRetry](cmds) {
		t.Fatal("retry must be a no-op on a running run")
	}
	m, _ = update(m, runSnapshot{ID: "a1", Status: "failed"})
	if _, cmds := update(m, keyMsg('R')); !hasCmd[cmdRetry](cmds) {
		t.Fatal("retry must fire on a failed run")
	}
}

func TestSseEventAppendsToLogAndRefreshes(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	before := len(m.log)
	m, cmds := update(m, sseEvent(event.Event{Seq: 7, RunID: "a1", StepID: "plan", Kind: event.GateAwaiting}))
	if len(m.log) != before+1 {
		t.Fatalf("log not appended: %d->%d", before, len(m.log))
	}
	if m.lastSeq != 7 {
		t.Fatalf("lastSeq=%d", m.lastSeq)
	}
	if !hasCmd[cmdRefresh](cmds) { // lifecycle event triggers a snapshot-only refresh
		t.Fatalf("want a cmdRefresh after a lifecycle event")
	}
	if hasCmd[cmdFocus](cmds) { // ...and must NOT re-focus (that would reopen the SSE stream)
		t.Fatal("lifecycle refresh must not emit cmdFocus (would tear down the SSE stream)")
	}
}

func TestCancelRequiresConfirm(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running"})
	// 'c' must arm a confirm, NOT cancel yet.
	m, cmds := update(m, keyMsg('c'))
	if hasCmd[cmdCancel](cmds) {
		t.Fatal("cancel must not fire before confirm")
	}
	if m.mode != modeConfirmCancel {
		t.Fatalf("mode=%v, want modeConfirmCancel", m.mode)
	}
	// 'y' confirms and fires the cancel, returning to normal mode.
	m, cmds = update(m, keyMsg('y'))
	if !hasCmd[cmdCancel](cmds) {
		t.Fatal("y must confirm the cancel")
	}
	if m.mode != modeNormal {
		t.Fatalf("mode=%v, want modeNormal after confirm", m.mode)
	}
}

func TestCancelAbortedByOtherKey(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running"})
	m, _ = update(m, keyMsg('c'))
	m, cmds := update(m, keyMsg('n')) // any non-y key aborts
	if hasCmd[cmdCancel](cmds) || m.mode != modeNormal {
		t.Fatalf("n must abort the cancel confirm (mode=%v)", m.mode)
	}
}

func TestReasonInputThenReject(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running", Steps: []StepView{{ID: "plan", Status: "awaiting"}}})
	// 'r' enters reason-input mode (no command yet).
	m, cmds := update(m, keyMsg('r'))
	if hasCmd[cmdApprove](cmds) || m.mode != modeReason {
		t.Fatalf("r must enter reason mode without rejecting (mode=%v)", m.mode)
	}
	for _, ch := range "no" { // type the reason
		m, _ = update(m, keyMsg(ch))
	}
	if m.reasonBuf != "no" {
		t.Fatalf("reasonBuf=%q, want \"no\"", m.reasonBuf)
	}
	// Enter submits reject with the typed reason.
	m, cmds = update(m, keyMsg('\r'))
	if m.mode != modeNormal {
		t.Fatalf("mode=%v, want modeNormal after submit", m.mode)
	}
	for _, c := range cmds {
		if ap, ok := c.(cmdApprove); ok {
			if ap.OK != false || ap.Reason != "no" {
				t.Fatalf("got %+v, want reject with reason \"no\"", ap)
			}
			return
		}
	}
	t.Fatal("expected a cmdApprove on reason submit")
}

func TestReasonInputEscAborts(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running", Steps: []StepView{{ID: "plan", Status: "awaiting"}}})
	m, _ = update(m, keyMsg('r'))
	m, _ = update(m, keyMsg('x'))
	m, cmds := update(m, keyMsg(27)) // esc
	if hasCmd[cmdApprove](cmds) || m.mode != modeNormal || m.reasonBuf != "" {
		t.Fatalf("esc must abort reason input (mode=%v buf=%q)", m.mode, m.reasonBuf)
	}
}

func TestQuitKey(t *testing.T) {
	_, cmds := update(initialModel(), keyMsg('q'))
	if !hasCmd[cmdQuit](cmds) {
		t.Fatal("q must request quit")
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (undefined symbols).

- [ ] **Step 3: Implement `model.go`.** (Full code — message/cmd types, model, helpers, reducer.)

```go
package tui

import (
	"concentus/internal/event"
)

// ---- messages (driver -> reducer) ----
type runsLoaded []RunSummary
type runSnapshot RunDetail
type sseEvent event.Event
type keyMsg rune
type actionResult struct{ Err error }
type connMsg bool

// ---- commands (reducer -> driver) ----
type cmdFocus string   // snapshot + (re)open SSE for this run id (on enter)
type cmdRefresh string // snapshot-only refresh, no SSE change (on lifecycle event)
type cmdApprove struct {
	ID, Step string
	OK       bool
	Reason   string
}
type cmdCancel string
type cmdRetry string
type cmdQuit struct{}

const logCap = 500

// inputMode is the keyboard interaction mode: normal navigation, a y/n confirm
// before a destructive cancel, or a one-line reason editor before a reject.
type inputMode int

const (
	modeNormal inputMode = iota
	modeConfirmCancel
	modeReason
)

type model struct {
	runs      []RunSummary
	sel       int       // index into runs
	focus     string    // focused run id ("" = none)
	detail    RunDetail // focused run snapshot
	log       []event.Event
	lastSeq   int64
	conn      bool      // daemon reachable
	status    string    // transient status-bar message (e.g. an action error)
	mode      inputMode // normal / confirm-cancel / reason-input
	reasonBuf string    // accumulates the reject reason while in modeReason
}

func initialModel() model { return model{conn: true} }

func (m model) focusedRun() (RunDetail, bool) {
	if m.focus == "" || m.detail.ID != m.focus {
		return RunDetail{}, false
	}
	return m.detail, true
}

// gateStep returns the focused run's step awaiting a gate, if any.
func (m model) gateStep() (StepView, bool) {
	for _, s := range m.detail.Steps {
		if s.Status == "awaiting" || s.Status == "gate.awaiting" {
			return s, true
		}
	}
	return StepView{}, false
}

func isTerminal(status string) bool {
	return status == "failed" || status == "canceled" || status == "succeeded"
}

func isLifecycle(k event.Kind) bool {
	switch k {
	case event.StepStarted, event.StepDone, event.StepFailed, event.StepRetrying, event.GateAwaiting, event.RunDone:
		return true
	}
	return false
}

func (m *model) appendLog(e event.Event) {
	m.log = append(m.log, e)
	if len(m.log) > logCap {
		m.log = m.log[len(m.log)-logCap:]
	}
}

// update is the pure reducer: it maps the current model + a message to the next
// model and a list of side-effect commands for the driver to run.
func update(m model, ms any) (model, []any) {
	switch v := ms.(type) {
	case runsLoaded:
		m.runs = []RunSummary(v)
		m.conn = true
		if m.sel >= len(m.runs) {
			m.sel = max(0, len(m.runs)-1)
		}
		return m, nil

	case runSnapshot:
		if RunDetail(v).ID == m.focus {
			m.detail = RunDetail(v)
		}
		return m, nil

	case sseEvent:
		e := event.Event(v)
		if e.RunID != m.focus {
			return m, nil
		}
		m.appendLog(e)
		if e.Seq > m.lastSeq {
			m.lastSeq = e.Seq
		}
		if isLifecycle(e.Kind) {
			return m, []any{cmdRefresh(m.focus)} // snapshot-only refresh (keeps the SSE stream)
		}
		return m, nil

	case connMsg:
		m.conn = bool(v)
		return m, nil

	case actionResult:
		if v.Err != nil {
			m.status = v.Err.Error()
		} else {
			m.status = ""
		}
		return m, nil

	case keyMsg:
		return updateKey(m, rune(v))
	}
	return m, nil
}

func updateKey(m model, r rune) (model, []any) {
	// Modal keys take precedence so 'q'/'c'/'r' don't leak into a prompt.
	switch m.mode {
	case modeConfirmCancel:
		if r == 'y' || r == 'Y' {
			m.mode = modeNormal
			return m, []any{cmdCancel(m.focus)}
		}
		m.mode = modeNormal // any other key aborts
		return m, nil
	case modeReason:
		switch r {
		case '\r', '\n': // submit the reject with the typed reason
			step, ok := m.gateStep()
			reason := m.reasonBuf
			m.mode = modeNormal
			m.reasonBuf = ""
			if ok {
				return m, []any{cmdApprove{ID: m.focus, Step: step.ID, OK: false, Reason: reason}}
			}
			return m, nil
		case 27: // esc aborts
			m.mode = modeNormal
			m.reasonBuf = ""
			return m, nil
		case 127, 8: // backspace / delete
			if n := len(m.reasonBuf); n > 0 {
				m.reasonBuf = m.reasonBuf[:n-1]
			}
			return m, nil
		default:
			if r >= 32 && r < 127 { // printable ASCII only
				m.reasonBuf += string(r)
			}
			return m, nil
		}
	}

	// modeNormal:
	switch r {
	case 'q', 3: // q or Ctrl-C
		return m, []any{cmdQuit{}}
	case 'j', 14: // down
		if m.sel < len(m.runs)-1 {
			m.sel++
		}
		return m, nil
	case 'k', 16: // up
		if m.sel > 0 {
			m.sel--
		}
		return m, nil
	case '\r', '\n': // enter -> focus selected
		if m.sel < len(m.runs) {
			m.focus = m.runs[m.sel].ID
			m.detail = RunDetail{}
			m.log = nil
			m.lastSeq = 0
			return m, []any{cmdFocus(m.focus)}
		}
		return m, nil
	case 'a': // approve gate
		if s, ok := m.gateStep(); ok {
			return m, []any{cmdApprove{ID: m.focus, Step: s.ID, OK: true}}
		}
		return m, nil
	case 'r': // reject gate -> open the reason editor (no command yet)
		if _, ok := m.gateStep(); ok {
			m.mode = modeReason
			m.reasonBuf = ""
		}
		return m, nil
	case 'c': // cancel active run -> arm the y/n confirm (no command yet)
		if d, ok := m.focusedRun(); ok && !isTerminal(d.Status) {
			m.mode = modeConfirmCancel
		}
		return m, nil
	case 'R': // retry terminal run
		if d, ok := m.focusedRun(); ok && isTerminal(d.Status) {
			return m, []any{cmdRetry(m.focus)}
		}
		return m, nil
	}
	return m, nil
}
```

The reason editor relies on the driver forwarding raw bytes as `keyMsg` (Task 6's `readKeys` does exactly this): printable bytes append, `127`/`8` delete, `27` (esc) aborts, `\r` submits. No driver change is needed. Caveat: an arrow key sends a 3-byte escape sequence (`27 '[' 'A'`), so in `modeReason` pressing an arrow reads as esc-then-`[A` — it aborts the editor and types `[A`. Acceptable for v1 (navigation uses `j`/`k`, not arrows); a fuller escape-sequence decoder is a later refinement.

- [ ] **Step 4: Run — expect PASS.** `go test ./internal/tui/ -count=1`

- [ ] **Step 5: Commit.**

```bash
gofmt -w internal/tui/model.go internal/tui/model_test.go
git add internal/tui/model.go internal/tui/model_test.go
git commit -m "feat(tui): model and pure reducer for runs, gates, actions"
```

---

## Task 5: View renderer (pure)

**Files:**
- Create: `internal/tui/view.go`, `internal/tui/view_test.go`

**Interfaces:**
- Consumes: `model`, `RunSummary`, `StepView`, `event.Event`.
- Produces: `func view(m model, w, h int) string` — a full-screen string (rows joined by `\n`), exactly `h` rows, each ≤ `w` cells (plain ASCII; no ANSI required for tests). Renders: a runs column, the focused run's steps, a tail of the event log, and a bottom status/key bar.

- [ ] **Step 1: Write the failing tests** (assert content + that the gate row and key bar are present; keep assertions substring-based so layout can evolve).

```go
package tui

import (
	"strings"
	"testing"

	"concentus/internal/event"
)

func TestViewShowsRunsAndKeyBar(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Name: "feature", Status: "running"}})
	out := view(m, 80, 24)
	if !strings.Contains(out, "feature") || !strings.Contains(out, "running") {
		t.Fatalf("runs not rendered:\n%s", out)
	}
	if !strings.Contains(out, "approve") || !strings.Contains(out, "quit") {
		t.Fatalf("key bar missing:\n%s", out)
	}
	if got := strings.Count(out, "\n") + 1; got != 24 {
		t.Fatalf("want 24 rows, got %d", got)
	}
}

func TestViewHighlightsGateAndLog(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Name: "feature", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Name: "feature", Status: "running", Steps: []StepView{{ID: "plan", Status: "awaiting"}}})
	m, _ = update(m, sseEvent(event.Event{Seq: 1, RunID: "a1", StepID: "plan", Kind: event.GateAwaiting}))
	out := view(m, 100, 24)
	if !strings.Contains(out, "plan") || !strings.Contains(out, "awaiting") {
		t.Fatalf("gate step not shown:\n%s", out)
	}
	if !strings.Contains(out, "gate.awaiting") {
		t.Fatalf("event log not shown:\n%s", out)
	}
}

func TestViewDisconnectedBanner(t *testing.T) {
	m := initialModel()
	m, _ = update(m, connMsg(false))
	if !strings.Contains(view(m, 80, 24), "disconnected") {
		t.Fatal("want a disconnected indicator")
	}
}

func TestViewShowsCancelConfirm(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running"})
	m, _ = update(m, keyMsg('c'))
	if !strings.Contains(view(m, 80, 24), "(y/n)") {
		t.Fatal("want a cancel confirm prompt in the status bar")
	}
}

func TestViewShowsReasonPrompt(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running", Steps: []StepView{{ID: "plan", Status: "awaiting"}}})
	m, _ = update(m, keyMsg('r'))
	m, _ = update(m, keyMsg('n'))
	m, _ = update(m, keyMsg('o'))
	out := view(m, 80, 24)
	if !strings.Contains(out, "reason") || !strings.Contains(out, "no") {
		t.Fatalf("want a reason editor showing the typed text:\n%s", out)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`undefined: view`).

- [ ] **Step 3: Implement `view.go`.** Keep it a straightforward line builder (no layout lib). Pad/truncate each row to `w`; produce exactly `h` rows.

```go
package tui

import (
	"fmt"
	"strings"
)

func clip(s string, w int) string {
	if len(s) > w {
		if w <= 1 {
			return s[:w]
		}
		return s[:w-1] + "…"
	}
	return s + strings.Repeat(" ", w-len(s))
}

func view(m model, w, h int) string {
	rows := make([]string, 0, h)

	// Header.
	conn := "connected"
	if !m.conn {
		conn = "disconnected"
	}
	title := "cm tui — magisterd"
	if d, ok := m.focusedRun(); ok {
		title = fmt.Sprintf("cm tui — %s %s [%s]", d.Name, d.ID, d.Status)
	}
	rows = append(rows, clip(fmt.Sprintf("%s    (%s)", title, conn), w))

	// Body: left runs column (width ~24), right detail/log.
	left := w / 3
	if left < 16 {
		left = 16
	}
	if left > 30 {
		left = 30
	}
	right := w - left - 1

	bodyH := h - 2 // minus header + key bar
	if bodyH < 1 {
		bodyH = 1
	}

	// Left lines.
	leftLines := make([]string, 0, bodyH)
	leftLines = append(leftLines, clip("RUNS", left))
	for i, r := range m.runs {
		cursor := "  "
		if i == m.sel {
			cursor = "> "
		}
		leftLines = append(leftLines, clip(fmt.Sprintf("%s%s %s", cursor, r.ID, r.Status), left))
	}

	// Right lines: steps then a separator then the log tail.
	rightLines := make([]string, 0, bodyH)
	if d, ok := m.focusedRun(); ok {
		rightLines = append(rightLines, clip("STEPS", right))
		for _, s := range d.Steps {
			mark := ""
			if s.Status == "awaiting" || s.Status == "gate.awaiting" {
				mark = "  <-- approve?"
			}
			rightLines = append(rightLines, clip(fmt.Sprintf("%-12s %s%s", s.ID, s.Status, mark), right))
		}
		rightLines = append(rightLines, clip("EVENTS", right))
		start := 0
		remaining := bodyH - len(rightLines)
		if len(m.log) > remaining && remaining > 0 {
			start = len(m.log) - remaining
		}
		for _, e := range m.log[start:] {
			rightLines = append(rightLines, clip(fmt.Sprintf("%s %s %s", e.At.Format("15:04:05"), e.Kind, e.StepID), right))
		}
	} else {
		rightLines = append(rightLines, clip("(select a run and press enter)", right))
	}

	for i := 0; i < bodyH; i++ {
		l := clip("", left)
		if i < len(leftLines) {
			l = leftLines[i]
		}
		r := clip("", right)
		if i < len(rightLines) {
			r = rightLines[i]
		}
		rows = append(rows, l+" "+r)
	}

	// Status / key bar. Modal prompts take over the bar.
	bar := "j/k select · enter open · a approve · r reject · c cancel · R retry · q quit"
	switch m.mode {
	case modeConfirmCancel:
		bar = fmt.Sprintf("cancel run %s? (y/n)", m.focus)
	case modeReason:
		bar = "reject reason: " + m.reasonBuf + "_"
	default:
		if m.status != "" {
			bar = "! " + m.status
		}
	}
	rows = append(rows, clip(bar, w))

	// Force exactly h rows.
	for len(rows) < h {
		rows = append(rows, clip("", w))
	}
	if len(rows) > h {
		rows = rows[:h]
	}
	return strings.Join(rows, "\n")
}
```

- [ ] **Step 4: Run — expect PASS.** `go test ./internal/tui/ -count=1`

- [ ] **Step 5: Commit.**

```bash
gofmt -w internal/tui/view.go internal/tui/view_test.go
git add internal/tui/view.go internal/tui/view_test.go
git commit -m "feat(tui): pure full-screen view renderer"
```

---

## Task 6: Driver + `cm tui` wiring

The imperative shell: wire sources → `update` → render, handle raw mode/resize/restore, and add the subcommand. Build- and manually-verified (no TTY in CI), but include one smoke test for the run loop using a fake input.

**Files:**
- Create: `internal/tui/driver.go`, `internal/tui/driver_test.go`
- Modify: `cmd/cm/main.go` (add `case "tui"` + usage line)

**Interfaces:**
- Consumes: everything above (`NewClient`, `StreamEvents`, `initialModel`, `update`, `view`, `size`, `enterRaw`).
- Produces: `func Run(base, token string) error` — the entry point `cm tui` calls.

- [ ] **Step 1: Write a smoke test for the command runner (no TTY).** Factor the message loop into a testable `runLoop` that takes injected channels and a render sink, so we can drive it without a terminal.

```go
package tui

import (
	"context"
	"testing"
	"time"
)

// runLoop must process messages, apply update, render via render(), and exit on cmdQuit.
func TestRunLoopRendersAndQuits(t *testing.T) {
	msgs := make(chan any, 8)
	var lastFrame string
	rendered := make(chan struct{}, 16)
	exec := func(cmds []any) {
		for _, c := range cmds {
			if _, ok := c.(cmdQuit); ok {
				close(msgs)
			}
		}
	}
	render := func(m model) {
		lastFrame = view(m, 80, 24)
		select {
		case rendered <- struct{}{}:
		default:
		}
	}
	go func() {
		msgs <- runsLoaded{{ID: "a1", Name: "feature", Status: "running"}}
		<-rendered
		msgs <- keyMsg('q')
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runLoop(ctx, msgs, exec, render); err != nil {
		t.Fatal(err)
	}
	if lastFrame == "" {
		t.Fatal("expected at least one rendered frame")
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`undefined: runLoop`).

- [ ] **Step 3: Implement `driver.go`.** `runLoop` is the pure-ish core; `Run` is the terminal wiring around it.

```go
package tui

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// runLoop consumes messages, applies the reducer, executes returned commands via
// exec, and renders each new model via render. It returns when msgs is closed.
func runLoop(ctx context.Context, msgs <-chan any, exec func([]any), render func(model)) error {
	m := initialModel()
	render(m)
	for {
		select {
		case <-ctx.Done():
			return nil
		case ms, ok := <-msgs:
			if !ok {
				return nil
			}
			var cmds []any
			m, cmds = update(m, ms)
			render(m)
			if len(cmds) > 0 {
				exec(cmds)
			}
		}
	}
}

// Run is the cm tui entry point: raw terminal, input + poll + SSE sources wired
// into runLoop, ANSI screen rendering, guaranteed restore on exit.
func Run(base, token string) error {
	c := NewClient(base, token)
	fd := int(os.Stdin.Fd())

	restore, err := enterRaw(fd)
	if err != nil {
		return err
	}
	defer restore() //nolint:errcheck // best-effort terminal restore

	os.Stdout.WriteString("\x1b[?1049h") // alt screen
	defer os.Stdout.WriteString("\x1b[?1049l\x1b[?25h")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	msgs := make(chan any, 64)
	var focusCancel context.CancelFunc

	// exec turns reducer commands into goroutines that push result messages back.
	exec := func(cmds []any) {
		for _, cmd := range cmds {
			switch v := cmd.(type) {
			case cmdQuit:
				cancel()
			case cmdFocus:
				id := string(v)
				if focusCancel != nil {
					focusCancel()
				}
				var fctx context.Context
				fctx, focusCancel = context.WithCancel(ctx)
				go func() {
					if d, err := c.GetRun(fctx, id); err == nil {
						trySend(msgs, runSnapshot(d))
					}
				}()
				go streamLoop(fctx, c, id, msgs)
			case cmdRefresh:
				// Snapshot-only refresh on a lifecycle event — does NOT touch the
				// SSE stream (that would tear down the stream delivering events and
				// reconnect from seq 0). Uses the parent ctx; a stale snapshot for a
				// run that is no longer focused is dropped by the reducer's guard.
				id := string(v)
				go func() {
					if d, err := c.GetRun(ctx, id); err == nil {
						trySend(msgs, runSnapshot(d))
					}
				}()
			case cmdApprove:
				go func() { trySend(msgs, actionResult{c.Approve(ctx, v.ID, v.Step, v.OK, v.Reason)}) }()
			case cmdCancel:
				go func() { trySend(msgs, actionResult{c.Cancel(ctx, string(v))}) }()
			case cmdRetry:
				go func() { trySend(msgs, actionResult{c.Retry(ctx, string(v))}) }()
			}
		}
	}

	// Input reader.
	go readKeys(ctx, os.Stdin, msgs)
	// Poll loop.
	go func() {
		t := time.NewTicker(1500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if runs, err := c.ListRuns(ctx); err == nil {
					trySend(msgs, runsLoaded(runs))
				} else {
					trySend(msgs, connMsg(false))
				}
			}
		}
	}()
	// Resize -> just re-render on next message; also handle SIGWINCH to force a frame.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-winch:
				trySend(msgs, connMsg(true)) // benign message to trigger a re-render
			}
		}
	}()

	render := func(m model) {
		w, h, err := size(fd)
		if err != nil || w == 0 {
			w, h = 80, 24
		}
		os.Stdout.WriteString("\x1b[H\x1b[2J") // home + clear
		os.Stdout.WriteString(view(m, w, h))
	}

	// Kick an initial fetch.
	go func() {
		if runs, err := c.ListRuns(ctx); err == nil {
			trySend(msgs, runsLoaded(runs))
		}
	}()

	return runLoop(ctx, msgs, exec, render)
}

func trySend(ch chan any, m any) {
	select {
	case ch <- m:
	default:
	}
}

// streamLoop keeps the per-run SSE stream open, reconnecting until fctx ends.
func streamLoop(ctx context.Context, c *Client, id string, msgs chan any) {
	var last int64
	for ctx.Err() == nil {
		_ = c.StreamEvents(ctx, id, last, func(e event.Event) {
			if e.Seq > last {
				last = e.Seq
			}
			trySend(msgs, sseEvent(e))
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond): // backoff before reconnect
		}
	}
}

// readKeys reads single bytes and forwards them as keyMsg.
func readKeys(ctx context.Context, in *os.File, msgs chan any) {
	buf := make([]byte, 1)
	for ctx.Err() == nil {
		n, err := in.Read(buf)
		if err != nil {
			return
		}
		if n == 1 {
			trySend(msgs, keyMsg(rune(buf[0])))
		}
	}
}
```

Note the missing `event` import in `streamLoop` — add `"concentus/internal/event"` and `"time"` to the import block (the implementer wires imports to satisfy the compiler; `go build` will flag any miss).

- [ ] **Step 4: Run the smoke test — expect PASS.** `go test ./internal/tui/ -run TestRunLoop -count=1`

- [ ] **Step 5: Add the `cm tui` subcommand.** In `cmd/cm/main.go`, add a `case "tui"` to the dispatch switch and the usage line. Use the existing `base` value and read the token from env.

```go
case "tui":
	return runTUI(base)
```

and a small helper near the other subcommand funcs:

```go
func runTUI(base string) int {
	if err := tui.Run(base, os.Getenv("MAGISTER_BEARER_TOKEN")); err != nil {
		fmt.Fprintln(os.Stderr, "tui:", err)
		return 1
	}
	return 0
}
```

Add `"concentus/internal/tui"` to the imports and `tui` to the top usage line (`<run|ls|…|tui>`). Match the existing dispatch's return/exit convention (the surrounding cases return an int code).

- [ ] **Step 6: Verify build + whole suite.**

Run: `go build ./...` then `go test ./... -count=1`
Expected: builds; all packages pass (the TUI package's unit tests + the rest unchanged).

- [ ] **Step 7: Manual smoke (documented, not CI).** With a daemon running (`./bin/magisterd`) and a submitted run with a manual gate, `./bin/cm tui` shows the run, live events stream, `a` releases the gate; `r` opens the reason editor and submits a reject; `c` then `y` cancels an active run (and any other key aborts the confirm); `q` exits cleanly and the terminal is restored. Record the result in the task report.

- [ ] **Step 8: Commit.**

```bash
gofmt -w internal/tui/driver.go internal/tui/driver_test.go cmd/cm/main.go
git add internal/tui/driver.go internal/tui/driver_test.go cmd/cm/main.go
git commit -m "feat(tui): terminal driver and cm tui subcommand"
```

---

## Final verification (after all tasks)

- [ ] `go build ./...` clean; `go.mod` still reads `go 1.22`.
- [ ] `go test ./... -count=1` green (and `-race` on `./internal/tui/`).
- [ ] `gofmt -l internal/tui cmd/cm` prints nothing.
- [ ] Manual: `cm tui` against a live daemon — monitor + approve/reject/cancel/retry work; terminal restores on exit.
- [ ] Whole-branch review (Opus).

## Notes for the implementer

- The driver (`driver.go`, `term.go`) is the only code not fully unit-tested; the `runLoop`/reducer/view/client/sse split keeps all decision logic testable. Don't push logic into the driver — keep it a thin wiring layer.
- Step statuses come from the daemon's `core.StepStatus`/`core.RunStatus` strings; the reducer compares against `"awaiting"`/`"gate.awaiting"`, `"failed"/"canceled"/"succeeded"`. Verify the exact status strings in `internal/core` and adjust the constants in `model.go` if they differ (e.g. the awaiting-gate status) — the daemon is the source of truth.
- The cancel confirm (`modeConfirmCancel`) and reject-reason editor (`modeReason`) are reducer-only state machines — the driver forwards raw key bytes unchanged. Don't add prompt logic to the driver. The one rough edge is arrow keys in `modeReason` (escape sequences); navigation is `j`/`k`, so this is acceptable for v1.
