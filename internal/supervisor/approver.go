package supervisor

import (
	"context"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// RegistryApprover is the API-backed gate.Approver: a manual gate blocks here
// until a human resolves it via the API (Resolve on the registry) or the run's
// context is canceled.
type RegistryApprover struct {
	Reg *ApprovalRegistry
}

func (a *RegistryApprover) Approve(ctx context.Context, runID core.RunID, step *flow.Step, _ core.Result) (bool, error) {
	ch := a.Reg.Await(runID, step.ID)
	select {
	case d := <-ch:
		return d.Approved, nil
	case <-ctx.Done():
		a.Reg.Cancel(runID, step.ID)
		return false, ctx.Err()
	}
}
