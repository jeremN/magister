package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

func TestTimeoutMiddlewareDeliveryRoutesGetLongerBound(t *testing.T) {
	var remaining time.Duration
	h := timeoutMiddleware(30 * time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dl, ok := r.Context().Deadline(); ok {
			remaining = time.Until(dl)
		} else {
			remaining = -1 // no deadline (exempt)
		}
	}))
	check := func(method, path string) time.Duration {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
		return remaining
	}

	if d := check(http.MethodPost, "/v1/runs/r1/ship"); d < 60*time.Second {
		t.Errorf("ship deadline = %v, want ~120s", d)
	}
	if d := check(http.MethodPost, "/v1/runs/r1/push"); d < 60*time.Second {
		t.Errorf("push deadline = %v, want ~120s", d)
	}
	if d := check(http.MethodGet, "/v1/runs/r1"); d > 31*time.Second {
		t.Errorf("normal deadline = %v, want ~30s", d)
	}
	if d := check(http.MethodGet, "/v1/runs/r1/events"); d != -1 {
		t.Errorf("events should be exempt (no deadline), got %v", d)
	}
}
