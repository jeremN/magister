package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"concentus/internal/core"
)

func names(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name()
	}
	return out
}

func TestSubmitRunIsChildOfServerSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	hs, _, st := testServer(t)

	resp, err := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString(oneStepFlow))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v1/runs = %d: %s", resp.StatusCode, b)
	}
	var rr runResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatal(err)
	}
	if rr.ID == "" {
		t.Fatal("no run ID returned")
	}

	waitForStatus(t, st, rr.ID, core.RunSucceeded)

	// Force-flush so all spans are recorded before we inspect them.
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	spans := sr.Ended()
	var server, run sdktrace.ReadOnlySpan
	for _, s := range spans {
		switch s.Name() {
		case "POST /v1/runs":
			server = s
		case "run " + string(rr.ID):
			run = s
		}
	}
	if server == nil || run == nil {
		t.Fatalf("missing spans; got %v", names(spans))
	}
	if run.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Errorf("run parent = %v, want submit server span %v", run.Parent().SpanID(), server.SpanContext().SpanID())
	}
}
