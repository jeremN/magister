package engine

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/executor"
	"concentus/internal/flow"
)

func TestEngineEmitsSpanTree(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	eng, st, bus := newEngine(t, map[string]core.Executor{"mock": executor.Mock{Name: "mock"}}, nil)
	f := &flow.Flow{Name: "feat", Steps: []*flow.Step{
		{ID: "greet", Agent: "mock", Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
	}}
	runID := core.RunID("r1")
	mustCreate(t, st, runID, f)
	ch, unsub := bus.Subscribe(64)

	if err := eng.Run(context.Background(), runID, f); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, _ := st.GetRun(context.Background(), runID)
	if got.Status != core.RunSucceeded {
		t.Fatalf("run status = %q, want succeeded", got.Status)
	}

	unsub()
	var sawStart, sawDone bool
	for ev := range ch {
		switch ev.Kind {
		case event.RunStarted:
			sawStart = true
		case event.RunDone:
			sawDone = true
		}
	}
	if !sawStart || !sawDone {
		t.Errorf("missing run bookends: start=%v done=%v", sawStart, sawDone)
	}

	spans := sr.Ended()
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}
	for _, want := range []string{"run " + string(runID), "step greet", "agent mock"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("missing span %q; got %v", want, names(spans))
		}
	}
	// nesting: step greet's parent is the run span; agent mock's parent is a step span.
	run := byName["run "+string(runID)]
	if p := byName["step greet"].Parent().SpanID(); p != run.SpanContext().SpanID() {
		t.Errorf("step greet parent = %v, want run span %v", p, run.SpanContext().SpanID())
	}
	if p := byName["agent mock"].Parent().SpanID(); p != byName["step greet"].SpanContext().SpanID() {
		t.Errorf("agent mock parent = %v, want step greet span %v", p, byName["step greet"].SpanContext().SpanID())
	}
}

func names(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name()
	}
	return out
}
