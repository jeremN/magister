package core

import (
	"context"
	"time"

	"concentus/internal/event"
	"concentus/internal/flow"
)

// Artifact points at something a step produced on disk. The filesystem is the
// source of truth for handoffs; artifacts are just pointers. For a committed
// isolated step, Branch/Commit also name the git ref that carries the work, so
// fan-in joins can `git merge` it. Branch/Commit are empty for shared steps and
// the mock executor (path-only).
type Artifact struct {
	StepID string
	Path   string
	Branch string
	Commit string
}

// Task is what the engine hands an executor for one step attempt.
type Task struct {
	RunID   RunID
	StepID  string
	Role    string
	Prompt  string
	Inputs  []Artifact
	WorkDir string
	// Feedback is non-empty on a retry after an auto-gate verifier failed: the
	// previous attempt's captured verifier output (tail-capped). Executors
	// incorporate it into the model prompt so the agent can fix the specific
	// failure; empty on the first attempt. Mock ignores it.
	Feedback string
	// Emit publishes a mid-step milestone event (e.g. agent.tool). The engine binds
	// it to persist-then-publish; nil for callers that don't stream (e.g. Mock, or a
	// non-engine test) — executors must nil-check or use a no-op wrapper.
	Emit func(ev event.Event)
}

// Result is what an executor returns for one step.
type Result struct {
	StepID    string
	Summary   string
	Artifacts []Artifact
	CostUSD   float64
}

// Executor runs one step's work. This is the seam a future non-CLI executor
// slots into.
type Executor interface {
	Run(ctx context.Context, t Task) (Result, error)
}

// Workspace hands a step a working directory and a cleanup func, and tears down a
// run's isolated worktrees when the run ends.
type Workspace interface {
	For(runID RunID, s *flow.Step) (dir string, cleanup func() error, err error)
	// Commit records the step's worktree as a commit on its branch and returns the
	// branch name and commit sha. A no-op (returns "", "", nil) for workspaces with
	// no git backing (the plain Manager) and acceptable to call for any step; the
	// engine only calls it for committed isolated steps.
	Commit(runID RunID, s *flow.Step, workDir string) (branch, commit string, err error)
	// Provision records the run's source repo + pinned base commit SHA before any
	// step runs. An empty repo selects the synthetic empty-base scratch repo
	// (default; today's behavior). A no-op for the plain Manager (no git backing).
	Provision(runID RunID, repo, base string) error
	// BasePath returns the on-disk path of a run's per-run base repo (for an
	// external-repo run, the scratch clone). Safe to call any time; the path may not
	// exist yet. Post-run delivery (push) reads the result branch from here.
	BasePath(runID RunID) string
	// TeardownRun removes the run's isolated worktrees (the base repo persists). It
	// is best-effort, idempotent, and a no-op for a run with no worktrees.
	TeardownRun(runID RunID) error
	// Reclaim removes the run's entire scratch directory (base repo + worktrees) and
	// reports whether a directory was actually removed. Best-effort and idempotent: a
	// missing directory returns (false, nil). The scratch janitor calls it once a run
	// is terminal and past its retention TTL; the removed bool lets a store-driven
	// sweep count (and log) only real reclaims, not re-selections of already-cleaned runs.
	Reclaim(runID RunID) (removed bool, err error)
}

// Publisher receives engine events for live observers. Lossy by contract.
type Publisher interface {
	Publish(e event.Event)
}

// Clock is injected so retry/backoff/timeout logic is testable without sleeping.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// SystemClock is the real Clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time                         { return time.Now() }
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
