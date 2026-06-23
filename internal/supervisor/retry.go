package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// RetryError carries an HTTP status so the API layer maps failures without
// string-matching (mirrors PushError/PRError).
type RetryError struct {
	Status int
	Msg    string
}

func (e *RetryError) Error() string { return e.Msg }

func retryErr(status int, format string, a ...any) *RetryError {
	return &RetryError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// Retry resumes a failed or canceled run in place: it keeps the run's own id and
// reuses its preserved scratch, skipping already-succeeded steps and re-running
// from the failed step onward (engine.Resume). The guard ordering is load-bearing:
// the run is flipped out of its terminal status (step 5) before the scratch is
// checked (step 6) so the scratch GC — which only selects terminal runs — cannot
// reclaim it mid-retry. Errors are *RetryError with an HTTP status.
func (s *Supervisor) Retry(ctx context.Context, runID core.RunID) (core.RunID, error) {
	// 1. reject if the run is still active (running or unwinding).
	s.mu.Lock()
	_, active := s.runs[runID]
	s.mu.Unlock()
	if active {
		return "", retryErr(http.StatusConflict, "run %q still in progress", runID)
	}
	// 2. load.
	rs, err := s.store.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, core.ErrRunNotFound) {
			return "", retryErr(http.StatusNotFound, "unknown run %q", runID)
		}
		return "", retryErr(http.StatusInternalServerError, "load run %q: %v", runID, err)
	}
	// 3. only failed/canceled runs are resumable.
	switch rs.Status {
	case core.RunFailed, core.RunCanceled:
		// resumable
	case core.RunSucceeded:
		return "", retryErr(http.StatusConflict, "run %q succeeded; nothing to retry", runID)
	default: // pending/running persisted but not in the active map
		return "", retryErr(http.StatusConflict, "run %q still in progress", runID)
	}
	// 4. re-parse + validate the stored flow (it validated at submit, so a failure
	//    here is corrupt persisted state).
	f, err := flow.ParseBytes([]byte(rs.FlowYAML))
	if err != nil {
		return "", retryErr(http.StatusInternalServerError, "stored flow no longer parses: %v", err)
	}
	if err := flow.Validate(f); err != nil {
		return "", retryErr(http.StatusInternalServerError, "stored flow no longer valid: %v", err)
	}
	// 5. flip out of the terminal state first so the scratch GC can't reclaim it
	//    between the check below and the resume.
	if err := s.store.SetRunStatus(ctx, runID, core.RunPending, ""); err != nil {
		return "", retryErr(http.StatusInternalServerError, "reset run status: %v", err)
	}
	// 6. pre-flight scratch presence (same check Push uses). If gone, fully revert
	//    the run to its original terminal status + error and reject.
	base := s.engine.BasePath(runID)
	if base == "" || !dirHasGit(base) {
		if err := s.store.SetRunStatus(ctx, runID, rs.Status, rs.Err); err != nil {
			s.logger().Error("retry: revert status after reclaimed scratch", "run", runID, "err", err)
		}
		return "", retryErr(http.StatusConflict, "scratch for run %q reclaimed; resubmit the flow", runID)
	}
	// 7. resume in place (reset non-succeeded steps, re-provision, start Resume).
	if err := s.resumeRun(ctx, rs, f); err != nil {
		if rerr := s.store.SetRunStatus(ctx, runID, rs.Status, rs.Err); rerr != nil {
			s.logger().Error("retry: revert status after provision failure", "run", runID, "err", rerr)
		}
		return "", retryErr(http.StatusInternalServerError, "%v", err)
	}
	return runID, nil
}
