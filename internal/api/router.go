package api

import (
	"net/http"
	"time"
)

// Router builds the HTTP handler: stdlib ServeMux with Go 1.22 method+wildcard
// patterns, wrapped in the middleware chain. /healthz is exempt from auth so
// liveness probes work without a token. token == "" disables auth (loopback).
func (s *Server) Router(token string) http.Handler {
	mux := http.NewServeMux()

	// health + metrics are mounted outside the authed group
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	v1 := http.NewServeMux()
	v1.HandleFunc("POST /v1/runs", s.handleCreateRun)
	v1.HandleFunc("GET /v1/runs", s.handleListRuns)
	v1.HandleFunc("GET /v1/runs/{id}", s.handleGetRun)
	v1.HandleFunc("DELETE /v1/runs/{id}", s.handleCancelRun)
	v1.HandleFunc("GET /v1/runs/{id}/events", s.handleEvents)
	v1.HandleFunc("POST /v1/runs/{id}/steps/{step}/approve", s.handleApprove)
	v1.HandleFunc("POST /v1/runs/{id}/push", s.handlePush)
	v1.HandleFunc("POST /v1/runs/{id}/pr", s.handlePR)
	v1.HandleFunc("POST /v1/runs/{id}/ship", s.handleShip)
	v1.HandleFunc("POST /v1/runs/{id}/retry", s.handleRetry)
	v1.HandleFunc("GET /v1/loglevel", s.handleGetLogLevel)
	v1.HandleFunc("POST /v1/loglevel", s.handleSetLogLevel)

	authed := chain(v1,
		authMiddleware(token),
		timeoutMiddleware(30*time.Second), // SSE is exempted inside the middleware
	)
	mux.Handle("/v1/", authed)

	return chain(mux,
		requestIDMiddleware,
		tracingMiddleware(v1),
		loggingMiddleware(s.Log),
		metricsMiddleware(s.Metrics, v1),
		recoverMiddleware(s.Log),
		securityHeaders,
	)
}
