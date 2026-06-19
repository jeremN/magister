// Package workspace hands each step a working directory. M1 is filesystem-only:
// shared steps reuse the run root, isolated steps get their own subdir. Git
// worktrees (and real teardown) arrive in M4 behind this same interface.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Manager allocates working directories under Root.
type Manager struct {
	Root string
}

// TeardownRun is a no-op: the plain Manager allocates plain directories, which the
// caller's run dir cleanup (or the OS temp dir) reclaims. GitManager does real teardown.
func (m *Manager) TeardownRun(core.RunID) error { return nil }

// Commit is a no-op: the plain Manager has no git backing, so steps stay path-only.
func (m *Manager) Commit(core.RunID, *flow.Step, string) (string, string, error) {
	return "", "", nil
}

// Provision is a no-op: the plain Manager has no git backing, so there is no repo
// to clone. External-repo runs require the GitManager.
func (m *Manager) Provision(core.RunID, string, string) error { return nil }

// BasePath returns the run's directory. The plain Manager has no git backing, so
// this is informational; push only targets GitManager-backed external-repo runs.
func (m *Manager) BasePath(runID core.RunID) string {
	return filepath.Join(m.Root, string(runID))
}

// Reclaim removes the run's scratch directory. Mirrors GitManager.Reclaim with the
// same safety guard; the plain Manager allocates plain dirs under Root.
func (m *Manager) Reclaim(runID core.RunID) error {
	if !safeRunID(runID) {
		return fmt.Errorf("refusing to reclaim unsafe run id %q", runID)
	}
	return os.RemoveAll(filepath.Join(m.Root, string(runID)))
}

func (m *Manager) For(runID core.RunID, s *flow.Step) (string, func() error, error) {
	dir := filepath.Join(m.Root, string(runID))
	if s.Workspace == flow.WSIsolated {
		dir = filepath.Join(dir, s.ID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	noop := func() error { return nil } // M4 replaces this with worktree teardown
	return dir, noop, nil
}
