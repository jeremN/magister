package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// runLoop must process messages, apply update, render via render(), and exit on cmdQuit.
func TestRunLoopRendersAndQuits(t *testing.T) {
	msgs := make(chan any, 8)
	var lastFrame string
	rendered := make(chan struct{}, 16)
	exec := func(cmds []any) {
		for _, c := range cmds {
			if _, ok := c.(cmdQuit); ok {
				close(msgs)
			}
		}
	}
	render := func(m model) {
		lastFrame = view(m, 80, 24)
		select {
		case rendered <- struct{}{}:
		default:
		}
	}
	go func() {
		msgs <- runsLoaded{{ID: "a1", Name: "feature", Status: "running"}}
		<-rendered
		msgs <- keyMsg('q')
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runLoop(ctx, msgs, exec, render); err != nil {
		t.Fatal(err)
	}
	if lastFrame == "" {
		t.Fatal("expected at least one rendered frame")
	}
}

// streamLoop must stop reconnecting once the endpoint returns a non-2xx (the run
// is gone / the server is refusing) — otherwise it hammer-reconnects at ~2/s.
func TestStreamLoopStopsOnNon2xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	// Generous safety ctx so a regression hangs here rather than wedging the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		streamLoop(ctx, NewClient(srv.URL, ""), "a1", make(chan any, 1))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("streamLoop did not return promptly on a 404 — it kept reconnecting")
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("server hit %d times, want exactly 1 (no reconnect)", n)
	}
}
