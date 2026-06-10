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

func TestEscalateUsesApprover(t *testing.T) {
	s := &flow.Step{ID: "a", Gate: flow.Gate{
		Policy: flow.GateAuto, Verifier: &flow.Verifier{Command: "false"}, OnFail: flow.FailEscalate}}

	approved := &Evaluator{Approver: fixedApprover(true), Verifier: CommandVerifier{}}
	if ok, err := approved.Escalate(context.Background(), "r1", s, core.Result{}); err != nil || !ok {
		t.Fatalf("approve path: ok=%v err=%v, want true/nil", ok, err)
	}
	rejected := &Evaluator{Approver: fixedApprover(false), Verifier: CommandVerifier{}}
	if ok, _ := rejected.Escalate(context.Background(), "r1", s, core.Result{}); ok {
		t.Fatal("reject path: ok=true, want false")
	}
}

// failIfCalledApprover fails the test if a conditional gate consults a human approver.
type failIfCalledApprover struct{ t *testing.T }

func (a failIfCalledApprover) Approve(context.Context, core.RunID, *flow.Step, core.Result) (bool, error) {
	a.t.Fatal("conditional gate must not call the Approver")
	return false, nil
}

func condStep(t *testing.T, expr string) *flow.Step {
	t.Helper()
	c := &flow.Condition{Expr: expr}
	if err := c.Compile(); err != nil {
		t.Fatalf("compile %q: %v", expr, err)
	}
	return &flow.Step{ID: "a", Gate: flow.Gate{Policy: flow.GateConditional, Condition: c}}
}

func TestConditionalGateTruePassesWithoutApprover(t *testing.T) {
	e := &Evaluator{Approver: failIfCalledApprover{t}, Verifier: CommandVerifier{}}
	s := condStep(t, "result.cost_usd < 1.0")
	ok, err := e.Evaluate(context.Background(), "r1", s, core.Result{CostUSD: 0.5}, t.TempDir())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want true/nil", ok, err)
	}
}

func TestConditionalGateFalseFails(t *testing.T) {
	e := &Evaluator{Approver: failIfCalledApprover{t}, Verifier: CommandVerifier{}}
	s := condStep(t, "result.cost_usd < 1.0")
	ok, err := e.Evaluate(context.Background(), "r1", s, core.Result{CostUSD: 2.0}, t.TempDir())
	if err != nil {
		t.Fatalf("a false condition should be a result, not an error: %v", err)
	}
	if ok {
		t.Fatal("gate should have failed (cost 2.0 is not < 1.0)")
	}
}

// TestConditionalGateMapsResultFields verifies that core.Result.Artifacts (via
// artifactPaths, which maps each Artifact.Path) and core.Result.StepID are
// correctly wired into flow.GateEnv when Evaluate builds the conditional env.
func TestConditionalGateMapsResultFields(t *testing.T) {
	e := &Evaluator{Approver: failIfCalledApprover{t}, Verifier: CommandVerifier{}}
	s := condStep(t, `result.step_id == "a" && len(result.artifacts) == 2 && result.artifacts[0] == "x"`)
	res := core.Result{
		StepID:    "a",
		Artifacts: []core.Artifact{{Path: "x"}, {Path: "y"}},
	}
	ok, err := e.Evaluate(context.Background(), "r1", s, res, t.TempDir())
	if err != nil {
		t.Fatalf("artifacts/step_id field mapping: unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("artifacts/step_id field mapping: gate returned false; check artifactPaths maps .Path and StepID is forwarded")
	}
}
