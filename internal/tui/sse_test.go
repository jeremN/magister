package tui

import (
	"context"
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
