// Package gate resolves the checkpoint after each step. The key design point:
// interactive vs autonomous mode is not a branch in the engine — it is which
// Approver implementation is injected here.
package gate

import (
	"context"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Approver resolves a manual gate. The service (M3) supplies an Approver backed
// by the API approval registry; AutoApprover backs the keyless demo and tests.
type Approver interface {
	Approve(ctx context.Context, step *flow.Step, res core.Result) (bool, error)
}

// AutoApprover passes every manual gate.
type AutoApprover struct{}

func (AutoApprover) Approve(context.Context, *flow.Step, core.Result) (bool, error) {
	return true, nil
}
