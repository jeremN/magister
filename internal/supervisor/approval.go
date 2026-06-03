// Package supervisor owns the daemon's active runs, the pending-approval
// registry for blocking manual gates, and drives engine.Run/Resume.
package supervisor

import (
	"sync"

	"concentus/internal/core"
)

// Decision is the outcome of a human gate approval.
type Decision struct {
	Approved bool
	Reason   string
}

// ApprovalRegistry tracks manual gates blocked awaiting a human decision,
// keyed by (run, step). The blocking approver Awaits; the API Resolves.
type ApprovalRegistry struct {
	mu      sync.Mutex
	pending map[string]chan Decision
}

func NewApprovalRegistry() *ApprovalRegistry {
	return &ApprovalRegistry{pending: make(map[string]chan Decision)}
}

func approvalKey(runID core.RunID, stepID string) string {
	return string(runID) + "\x00" + stepID
}

// Await registers a pending approval and returns the channel its decision will
// arrive on (buffered, so Resolve never blocks). One waiter per (run,step).
func (r *ApprovalRegistry) Await(runID core.RunID, stepID string) <-chan Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan Decision, 1)
	r.pending[approvalKey(runID, stepID)] = ch
	return ch
}

// Resolve delivers a decision to a waiting gate. Returns false if no gate is
// awaiting (unknown run/step, or already resolved/canceled).
func (r *ApprovalRegistry) Resolve(runID core.RunID, stepID string, d Decision) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := approvalKey(runID, stepID)
	ch, ok := r.pending[k]
	if !ok {
		return false
	}
	delete(r.pending, k)
	ch <- d
	return true
}

// Cancel drops a pending approval without resolving it (run canceled/shutdown).
func (r *ApprovalRegistry) Cancel(runID core.RunID, stepID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, approvalKey(runID, stepID))
}
