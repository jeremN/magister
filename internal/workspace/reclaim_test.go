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

	removed, err := m.Reclaim("run1")
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if !removed {
		t.Errorf("Reclaim removed=false, want true (a populated scratch was deleted)")
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("run dir still present: %v", err)
	}
	// idempotent: a second reclaim of a missing dir is not an error and removes nothing
	removed, err = m.Reclaim("run1")
	if err != nil {
		t.Errorf("second Reclaim: %v", err)
	}
	if removed {
		t.Errorf("second Reclaim removed=true, want false (dir already gone)")
	}
}

func TestGitManagerReclaimRejectsUnsafeID(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, "keep")
	mkdirAll(t, sentinel)
	m := &workspace.GitManager{Root: root}

	for _, id := range []core.RunID{"", ".", "..", "a/b", "../keep"} {
		if removed, err := m.Reclaim(id); err == nil || removed {
			t.Errorf("Reclaim(%q) = (%v, %v), want (false, error)", id, removed, err)
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
	removed, err := m.Reclaim("run9")
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if !removed {
		t.Errorf("Reclaim removed=false, want true")
	}
	if _, err := os.Stat(filepath.Join(root, "run9")); !os.IsNotExist(err) {
		t.Errorf("run dir still present")
	}
	// idempotent: a second reclaim of the now-missing dir removes nothing
	if removed, err := m.Reclaim("run9"); err != nil || removed {
		t.Errorf("second Reclaim(\"run9\") = (%v, %v), want (false, nil)", removed, err)
	}
	if removed, err := m.Reclaim(".."); err == nil || removed {
		t.Errorf("Reclaim(\"..\") = (%v, %v), want (false, error)", removed, err)
	}
}
