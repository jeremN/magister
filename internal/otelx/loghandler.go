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
