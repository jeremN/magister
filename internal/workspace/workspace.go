// Package workspace hands each step a working directory. M1 is filesystem-only:
// shared steps reuse the run root, isolated steps get their own subdir. Git
// worktrees (and real teardown) arrive in M4 behind this same interface.
package workspace

import (
	"os"
	"path/filepath"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Manager allocates working directories under Root.
type Manager struct {
	Root string
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
