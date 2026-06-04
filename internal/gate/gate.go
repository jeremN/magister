package gate

import (
	"context"
	"fmt"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Evaluator resolves the gate after a step and returns whether the flow may
// proceed. It does not itself apply retry/abort — the engine owns that policy.
type Evaluator struct {
	Approver Approver
	Verifier Verifier
}

// Escalate turns a failed AUTO gate into a human approval, reusing the same Approver
// path as a manual gate (the engine calls this when on_fail=escalate and the attempt
// budget is spent). approve → the step's existing result stands; reject → the run aborts.
func (e *Evaluator) Escalate(ctx context.Context, runID core.RunID, s *flow.Step, res core.Result) (bool, error) {
	return e.Approver.Approve(ctx, runID, s, res)
}

func (e *Evaluator) Evaluate(ctx context.Context, runID core.RunID, s *flow.Step, res core.Result, workDir string) (bool, error) {
	switch s.Gate.Policy {
	case "", flow.GateManual, flow.GateConditional:
		// M1: conditional falls back to manual approval (parity with the phase-1
		// prototype). The expr-lang evaluator arrives in M5.
		return e.Approver.Approve(ctx, runID, s, res)
	case flow.GateAuto:
		ok, err := e.Verifier.Verify(ctx, s.Gate.Verifier.Command, workDir)
		if err != nil {
			return false, fmt.Errorf("verifier error: %w", err)
		}
		return ok, nil
	default:
		return false, fmt.Errorf("step %q: unknown gate policy %q", s.ID, s.Gate.Policy)
	}
}
