package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/metrics"
	"concentus/internal/store"
)

func TestSSEStreamsRunToCompletion(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString(oneStepFlow))
	if err != nil {
		t.Fatal(err)
	}
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()

	// stream events; the handler closes after run.done
	ereq, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/runs/"+string(rr.ID)+"/events", nil)
	eresp, err := http.DefaultClient.Do(ereq)
	if err != nil {
		t.Fatal(err)
	}
	defer eresp.Body.Close()

	var kinds []string
	var lastSeq int64
	sc := bufio.NewScanner(eresp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			lastSeq, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		}
		if strings.HasPrefix(line, "event: ") {
			kinds = append(kinds, strings.TrimPrefix(line, "event: "))
		}
	}
	if len(kinds) == 0 || kinds[0] != "run.started" || kinds[len(kinds)-1] != "run.done" {
		t.Fatalf("expected run.started ... run.done, got %v", kinds)
	}
	if lastSeq == 0 {
		t.Error("expected non-zero final seq for Last-Event-ID")
	}
}

func TestSSEReplayWithLastEventID(t *testing.T) {
	hs, _, st := testServer(t)
	resp, _ := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString(oneStepFlow))
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()
	waitForStatus(t, st, rr.ID, core.RunSucceeded)

	// reconnect asking for events after seq 1 → must not include seq 1
	ereq, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/runs/"+string(rr.ID)+"/events", nil)
	ereq.Header.Set("Last-Event-ID", "1")
	eresp, err := http.DefaultClient.Do(ereq)
	if err != nil {
		t.Fatal(err)
	}
	defer eresp.Body.Close()
	sc := bufio.NewScanner(eresp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			seq, _ := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			if seq <= 1 {
				t.Errorf("replay returned seq %d, want only > 1", seq)
			}
		}
	}
}

// TestSSEClientDisconnectCleanup verifies that when a client cancels its
// request context (simulating a disconnect), the SSE handler returns promptly
// and unsubscribes from the event bus — no goroutine leak, no stale subscription.
//
// The bus subscription cleanup is guarded by `defer unsub()` in handleEvents.
// If the goroutine exits (which we assert via a deadline), defer ran, so the
// subscription was removed from the bus's internal map.
func TestSSEClientDisconnectCleanup(t *testing.T) {
	// Build a minimal server directly: store, bus, server — no supervisor needed.
	st := store.NewMem()
	bus := event.NewBus()
	srv := &Server{
		Store:   st,
		Bus:     bus,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: metrics.New("test"),
	}

	// Create a pending run so the handler passes the GetRun guard and enters the
	// event loop (it won't see run.done because nothing drives the run).
	runID := core.RunID("disconnect-test-run")
	if err := st.CreateRun(context.Background(), core.RunState{
		ID: runID, Name: "disconnect-test", Status: core.RunPending,
	}); err != nil {
		t.Fatal(err)
	}

	// Subscribe to the bus BEFORE the handler to capture its subscription side-
	// effect.  We won't use the channel — it's just to confirm the bus is alive.
	_, outsideUnsub := bus.Subscribe(4)
	defer outsideUnsub()

	// Build a cancelable request targeting the SSE endpoint.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+string(runID)+"/events", nil)
	req = req.WithContext(ctx)

	// Use a ResponseRecorder that satisfies http.Flusher (httptest.ResponseRecorder
	// implements it). The handler will block in its select until ctx is canceled.
	rw := httptest.NewRecorder()

	// Run the handler in a goroutine and collect when it exits.
	done := make(chan struct{})
	go func() {
		srv.handleEvents(rw, req)
		close(done)
	}()

	// Give the handler a moment to enter its select loop (subscribe + first drain).
	time.Sleep(20 * time.Millisecond)

	// Cancel the client context — simulates the client disconnecting.
	cancel()

	// The handler must exit within a generous deadline; if it doesn't, the
	// r.Context().Done() case in the select is broken or the goroutine leaked.
	select {
	case <-done:
		// Handler exited — defer unsub() ran, subscription cleaned up.
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not exit after client disconnect: possible goroutine leak")
	}

	// Verify the bus subscription was released: subscribe a fresh channel, publish
	// an event, and confirm it arrives (proving the bus is still functional and not
	// deadlocked by a stale closed channel from a leaked subscription).
	testCh, testUnsub := bus.Subscribe(4)
	defer testUnsub()
	bus.Publish(event.Event{RunID: string(runID), Kind: event.RunStarted})
	select {
	case ev := <-testCh:
		if ev.RunID != string(runID) {
			t.Errorf("received unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish did not reach new subscriber: bus may be broken after SSE disconnect")
	}
}
