// Package logctx carries a *slog.Logger on a context so deep callers (the engine's
// agent seam, the executor) can log under a run-scoped logger without threading it
// through every function signature.
package logctx

import (
	"context"
	"io"
	"log/slog"
)

type ctxKey struct{}

// discard is returned by From when no logger is set, so callers never nil-check.
var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// With returns a context carrying log, retrievable by From.
func With(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, log)
}

// From returns the logger stored by With, or a no-op discard logger if none is
// set. It never returns nil.
func From(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && log != nil {
		return log
	}
	return discard
}
