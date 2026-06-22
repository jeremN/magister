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
	t.Cleanup(func() { span.End() })
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
