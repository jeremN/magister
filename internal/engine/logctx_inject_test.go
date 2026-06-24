package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"concentus/internal/core"
	"concentus/internal/logctx"
)

// ctxLogExec logs via the context logger so the test can assert the engine
// injected a run-scoped logger before invoking the agent.
type ctxLogExec struct{}

func (ctxLogExec) Run(ctx context.Context, t core.Task) (core.Result, error) {
	logctx.From(ctx).Info("agent ran")
	return core.Result{StepID: t.StepID, Summary: "ok"}, nil
}

func TestRunAgentInjectsScopedLogger(t *testing.T) {
	var buf bytes.Buffer
	// Metrics is left nil — runAgent's ObserveAgentRun/AddCost both nil-guard the
	// receiver; Bus/Store are unused because ctxLogExec never emits.
	e := &Engine{
		Execs: map[string]core.Executor{"mock": ctxLogExec{}},
		Log:   slog.New(slog.NewTextHandler(&buf, nil)),
		Clock: core.SystemClock{},
	}
	if _, err := e.runAgent(context.Background(), "run-1", "step-a", "impl", "mock", "prompt", t.TempDir(), 1, nil, ""); err != nil {
		t.Fatalf("runAgent: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"agent ran", "run=run-1", "step=step-a", "agent=mock"} {
		if !strings.Contains(out, want) {
			t.Errorf("agent log missing %q; got: %s", want, out)
		}
	}
}
