# OTel distributed tracing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add OpenTelemetry tracing (OTLP/HTTP, off by default) spanning HTTP request → async run → step → agent subprocess → gate/join → push/pr/ship delivery, with trace IDs in logs.

**Architecture:** A new `internal/otelx` package owns all OTel SDK contact (provider build + no-op gating + the log-correlation slog handler). Instrumented packages (`engine`, `api`, `supervisor`) use only the OTel **API** via a package-level `var tracer = otel.Tracer("concentus")` — a no-op until the daemon installs an SDK provider, so disabled = byte-for-byte today's runtime. The daemon wires the provider when `-otel-endpoint` is set and flushes it on drain.

**Tech Stack:** Go 1.22 stdlib + OpenTelemetry-Go (API + SDK only) + a **hand-rolled OTLP/HTTP-JSON span exporter** (net/http). No official OTLP exporter module, no grpc, no protobuf. (See the spec's "Revision (2026-06-22)" for why: the official `otlptracehttp` exporter compiles 59 grpc packages into the binary and forces `go 1.22.7`.)

## Global Constraints

- Go 1.22; **`go.mod`'s `go 1.22` line is NOT bumped.** OTel deps pinned to **v1.32.0** (the whole v1.32.0 train declares bare `go 1.22`; without the official exporter module, `proto/otlp`/`auto/sdk` are not pulled, so the directive stays `go 1.22`). Existing pinned deps untouched.
- **Exactly three new direct deps, all OTel, all bare `go 1.22`, ZERO grpc/protobuf/exporter-module:** `go.opentelemetry.io/otel` (API) · `go.opentelemetry.io/otel/trace` (API) · `go.opentelemetry.io/otel/sdk` (provider/batcher/resource). `otel/metric` enters only as a pure indirect of the SDK. **`go.mod` must contain no `google.golang.org/grpc`, no `go.opentelemetry.io/proto/otlp`, no `…/exporters/otlp/…`.**
- **Dependency containment:** the SDK, `sdk/resource`, `propagation`, and the custom exporter (in `otelx`) are imported only by `internal/otelx` and `cmd/magisterd`; instrumented packages import only the OTel **API** — `go.opentelemetry.io/otel`, `…/otel/trace`, `…/otel/attribute`, `…/otel/codes` — plus `…/otel/propagation` in `internal/api` (HTTP header carrier; core, lightweight).
- **Off by default ⇒ byte-for-byte** today's runtime: no spans, no exporter, no network, unchanged log output. The existing test suites of `engine`/`api`/`supervisor` (which install no provider) must stay green unchanged — that IS the regression proof.
- Telemetry is **never fatal**: exporter init / export / flush failures are logged at warn and the daemon continues.
- No new SSE event kind, migration, or schema change; run lifecycle + HTTP status mappings unchanged.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge; `go mod tidy` run and its `go.mod`/`go.sum` committed.

---

## File Structure

- `internal/otelx/otelx.go` — `Config`, `Init` (provider build with the custom exporter + global install + propagator), `tracesURL` helper. **One of only two new SDK importers (with main).**
- `internal/otelx/otlpjson.go` — the hand-rolled `sdktrace.SpanExporter`: serializes spans to OTLP-JSON and POSTs to `{endpoint}/v1/traces` over net/http. No grpc/proto.
- `internal/otelx/loghandler.go` — `NewLogHandler` decorator slog.Handler (adds trace_id/span_id).
- `internal/otelx/otelx_test.go`, `internal/otelx/otlpjson_test.go`, `internal/otelx/loghandler_test.go`.
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
- Create: `internal/otelx/otelx.go`, `internal/otelx/otlpjson.go`, `internal/otelx/loghandler.go`, `internal/otelx/otelx_test.go`, `internal/otelx/otlpjson_test.go`, `internal/otelx/loghandler_test.go`
- Modify: `go.mod`, `go.sum` (via `go get` + `go mod tidy`)

**Interfaces:**
- Produces: `otelx.Config{Endpoint, ServiceName, ServiceVersion string}`; `func otelx.Init(ctx context.Context, cfg Config) (*sdktrace.TracerProvider, error)` — returns `(nil, nil)` when `Endpoint==""`; `func otelx.NewLogHandler(inner slog.Handler) slog.Handler`. Task 4 consumes both. Internal: `newOTLPJSONExporter(tracesURL string) *otlpJSONExporter` implementing `sdktrace.SpanExporter`.

- [ ] **Step 1: Add the dependencies (three OTel modules, v1.32.0, no exporter module)**

The deps land when `go mod tidy` runs *after* the source files import them (Steps 2–4 import `otel`, `otel/trace`, `otel/sdk/...`). So write the source first, then in Step 6 run:
```bash
go get go.opentelemetry.io/otel@v1.32.0 go.opentelemetry.io/otel/trace@v1.32.0 go.opentelemetry.io/otel/sdk@v1.32.0
go mod tidy
go build ./...
```
**Do NOT** `go get` any `…/exporters/otlp/…` module — that is the whole point of this revision (it would re-introduce grpc + proto + `go 1.22.7`). After tidy, the go directive must read **`go 1.22`** (bare). If `go mod tidy` raised it (it won't, with only these three deps), `go mod edit -go=1.22 && go mod tidy` and confirm it holds. Then the footprint gate — all three must pass:
```bash
grep -nE '^go ' go.mod                                  # MUST be: go 1.22
grep google.golang.org/grpc go.mod || echo "OK: no grpc"
grep -E 'proto/otlp|exporters/otlp' go.mod || echo "OK: no otlp exporter/proto"
```
Expected: `go 1.22`, `OK: no grpc`, `OK: no otlp exporter/proto`.

- [ ] **Step 2: Write `internal/otelx/otelx.go`**

```go
// Package otelx wires OpenTelemetry tracing for magisterd. It is the only package
// (besides cmd/magisterd) that imports the OTel SDK; instrumented packages use only
// the OTel API (otel.Tracer), which is a no-op until Init installs a provider. Spans
// are exported by a hand-rolled OTLP/HTTP-JSON exporter (otlpjson.go) — no grpc, no
// protobuf, no official exporter module.
package otelx

import (
	"context"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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

// Init installs a global TracerProvider (custom OTLP/HTTP-JSON exporter + batch
// processor) and the W3C TraceContext propagator, returning the provider so the
// caller can Shutdown it. Returns (nil, nil) when cfg.Endpoint == "" — tracing
// disabled, the global stays the built-in no-op provider. The exporter does no
// network I/O until the batch processor flushes its first span, so Init never blocks.
func Init(ctx context.Context, cfg Config) (*sdktrace.TracerProvider, error) {
	if cfg.Endpoint == "" {
		return nil, nil
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
		sdktrace.WithBatcher(newOTLPJSONExporter(tracesURL(cfg.Endpoint))),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp, nil
}

// tracesURL ensures the OTLP/HTTP traces path: a base URL with no path (e.g.
// "http://host:4318") gets "/v1/traces" appended; a URL that already has a path is
// left unchanged.
func tracesURL(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Path != "" && u.Path != "/" {
		return endpoint
	}
	return strings.TrimRight(endpoint, "/") + "/v1/traces"
}
```

- [ ] **Step 3: Write `internal/otelx/otlpjson.go` (the hand-rolled exporter)**

A `sdktrace.SpanExporter` that serializes a batch of spans to OTLP-JSON and POSTs it to `{endpoint}/v1/traces`. Honors the four encoding rules (hex IDs, string nanos, kind-as-int, **status-code remap**). Imports only stdlib + the OTel SDK/API — no grpc, no proto.

```go
package otelx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// otlpJSONExporter implements sdktrace.SpanExporter by POSTing spans to an OTLP/HTTP
// collector encoded as OTLP-JSON (Content-Type: application/json). It depends only on
// net/http + the OTel SDK — no grpc, no protobuf, no exporter module.
type otlpJSONExporter struct {
	endpoint string // full traces URL, e.g. http://collector:4318/v1/traces
	client   *http.Client
}

func newOTLPJSONExporter(tracesURL string) *otlpJSONExporter {
	return &otlpJSONExporter{endpoint: tracesURL, client: &http.Client{Timeout: 10 * time.Second}}
}

func (e *otlpJSONExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	body, err := json.Marshal(buildTracesPayload(spans))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("otlp-json export: %s", resp.Status)
	}
	return nil
}

func (e *otlpJSONExporter) Shutdown(context.Context) error { return nil }

// --- OTLP-JSON wire structs (the subset we emit; camelCase = proto3 JSON) ---

type otlpPayload struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}
type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}
type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes,omitempty"`
}
type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}
type otlpScope struct {
	Name string `json:"name,omitempty"`
}
type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            otlpStatus     `json:"status"`
}
type otlpStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}
type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}
type otlpAnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"` // int64 as decimal string
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

func buildTracesPayload(spans []sdktrace.ReadOnlySpan) otlpPayload {
	res := otlpResource{Attributes: kvList(spans[0].Resource().Attributes())}
	byScope := map[string][]otlpSpan{}
	var order []string
	for _, s := range spans {
		name := s.InstrumentationScope().Name
		if _, ok := byScope[name]; !ok {
			order = append(order, name)
		}
		byScope[name] = append(byScope[name], toOTLPSpan(s))
	}
	scopeSpans := make([]otlpScopeSpans, 0, len(order))
	for _, name := range order {
		scopeSpans = append(scopeSpans, otlpScopeSpans{Scope: otlpScope{Name: name}, Spans: byScope[name]})
	}
	return otlpPayload{ResourceSpans: []otlpResourceSpans{{Resource: res, ScopeSpans: scopeSpans}}}
}

func toOTLPSpan(s sdktrace.ReadOnlySpan) otlpSpan {
	sc := s.SpanContext()
	out := otlpSpan{
		TraceID:           sc.TraceID().String(), // SDK emits lowercase hex
		SpanID:            sc.SpanID().String(),
		Name:              s.Name(),
		Kind:              int(s.SpanKind()), // OTel SpanKind values match the OTLP enum
		StartTimeUnixNano: strconv.FormatInt(s.StartTime().UnixNano(), 10),
		EndTimeUnixNano:   strconv.FormatInt(s.EndTime().UnixNano(), 10),
		Attributes:        kvList(s.Attributes()),
		Status:            otlpStatus{Code: otlpStatusCode(s.Status().Code), Message: s.Status().Description},
	}
	if p := s.Parent(); p.HasSpanID() {
		out.ParentSpanID = p.SpanID().String()
	}
	return out
}

// otlpStatusCode remaps OTel SDK status codes to the OTLP proto enum. They DIFFER:
// SDK codes are Unset=0, Error=1, Ok=2; OTLP is Unset=0, Ok=1, Error=2. A direct cast
// would turn every error span into "OK".
func otlpStatusCode(c codes.Code) int {
	switch c {
	case codes.Ok:
		return 1
	case codes.Error:
		return 2
	default:
		return 0
	}
}

func kvList(attrs []attribute.KeyValue) []otlpKeyValue {
	out := make([]otlpKeyValue, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, otlpKeyValue{Key: string(a.Key), Value: anyValue(a.Value)})
	}
	return out
}

func anyValue(v attribute.Value) otlpAnyValue {
	switch v.Type() {
	case attribute.BOOL:
		b := v.AsBool()
		return otlpAnyValue{BoolValue: &b}
	case attribute.INT64:
		s := strconv.FormatInt(v.AsInt64(), 10)
		return otlpAnyValue{IntValue: &s}
	case attribute.FLOAT64:
		f := v.AsFloat64()
		return otlpAnyValue{DoubleValue: &f}
	default:
		s := v.AsString()
		return otlpAnyValue{StringValue: &s}
	}
}
```

- [ ] **Step 4: Write `internal/otelx/loghandler.go`**

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

- [ ] **Step 5: Write the tests**

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

`internal/otelx/otlpjson_test.go` (validates the wire-format rules offline — an httptest server captures the POST body):
```go
package otelx

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestOTLPJSONExporterEncodesSpan(t *testing.T) {
	var body []byte
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp := newOTLPJSONExporter(srv.URL)
	res, _ := resource.New(context.Background(), resource.WithAttributes(attribute.String("service.name", "test")))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithResource(res)) // WithSyncer = export on End
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, span := tp.Tracer("concentus").Start(context.Background(), "run x",
		trace.WithAttributes(attribute.Int("magister.attempt", 3)))
	span.SetStatus(codes.Error, "boom")
	span.End() // exports synchronously; the POST completes before End returns

	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("payload not JSON: %v\n%s", err, body)
	}
	sp := drillToSpan(t, p)

	// (1) traceId: 32 lowercase hex, NOT base64
	if tid, _ := sp["traceId"].(string); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(tid) {
		t.Errorf("traceId = %q, want 32-hex", tid)
	}
	// (2) startTimeUnixNano: a JSON STRING, not a number
	if _, ok := sp["startTimeUnixNano"].(string); !ok {
		t.Errorf("startTimeUnixNano = %v (%T), want string", sp["startTimeUnixNano"], sp["startTimeUnixNano"])
	}
	// (3) status.code: remapped to OTLP ERROR=2 (NOT the SDK's Error=1)
	if st := sp["status"].(map[string]any); st["code"].(float64) != 2 {
		t.Errorf("status.code = %v, want 2 (OTLP ERROR)", st["code"])
	}
	// (4) int attribute: encoded as a STRING intValue
	found := false
	for _, a := range sp["attributes"].([]any) {
		kv := a.(map[string]any)
		if kv["key"] == "magister.attempt" {
			found = true
			if iv, ok := kv["value"].(map[string]any)["intValue"].(string); !ok || iv != "3" {
				t.Errorf("magister.attempt intValue = %v, want string \"3\"", kv["value"])
			}
		}
	}
	if !found {
		t.Error("magister.attempt attribute missing from payload")
	}
}

func drillToSpan(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	rs := p["resourceSpans"].([]any)[0].(map[string]any)
	ss := rs["scopeSpans"].([]any)[0].(map[string]any)
	return ss["spans"].([]any)[0].(map[string]any)
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

- [ ] **Step 6: Run the deps + tidy + footprint gate + tests**

Now that the source imports `otel`/`otel/trace`/`otel/sdk/...`, materialize and verify the deps (Step 1's commands), then run the suite (the `go get`/`tidy`/`build` need network — disable the sandbox for those):
```bash
go get go.opentelemetry.io/otel@v1.32.0 go.opentelemetry.io/otel/trace@v1.32.0 go.opentelemetry.io/otel/sdk@v1.32.0
go mod tidy
grep -nE '^go ' go.mod                                   # MUST be: go 1.22
grep google.golang.org/grpc go.mod || echo "OK: no grpc"
grep -E 'proto/otlp|exporters/otlp' go.mod || echo "OK: no otlp exporter/proto"
go build ./...
go test ./internal/otelx/ -count=1
```
Expected: `go 1.22`; `OK: no grpc`; `OK: no otlp exporter/proto`; build clean; PASS (6 tests: 3 otelx + 1 otlpjson + 3 loghandler... = TestInitDisabled, TestInitEnabled, TestTracesURL, TestOTLPJSONExporterEncodesSpan, TestLogHandlerAddsSpanIDsWhenSpanActive, TestLogHandlerNoSpanNoIDs, TestLogHandlerSurvivesWith). gofmt + vet: `gofmt -l internal/otelx/ ; go vet ./internal/otelx/` → no output.

- [ ] **Step 7: Commit**

```bash
git add internal/otelx/ go.mod go.sum
git commit -m "feat(otelx): tracer provider + OTLP/HTTP-JSON exporter + log-correlation handler"
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
