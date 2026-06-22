# Observability — OpenTelemetry distributed tracing (OTLP/HTTP, off by default)

## Summary

Add distributed tracing to the daemon: a connected trace spanning the incoming HTTP request → the asynchronous run → each step → each agent-CLI subprocess → gate/join → the post-run push/pr/ship delivery, exported via **OTLP over HTTP** to any OpenTelemetry collector (Jaeger, Tempo, Grafana, Honeycomb, …). This is the last gap in the observability arc (metrics + structured logging + health are done). Tracing is **off by default** — disabled, the runtime is byte-for-byte today's: no spans, no exporter, no network, no log change.

This is the **first deliberate relaxation of the project's stdlib-only invariant**: it adds the OpenTelemetry Go API + SDK (transitively `google.golang.org/protobuf` only; **no grpc**). The heavy dependencies are confined to a new `internal/otelx` package and `cmd/magisterd`; the instrumented packages (`internal/engine`, `internal/api`, `internal/supervisor`) import only the lightweight OTel **API** (`go.opentelemetry.io/otel/trace`). Go 1.22 is **not** bumped — versions are pinned to the latest OTel release that still supports go 1.22.

## Revision (2026-06-22): hand-rolled OTLP/HTTP-JSON exporter — supersedes the official-exporter plan

**Finding (during implementation).** The official OTLP/HTTP exporter `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` does **not** deliver the "no grpc / minimal footprint / go 1.22" premise this spec was approved on. `go list -deps` shows it compiles **59 `google.golang.org/grpc` packages into the binary** — the HTTP and gRPC OTLP exporters share an `internal/otlpconfig` package that imports grpc, so even the HTTP exporter links grpc (chain: `otelx → otlptracehttp → otlptracehttp/internal/otlpconfig → grpc`). It also drags `grpc-gateway`, `genproto`, and `go.opentelemetry.io/proto/otlp`, and one of those transitively forces the module's `go` directive to **`go 1.22.7`** (a patch bump above bare `go 1.22`). `proto/otlp` requires grpc in its own `go.mod` at every version, so no version pin avoids it. This directly contradicts the project's defining near-zero-dependency invariant (the Prometheus `/metrics` endpoint was hand-rolled for exactly this reason).

**Decision (approved).** Replace the official exporter with a **hand-rolled `sdktrace.SpanExporter`** (`internal/otelx/otlpjson.go`) that serializes spans to **OTLP-JSON** and POSTs them to `{endpoint}/v1/traces` (`Content-Type: application/json`) over `net/http`. The OTel **SDK** module (`otel/sdk`) does not depend on grpc/proto — only the *exporter* modules do — so the dependency set shrinks to exactly three OTel modules, all bare `go 1.22`, **zero grpc, zero protobuf, zero exporter module**:

- `go.opentelemetry.io/otel` (API) · `go.opentelemetry.io/otel/trace` (API) · `go.opentelemetry.io/otel/sdk` (provider + batch processor + resource). Pinned **v1.32.0** (its whole train declares bare `go 1.22`; `proto/otlp`/`auto/sdk` are not pulled without the exporter module). `go.mod`'s `go 1.22` line is unchanged. (`go.opentelemetry.io/otel/metric` comes in as a pure indirect of the SDK — no grpc.)

This keeps real network export to any standard OTLP collector (Jaeger/Tempo/Grafana/Honeycomb all accept OTLP-JSON on `/v1/traces`), at the cost of ~200 lines of exporter code the project owns — the same trade it already made for Prometheus.

**OTLP-JSON encoding rules the exporter must honor** (offline-tested in `otlpjson_test.go`):
- `traceId`/`spanId`/`parentSpanId`: lowercase **hex** strings (the OTLP-JSON special case for ID fields; the SDK's `TraceID.String()`/`SpanID.String()` already emit this — *not* base64).
- 64-bit numerics (`startTimeUnixNano`, `endTimeUnixNano`, attribute `intValue`): decimal **strings** (JSON cannot hold uint64 safely).
- `span.kind`: integer enum — OTel `trace.SpanKind` values match the OTLP enum (cast directly).
- `status.code`: integer enum — **must be remapped**: OTel SDK `codes` is `Unset=0, Error=1, Ok=2` but the OTLP proto enum is `Unset=0, Ok=1, Error=2`. A naive `int(status.Code)` cast silently turns every error span into "OK".

**What this revision supersedes:** the "Dependency footprint & version pinning" section below (the exporter module and its transitive-protobuf description) and the `Init` bullet's "builds the OTLP/HTTP exporter (`otlptracehttp.New`…)" clause — `Init` now builds the custom `otlpjson` exporter and wraps it in the SDK batch processor instead. Everything else (span tree, async submit→run propagation, log↔trace correlation, config flags, shutdown flush, off-by-default no-op gating) is exporter-agnostic and unchanged.

## Motivation

The orchestrator already exposes Prometheus metrics (per-agent calls/cost/latency, in-flight gauges, HTTP histograms), request/run-scoped structured logging with a runtime-adjustable level, and liveness/readiness. What it lacks is **causal, per-run timing**: when a run is slow, metrics tell you the aggregate agent latency but not *which step's agent call on which run* dominated, nor how the DAG's gates/joins/retries interleaved. A trace makes one run's execution legible end-to-end — from the API call, through the goroutine-per-step DAG, down to each `claude`/`codex` subprocess — and the agent spans are exactly where latency and cost concentrate. Shipping via OTLP lets operators use any standard backend. Keeping it off-by-default and dependency-isolated means the orchestrator a user runs without a collector is unchanged.

## Design

### Dependency footprint & version pinning

New **direct** dependencies, imported only by `internal/otelx` and `cmd/magisterd`:

- `go.opentelemetry.io/otel` — API (also imported by instrumented packages, lightweight).
- `go.opentelemetry.io/otel/trace` — API tracer/span types (imported by `internal/engine`, `internal/api`, `internal/supervisor`).
- `go.opentelemetry.io/otel/sdk` — the SDK (TracerProvider, batch span processor, samplers, resource).
- `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` — the OTLP/HTTP exporter.

Transitive additions are bounded: `google.golang.org/protobuf` and the otlptrace support packages. **No `google.golang.org/grpc`.** Versions are pinned to the latest OTel release line that still declares `go 1.22` support (≈ v1.32–v1.33; the exact tags are chosen and pinned at implementation time by checking each module's `go` directive — do not select a release that requires go 1.23+). `go.mod`'s `go 1.22` line is unchanged; the existing pinned deps (sqlite, goose, ulid, expr) are untouched.

**Containment rule:** instrumented packages may import only the OTel **API** packages — `go.opentelemetry.io/otel` (global tracer accessor), `…/otel/trace` (tracer/span types + `SpanContextFromContext`), `…/otel/attribute` (span attrs), `…/otel/codes` (`SetStatus`) — all lightweight and inert without an installed SDK provider. The SDK, exporter, propagation, and resource packages are imported solely by `internal/otelx` and `cmd/magisterd`. This keeps the heavy tree at the edges and the no-op-capable API in the core.

### `internal/otelx` — the tracing seam

A new package owns all SDK contact and the disabled/no-op gating:

- `type Config struct { Endpoint, ServiceName, ServiceVersion string }` — `Endpoint == ""` means tracing disabled.
- `func Init(ctx context.Context, cfg Config) (*sdktrace.TracerProvider, error)` — builds the OTLP/HTTP exporter (`otlptracehttp.New` against `cfg.Endpoint`), wraps it in a batch span processor, attaches a `Resource` carrying `service.name` (= `cfg.ServiceName`, default `magisterd`) and `service.version`, constructs the `TracerProvider`, then sets it as the **global** provider (`otel.SetTracerProvider`) and installs the W3C `TraceContext` propagator (`otel.SetTextMapPropagator(propagation.TraceContext{})`). Returns the provider so the daemon can `Shutdown` it. Called by the daemon only when an endpoint is configured.
- `func NewLogHandler(inner slog.Handler) slog.Handler` — the log↔trace correlation decorator (see below).

**Off by default:** when no endpoint is configured the daemon never calls `Init`. The global TracerProvider stays OpenTelemetry's built-in **no-op**, so `otel.Tracer("concentus")` returns a no-op tracer: `tracer.Start` allocates nothing meaningful, records nothing, exports nothing, opens no socket. This mirrors the existing `Server.Metrics`/`Server.LogLevel` nil-guard pattern — the instrumented code always calls the tracer; the tracer is inert when disabled.

### Span tree & creation sites

The tracer is obtained via `otel.Tracer("concentus")` (the global; no-op when disabled). Each instrumented site does `ctx, span := tracer.Start(ctx, name, attrs…); … ; span.End()`, threading the returned `ctx` to children.

- **HTTP server spans** (`internal/api`): a new tracing middleware added to the existing middleware chain (alongside the metrics `ObserveHTTP` middleware in `middleware.go`). It extracts an inbound `traceparent` via the configured propagator, starts a server span named by the existing bounded `routeLabel(r, routes)` template (so cardinality stays bounded — `/healthz`/`/metrics` collapse as today), records `http.method`/`http.route`/`http.status_code`, injects the span context into the request context, and ends the span on response.
- **Engine spans** (`internal/engine`): created at the points that already emit Debug lines —
  - **run root span** (`run <id>`): started when a run begins executing, ended at terminal status (`run.done`/failed/cancelled). Parented to the submit request's trace context (see propagation).
  - **step span** (`step <id>`, child of run): spans a step's execution including its retry attempts; `step.id`, attempt count on attributes.
  - **agent span** (`agent <name>`, child of step): wraps the existing "agent starting" → "agent finished" region — the executor subprocess timing. `agent`, `role`, `attempt`, and (on finish) result/cost attributes; subprocess error sets span status `Error`.
  - **gate span** (`gate <id>`) and **join span** (`join <id>`): children of step/run at the existing gate-evaluated / join-starting points; `strategy`, `inputs` on the join.
- **Delivery spans** (`internal/supervisor`): `push <id>` / `pr <id>` / `ship <id>` wrap the supervisor's git/`gh` operations (children of the corresponding HTTP server spans). Git/`gh` failure sets the span status `Error` with the message.

Span errors use `span.RecordError(err)` + `span.SetStatus(codes.Error, msg)`; the existing error returns and HTTP status mapping are unchanged.

### Context propagation (the async run)

A run is asynchronous: `POST /v1/runs` returns `202` immediately and the run executes in the goroutine-per-step DAG. Propagation:

1. The submit **server span** holds the inbound trace context (extracted `traceparent`, or a fresh root if none).
2. When the engine starts the run, the **run root span** is started from the context that carries the submit span — so the run trace is linked to (a child of) the submit. The root span then lives on the run's own long-lived context, independent of the now-completed HTTP response.
3. The run's context is threaded through the DAG (the engine already passes `context.Context` to steps/agents/gates/joins), so step/agent/gate/join spans nest correctly under the run root.

The result is one connected trace: `client → POST /v1/runs → run → step → agent (subprocess) → gate/join`. Agent **subprocesses are not traced into** — they do not emit OTel — so the agent span is parent-side timing only; injecting `traceparent` into the child process environment is out of scope.

### Log↔trace correlation

`otelx.NewLogHandler(inner slog.Handler)` returns a decorator handler whose `Handle(ctx, rec)` calls `trace.SpanContextFromContext(ctx)`; if the span context `IsValid()`, it adds `trace_id` and `span_id` string attrs to the record before delegating to `inner`. The daemon wraps its root handler with this decorator.

For the decorator to see the active span, the high-value log call sites pass context: the existing engine step/agent/gate/join Debug lines switch to the `…Context(ctx, …)` slog variants (`DebugContext`/`InfoContext`/etc.), and the HTTP request log line likewise. The conversion is mechanical (the context is already in scope at each site) and bounded to those high-value lines — not a blanket sweep.

**Disabled is byte-for-byte:** with tracing off, no span is ever started, so `SpanContextFromContext` returns an invalid context at every log call, the decorator adds nothing, and the log output (and the `…Context` calls, which behave identically to their non-context forms under the standard handler) is unchanged.

### Config surface

Two new daemon flags on `cmd/magisterd` (added to the existing `cfg`):

- `-otel-endpoint <url>` — the OTLP/HTTP collector endpoint (e.g. `http://collector:4318`). Empty ⇒ tracing disabled.
- `-otel-service-name <name>` — `service.name` resource attribute; default `magisterd`.

The OTLP exporter additionally honors the standard `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_SERVICE_NAME` environment variables natively (the SDK reads them); the explicit flags take precedence when set. Sampling is always-on (parent-based `AlwaysSample`) — runs are low-volume, so no ratio sampler is warranted.

### Shutdown / flush

The batch span processor buffers spans, so it must be flushed on exit. The daemon's existing graceful-drain path (after `sup.Shutdown`, around `httpSrv.Shutdown`) calls `tp.Shutdown(shutdownCtx)` (bounded by the existing `ShutdownTimeout`) when a provider was created. A flush/shutdown error is logged, non-fatal.

### Error handling

- A missing/empty endpoint ⇒ tracing simply disabled (not an error).
- A malformed endpoint or exporter-init failure ⇒ **logged at warn and non-fatal**: the daemon continues without tracing. Telemetry must never crash the orchestrator.
- Export failures at runtime (collector down/unreachable) are handled by the SDK's batch processor (dropped/retried per its defaults) and never propagate into request or run handling.

## Testing

- **`internal/otelx`:** `Init` builds a working provider against a `httptest` server standing in for an OTLP/HTTP collector (or an in-memory `tracetest.SpanRecorder` exporter) and a mock run produces the expected span tree — assert span **names**, **parent/child nesting** (run → step → agent; gate/join under their parents), and key **attributes** (run id, step id, agent, attempt). The disabled path (`Endpoint==""`) installs no provider and records zero spans.
- **Log decorator:** `NewLogHandler` adds `trace_id`/`span_id` to a record whose context carries a valid span, and adds nothing when the context has no span — asserted by capturing handler output.
- **`internal/engine` / `internal/api` / `internal/supervisor`:** with an in-memory span recorder installed, a mock run / a test request / a delivery action emit the expected spans; with tracing disabled (global no-op), the same paths emit none and behavior/log output is unchanged (regression).
- **Daemon:** `run()` with `-otel-endpoint` set wires the provider and shuts it down on drain (flush called); without it, no provider is created.
- Full `go test -race ./...` green; `go vet` and `gofmt -l` clean. `go mod tidy` leaves a minimal, grpc-free dependency graph (assert no `google.golang.org/grpc` in `go.mod`).

### Live smoke (manual proof)

Run a local OTLP collector (e.g. `otel/opentelemetry-collector` or Jaeger all-in-one exposing OTLP/HTTP on `:4318`). Start the daemon with `-otel-endpoint http://127.0.0.1:4318`, run `flows/git-native-merge.yaml` (or a real-agent flow), and confirm in the backend a single trace per run with the nested span tree (run → steps → agents → join), correct timing, and that a log line for the run carries the same `trace_id`. Then run the daemon **without** `-otel-endpoint` and confirm no spans/no network and unchanged logs.

## Out of scope

- **Metrics/logs via OTel** — the project keeps its existing Prometheus `/metrics` and `slog`; this slice adds traces only (log correlation is one-directional: trace IDs into logs).
- **Tracing into agent subprocesses** — agents don't emit OTel; no `traceparent` injection into the child environment.
- **OTLP/gRPC and the stdout exporter** — OTLP/HTTP only.
- **Ratio/tail sampling, span links across runs, baggage** — always-on head sampling only.
- **Runtime enable/disable of tracing** — configured at startup (unlike the runtime log level); changing it needs a restart.
- **Persisting trace IDs on the run record / SSE** — no schema or event change.

## Global constraints

- Go 1.22; **`go.mod`'s `go 1.22` is not bumped**. OTel deps pinned to the latest versions that still support go 1.22 (no release requiring go 1.23+). The existing pinned deps (modernc.org/sqlite v1.36.1, pressly/goose/v3 v3.24.1, oklog/ulid/v2 v2.1.1, expr-lang/expr v1.17.8) are untouched.
- **Dependency containment:** SDK + exporter + propagation + resource imported only by `internal/otelx` and `cmd/magisterd`; instrumented packages import only the OTel **API** (`go.opentelemetry.io/otel`, `…/otel/trace`, `…/otel/attribute`, `…/otel/codes`). No `google.golang.org/grpc` in the dependency graph.
- **Off by default ⇒ byte-for-byte** today's runtime: no spans, no exporter, no network, and unchanged log output (the `…Context` log calls behave identically under the standard handler; the decorator adds nothing without an active span).
- Telemetry is **never fatal**: exporter init / export / flush failures are logged and the orchestrator continues.
- No new SSE event kind, no migration, no schema change. The run lifecycle and all existing HTTP status mappings are unchanged.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge; `go mod tidy` run and its `go.mod`/`go.sum` changes committed.
