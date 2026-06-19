# `/healthz` liveness + `/readyz` readiness split Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep `/healthz` as a dependency-free liveness probe and add `/readyz` that returns 200 only when the daemon is not draining and the store is reachable, with a graceful-shutdown drain.

**Architecture:** Add a `Ping` method to the `core.Store` port (SQLite pings its reader pool; Mem returns nil). Give the api `Server` a `draining atomic.Bool` + `SetDraining`; `/readyz` returns 503 when draining or when the store ping fails, else 200. The daemon flips draining at shutdown, optionally sleeps a configurable drain grace, then shuts down as today.

**Tech Stack:** Go 1.22, standard library only. Packages `internal/core`, `internal/store`, `internal/api`, `internal/config`, `cmd/magisterd`.

## Global Constraints

- Go 1.22; **stdlib only, NO new dependency** (do not touch `go.mod`); no DB migration; no new store table.
- `/healthz` and `/readyz` are both auth-exempt; `/healthz` response is byte-for-byte unchanged (`200 {"status":"ok"}`).
- `core.Store.Ping(ctx context.Context) error` is added to the interface AND to every implementer (`*store.SQLite`, `*store.Mem` — the only two; both carry `var _ core.Store` assertions).
- `/readyz` bodies: `200 {"status":"ready"}`, `503 {"status":"draining"}`, `503 {"status":"store unreachable"}`. The draining check is evaluated BEFORE the store ping.
- `-shutdown-drain` / `MAGISTER_SHUTDOWN_DRAIN` defaults to `0` (today's shutdown timing preserved); the draining flag still flips regardless.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`. `gofmt -l`, `go vet`, `go test -race ./...` clean before merge.

## File Structure

- `internal/core/store.go` — add `Ping(ctx) error` to the `Store` interface. (Task 1)
- `internal/store/sqlite.go`, `internal/store/mem.go` — implement `Ping`. (Task 1)
- `internal/store/sqlite_test.go`, `internal/store/mem_test.go` — `Ping` tests. (Task 1)
- `internal/api/handlers.go` — `Server.draining` field, `SetDraining`, `handleReadyz`. (Task 2)
- `internal/api/router.go` — register `GET /readyz`. (Task 2)
- `internal/api/middleware.go` — `routeLabel` case for `/readyz`. (Task 2)
- `internal/api/readyz_test.go` (new) — readyz tests. (Task 2)
- `internal/config/config.go`, `internal/config/config_test.go` — `ShutdownDrain` flag/env. (Task 3)
- `cmd/magisterd/main.go`, `cmd/magisterd/main_test.go` — shutdown drain wiring + test. (Task 3)

---

### Task 1: `core.Store.Ping` port method + SQLite/Mem implementations

**Files:**
- Modify: `internal/core/store.go` (the `Store` interface)
- Modify: `internal/store/sqlite.go`, `internal/store/mem.go`
- Test: `internal/store/sqlite_test.go`, `internal/store/mem_test.go`

**Interfaces:**
- Produces: `Ping(ctx context.Context) error` on `core.Store`, `*store.SQLite`, `*store.Mem`. Consumed by `internal/api` (Task 2).

- [ ] **Step 1: Write the failing tests**

In `internal/store/mem_test.go` (package `store`; `context` already imported), add:

```go
func TestMemPing(t *testing.T) {
	if err := NewMem().Ping(context.Background()); err != nil {
		t.Fatalf("Mem.Ping = %v, want nil", err)
	}
}
```

In `internal/store/sqlite_test.go` (package `store`; `context` already imported; `tempDB(t) *SQLite` helper exists), add:

```go
func TestSQLitePing(t *testing.T) {
	s := tempDB(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("SQLite.Ping = %v, want nil", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestMemPing|TestSQLitePing'`
Expected: compile failure — `s.Ping undefined` / `NewMem().Ping undefined`.

- [ ] **Step 3: Add `Ping` to the `core.Store` interface**

In `internal/core/store.go`, inside the `Store interface { ... }` block, add this method after the `ReclaimableRuns(...)` line (before the closing `}`):

```go
	// Ping verifies the store backend is reachable. The readiness probe uses it.
	Ping(ctx context.Context) error
```

(`context` is already imported in this file.)

- [ ] **Step 4: Implement `Ping` on `*store.SQLite`**

In `internal/store/sqlite.go`, add (the struct's reader handle is `s.r *sql.DB`):

```go
// Ping verifies the database is reachable (readiness probe).
func (s *SQLite) Ping(ctx context.Context) error {
	return s.r.PingContext(ctx)
}
```

- [ ] **Step 5: Implement `Ping` on `*store.Mem`**

In `internal/store/mem.go`, add:

```go
// Ping always succeeds: the in-memory store is reachable whenever the process is.
func (m *Mem) Ping(_ context.Context) error {
	return nil
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -race ./internal/store/`
Expected: PASS (all store tests, including `TestMemPing` and `TestSQLitePing`; the `var _ core.Store = (*Mem)(nil)` / `(*SQLite)(nil)` assertions still compile).

- [ ] **Step 7: Verify formatting and vet**

Run: `gofmt -l internal/core/ internal/store/ && go vet ./internal/core/ ./internal/store/`
Expected: no `gofmt -l` output; `go vet` clean.

- [ ] **Step 8: Commit**

```bash
git add internal/core/store.go internal/store/sqlite.go internal/store/mem.go internal/store/sqlite_test.go internal/store/mem_test.go
git commit -m "feat(store): Ping for readiness probes"
```

---

### Task 2: `/readyz` readiness endpoint + draining flag

**Files:**
- Modify: `internal/api/handlers.go` (imports, `Server` struct, new method + handler)
- Modify: `internal/api/router.go` (register `GET /readyz`)
- Modify: `internal/api/middleware.go` (`routeLabel`)
- Test: `internal/api/readyz_test.go` (new)

**Interfaces:**
- Consumes: `core.Store.Ping(ctx) error` (Task 1).
- Produces: `(*Server).SetDraining(v bool)` — consumed by the daemon (Task 3). New auth-exempt route `GET /readyz`.

- [ ] **Step 1: Write the failing tests**

Create `internal/api/readyz_test.go` (package `api`):

```go
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"concentus/internal/metrics"
	"concentus/internal/store"
)

func readyBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatalf("decode readyz body: %v", err)
	}
	return b["status"]
}

func TestReadyzReady(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Get(hs.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz = %d, want 200", resp.StatusCode)
	}
	if s := readyBody(t, resp); s != "ready" {
		t.Errorf("status = %q, want ready", s)
	}
}

func TestReadyzDraining(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	srv.SetDraining(true)
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz while draining = %d, want 503", resp.StatusCode)
	}
	if s := readyBody(t, resp); s != "draining" {
		t.Errorf("status = %q, want draining", s)
	}
}

type pingErrStore struct{ *store.Mem }

func (pingErrStore) Ping(context.Context) error { return errors.New("store down") }

func TestReadyzStoreUnreachable(t *testing.T) {
	srv := &Server{
		Store:   pingErrStore{store.NewMem()},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: metrics.New("test"),
	}
	hs := httptest.NewServer(srv.Router(""))
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz with dead store = %d, want 503", resp.StatusCode)
	}
	if s := readyBody(t, resp); s != "store unreachable" {
		t.Errorf("status = %q, want store unreachable", s)
	}
}

func TestReadyzAuthExempt(t *testing.T) {
	srv, _, _ := newServerOnly(t)
	hs := httptest.NewServer(srv.Router("secret"))
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("readyz should be auth-exempt, got 401")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/api/ -run TestReadyz`
Expected: compile failure — `srv.SetDraining undefined` and no `/readyz` route.

- [ ] **Step 3: Add imports + the draining field to `Server`**

In `internal/api/handlers.go`, extend the import block to add `context`, `sync/atomic`, and `time` (keep the existing imports):

```go
import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/flow"
	"concentus/internal/metrics"
	"concentus/internal/supervisor"
	"concentus/internal/workspace"
)
```

In the `Server` struct, add the `draining` field after the `Metrics *metrics.Metrics` field (it is the last field, before the closing `}`):

```go
	// draining is set true at shutdown so /readyz returns 503 while liveness
	// (/healthz) stays 200. Zero value = not draining.
	draining atomic.Bool
```

- [ ] **Step 4: Add `SetDraining` and `handleReadyz`**

In `internal/api/handlers.go`, immediately after `handleHealthz` (which ends with its closing `}`), add:

```go
// SetDraining flips the readiness state. After SetDraining(true), /readyz returns 503
// while /healthz (liveness) stays 200. The daemon calls it at graceful shutdown.
func (s *Server) SetDraining(v bool) { s.draining.Store(v) }

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "store unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
```

- [ ] **Step 5: Register the route**

In `internal/api/router.go`, after the `mux.HandleFunc("GET /metrics", s.handleMetrics)` line, add:

```go
	mux.HandleFunc("GET /readyz", s.handleReadyz)
```

- [ ] **Step 6: Label the route in metrics**

In `internal/api/middleware.go`, in `routeLabel`, add a `case` after `/metrics`:

```go
	switch r.URL.Path {
	case "/healthz":
		return "/healthz"
	case "/metrics":
		return "/metrics"
	case "/readyz":
		return "/readyz"
	}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test -race ./internal/api/ -run TestReadyz`
Expected: PASS (`TestReadyzReady`, `TestReadyzDraining`, `TestReadyzStoreUnreachable`, `TestReadyzAuthExempt`).

- [ ] **Step 8: Run the whole api package + verify formatting/vet**

Run: `go test -race ./internal/api/ && gofmt -l internal/api/ && go vet ./internal/api/`
Expected: all api tests PASS (the existing `/healthz` tests unchanged); no `gofmt -l` output; `go vet` clean.

- [ ] **Step 9: Commit**

```bash
git add internal/api/handlers.go internal/api/router.go internal/api/middleware.go internal/api/readyz_test.go
git commit -m "feat(api): /readyz readiness probe + draining flag"
```

---

### Task 3: daemon graceful drain + config

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`
- Modify: `cmd/magisterd/main.go` (shutdown path), `cmd/magisterd/main_test.go`

**Interfaces:**
- Consumes: `(*api.Server).SetDraining(bool)` (Task 2); `config.Config.ShutdownDrain`.
- Produces: nothing new (terminal task).

- [ ] **Step 1: Write the failing config test**

In `internal/config/config_test.go` (package `config`; `testing` and `time` already imported), add:

```go
func TestShutdownDrainDefaultFlagEnv(t *testing.T) {
	c := Parse(nil, func(string) string { return "" })
	if c.ShutdownDrain != 0 {
		t.Errorf("default ShutdownDrain = %v, want 0", c.ShutdownDrain)
	}
	c = Parse([]string{"-shutdown-drain", "5s"}, func(string) string { return "" })
	if c.ShutdownDrain != 5*time.Second {
		t.Errorf("flag ShutdownDrain = %v, want 5s", c.ShutdownDrain)
	}
	c = Parse(nil, func(k string) string {
		if k == "MAGISTER_SHUTDOWN_DRAIN" {
			return "3s"
		}
		return ""
	})
	if c.ShutdownDrain != 3*time.Second {
		t.Errorf("env ShutdownDrain = %v, want 3s", c.ShutdownDrain)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -run TestShutdownDrainDefaultFlagEnv`
Expected: compile failure — `c.ShutdownDrain undefined`.

- [ ] **Step 3: Add the config field, flag, and env**

In `internal/config/config.go`, add the field to the `Config` struct after `ScratchSweepInterval`:

```go
	ShutdownDrain        time.Duration
```

Register the flag after the `scratch-sweep-interval` `DurationVar` line:

```go
	fs.DurationVar(&c.ShutdownDrain, "shutdown-drain", 0, "after shutdown begins, keep serving (readyz=503) this long so load balancers drain before accept stops (0 disables)")
```

Add the env block after the `MAGISTER_SCRATCH_TTL` block (before `return c`):

```go
	if v := env("MAGISTER_SHUTDOWN_DRAIN"); v != "" && !flagSet(fs, "shutdown-drain") {
		if d, err := time.ParseDuration(v); err == nil {
			c.ShutdownDrain = d
		}
	}
```

- [ ] **Step 4: Run the config test to verify it passes**

Run: `go test ./internal/config/ -run TestShutdownDrainDefaultFlagEnv`
Expected: PASS.

- [ ] **Step 5: Wire the drain into the shutdown path**

In `cmd/magisterd/main.go`, the shutdown block currently reads:

```go
	log.Info("shutting down")
	sup.Shutdown(cfg.ShutdownTimeout) // cancel active runs first
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
```

Replace it with (the `srv` variable is already in scope; `time` is already imported):

```go
	log.Info("shutting down")
	srv.SetDraining(true) // /readyz → 503 so load balancers stop routing
	if cfg.ShutdownDrain > 0 {
		log.Info("draining", "grace", cfg.ShutdownDrain)
		time.Sleep(cfg.ShutdownDrain)
	}
	sup.Shutdown(cfg.ShutdownTimeout) // cancel active runs first
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
```

- [ ] **Step 6: Extend the daemon test to assert `/readyz` is 200 while live**

In `cmd/magisterd/main_test.go`, in `TestRunServesHealthzAndShutsDown`, replace the inner `if resp.StatusCode == http.StatusOK { close(stop); return }` block so it also checks `/readyz`. The full `onListen` callback body becomes:

```go
			// addr callback: hit healthz + readyz, then signal stop
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				resp, err := http.Get("http://" + addr + "/healthz")
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						rresp, rerr := http.Get("http://" + addr + "/readyz")
						if rerr != nil {
							t.Errorf("readyz GET failed: %v", rerr)
						} else {
							if rresp.StatusCode != http.StatusOK {
								t.Errorf("readyz while live = %d, want 200", rresp.StatusCode)
							}
							rresp.Body.Close()
						}
						close(stop)
						return
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Error("healthz never became reachable")
			close(stop)
```

- [ ] **Step 7: Run the daemon + config packages**

Run: `go test -race ./cmd/magisterd/ ./internal/config/`
Expected: PASS (`TestRunServesHealthzAndShutsDown` now also asserts readyz 200; the default drain is 0 so shutdown timing is unchanged).

- [ ] **Step 8: Run the whole suite + verify formatting/vet**

Run: `go test -race ./... && gofmt -l internal cmd && go vet ./...`
Expected: ALL packages PASS (expect 17 packages). Report the pass count. No `gofmt -l` output; `go vet` clean.

- [ ] **Step 9: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go cmd/magisterd/main.go cmd/magisterd/main_test.go
git commit -m "feat(magisterd): shutdown drain + readiness wiring"
```

---

## Notes for the implementer

- `*store.SQLite` and `*store.Mem` are the ONLY two `core.Store` implementers (each has a `var _ core.Store = (*T)(nil)` assertion that will fail to compile if `Ping` is missing). There are no test fakes to update except the `pingErrStore` introduced in Task 2's test.
- `atomic.Bool` must not be copied; `Server` is always used as `*Server`, so the field is safe. The daemon constructs `&api.Server{...}` with named fields, so the new unexported `draining` field needs no literal change.
- The store-ping timeout (2s) bounds the probe so a wedged DB can't hang it.
- Default `ShutdownDrain == 0` ⇒ no `time.Sleep`, so the daemon's shutdown timing (and `TestRunServesHealthzAndShutsDown`) is unchanged; the draining flag still flips, so `/readyz` is correct immediately.
- The post-Edit hook emits a harmless path-doubling error on worktree edits; the edit still succeeds.
