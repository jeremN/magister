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
