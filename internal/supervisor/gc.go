package supervisor

import (
	"context"
	"time"
)

// SweepScratch reclaims the scratch directory of every terminal run whose last
// update is before olderThan. It is best-effort: a single run's reclaim failure is
// logged and the sweep continues. Returns the number of runs whose scratch was
// ACTUALLY removed — a terminal run stays in the store and keeps being re-selected
// every sweep, but its now-missing directory removes nothing, so steady state counts
// 0. A non-nil error means the store query failed (nothing was swept). The caller
// supplies the cutoff (e.g. time.Now().Add(-ttl)) so the sweep needs no clock of its own.
func (s *Supervisor) SweepScratch(ctx context.Context, olderThan time.Time) (int, error) {
	ids, err := s.store.ReclaimableRuns(ctx, olderThan)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		removed, err := s.engine.ReclaimScratch(id)
		if err != nil {
			s.logger().Error("scratch reclaim", "run", id, "err", err)
			continue
		}
		if removed {
			n++
		}
	}
	return n, nil
}
