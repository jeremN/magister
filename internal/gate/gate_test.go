package gate

import (
	"context"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

func TestAutoGatePassesOnZeroExit(t *testing.T) {
	e := &Evaluator{Approver: AutoApprover{}, Verifier: CommandVerifier{}}
	s := &flow.Step{ID: "a", Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "true"}}}
	ok, err := e.Evaluate(context.Background(), "r1", s, core.Result{}, t.TempDir())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want true/nil", ok, err)
	}
}

func TestAutoGateFailsOnNonZeroExit(t *testing.T) {
	e := &Evaluator{Approver: AutoApprover{}, Verifier: CommandVerifier{}}
	s := &flow.Step{ID: "a", Gate: flow.Gate{Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "false"}}}
	ok, err := e.Evaluate(context.Background(), "r1", s, core.Result{}, t.TempDir())
	if err != nil {
		t.Fatalf("non-zero exit should be a result, not an error: %v", err)
	}
	if ok {
		t.Fatal("gate should have failed")
	}
}

func TestManualGateUsesApprover(t *testing.T) {
	e := &Evaluator{Approver: fixedApprover(false), Verifier: CommandVerifier{}}
	s := &flow.Step{ID: "a", Gate: flow.Gate{Policy: flow.GateManual}}
	ok, _ := e.Evaluate(context.Background(), "r1", s, core.Result{}, t.TempDir())
	if ok {
		t.Fatal("approver returned false; gate should fail")
	}
}

type fixedApprover bool

func (f fixedApprover) Approve(context.Context, core.RunID, *flow.Step, core.Result) (bool, error) {
	return bool(f), nil
}
