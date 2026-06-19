package workspace_test

import (
	"os"
	"path/filepath"
	"testing"

	"concentus/internal/core"
	"concentus/internal/workspace"
)

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func TestGitManagerReclaimRemovesRunScratch(t *testing.T) {
	root := t.TempDir()
	m := &workspace.GitManager{Root: root}
	runDir := filepath.Join(root, "run1")
	mkdirAll(t, filepath.Join(runDir, "base"))
	mkdirAll(t, filepath.Join(runDir, "wt", "stepA"))

	if err := m.Reclaim("run1"); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("run dir still present: %v", err)
	}
	// idempotent: a second reclaim of a missing dir is not an error
	if err := m.Reclaim("run1"); err != nil {
		t.Errorf("second Reclaim: %v", err)
	}
}

func TestGitManagerReclaimRejectsUnsafeID(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, "keep")
	mkdirAll(t, sentinel)
	m := &workspace.GitManager{Root: root}

	for _, id := range []core.RunID{"", ".", "..", "a/b", "../keep"} {
		if err := m.Reclaim(id); err == nil {
			t.Errorf("Reclaim(%q) = nil, want error", id)
		}
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("sentinel under root was disturbed: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("root removed: %v", err)
	}
}

func TestManagerReclaimRemovesRunDir(t *testing.T) {
	root := t.TempDir()
	m := &workspace.Manager{Root: root}
	mkdirAll(t, filepath.Join(root, "run9", "stepX"))
	if err := m.Reclaim("run9"); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "run9")); !os.IsNotExist(err) {
		t.Errorf("run dir still present")
	}
	if err := m.Reclaim(".."); err == nil {
		t.Errorf("Reclaim(\"..\") = nil, want error")
	}
}
