# Ops — `/healthz` liveness + `/readyz` readiness split

## Summary

Split the daemon's single health endpoint into the two probes a load balancer / orchestrator expects:

- **`/healthz` (liveness)** — unchanged: an unconditional `200 {"status":"ok"}`, dependency-free. Answers "is the process alive?" → decides whether to **restart**.
- **`/readyz` (readiness, new)** — `200 {"status":"ready"}` only when the daemon is **not draining AND the store is reachable**; otherwise `503`. Answers "can this process serve traffic right now?" → decides whether to **route** traffic, and drains the pod on graceful shutdown.

Same posture as the metrics slices: stdlib only, **no new dependency**, no DB migration, no new store table. Both probes are auth-exempt (like `/healthz` and `/metrics` today). `/healthz` behavior is byte-for-byte unchanged.

## Motivation

Today `/healthz` always returns 200, so it conflates two questions. Liveness must stay dependency-free — a transient DB blip should never trip a liveness probe and cause a needless restart. Readiness is the opposite: it *should* reflect dependency health and, critically, flip to 503 the moment graceful shutdown begins so the load balancer stops routing new requests before the server stops accepting connections. The split is the standard k8s/LB contract and the natural companion to the `/metrics` work.

## Endpoints

| Path | Probe | 200 when | 503 when | Auth |
|---|---|---|---|---|
| `/healthz` | liveness | always (process serving) | never | exempt |
| `/readyz` | readiness | not draining AND store reachable | draining, or store ping fails | exempt |

`/readyz` bodies: `{"status":"ready"}` (200), `{"status":"draining"}` (503, shutting down), `{"status":"store unreachable"}` (503, ping failed). `/healthz` body stays `{"status":"ok"}`.

The `draining` check is evaluated before the store ping (cheap, and during shutdown we want 503 regardless of store state).

## Design

### `core.Store.Ping(ctx) error` (new port method)

Add to the `core.Store` interface, following the blessed-addition pattern used by `Reclaim`/`Provision`/`BasePath`:

```go
// Ping verifies the store backend is reachable. Used by the readiness probe.
Ping(ctx context.Context) error
```

- **SQLite** (`internal/store/sqlite.go`): `return s.r.PingContext(ctx)` — pings the reader connection pool, confirming the DB is reachable.
- **Mem** (`internal/store/mem.go`): `return nil` (always reachable).
- Every other `core.Store` implementer — including any test fakes — gains `Ping`. The implementation plan sweeps the codebase for implementers and adds the method to each.

### `internal/api` — readiness endpoint + draining flag

- `Server` gains an unexported `draining atomic.Bool` and an exported `SetDraining(v bool)` method. Zero value = not draining, so the existing `&api.Server{...}` literal needs no change.
- `handleReadyz`:
  - if `s.draining.Load()` → `503 {"status":"draining"}`.
  - else ping the store under a short timeout (2s) derived from the request context; on error → `503 {"status":"store unreachable"}`.
  - otherwise → `200 {"status":"ready"}`.
- `handleHealthz` is unchanged.
- Router (`internal/api/router.go`): register `mux.HandleFunc("GET /readyz", s.handleReadyz)` on the **outer** mux (auth-exempt, beside `/healthz` and `/metrics`).
- `routeLabel` (`internal/api/middleware.go`): add a `case "/readyz": return "/readyz"` so the HTTP metrics middleware labels readiness requests with a bounded route template.

The store-ping timeout bounds a probe against a wedged DB so the probe itself can't hang.

### `cmd/magisterd` + `internal/config` — graceful drain

- New config field `ShutdownDrain time.Duration`, from flag `-shutdown-drain` and env `MAGISTER_SHUTDOWN_DRAIN`, **default `0`**.
- The shutdown path (after `<-stopCh`, `log.Info("shutting down")`) becomes:
  1. `srv.SetDraining(true)` — `/readyz` now returns 503 so load balancers stop routing.
  2. if `cfg.ShutdownDrain > 0`: `log.Info("draining", "grace", cfg.ShutdownDrain)` then `time.Sleep(cfg.ShutdownDrain)` — holds the server open (still accepting, in-flight requests still served) long enough for an LB to observe the 503.
  3. `sup.Shutdown(cfg.ShutdownTimeout)` (unchanged).
  4. `httpSrv.Shutdown(shutdownCtx)` (unchanged).

With the default `ShutdownDrain == 0` there is no sleep and the shutdown timing is byte-for-byte today's; the draining flag still flips, so `/readyz` is correct immediately. The sleep is a plain `time.Sleep` (not interruptible) — k8s sends SIGKILL after its own grace period, which is the hard stop.

## Testing

- **store** (`internal/store`): SQLite `Ping` returns nil against a live DB; Mem `Ping` returns nil.
- **api** (`internal/api`):
  - `/readyz` → 200 `{"status":"ready"}` when not draining and the store pings OK.
  - `/readyz` → 503 after `SetDraining(true)` (body `{"status":"draining"}`).
  - `/readyz` → 503 when the store's `Ping` returns an error (drive via a fake `core.Store` whose `Ping` errors).
  - `/readyz` is auth-exempt — reachable without a token (mirror the existing `/healthz` exemption test).
  - The existing `/healthz` 200 test is unchanged.
- **daemon** (`cmd/magisterd`): the existing `TestRunServesHealthzAndShutsDown` stays green (default drain 0); extend it to also assert `/readyz` → 200 while the daemon is live.
- Full `go test -race ./...` green; `go vet` + `gofmt` clean.

## Out of scope

- A per-dependency readiness breakdown in the body (only the store is checked; a single status string suffices).
- Probing the supervisor/engine internals or the agent CLIs.
- A `/livez` alias (keep `/healthz` as the liveness path).
- Making the drain sleep interruptible by a second signal.
- Any change to `/healthz`, `/metrics`, auth, or the existing routes.

## Global constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no DB migration; no new store table.
- `/healthz` and `/readyz` are both auth-exempt; `/healthz` response is byte-for-byte unchanged.
- `core.Store.Ping(ctx context.Context) error` added to the interface and to EVERY implementer.
- `-shutdown-drain` / `MAGISTER_SHUTDOWN_DRAIN` defaults to `0` (today's shutdown timing preserved).
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.
