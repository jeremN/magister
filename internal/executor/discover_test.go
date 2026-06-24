package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initGitRepo creates a temp git repo with an empty base commit and returns its path.
func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-m", "base"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestDiscoverGitListsChangedFiles(t *testing.T) {
	dir := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	arts, err := discoverGit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || arts[0].Path != filepath.Join(dir, "out.txt") {
		t.Fatalf("discoverGit = %+v, want one out.txt artifact (abs path)", arts)
	}
}

func TestDiscoverGitEmptyWhenClean(t *testing.T) {
	dir := initGitRepo(t)
	arts, err := discoverGit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 0 {
		t.Fatalf("clean tree should yield no artifacts, got %+v", arts)
	}
}
