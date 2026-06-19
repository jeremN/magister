package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/executor"
	"concentus/internal/flow"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/metrics"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

func TestEngineRecordsMetrics(t *testing.T) {
	st := store.NewMem()
	m := metrics.New("test")
	eng := &Engine{
		Execs:   map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:      &workspace.Manager{Root: t.TempDir()},
		Gate:    &gate.Evaluator{Verifier: gate.CommandVerifier{}}, // auto gate needs no approver
		Joins:   join.Default(),
		Store:   st,
		Bus:     event.NewBus(),
		Clock:   core.SystemClock{},
		Metrics: m,
	}
	yaml := "name: f\nsteps:\n  - id: a\n    agent: mock\n    gate: { policy: auto, verifier: { command: \"true\" } }\n"
	f, err := flow.ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rid := core.RunID("r1")
	if err := st.CreateRun(context.Background(), core.RunState{ID: rid, Name: "f", FlowYAML: yaml}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := eng.Run(context.Background(), rid, f); err != nil {
		t.Fatalf("run: %v", err)
	}
	var buf bytes.Buffer
	m.WriteProm(&buf)
	out := buf.String()
	for _, want := range []string{
		`magister_runs_total{status="succeeded"} 1`,
		`magister_steps_total{status="succeeded"} 1`,
		"magister_run_duration_seconds_count 1",
		"magister_step_duration_seconds_count 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q\n---\n%s", want, out)
		}
	}
}
