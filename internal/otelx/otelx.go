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
