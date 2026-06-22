package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"concentus/internal/metrics"
)

type ctxKey int

const requestIDKey ctxKey = 0

// chain composes middlewares so the first listed runs outermost.
func chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := ulid.Make().String()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// statusRecorder captures the status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// tracingMiddleware starts a server span per request named by the bounded route
// template, extracting an inbound W3C traceparent so a client's trace connects. A
// no-op until the daemon installs an SDK provider.
func tracingMiddleware(routes *http.ServeMux) func(http.Handler) http.Handler {
	tracer := otel.Tracer("concentus")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			route := routeLabel(r, routes)
			spanName := r.Method + " " + route
			ctx, span := tracer.Start(ctx, spanName,
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

func loggingMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			id, _ := r.Context().Value(requestIDKey).(string)
			log.InfoContext(r.Context(), "request",
				"id", id, "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "dur_ms", time.Since(start).Milliseconds())
		})
	}
}

func recoverMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic", "path", r.URL.Path, "value", v)
					writeError(w, http.StatusInternalServerError, "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// authMiddleware enforces a static bearer token via constant-time compare. An
// empty token disables auth (loopback trust boundary, §9).
func authMiddleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if token == "" {
			return next
		}
		want := []byte("Bearer " + token)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := []byte(r.Header.Get("Authorization"))
			if subtle.ConstantTimeCompare(got, want) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func timeoutMiddleware(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// SSE streams must not be force-timed-out; they manage their own lifetime.
			if strings.HasSuffix(r.URL.Path, "/events") {
				next.ServeHTTP(w, r)
				return
			}
			timeout := d
			// Delivery operations shell out to git/gh over the network; give them a
			// longer bound than ordinary requests, but still bound them.
			if p := r.URL.Path; strings.HasSuffix(p, "/push") || strings.HasSuffix(p, "/pr") || strings.HasSuffix(p, "/ship") {
				timeout = 120 * time.Second
			}
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// metricsMiddleware records request count + duration per request, labeled by the
// matched route TEMPLATE (not the raw path) so cardinality stays bounded. routes is
// the /v1 sub-mux, consulted via Handler(r) to resolve the template — http.Request.
// Pattern is Go 1.23+ and unavailable here. Placed OUTSIDE recoverMiddleware so a
// recovered panic is recorded as its 500 status.
func metricsMiddleware(m *metrics.Metrics, routes *http.ServeMux) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.HTTPStarted()
			defer m.HTTPFinished()
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			m.ObserveHTTP(r.Method, routeLabel(r, routes), rec.status, time.Since(start))
		})
	}
}

// routeLabel resolves a request to a bounded route-template label. /healthz and
// /metrics are matched by path (they live on the outer mux); /v1 routes are resolved
// via the v1 sub-mux's Handler; anything else is "unmatched".
func routeLabel(r *http.Request, routes *http.ServeMux) string {
	switch r.URL.Path {
	case "/healthz":
		return "/healthz"
	case "/metrics":
		return "/metrics"
	case "/readyz":
		return "/readyz"
	}
	if _, pattern := routes.Handler(r); pattern != "" {
		return stripMethod(pattern)
	}
	return "unmatched"
}

// stripMethod drops a leading "METHOD " from a ServeMux pattern, e.g.
// "GET /v1/runs/{id}" → "/v1/runs/{id}".
func stripMethod(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
}
