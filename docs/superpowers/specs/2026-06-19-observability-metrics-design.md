# Observability — Prometheus /metrics endpoint — Design

**Date:** 2026-06-19
**Status:** Approved (design); ready for implementation plan.

## Problem

`magisterd`'s **logging** is already mature: stdlib `log/slog` threaded through Engine/Supervisor/Server via optional `Log` fields + nil-safe `logger()` accessors, and the HTTP layer already has per-request ULID (`X-Request-ID`), request logging (`method/path/status/dur_ms`), panic recovery, and security headers. There is even a `GET /healthz` (auth-exempt, static `{"status":"ok"}`).

**Metrics, by contrast, are completely absent** — zero counters/gauges/histograms, no `/metrics`, no `expvar`. Yet the engine already emits a rich lifecycle event stream (`run.started/done`, `step.started/done/failed/retrying`, `gate.awaiting`, `agent.tool`, each carrying `CostUSD`) — a ready-made source of quantitative signal that is currently only observable as a live SSE feed or by querying the store. There is no way to scrape aggregate operational metrics for monitoring/alerting.

## Goal

Add a scrapeable **`GET /metrics`** endpoint in **Prometheus text-exposition format**, hand-rolled with **zero new Go dependencies** (the Prometheus *text format* is just text; the client library is not needed and would violate the project's no-new-deps convention). It exposes run/step lifecycle, HTTP, agent cost/tool, and Go-runtime/build metrics. The endpoint is **auth-exempt** (like `/healthz`), so a loopback Prometheus scraper works with zero config.

**Scope decisions (locked during brainstorming):**
- **Prometheus text format, hand-rolled, zero deps.** Not `expvar` (no histograms, clunky labels), not the Prometheus client library (new dep).
- **In-process collection** via a small `internal/metrics` registry instrumented at the engine's existing lifecycle points + HTTP middleware. NOT an event-bus subscriber: the event `Bus` is intentionally lossy (it drops events for full/slow subscribers — the store holds the durable copy), so bus-derived counters would silently undercount.
- **Auth-exempt** `/metrics`, mirroring `/healthz` and the loopback-trust posture (auth only matters when `MAGISTER_BEARER_TOKEN` is set).
- **All four metric families:** run/step lifecycle, HTTP, agent cost/tool, Go-runtime/build.

## Architecture — a hand-rolled `internal/metrics` registry, threaded like the logger

A new self-contained, stdlib-only **`internal/metrics`** package. The Engine and the API Server each hold an optional `*metrics.Metrics`, exactly mirroring how they already hold an optional `*slog.Logger`. The engine increments domain metrics at its existing lifecycle points; the server records HTTP metrics in middleware and serves `/metrics`; `cmd/magisterd` constructs one `*metrics.Metrics` and assigns it to both. Runtime/build metrics are read live at scrape time. No store change, no new dependency, no DB migration, no new control flow — each instrumentation site is a single nil-safe method call beside an existing event emission.

```
engine lifecycle (run/step/gate/agent.tool) ──► m.ObserveRun/ObserveStep/GateAwaited/AgentTool/AddCost
HTTP middleware (after next.ServeHTTP)        ──► m.ObserveHTTP(method, route, status, dur)
GET /metrics (auth-exempt)                    ──► m.WriteProm(w)  ──► [stored families] + [runtime/build read on-scrape]
```

This is the project's ports-and-adapters discipline applied once more: metrics collection is an optional collaborator the engine/server depend on through a tiny interface-shaped struct, independently testable, defaulting to a safe no-op when unwired.

## Components

### 1. `internal/metrics` package (new)

**Primitives** (concurrency-safe):
- `Counter` — monotonic `float64` stored as `atomic.Uint64` (via `math.Float64bits`); `Add(delta float64)` (CAS loop). Integer counts use `Add(1)`; cost uses `Add(usd)`.
- `Gauge` — settable `float64` as `atomic.Uint64`; `Set(v float64)`.
- `Histogram` — fixed ascending `[]float64` upper bounds + a per-bucket `[]atomic.Uint64` of cumulative counts (conceptually) + atomic `sum` (float bits) + atomic `count`. `Observe(v float64)` finds the bucket and updates sum/count and the bucket tally; the `+Inf` overflow bucket is implicit (`count` − last finite bucket).
- `CounterVec` / `HistogramVec` — labeled variants: a `map[labelKey]*Counter|*Histogram` guarded by a `sync.RWMutex`. `labelKey` is the ordered label values joined by a separator. Lookup takes a read lock; first-time series creation takes a write lock; the returned primitive is then mutated lock-free via its atomics.

**`Metrics` struct** — the fixed, named set of families (no dynamic registry; the set is known, so named struct fields are the most readable and testable form). Fields per the metric set below. Methods:
- `New(version string) *Metrics` — constructs all families with their bucket boundaries and stashes the build version.
- One instrumentation method per site: `ObserveRun(status string, d time.Duration)`, `ObserveStep(status string, d time.Duration)`, `GateAwaited()`, `AgentTool()`, `AddCost(usd float64)`, `ObserveHTTP(method, route string, status int, d time.Duration)`.
- `WriteProm(w io.Writer)` — renders all stored families then appends the on-scrape runtime/build metrics.
- **Nil-safe by receiver:** every method begins `if m == nil { return }`, so call sites are plain `e.Metrics.ObserveStep(...)` and an unwired (nil) `Metrics` is a safe no-op. (Mirrors the optional `Log` field; no accessor indirection needed.)

### 2. The metric set

All `magister_`-prefixed except the conventional `go_*` runtime metrics.

**Run + step lifecycle (engine-sourced)**
| Metric | Type | Labels | Source / notes |
|---|---|---|---|
| `magister_runs_total` | counter | `status` | run-done paths; status ∈ `succeeded`/`failed`/`canceled` |
| `magister_run_duration_seconds` | histogram | — | run.done − run.started; buckets `5,30,60,300,600,1800,3600` |
| `magister_steps_total` | counter | `status` | step done/failed/retrying; status ∈ `succeeded`/`failed`/`retrying` |
| `magister_step_duration_seconds` | histogram | — | step.done − step.started; buckets `1,5,10,30,60,120,300,600` |
| `magister_gates_awaiting_total` | counter | — | `gate.awaiting` emission |

**Agent (engine-sourced)**
| Metric | Type | Labels | Source / notes |
|---|---|---|---|
| `magister_agent_tool_calls_total` | counter | — | one per `agent.tool` milestone |
| `magister_agent_cost_usd_total` | counter | — | sums `CostUSD` (already on step.done + agent.tool) |

**HTTP (middleware-sourced)**
| Metric | Type | Labels | Source / notes |
|---|---|---|---|
| `magister_http_requests_total` | counter | `method,route,status` | `route` = matched template via `mux.Handler(r)` (see §4 — NOT `r.Pattern`, a Go 1.23 field); unmatched → `"unmatched"` |
| `magister_http_request_duration_seconds` | histogram | `method,route` | buckets `.005,.01,.025,.05,.1,.25,.5,1,2.5,5,10` |

**Runtime + build (on-scrape)**
| Metric | Type | Labels | Source / notes |
|---|---|---|---|
| `go_goroutines` | gauge | — | `runtime.NumGoroutine()` |
| `go_memstats_alloc_bytes` | gauge | — | `runtime.ReadMemStats().Alloc` |
| `go_memstats_heap_sys_bytes` | gauge | — | `ReadMemStats().HeapSys` |
| `go_gc_count_total` | counter | — | `ReadMemStats().NumGC` (rendered as a counter) |
| `magister_build_info` | gauge=1 | `version,go_version` | `version` from `debug.ReadBuildInfo()` vcs.revision; `go_version` from `runtime.Version()` |

`status` label values are a small closed set; `route` is a template not a raw path. No user-controlled value reaches a label, so cardinality is bounded by design.

### 3. Engine instrumentation (`internal/engine`)

Each call sits beside an existing event emission, adding no new control flow:
- **Run:** capture a monotonic start where the run is marked Running (`run.started`, ~engine.go:102); at each run-done path (succeeded/failed/canceled, ~215–239) call `m.ObserveRun(status, since(start))`.
- **Step:** capture start at step start (~268); at step done/failed/retrying (~260, 275) call `m.ObserveStep(status, dur)`.
- **Gate:** at `gate.awaiting` (~334) call `m.GateAwaited()`.
- **Agent:** in the `agent.tool` milestone path (~356–363) call `m.AgentTool()`, and where `CostUSD` is populated (step done + agent.tool) call `m.AddCost(usd)`.

**Duration timing:** use a monotonic start captured in the **same execution scope** as the matching done (the engine emits started→done within its run/step execution path), reusing the engine's existing clock — race-free, no map. The plan verifies the exact scope per metric; the documented fallback (only if a started/done pair is split across goroutines) is a small `map[id]time.Time` under a mutex. Local scoping is expected to suffice.

### 4. HTTP middleware (`internal/api`)

A small `metricsMiddleware` wrapping the mux records, after `next.ServeHTTP`, `m.ObserveHTTP(method, route, status, dur)`:
- It reuses the **status-capturing response recorder** the existing `loggingMiddleware` already uses (no second recorder).
- **`route` label source (Go 1.22-safe):** `http.Request.Pattern` is a **Go 1.23** field and is unavailable here, so the route template is obtained from **`mux.Handler(r)`** — `(*ServeMux).Handler(r) (Handler, pattern string)` returns the registered pattern that matches the request (method + path), e.g. `GET /v1/runs/{id}`. The middleware holds a reference to the routing mux and calls `_, pattern := mux.Handler(r)` to derive the label; an empty/no-match pattern → `"unmatched"`. The method may be stripped from the returned pattern for a cleaner `route` value (the `method` label already carries it).
- **Mux-structure caveat (plan verifies):** `mux.Handler(r)` returns the matching pattern *of the mux it is called on*. If all `/v1` routes are registered on a single mux (auth/timeout applied as middleware wrapping that mux), it returns the specific route — correct. If the router nests a sub-mux under a `/v1/` prefix, the outer mux would return the subtree prefix; in that case the metrics middleware is applied at the layer whose `mux.Handler(r)` yields the specific route, **or** falls back to a small explicit `routeLabel(method, path)` normalizer over the known, fixed route set (mapping `{id}`/`{step}` segments to placeholders). The plan reads `router.go` and picks the mechanism the actual structure supports; either way the label is a bounded template, never a raw `/v1/runs/<ulid>` path.

### 5. Endpoint, exposition format & wiring

**Endpoint:** `GET /metrics`, registered alongside `/healthz` — **outside** the `/v1` auth+timeout wrappers, so auth-exempt and not subject to the `/v1` timeout. Handler sets `Content-Type: text/plain; version=0.0.4; charset=utf-8` and calls `m.WriteProm(w)`.

**Exposition format** — standard Prometheus text:
```
# HELP magister_runs_total Total runs by terminal status.
# TYPE magister_runs_total counter
magister_runs_total{status="succeeded"} 42
magister_runs_total{status="failed"} 3
# HELP magister_step_duration_seconds Step execution time in seconds.
# TYPE magister_step_duration_seconds histogram
magister_step_duration_seconds_bucket{le="1"} 5
magister_step_duration_seconds_bucket{le="5"} 9
magister_step_duration_seconds_bucket{le="+Inf"} 11
magister_step_duration_seconds_sum 37.2
magister_step_duration_seconds_count 11
```
- Each family emits `# HELP` + `# TYPE` then its series; histograms emit cumulative `_bucket{le=...}` (ascending, always ending `le="+Inf"`), then `_sum`, then `_count`.
- **Label-value escaping:** `\` → `\\`, `"` → `\"`, newline → `\n`.
- **Deterministic ordering** (sorted label-key tuples) so output is stable and testable.

**Wiring (`cmd/magisterd/main.go`):** construct `m := metrics.New(version)` once (version from `debug.ReadBuildInfo()`); set `eng.Metrics = m` and `srv.Metrics = m`. That is the whole change to `main` — nil stays a safe no-op, so nothing else is touched.

## Data flow

`GET /metrics` → (auth-exempt) handler → `m.WriteProm(w)` → render stored families (counters/gauges/histograms accumulated in-process since boot) → append on-scrape runtime/build gauges → Prometheus text. Domain counters are populated as runs execute (engine instrumentation); HTTP counters as requests are served (middleware). No store read on the scrape path.

## Error handling & edge cases

- **Concurrency:** primitives are atomic; vec maps use `RWMutex` (read-lock to find a series, write-lock only to create one). `WriteProm` snapshots under read locks. Verified with a `-race` concurrency test.
- **Scrape never errors the daemon:** `WriteProm` only reads; a client write error is ignored (standard). No panic on an empty registry (zero series → just HELP/TYPE, or nothing).
- **Counter reset semantics:** in-process counters reset to 0 on daemon restart — standard Prometheus counter behavior (`rate()` handles resets). Lifetime totals are deliberately NOT rebuilt from the store on scrape.
- **Histogram overflow:** values above the top finite bucket land only in `+Inf`; `_sum` still includes them.
- **Cardinality:** bounded — `route` is a template (unmatched → `"unmatched"`), `status` is a closed set; no user-controlled label values.

## Testing

- **`internal/metrics` unit tests:** primitives (`Counter.Add` incl. float cost, `Gauge.Set`, `Histogram.Observe` at/across bucket boundaries + `_sum`/`_count`/`+Inf`); vec types (distinct tuples → distinct series, same tuple reused); `WriteProm` output (HELP/TYPE lines, cumulative ascending buckets ending `+Inf`, label escaping, deterministic ordering — golden-style string assertion); **concurrency** (N goroutines under `-race`, exact final sum/count).
- **`internal/api` tests (httptest):** hit a real route, scrape `/metrics`, assert `magister_http_requests_total{...route="/v1/runs"...}` bumped and a duration observed, with the label being the **template** not a raw `/v1/runs/<ulid>` path; `/metrics` returns 200 + Prometheus content-type and is reachable **even when a bearer token is set** (auth-exempt regression guard).
- **`internal/engine` tests (mock flow):** run a mock flow to success; assert `magister_runs_total{status="succeeded"}=1`, `magister_steps_total` bumped, run/step duration `_count=1`, and the cost counter reflects the mock's `CostUSD`.
- **Daemon smoke (optional, `main_test.go` style):** boot → `GET /metrics` → 200, parseable.
- **Manual proof (live):** run the mock flow against the live daemon, `curl /metrics`, eyeball the families populate (runs/steps/http/cost) and that values move across runs. Zero-cost, no external deps.

## Out of scope (noted follow-ups)

- **`magister_runs_in_flight` gauge** — would need a new `Store` count method/query; deferred so this slice touches no store code (and needs no migration). Add later if live-state gauges prove valuable.
- **Per-agent labels** (e.g. `magister_agent_cost_usd_total{agent="codex"}`) — start unlabeled; add a label dimension later if a real need appears.
- **Full Prometheus go-collector parity** — only a lean, useful runtime subset (goroutines, alloc, heap sys, GC count) is exposed, not the entire `go_*`/`process_*` surface.
- **Tracing / OpenTelemetry spans** — a different observability axis; not in this slice.
- **Supervisor delivery-op metrics** (push/pr/ship counts) — not in the chosen families; trivially addable later via the same `Metrics` struct.

## Global constraints

- Go 1.22; stdlib only, **no new Go dependency** (Prometheus text format is hand-rolled); **no DB migration; no new store method; no engine control-flow change** (only nil-safe metric calls beside existing emissions).
- **Auth-exempt** `/metrics` (consistent with `/healthz`); aggregate data only (no run content).
- Metrics collection is an **optional collaborator** — a nil `*metrics.Metrics` is a safe no-op, so the daemon wires it but tests need not.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`, never `--no-verify`. `gofmt`/`go vet`/`go test -race ./...` clean before merge.
