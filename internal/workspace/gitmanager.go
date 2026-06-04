package workspace

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// GitManager gives each isolated step a real git worktree off a scratch per-run
// repo, and tears the worktrees down at run end. Shared steps use the base working
// tree. It is stateless aside from a per-run lock that serialises a run's git
// invocations (concurrent isolated steps must not race on the repo index).
type GitManager struct {
	Root  string
	Name  string // commit identity for the empty base commit; defaulted if empty
	Email string

	mu sync.Mutex
	// locks holds one mutex per run, serialising that run's git invocations. Entries
	// are intentionally NOT pruned at teardown: the per-run footprint is a few bytes,
	// and pruning would risk a late For racing a teardown-time delete.
	locks map[core.RunID]*sync.Mutex
}

func (m *GitManager) name() string {
	if m.Name != "" {
		return m.Name
	}
	return "magisterd"
}

func (m *GitManager) email() string {
	if m.Email != "" {
		return m.Email
	}
	return "magisterd@localhost"
}

func (m *GitManager) runLock(id core.RunID) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = make(map[core.RunID]*sync.Mutex)
	}
	l, ok := m.locks[id]
	if !ok {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	return l
}

func (m *GitManager) baseDir(id core.RunID) string { return filepath.Join(m.Root, string(id), "base") }
func (m *GitManager) wtDir(id core.RunID) string   { return filepath.Join(m.Root, string(id), "wt") }

// run executes git in dir and returns combined output. Args are orchestrator-
// controlled (run/step IDs, fixed subcommands); no shell is involved.
func (m *GitManager) run(dir string, args ...string) ([]byte, error) {
	// #nosec G204 -- git with orchestrator-supplied args (validated run/step IDs),
	// invoked without a shell. This is the intended capability, not user input.
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(out))
	}
	return out, nil
}

// ensureRepo lazily inits the per-run base repo with one empty commit. Idempotent,
// and self-healing: a crash between `git init` and the first commit leaves an unborn
// HEAD, which would wedge every later `worktree add ... HEAD`; re-issuing the empty
// commit heals it. The guard on rev-parse HEAD avoids stacking commits on a healthy repo.
func (m *GitManager) ensureRepo(base string) error {
	if _, err := os.Stat(filepath.Join(base, ".git")); err != nil {
		if err := os.MkdirAll(base, 0o755); err != nil {
			return err
		}
		if _, err := m.run(base, "init"); err != nil {
			return err
		}
	}
	if _, err := m.run(base, "rev-parse", "--verify", "-q", "HEAD"); err == nil {
		return nil // base commit already present
	}
	if _, err := m.run(base,
		"-c", "user.name="+m.name(), "-c", "user.email="+m.email(),
		"commit", "--allow-empty", "-m", "base"); err != nil {
		return err
	}
	return nil
}

// freshWorktree (re-)creates a clean worktree at wt on branch step/<stepID>. Any
// stale worktree/branch (e.g. left by a crashed run) is removed first, so this is
// safe to call on resume.
func (m *GitManager) freshWorktree(base, wt, stepID string) error {
	branch := "step/" + stepID
	if _, err := os.Stat(wt); err == nil {
		_, _ = m.run(base, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(wt) // clear the path even if it wasn't a registered worktree
	}
	_, _ = m.run(base, "worktree", "prune")
	_, _ = m.run(base, "branch", "-D", branch) // best-effort; no-op if absent
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return err
	}
	_, err := m.run(base, "worktree", "add", wt, "-b", branch, "HEAD")
	return err
}

func (m *GitManager) For(runID core.RunID, s *flow.Step) (string, func() error, error) {
	lock := m.runLock(runID)
	lock.Lock()
	defer lock.Unlock()

	base := m.baseDir(runID)
	if err := m.ensureRepo(base); err != nil {
		return "", nil, err
	}
	noop := func() error { return nil } // worktrees outlive the step; TeardownRun reclaims them

	if s.Workspace != flow.WSIsolated {
		return base, noop, nil
	}
	wt := filepath.Join(m.wtDir(runID), s.ID)
	if err := m.freshWorktree(base, wt, s.ID); err != nil {
		return "", nil, err
	}
	return wt, noop, nil
}

// TeardownRun removes the run's isolated worktrees (the base repo persists). Best-
// effort and idempotent: a no-op if the run never set up a repo.
func (m *GitManager) TeardownRun(runID core.RunID) error {
	lock := m.runLock(runID)
	lock.Lock()
	defer lock.Unlock()

	base := m.baseDir(runID)
	if _, err := os.Stat(filepath.Join(base, ".git")); err != nil {
		return nil // never started
	}
	entries, err := os.ReadDir(m.wtDir(runID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no isolated steps
		}
		return err
	}
	var firstErr error
	for _, e := range entries {
		wt := filepath.Join(m.wtDir(runID), e.Name())
		if _, err := m.run(base, "worktree", "remove", "--force", wt); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	_, _ = m.run(base, "worktree", "prune")
	return firstErr
}
