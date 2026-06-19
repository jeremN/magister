package supervisor

import (
	"context"

	"concentus/internal/core"
)

// ShipOpts is the union of PushOpts and PROpts. Shared fields (Remote/As/Step) feed
// both operations, so the push destination and the PR head branch can never disagree.
// Force is push-only; Base/Title/Body/Draft are pr-only.
type ShipOpts struct {
	Remote, As, Step, Base, Title, Body string
	Draft, Force                        bool
}

// ShipResult bundles the push outcome, the PR outcome, and whether the PR already
// existed (idempotent re-run).
type ShipResult struct {
	Push      PushResult
	PR        PRResult
	PRExisted bool
}

// Ship pushes a succeeded external-repo run's result branch, then ensures a PR exists
// for it. Push runs first (it needs the scratch clone); an already-open PR is success
// (PRExisted=true), so ship is safe to re-run and converges. On failure it returns the
// underlying *PushError (push half) or *PRError (pr half), which the API maps via
// errors.As. Post-run and store-driven; the engine is untouched.
func (s *Supervisor) Ship(ctx context.Context, runID core.RunID, opts ShipOpts) (ShipResult, error) {
	pushRes, err := s.Push(ctx, runID, PushOpts{
		Remote: opts.Remote, As: opts.As, Step: opts.Step, Force: opts.Force,
	})
	if err != nil {
		return ShipResult{}, err // *PushError; no PR attempted
	}
	prRes, existed, err := s.prCore(ctx, runID, PROpts{
		Remote: opts.Remote, As: opts.As, Step: opts.Step, Base: opts.Base,
		Title: opts.Title, Body: opts.Body, Draft: opts.Draft,
	})
	if err != nil {
		return ShipResult{}, err // *PRError; the push already happened
	}
	return ShipResult{Push: pushRes, PR: prRes, PRExisted: existed}, nil
}
