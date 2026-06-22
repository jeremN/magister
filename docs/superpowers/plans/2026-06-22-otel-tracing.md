# OTel distributed tracing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add OpenTelemetry tracing (OTLP/HTTP, off by default) spanning HTTP request → async run → step → agent subprocess → gate/join → push/pr/ship delivery, with trace IDs in logs.

**Architecture:** A new `internal/otelx` package owns all OTel SDK contact (provider build + no-op gating + the log-correlation slog handler). Instrumented packages (`engine`, `api`, `supervisor`) use only the OTel **API** via a package-level `var tracer = otel.Tracer("concentus")` — a no-op until the daemon installs an SDK provider, so disabled = byte-for-byte today's runtime. The daemon wires the provider when `-otel-endpoint` is set and flushes it on drain.

**Tech Stack:** Go 1.22 stdlib + OpenTelemetry-Go (API + SDK + `otlptracehttp` exporter, protobuf, **no grpc**).

## Global Constraints

- Go 1.22; **`go.mod`'s `go 1.22` line is NOT bumped.** OTel deps pinned to the latest versions whose own `go.mod` `go` directive is ≤ 1.22 (no release requiring go 1.23+). Existing pinned deps untouched.
- **Dependency containment:** the SDK, exporter, `sdk/resource`, and `propagation` (in `otelx`) are imported only by `internal/otelx` and `cmd/magisterd`; instrumented packages import only the OTel **API** — `go.opentelemetry.io/otel`, `…/otel/trace`, `…/otel/attribute`, `…/otel/codes` — plus `…/otel/propagation` in `internal/api` (HTTP header carrier; core, lightweight). **No `google.golang.org/grpc` in `go.mod`.**
- **Off by default ⇒ byte-for-byte** today's runtime: no spans, no exporter, no network, unchanged log output. The existing test suites of `engine`/`api`/`supervisor` (which install no provider) must stay green unchanged — that IS the regression proof.
- Telemetry is **never fatal**: exporter init / export / flush failures are logged at warn and the daemon continues.
- No new SSE event kind, migration, or schema change; run lifecycle + HTTP status mappings unchanged.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge; `go mod tidy` run and its `go.mod`/`go.sum` committed.

---

## File Structure

- `internal/otelx/otelx.go` — `Config`, `Init` (provider build + global install + propagator), `tracesURL` helper. **The only new SDK/exporter importer besides main.**
- `internal/otelx/loghandler.go` — `NewLogHandler` decorator slog.Handler (adds trace_id/span_id).
- `internal/otelx/otelx_test.go`, `internal/otelx/loghandler_test.go`.
- `internal/engine/engine.go` — package `var tracer`; run/step/agent/gate/join spans; existing Debug lines → `…Context`.
- `internal/engine/trace_test.go` — span-tree assertions with a `tracetest` recorder.
- `internal/api/middleware.go` — `tracingMiddleware`; `internal/api/router.go` — add it to the chain.
- `internal/api/trace_test.go` — server-span + submit→run parent assertions.
- `internal/supervisor/supervisor.go` — `start` takes a parent ctx (submit→run span linkage); package `var tracer`; `Push`/`prCore`/`Ship` delivery spans.
- `internal/config/config.go` — `OTelEndpoint`/`OTelServiceName` fields + flags + env fallback.
- `cmd/magisterd/main.go` — call `otelx.Init`, wrap log handler, shutdown the provider on drain.

---

## Task 1: `internal/otelx` — provider, log-correlation handler, dependencies

**Files:**
- Create: `internal/otelx/otelx.go`, `internal/otelx/loghandler.go`, `internal/otelx/otelx_test.go`, `internal/otelx/loghandler_test.go`
- Modify: `go.mod`, `go.sum` (via `go get` + `go mod tidy`)

**Interfaces:**
- Produces: `otelx.Config{Endpoint, ServiceName, ServiceVersion string}`; `func otelx.Init(ctx context.Context, cfg Config) (*sdktrace.TracerProvider, error)` — returns `(nil, nil)` when `Endpoint==""`; `func otelx.NewLogHandler(inner slog.Handler) slog.Handler`. Task 4 consumes both.

- [ ] **Step 1: Add the dependencies (pinned for go 1.22)**

Run (start with the v1.33.0 release train; the matching exporter module shares the tag):
```bash
go get go.opentelemetry.io/otel@v1.33.0 go.opentelemetry.io/otel/sdk@v1.33.0 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.33.0
go mod tidy
go build ./...
```
Expected: builds clean under go 1.22. **If the build/toolchain complains that an OTel module requires go ≥ 1.23, step the three versions down together (try v1.32.0, then v1.31.0) until `go build ./...` succeeds, and confirm `go.mod`'s module line still says `go 1.22`** (do not let `go mod tidy` bump it — if it did, revert that line to `go 1.22` and re-tidy). Then verify no grpc:
```bash
grep -q google.golang.org/grpc go.mod && echo "FAIL: grpc pulled in" || echo "OK: no grpc"
```
Expected: `OK: no grpc`.

- [ ] **Step 2: Write `internal/otelx/otelx.go`**

```go
// Package otelx wires OpenTelemetry tracing for magisterd. It is the only package
// (besides cmd/magisterd) that imports the OTel SDK + exporter; instrumented packages
// use only the OTel API (otel.Tracer), which is a no-op until Init installs a provider.
package otelx

import (
	"context"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Config configures tracing. Endpoint == "" disables tracing entirely.
type Config struct {
	Endpoint       string // OTLP/HTTP collector base URL, e.g. http://collector:4318
	ServiceName    string // service.name resource attr (default "magisterd")
	ServiceVersion string // service.version resource attr
}

// Init installs a global OTLP/HTTP TracerProvider and the W3C TraceContext propagator,
// returning the provider so the caller can Shutdown it. Returns (nil, nil) when
// cfg.Endpoint == "" — tracing disabled, the global stays the built-in no-op provider.
// The exporter is lazy (it connects on first export), so Init performs no network I/O.
func Init(ctx context.Context, cfg Config) (*sdktrace.TracerProvider, error) {
	if cfg.Endpoint == "" {
		return nil, nil
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(tracesURL(cfg.Endpoint)))
	if err != nil {
		return nil, err
	}
	name := cfg.ServiceName
	if name == "" {
		name = "magisterd"
	}
	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", name),
		attribute.String("service.version", cfg.ServiceVersion),
	))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp, nil
}

// tracesURL ensures the OTLP/HTTP traces path: a base URL with no path (e.g.
// "http://host:4318") gets "/v1/traces" appended; a URL that already has a path is
// left unchanged. A malformed URL is passed through with the suffix (otlptracehttp.New
// then surfaces the parse error).
func tracesURL(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Path != "" && u.Path != "/" {
		return endpoint
	}
	return strings.TrimRight(endpoint, "/") + "/v1/traces"
}
```

- [ ] **Step 3: Write `internal/otelx/loghandler.go`**

```go
package otelx

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// logHandler decorates an slog.Handler, adding trace_id/span_id attributes to every
// record whose context carries a valid span. With tracing disabled no span is ever
// active, so it adds nothing and the output is byte-for-byte unchanged.
type logHandler struct{ inner slog.Handler }

// NewLogHandler wraps inner so records logged with a span-carrying context gain
// trace_id and span_id fields (log↔trace correlation).
func NewLogHandler(inner slog.Handler) slog.Handler { return logHandler{inner} }

func (h logHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h logHandler) Handle(ctx context.Context, rec slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		rec.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, rec)
}

// WithAttrs/WithGroup re-wrap so the decorator survives logger.With(...) — the
// codebase derives run/step-scoped loggers via With, and those must keep trace IDs.
func (h logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return logHandler{h.inner.WithAttrs(attrs)}
}

func (h logHandler) WithGroup(name string) slog.Handler {
	return logHandler{h.inner.WithGroup(name)}
}
```

- [ ] **Step 4: Write the tests**

`internal/otelx/otelx_test.go`:
```go
package otelx

import (
	"context"
	"testing"
)

func TestInitDisabledReturnsNilProvider(t *testing.T) {
	tp, err := Init(context.Background(), Config{Endpoint: ""})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if tp != nil {
		t.Errorf("provider = %v, want nil when endpoint empty", tp)
	}
}

func TestInitEnabledBuildsProvider(t *testing.T) {
	tp, err := Init(context.Background(), Config{Endpoint: "http://127.0.0.1:4318", ServiceName: "test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if tp == nil {
		t.Fatal("provider = nil, want non-nil when endpoint set")
	}
	if err := tp.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestTracesURL(t *testing.T) {
	cases := map[string]string{
		"http://host:4318":            "http://host:4318/v1/traces",
		"http://host:4318/":           "http://host:4318/v1/traces",
		"http://host:4318/v1/traces":  "http://host:4318/v1/traces",
		"https://collector/otlp/path": "https://collector/otlp/path",
	}
	for in, want := range cases {
		if got := tracesURL(in); got != want {
			t.Errorf("tracesURL(%q) = %q, want %q", in, got, want)
		}
	}
}
```

`internal/otelx/loghandler_test.go`:
```go
package otelx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// spanCtx returns a context carrying a valid span (using an exporter-less SDK provider).
func spanCtx(t *testing.T) context.Context {
	t.Helper()
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "s")
	t.Cleanup(span.End)
	return ctx
}

func TestLogHandlerAddsSpanIDsWhenSpanActive(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil)))
	log.InfoContext(spanCtx(t), "hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log not json: %v", err)
	}
	if rec["trace_id"] == nil || rec["span_id"] == nil {
		t.Errorf("want trace_id+span_id, got %v", rec)
	}
}

func TestLogHandlerNoSpanNoIDs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil)))
	log.InfoContext(context.Background(), "hello") // no span in ctx

	var rec map[string]any
	json.Unmarshal(buf.Bytes(), &rec)
	if _, ok := rec["trace_id"]; ok {
		t.Errorf("trace_id present without a span: %v", rec)
	}
}

func TestLogHandlerSurvivesWith(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil))).With("run", "r1")
	log.InfoContext(spanCtx(t), "hello")

	var rec map[string]any
	json.Unmarshal(buf.Bytes(), &rec)
	if rec["trace_id"] == nil {
		t.Errorf("derived logger (.With) dropped trace_id: %v", rec)
	}
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/otelx/ -count=1`
Expected: PASS (5 tests). gofmt + vet: `gofmt -l internal/otelx/ ; go vet ./internal/otelx/` → no output.

- [ ] **Step 6: Commit**

```bash
git add internal/otelx/ go.mod go.sum
git commit -m "feat(otelx): OTLP/HTTP tracer provider + log-correlation slog handler"
```

---

## Task 2: Engine spans (run/step/agent/gate/join) + context-aware logs

**Files:**
- Modify: `internal/engine/engine.go`
- Test: `internal/engine/trace_test.go`

**Interfaces:**
- Consumes: `otel.Tracer` (API). Produces: a span tree `run <id>` → `step <id>` → `agent <name>`, with `gate <id>`/`join <id>` children, emitted by the engine when an SDK provider is installed.

- [ ] **Step 1: Write the failing test** (`internal/engine/trace_test.go`)

This installs a `tracetest` recorder as the global provider, runs a tiny two-step mock flow (one normal step + one join over it — both isolated `mock`, an auto gate), and asserts the span tree. Build the engine with the existing **`newEngine(t, map[string]core.Executor{"mock": …}, nil)` helper (`internal/engine/engine_test.go:28`, returns `(*Engine, *store.Mem, *event.Bus)`)** and drive a flow to `core.RunSucceeded` by copying the run-and-wait body of the **first test in `engine_test.go`** verbatim (subscribe to the bus / poll the store until `RunSucceeded`, exactly as that test does — copy, don't paraphrase). The assertions:

```go
package engine

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestEngineEmitsSpanTree(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	// --- build an Engine + a minimal mock flow + run it to completion ---
	// (Use the SAME construction the existing engine tests use: an Engine with
	// Execs{"mock": ...}, an in-memory Store, a NopBus/recording bus, SystemClock,
	// a workspace manager, gate.Evaluator with an auto verifier, join.Default().
	// Run a flow with one normal `mock` step `greet` (auto gate) and a join step
	// `integrate` (strategy that needs `greet`). Wait for RunSucceeded.)
	// runID := ... ; runEngineToSuccess(t, eng, flow, runID)

	spans := sr.Ended()
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}
	for _, want := range []string{"run " + string(runID), "step greet", "agent mock"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("missing span %q; got %v", want, names(spans))
		}
	}
	// nesting: step greet's parent is the run span; agent mock's parent is a step span.
	run := byName["run "+string(runID)]
	if p := byName["step greet"].Parent().SpanID(); p != run.SpanContext().SpanID() {
		t.Errorf("step greet parent = %v, want run span %v", p, run.SpanContext().SpanID())
	}
}

func names(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name()
	}
	return out
}
```
(Fill the elided run-construction block by copying the setup from the nearest existing engine test that runs a mock flow to success — keep the assertions above verbatim.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/engine/ -run TestEngineEmitsSpanTree -count=1`
Expected: FAIL — no spans recorded yet (`missing span "run …"`).

- [ ] **Step 3: Add the package tracer + imports**

At the top of `internal/engine/engine.go`, in the import block add:
```go
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
```
and a package-level var (near the other package vars, e.g. beside `discardLogger`):
```go
// tracer is the engine's OTel tracer; a no-op until the daemon installs an SDK provider.
var tracer = otel.Tracer("concentus")
```

- [ ] **Step 4: Run root span** — in `runDAG`, immediately after `ctx, cancel := context.WithCancel(parent)` / `defer cancel()` (the current lines 97–98), insert:
```go
	ctx, span := tracer.Start(ctx, "run "+string(runID),
		trace.WithAttributes(attribute.String("magister.run_id", string(runID)), attribute.String("magister.flow", f.Name)))
	defer span.End()
```
Then, immediately after the `e.WS.TeardownRun(runID)` block and **before** `final := context.WithoutCancel(ctx)` (current line 224), set the run span status from the outcome:
```go
	switch {
	case parent.Err() != nil:
		span.SetStatus(codes.Error, "canceled")
	case firstErr != nil:
		span.RecordError(firstErr)
		span.SetStatus(codes.Error, firstErr.Error())
	}
```

- [ ] **Step 5: Step span** — in `runDAG`, in the per-step goroutine, replace the run-step call (current lines 200–201):
```go
				stepStart := e.Clock.Now()
				res, err := e.runStep(ctx, runID, s, inputs)
```
with:
```go
				stepStart := e.Clock.Now()
				stepCtx, stepSpan := tracer.Start(ctx, "step "+s.ID,
					trace.WithAttributes(attribute.String("magister.step_id", s.ID)))
				res, err := e.runStep(stepCtx, runID, s, inputs)
				if err != nil {
					stepSpan.RecordError(err)
					stepSpan.SetStatus(codes.Error, err.Error())
				}
				stepSpan.End()
```

- [ ] **Step 6: Agent span** — in `runAgent`, immediately before `agentCtx := logctx.With(ctx, …)` (current line 393), insert:
```go
	ctx, span := tracer.Start(ctx, "agent "+agentName, trace.WithAttributes(
		attribute.String("magister.agent", agentName),
		attribute.String("magister.role", role),
		attribute.Int("magister.attempt", attemptNum)))
	defer span.End()
```
(`agentCtx := logctx.With(ctx, …)` then derives from the span-carrying ctx, so the executor + its logs nest under the agent span.) After the `ag.Run(...)` call returns, before `return res, err` (current line 413), insert:
```go
	span.SetAttributes(attribute.Float64("magister.cost_usd", res.CostUSD))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
```

- [ ] **Step 7: Gate span** — in `attempt`, replace the gate evaluation (current line 358 `ok, gerr := e.Gate.Evaluate(gateCtx, runID, s, res, workDir)`) with:
```go
	gateCtx, gateSpan := tracer.Start(gateCtx, "gate "+s.ID,
		trace.WithAttributes(attribute.String("magister.gate_policy", string(gatePolicyOf(s)))))
	ok, gerr := e.Gate.Evaluate(gateCtx, runID, s, res, workDir)
	switch {
	case gerr != nil:
		gateSpan.RecordError(gerr)
		gateSpan.SetStatus(codes.Error, gerr.Error())
	case !ok:
		gateSpan.SetStatus(codes.Error, "gate failed")
	}
	gateSpan.End()
```

- [ ] **Step 8: Join span** — in `execute`, replace the join call (current line 430 `res, err := strat.Join(ctx, s, inputs, workDir, run)`) with:
```go
		joinCtx, joinSpan := tracer.Start(ctx, "join "+s.ID, trace.WithAttributes(
			attribute.String("magister.join_strategy", string(s.Join.Strategy)),
			attribute.Int("magister.join_inputs", len(inputs))))
		res, err := strat.Join(joinCtx, s, inputs, workDir, run)
		if err != nil {
			joinSpan.RecordError(err)
			joinSpan.SetStatus(codes.Error, err.Error())
		}
		joinSpan.End()
```

- [ ] **Step 9: Context-aware logs** — convert the engine's existing instrumented log lines to the `…Context(ctx, …)` variants so the log-correlation handler sees the active span. Change exactly these calls (the `ctx` argument is already in scope at each):
  - line ~195 `e.logger().Debug("step slot acquired", …)` → `e.logger().DebugContext(ctx, "step slot acquired", …)`
  - line ~314 `e.logger().Warn("retry budget exhausted", …)` → `e.logger().WarnContext(ctx, "retry budget exhausted", …)`
  - line ~363 `e.logger().Debug("gate evaluated", gargs...)` → `e.logger().DebugContext(ctx, "gate evaluated", gargs...)`
  - line ~394 `e.logger().Debug("agent starting", …)` → `e.logger().DebugContext(ctx, "agent starting", …)`
  - line ~412 `e.logger().Debug("agent finished", args...)` → `e.logger().DebugContext(ctx, "agent finished", args...)`
  - line ~429 `e.logger().Debug("join starting", …)` → `e.logger().DebugContext(ctx, "join starting", …)`
  - line ~435 `e.logger().Debug("join finished", jargs...)` → `e.logger().DebugContext(ctx, "join finished", jargs...)`
  - line ~438 `e.logger().Warn("merge conflict detected", …)` → `e.logger().WarnContext(ctx, "merge conflict detected", …)`

- [ ] **Step 10: Run the new test + the full engine suite**

Run: `go test ./internal/engine/ -run TestEngineEmitsSpanTree -count=1` → PASS.
Run: `go test ./internal/engine/ -count=1` → PASS (all pre-existing engine tests unchanged — they install no provider, so spans are no-ops and behavior/log output is unchanged; this is the disabled-mode regression proof).
`gofmt -l internal/engine/ ; go vet ./internal/engine/` → no output.

- [ ] **Step 11: Commit**

```bash
git add internal/engine/engine.go internal/engine/trace_test.go
git commit -m "feat(engine): trace spans for run/step/agent/gate/join + context logs"
```

---

## Task 3: HTTP server spans, submit→run linkage, delivery spans

**Files:**
- Modify: `internal/api/middleware.go`, `internal/api/router.go`, `internal/supervisor/supervisor.go`
- Test: `internal/api/trace_test.go`

**Interfaces:**
- Consumes: the engine run-root span (Task 2). Produces: an HTTP server span per request; the run-root span parented to the submit span; `push`/`pr`/`ship` delivery spans.

- [ ] **Step 1: Write the failing test** (`internal/api/trace_test.go`)

Install a recorder, submit a run through the HTTP server, and assert (a) a server span named `POST /v1/runs` exists, and (b) the run-root span's parent is that server span (proving submit→run linkage). Build the harness by copying the submit-and-wait body of **`TestPostRunCreatesAndCompletes` (`internal/api/handlers_test.go:59`)** verbatim: `hs, _, st := testServer(t)` (`handlers_test.go:49`), POST the same mock flow it posts, decode the run id, then `waitForStatus(t, st, rr.ID, core.RunSucceeded)` (`handlers_test.go:164`). Define a local `names()` helper in this file (the engine test's copy is in a different package and is not importable):
```go
package api

import (
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestSubmitRunIsChildOfServerSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{}) // import go.opentelemetry.io/otel/propagation
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	// --- submit a mock run via the HTTP server and wait for success ---
	// (Use the existing api harness: newGitServer(t) or testServer(t), POST a mock
	// flow to /v1/runs, decode the run id, waitForStatus(...succeeded).)
	// runID := ...

	spans := sr.Ended()
	var server, run sdktrace.ReadOnlySpan
	for _, s := range spans {
		switch s.Name() {
		case "POST /v1/runs":
			server = s
		case "run " + string(runID):
			run = s
		}
	}
	if server == nil || run == nil {
		t.Fatalf("missing spans; got %v", names(spans)) // names() helper as in engine test
	}
	if run.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Errorf("run parent = %v, want submit server span %v", run.Parent().SpanID(), server.SpanContext().SpanID())
	}
}
```
(Fill the submit block from the nearest existing api test; keep the span assertions verbatim. Add a local `names()` helper if not shared.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/api/ -run TestSubmitRunIsChildOfServerSpan -count=1`
Expected: FAIL — no `POST /v1/runs` span / run not parented.

- [ ] **Step 3: Add `tracingMiddleware`** to `internal/api/middleware.go` (mirrors `metricsMiddleware`; reuses `statusRecorder` + `routeLabel`). Add imports `go.opentelemetry.io/otel`, `…/otel/attribute`, `…/otel/codes`, `…/otel/propagation`, `…/otel/trace`:
```go
// tracingMiddleware starts a server span per request named by the bounded route
// template, extracting an inbound W3C traceparent so a client's trace connects. A
// no-op until the daemon installs an SDK provider.
func tracingMiddleware(routes *http.ServeMux) func(http.Handler) http.Handler {
	tracer := otel.Tracer("concentus")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			route := routeLabel(r, routes)
			ctx, span := tracer.Start(ctx, route,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.request.method", r.Method),
					attribute.String("http.route", route)))
			defer span.End()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))
			span.SetAttributes(attribute.Int("http.response.status_code", rec.status))
			if rec.status >= 500 {
				span.SetStatus(codes.Error, http.StatusText(rec.status))
			}
		})
	}
}
```

- [ ] **Step 4: Wire the middleware** — in `internal/api/router.go`, add `tracingMiddleware(v1)` to the outer chain, right after `requestIDMiddleware` (so the span wraps logging/metrics/handler and the request log line can carry trace IDs):
```go
	return chain(mux,
		requestIDMiddleware,
		tracingMiddleware(v1),
		loggingMiddleware(s.Log),
		metricsMiddleware(s.Metrics, v1),
		recoverMiddleware(s.Log),
		securityHeaders,
	)
```

- [ ] **Step 5: Submit→run span linkage** — in `internal/supervisor/supervisor.go`, change `start` to seed the run ctx from the submitting request's span context (values only, not cancellation). Add imports `go.opentelemetry.io/otel`, `…/otel/attribute`, `…/otel/codes`, `…/otel/trace`, and a package `var tracer = otel.Tracer("concentus")`. Change the signature + body of `start`:
```go
func (s *Supervisor) start(parent context.Context, id core.RunID, run func(context.Context) error) {
	base := context.Background()
	if sc := trace.SpanContextFromContext(parent); sc.IsValid() {
		base = trace.ContextWithRemoteSpanContext(base, sc) // carry the trace, not the cancellation
	}
	runCtx, cancel := context.WithCancel(base)
	s.mu.Lock()
	s.runs[id] = cancel
	s.mu.Unlock()
	// ... rest unchanged ...
```
Update its two callers:
- `Submit` (the `s.start(id, func(runCtx context.Context) error {…})` call): pass the request ctx → `s.start(ctx, id, func(runCtx context.Context) error { return s.engine.Run(runCtx, id, f) })`.
- `ResumeAll` (the `s.start(rs.ID, …)` call): no request ctx → `s.start(context.Background(), rs.ID, func(runCtx context.Context) error { return s.engine.Resume(runCtx, rs, f) })`.

- [ ] **Step 6: Delivery spans** — in `internal/supervisor`, wrap each delivery operation. In `Push` (start of the function body), `prCore` (the shared PR core), and `Ship`, add a span. Push example:
```go
func (s *Supervisor) Push(ctx context.Context, runID core.RunID, opts PushOpts) (PushResult, error) {
	ctx, span := tracer.Start(ctx, "push "+string(runID))
	defer span.End()
	// ... existing body; on each error-return path the deferred span.End still runs ...
```
Add the same `ctx, span := tracer.Start(ctx, "pr "+string(runID))` / `"ship "+string(runID))` + `defer span.End()` at the top of `prCore` and `Ship`. After computing the final error in each (or at the natural single error site), set status — minimally, wrap the function's returned error: rename the return to a named `err` is unnecessary; instead set status on the existing error returns is verbose, so set it once via a deferred closure capturing a named error return. For each of `Push`/`prCore`/`Ship`, name the error return and add:
```go
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}()
```
(Place this defer right after `defer span.End()`; ensure the function's error return value is named so the closure observes it. If a function returns `*PushError`/`*PRError` as a non-error type, set status from that value instead.)

- [ ] **Step 7: Context-aware HTTP request log** — in `internal/api/middleware.go`, the `loggingMiddleware` request log call: change its `…(…)` to the `…Context(r.Context(), …)` variant so it carries trace IDs (the request ctx now holds the server span). (If `loggingMiddleware` logs via `log.Info(...)`, change to `log.InfoContext(r.Context(), ...)`.)

- [ ] **Step 8: Run the tests**

Run: `go test ./internal/api/ -run TestSubmitRunIsChildOfServerSpan -count=1` → PASS.
Run: `go test ./internal/api/ ./internal/supervisor/ -count=1` → PASS (pre-existing tests unchanged; real-git e2e tests need the sandbox disabled).
`gofmt -l internal/api/ internal/supervisor/ ; go vet ./internal/api/ ./internal/supervisor/` → no output.

- [ ] **Step 9: Commit**

```bash
git add internal/api/middleware.go internal/api/router.go internal/api/trace_test.go internal/supervisor/supervisor.go
git commit -m "feat(api): HTTP server spans, submit→run linkage, delivery spans"
```

---

## Task 4: Daemon wiring — flags, Init, log-handler wrap, shutdown flush

**Files:**
- Modify: `internal/config/config.go`, `cmd/magisterd/main.go`
- Test: `cmd/magisterd/main_test.go`

**Interfaces:**
- Consumes: `otelx.Init`, `otelx.NewLogHandler`, the engine/api/supervisor package tracers (Tasks 1–3).

- [ ] **Step 1: Config flags** — in `internal/config/config.go`, add fields to `Config`:
```go
	OTelEndpoint    string
	OTelServiceName string
```
add flags in `Parse` (beside the `-log-format`/`-log-level` flags):
```go
	fs.StringVar(&c.OTelEndpoint, "otel-endpoint", "", "OTLP/HTTP collector endpoint for traces, e.g. http://collector:4318 (empty disables tracing)")
	fs.StringVar(&c.OTelServiceName, "otel-service-name", "magisterd", "OpenTelemetry service.name resource attribute")
```
and, after `fs.Parse(args)` (and the existing env-override block), honor the standard env when the flag is unset:
```go
	if c.OTelEndpoint == "" {
		c.OTelEndpoint = env("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if v := env("OTEL_SERVICE_NAME"); v != "" && !flagSet(fs, "otel-service-name") {
		c.OTelServiceName = v
	}
```

- [ ] **Step 2: Daemon wiring** — in `cmd/magisterd/main.go` `run()`:
  - Add the import `"concentus/internal/otelx"`.
  - **Wrap the log handler** — after `h, err := newLogHandler(...)` (and its error check) and **before** `log := slog.New(h)`, insert:
    ```go
    h = otelx.NewLogHandler(h)
    ```
  - **Init the provider** — after the log handler is built (so warnings can be logged), add:
    ```go
    tp, err := otelx.Init(context.Background(), otelx.Config{
        Endpoint:       cfg.OTelEndpoint,
        ServiceName:    cfg.OTelServiceName,
        ServiceVersion: metrics.BuildVersion(),
    })
    if err != nil {
        log.Warn("tracing disabled: otel init failed", "err", err) // non-fatal
        tp = nil
    }
    ```
  - **Flush on drain** — in the shutdown section, after `sup.Shutdown(cfg.ShutdownTimeout)` and after `shutdownCtx` is created, add:
    ```go
    if tp != nil {
        if err := tp.Shutdown(shutdownCtx); err != nil {
            log.Warn("otel tracer shutdown", "err", err)
        }
    }
    ```
    (Place it before `return httpSrv.Shutdown(shutdownCtx)`.)

- [ ] **Step 3: Write the test** (`cmd/magisterd/main_test.go`, append) — the daemon starts with `-otel-endpoint` set and drains cleanly (exercises Init + the shutdown-flush path; the exporter is lazy so the dead endpoint causes no error). Mirror the existing `run()` harness test (it drives `run(args, env, stopCh, onListen)`):
```go
func TestRunWithOTelEndpointStartsAndDrains(t *testing.T) {
	dir := t.TempDir()
	stop := make(chan struct{})
	listening := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(
			[]string{"-addr", "127.0.0.1:0", "-db", filepath.Join(dir, "m.db"),
				"-otel-endpoint", "http://127.0.0.1:4318"},
			func(string) string { return "" },
			stop, func(addr string) { listening <- addr })
	}()
	select {
	case <-listening:
	case err := <-errCh:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("never listened")
	}
	close(stop)
	if err := <-errCh; err != nil {
		t.Errorf("run returned %v, want clean shutdown", err)
	}
}
```
(Match the existing daemon test's imports/harness — `filepath`, `time`. If the existing tests already have an identical start/stop helper, use it instead of duplicating.)

- [ ] **Step 4: Run the test + full build**

Run: `go test ./cmd/magisterd/ -run TestRunWithOTelEndpointStartsAndDrains -count=1` (sandbox disabled — the daemon spawns no git here but disabling is harmless) → PASS.
Run: `go build ./...` → clean.
`gofmt -l cmd/magisterd/main.go internal/config/config.go ; go vet ./cmd/magisterd/ ./internal/config/` → no output.

- [ ] **Step 5: Commit**

```bash
git add cmd/magisterd/main.go internal/config/config.go cmd/magisterd/main_test.go
git commit -m "feat(magisterd): wire otel tracing (flags, init, log handler, drain flush)"
```

---

## Final verification (after all tasks)

- [ ] `go test -race ./... 2>&1 | tail -30` (sandbox disabled) — all packages green.
- [ ] `gofmt -l .` — no output. `go vet ./...` — no output.
- [ ] `grep -q google.golang.org/grpc go.mod && echo FAIL || echo "OK: no grpc"` — `OK`.
- [ ] Confirm `go.mod`'s module-level `go` directive still reads `go 1.22`.
- [ ] Update the `running-the-orchestrator` skill: add `-otel-endpoint`/`-otel-service-name` to the daemon flags and a one-line tracing note (doc-only; fold into the Task 4 commit or a follow-up doc commit).

## Live smoke (post-merge, manual — not a task)

Start a local OTLP collector exposing OTLP/HTTP on `:4318` (Jaeger all-in-one or `otel/opentelemetry-collector`). Run the daemon with `-otel-endpoint http://127.0.0.1:4318`, submit `flows/git-native-merge.yaml`, and confirm in the backend one trace per run with the nested tree (`POST /v1/runs` → `run` → `step` → `agent`/`join`/`gate`) and that a run log line carries the same `trace_id`. Then run the daemon **without** `-otel-endpoint` → no spans, no network, unchanged logs.
