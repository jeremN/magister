package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"concentus/internal/core"
	"concentus/internal/event"
)

// handleEvents streams a run's journal as SSE. The durable events table is the
// source of truth (real seqs); the in-memory bus is only a "re-query now"
// wakeup, with a ticker backstop for dropped (lossy) wakeups. Last-Event-ID
// (or ?since=) resumes a reconnecting client from where it left off.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	id := core.RunID(r.PathValue("id"))
	if _, err := s.Store.GetRun(r.Context(), id); err != nil {
		if errors.Is(err, core.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "unknown run")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	since := parseSinceCursor(r)
	sub, unsub := s.Bus.Subscribe(64)
	defer unsub()

	// drain writes all events after `since`; returns false once run.done is sent.
	drain := func() bool {
		evs, err := s.Store.EventsSince(r.Context(), id, since)
		if err != nil {
			s.Log.Error("sse events read", "run", id, "err", err)
			return false
		}
		for _, e := range evs {
			// SSE is text/event-stream, not HTML; data is server-generated event
			// JSON on a loopback trust boundary — Fprintf here is intentional.
			data, _ := json.Marshal(e)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Kind, data)
			since = e.Seq
			if e.Kind == event.RunDone {
				flusher.Flush()
				return false
			}
		}
		flusher.Flush()
		return true
	}

	if !drain() {
		return
	}
	tick := time.NewTicker(time.Second) // backstop for dropped wakeups
	defer tick.Stop()
	for {
		select {
		case <-sub: // some event happened (maybe for another run) — re-query
			if !drain() {
				return
			}
		case <-tick.C:
			if !drain() {
				return
			}
		case <-r.Context().Done(): // client disconnected
			return
		}
	}
}

func parseSinceCursor(r *http.Request) int64 {
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		if v, err := strconv.ParseInt(h, 10, 64); err == nil {
			return v
		}
	}
	if q := r.URL.Query().Get("since"); q != "" {
		if v, err := strconv.ParseInt(q, 10, 64); err == nil {
			return v
		}
	}
	return 0
}
