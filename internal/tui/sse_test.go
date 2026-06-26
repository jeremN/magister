package tui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"concentus/internal/event"
)

func TestParseEventsFrames(t *testing.T) {
	stream := "id: 1\nevent: step.started\ndata: {\"seq\":1,\"run\":\"a1\",\"step\":\"plan\",\"kind\":\"step.started\"}\n\n" +
		"id: 2\nevent: gate.awaiting\ndata: {\"seq\":2,\"run\":\"a1\",\"step\":\"plan\",\"kind\":\"gate.awaiting\"}\n\n"
	var got []event.Event
	if err := parseEvents(strings.NewReader(stream), func(e event.Event) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 1 || got[1].Kind != "gate.awaiting" || got[1].StepID != "plan" {
		t.Fatalf("got %+v", got)
	}
}

func TestStreamEventsSendsLastEventIDAndAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/a1/events" {
			t.Fatalf("path %s", r.URL.Path)
		}
		if r.Header.Get("Last-Event-ID") != "5" {
			t.Fatalf("Last-Event-ID = %q", r.Header.Get("Last-Event-ID"))
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Fatalf("auth = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte("id: 6\nevent: run.done\ndata: {\"seq\":6,\"run\":\"a1\",\"kind\":\"run.done\"}\n\n"))
	}))
	defer srv.Close()
	var got []event.Event
	err := NewClient(srv.URL, "tok").StreamEvents(context.Background(), "a1", 5, func(e event.Event) { got = append(got, e) })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "run.done" {
		t.Fatalf("got %+v", got)
	}
}

func TestStreamEventsNon2xxReturnsTypedErrorWithoutEmitting(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"nope"}`, code)
		}))
		var emitted int
		err := NewClient(srv.URL, "").StreamEvents(context.Background(), "a1", 0, func(event.Event) { emitted++ })
		srv.Close()

		var se *streamStatusError
		if !errors.As(err, &se) {
			t.Fatalf("code %d: err = %v, want *streamStatusError", code, err)
		}
		if se.Status != code {
			t.Fatalf("Status = %d, want %d", se.Status, code)
		}
		if emitted != 0 {
			t.Fatalf("emit called %d times on a non-2xx, want 0", emitted)
		}
	}
}
