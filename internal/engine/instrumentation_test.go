package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/executor"
	"concentus/internal/flow"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

// debugLogger returns a slog logger writing DEBUG-and-above to buf, so a test
// can assert on the engine's Debug/Warn instrumentation.
func debugLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// hasLine reports whether some single line of out contains every substring in
// subs — a precise per-line check (avoids matching fields across separate lines).
func hasLine(out string, subs ...string) bool {
	for _, ln := range strings.Split(out, "\n") {
		all := true
		for _, s := range subs {
			if !strings.Contains(ln, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func TestRetryBackoffAndExhaustionLogs(t *testing.T) {
	var buf bytes.Buffer
	st := store.NewMem()
	eng := &Engine{
		Execs: map[string]core.Executor{"flaky": &flakyExecutor{failUntil: 99}}, // fails every attempt
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: gate.AutoApprover{}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st,
		Bus:   event.NewBus(),
		Clock: fakeClock{}, // makes backoff instant
		Log:   debugLogger(&buf),
	}
	f := &flow.Flow{Name: "retry", Steps: []*flow.Step{
		{ID: "a", Agent: "flaky", Retry: &flow.RetryPolicy{Max: 2, Backoff: flow.Duration(time.Second)},
			Gate: flow.Gate{Policy: flow.GateManual}},
	}}
	mustCreate(t, st, "r1", f)
	if err := eng.Run(context.Background(), "r1", f); err == nil {
		t.Fatal("expected the run to fail after exhausting retries")
	}
	out := buf.String()
	if !hasLine(out, "step backoff", "attempt=2", "delay=", "base=") {
		t.Errorf("missing/incomplete backoff Debug line:\n%s", out)
	}
	if !hasLine(out, "level=WARN", "retry budget exhausted", "attempts=2", "escalating=false") {
		t.Errorf("missing retry-budget-exhausted Warn line:\n%s", out)
	}
}

func TestNormalStepLogs(t *testing.T) {
	var buf bytes.Buffer
	eng, st, _ := newEngine(t, map[string]core.Executor{"mock": executor.Mock{Name: "mock"}}, nil)
	eng.Log = debugLogger(&buf)
	f := &flow.Flow{Name: "one", Steps: []*flow.Step{
		{ID: "greet", Agent: "mock", Role: "implementer",
			Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}},
	}}
	mustCreate(t, st, "r1", f)
	if err := eng.Run(context.Background(), "r1", f); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	if !hasLine(out, "agent starting", "agent=mock", "role=implementer", "attempt=1") {
		t.Errorf("missing agent-starting line:\n%s", out)
	}
	if !hasLine(out, "agent finished", "agent=mock", "dur=", "cost_usd=") {
		t.Errorf("missing agent-finished line:\n%s", out)
	}
	if !hasLine(out, "step slot acquired", "step=greet", "waited=") {
		t.Errorf("missing slot-acquired line:\n%s", out)
	}
	if !hasLine(out, "gate evaluated", "step=greet", "policy=auto", "pass=true") {
		t.Errorf("missing gate-evaluated line:\n%s", out)
	}
}
