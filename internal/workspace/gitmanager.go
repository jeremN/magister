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
	// specs holds each run's provisioning request (set by Provision). Guarded by
	// mu, like locks. Absent/zero spec ⇒ synthetic empty base.
	specs map[core.RunID]repoSpec
}

// repoSpec is a run's external-repo provisioning request: the source repo to
// clone and the pinned base commit SHA to check out. The zero value (empty repo)
// selects the synthetic empty-base scratch repo.
type repoSpec struct {
	repo string
	base string
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

// Provision records a run's source repo + pinned base SHA before its first step.
// Empty repo ⇒ synthetic empty base. Re-provisioning a run overwrites its spec
// (last write wins) — intended, so a resume can re-record the same run's repo/base.
func (m *GitManager) Provision(runID core.RunID, repo, base string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.specs == nil {
		m.specs = make(map[core.RunID]repoSpec)
	}
	m.specs[runID] = repoSpec{repo: repo, base: base}
	return nil
}

func (m *GitManager) specFor(runID core.RunID) repoSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.specs[runID] // zero value (empty repo) if never provisioned
}

func (m *GitManager) baseDir(id core.RunID) string { return filepath.Join(m.Root, string(id), "base") }
func (m *GitManager) wtDir(id core.RunID) string   { return filepath.Join(m.Root, string(id), "wt") }

// BasePath exposes the per-run base repo path for post-run delivery (push).
func (m *GitManager) BasePath(runID core.RunID) string { return m.baseDir(runID) }

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

// ensureRepo lazily provisions the per-run base repo. With a spec.repo set it
// clones the source at the pinned base SHA; otherwise it inits an empty base
// with one empty commit. Idempotent and self-healing: a present .git is reused
// (resume), and a crash between `git init` and the first commit (unborn HEAD) is
// healed by re-issuing the empty commit. A clone is born with a real HEAD, so the
// heal applies only to the empty-base path.
func (m *GitManager) ensureRepo(baseDir string, spec repoSpec) error {
	if _, err := os.Stat(filepath.Join(baseDir, ".git")); err != nil {
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			return err
		}
		if spec.repo != "" {
			return m.cloneBase(baseDir, spec)
		}
		if _, err := m.run(baseDir, "init"); err != nil {
			return err
		}
		if _, err := m.run(baseDir, "config", "user.name", m.name()); err != nil {
			return err
		}
		if _, err := m.run(baseDir, "config", "user.email", m.email()); err != nil {
			return err
		}
	}
	if spec.repo != "" {
		return nil // a clone is born with a real HEAD; nothing to heal
	}
	if _, err := m.run(baseDir, "rev-parse", "--verify", "-q", "HEAD"); err == nil {
		return nil // base commit already present
	}
	if _, err := m.run(baseDir,
		"-c", "user.name="+m.name(), "-c", "user.email="+m.email(),
		"commit", "--allow-empty", "-m", "base"); err != nil {
		return err
	}
	return nil
}

// cloneBase clones the source repo into baseDir and detaches HEAD at the pinned
// base SHA, so step worktrees fork from that exact commit. The source is read
// only (clone only reads it). Identity is set after clone — a clone does not copy
// the source's local user.name/user.email, which merge commits need.
//
// Argv hardening (defense in depth — the API layer also validates at submit):
//   - base must be a hex commit SHA (it is always a pinned SHA), so it cannot be a
//     "-"-leading flag smuggled into `checkout`.
//   - `--` separates clone options from the <repo> <dir> positionals, so a
//     "-"-leading repo cannot smuggle a flag.
//   - protocol.ext.allow=never disables the ext:: transport (a remote-code vector
//     if repo were attacker-controlled); local file clones are unaffected.
func (m *GitManager) cloneBase(baseDir string, spec repoSpec) error {
	if !isHexSHA(spec.base) {
		return fmt.Errorf("invalid base commit %q: want a hex sha", spec.base)
	}
	if _, err := m.run("", "-c", "protocol.ext.allow=never", "clone", "--", spec.repo, baseDir); err != nil {
		return err
	}
	if _, err := m.run(baseDir, "checkout", "--detach", spec.base); err != nil {
		// A clone that can't reach its pinned base must not be left half-provisioned:
		// the .git dir would make a later ensureRepo treat it as ready and silently
		// fork from the wrong commit. Remove it so the next attempt re-clones.
		_ = os.RemoveAll(baseDir)
		return err
	}
	if _, err := m.run(baseDir, "config", "user.name", m.name()); err != nil {
		return err
	}
	if _, err := m.run(baseDir, "config", "user.email", m.email()); err != nil {
		return err
	}
	return nil
}

// isHexSHA reports whether s is a plausible git commit SHA: 7–64 hex digits. A hex
// string cannot begin with "-", which is what makes it safe as a positional git arg.
func isHexSHA(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
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
	if err := m.ensureRepo(base, m.specFor(runID)); err != nil {
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

// Commit stages and commits everything in the step's worktree onto its branch,
// returning the branch name and the new commit sha. --allow-empty so a step that
// wrote nothing still advances its branch deterministically. Serialised by the
// run lock, like For/TeardownRun. Identity comes from the repo config ensureRepo set.
func (m *GitManager) Commit(runID core.RunID, s *flow.Step, workDir string) (string, string, error) {
	lock := m.runLock(runID)
	lock.Lock()
	defer lock.Unlock()

	if _, err := m.run(workDir, "add", "-A"); err != nil {
		return "", "", err
	}
	if _, err := m.run(workDir, "commit", "--allow-empty", "-m", "step/"+s.ID); err != nil {
		return "", "", err
	}
	out, err := m.run(workDir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	return "step/" + s.ID, string(bytes.TrimSpace(out)), nil
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
