package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"concentus/internal/core"
)

// ReclaimError carries an HTTP status so the API layer maps failures without
// string-matching (mirrors PushError/RetryError).
type ReclaimError struct {
	Status int
	Msg    string
}

func (e *ReclaimError) Error() string { return e.Msg }

func reclaimErr(status int, format string, a ...any) *ReclaimError {
	return &ReclaimError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// reclaimOne removes a single run's scratch and, on success, marks it reclaimed so
// the store never selects it again. "Success" means the workspace returned no error
// — whether it deleted the directory (removed==true) or found it already gone
// (removed==false); both mean the scratch is gone, so both should stop future
// selection. A reclaim error leaves the run unmarked so the next sweep retries. A
// MarkReclaimed error is non-fatal (the dir is already gone) — logged, not returned.
// Shared by SweepScratch and ReclaimRun so the mark-on-success rule lives in one
// place.
func (s *Supervisor) reclaimOne(ctx context.Context, id core.RunID) (bool, error) {
	removed, err := s.engine.ReclaimScratch(id)
	if err != nil {
		return false, err
	}
	if merr := s.store.MarkReclaimed(ctx, id); merr != nil {
		s.logger().Error("mark reclaimed", "run", id, "err", merr)
	}
	return removed, nil
}

// SweepScratch reclaims the scratch directory of every terminal, not-yet-reclaimed
// run whose last update is before olderThan. It is best-effort: a single run's
// reclaim failure is logged and the sweep continues. Returns the number of runs
// whose scratch was ACTUALLY removed. Each reclaimed run is marked, so subsequent
// sweeps no longer select it — steady state queries zero rows. A non-nil error means
// the store query failed (nothing was swept). The caller supplies the cutoff (e.g.
// time.Now().Add(-ttl)) so the sweep needs no clock of its own.
func (s *Supervisor) SweepScratch(ctx context.Context, olderThan time.Time) (int, error) {
	ids, err := s.store.ReclaimableRuns(ctx, olderThan)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		removed, err := s.reclaimOne(ctx, id)
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

// ReclaimRun reclaims a single run's scratch on demand (cm rm), independent of any
// TTL. Guards mirror Retry/Push: an active run (still in s.runs — running or being
// retried) → 409; an unknown run → 404; a non-terminal persisted run → 409. On
// success it returns whether a directory was actually removed (false when the
// scratch was already gone — idempotent). Errors are *ReclaimError with an HTTP
// status.
//
// The active-check is a point-in-time read of s.runs, not a reservation: a Retry
// that registers the run AFTER this check could have its fresh scratch removed
// mid-resume. That window is the same at-least-once edge the background janitor
// already carries (it selects a terminal run that is then retried); the reverse
// order is caught by Retry's own scratch pre-flight. Accepted and recoverable
// (resubmit). A run reclaimed in that window stays marked reclaimed even if a
// later Retry re-provisions a fresh scratch, so it drops out of the background
// TTL sweep; cm rm still reclaims it explicitly.
func (s *Supervisor) ReclaimRun(ctx context.Context, runID core.RunID) (bool, error) {
	s.mu.Lock()
	_, active := s.runs[runID]
	s.mu.Unlock()
	if active {
		return false, reclaimErr(http.StatusConflict, "run %q in progress; cannot reclaim its scratch", runID)
	}
	rs, err := s.store.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, core.ErrRunNotFound) {
			return false, reclaimErr(http.StatusNotFound, "unknown run %q", runID)
		}
		return false, reclaimErr(http.StatusInternalServerError, "load run %q: %v", runID, err)
	}
	switch rs.Status {
	case core.RunSucceeded, core.RunFailed, core.RunCanceled:
		// terminal — reclaimable
	default:
		return false, reclaimErr(http.StatusConflict, "run %q is %s, not terminal", runID, rs.Status)
	}
	removed, err := s.reclaimOne(ctx, runID)
	if err != nil {
		return false, reclaimErr(http.StatusInternalServerError, "reclaim scratch: %v", err)
	}
	return removed, nil
}
