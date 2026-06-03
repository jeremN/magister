package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestAuthRejectsMissingBearer(t *testing.T) {
	h := authMiddleware("secret")(okHandler())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/runs", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("missing bearer = %d, want 401", rr.Code)
	}
}

func TestAuthAcceptsCorrectBearer(t *testing.T) {
	h := authMiddleware("secret")(okHandler())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("correct bearer = %d, want 200", rr.Code)
	}
}

func TestAuthDisabledWhenNoToken(t *testing.T) {
	h := authMiddleware("")(okHandler()) // empty token => auth disabled (loopback)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/runs", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("no-token mode should allow, got %d", rr.Code)
	}
}

func TestRecoverTurnsPanicInto500(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := recoverMiddleware(log)(panicky)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("panic should map to 500, got %d", rr.Code)
	}
}
