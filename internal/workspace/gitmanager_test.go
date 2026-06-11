package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGitManagerSharedUsesBaseTree(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	d1, _, err := m.For("r1", &flow.Step{ID: "a", Workspace: flow.WSShared})
	if err != nil {
		t.Fatal(err)
	}
	d2, _, err := m.For("r1", &flow.Step{ID: "b", Workspace: flow.WSShared})
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Errorf("shared steps should share the base tree: %q vs %q", d1, d2)
	}
	info, err := os.Stat(filepath.Join(d1, ".git"))
	if err != nil || !info.IsDir() {
		t.Errorf("shared dir should be the base repo (.git dir), got err=%v", err)
	}
}

func TestGitManagerIsolatedGetsWorktree(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	m := &GitManager{Root: root}
	dir, _, err := m.For("r1", &flow.Step{ID: "step-a", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(root, "r1", "wt", "step-a") {
		t.Errorf("unexpected worktree dir: %q", dir)
	}
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil || info.IsDir() {
		t.Errorf("isolated dir should be a linked worktree (.git file), got err=%v", err)
	}
	if br := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); br != "step/step-a" {
		t.Errorf("worktree branch = %q, want step/step-a", br)
	}
}

func TestGitManagerTeardownRemovesWorktrees(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	m := &GitManager{Root: root}
	dir, _, err := m.For("r1", &flow.Step{ID: "a", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.TeardownRun("r1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("worktree should be removed after teardown, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "r1", "base", ".git")); err != nil {
		t.Errorf("base repo should persist after teardown: %v", err)
	}
}

func TestGitManagerForIsResumeIdempotent(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	step := &flow.Step{ID: "a", Workspace: flow.WSIsolated}
	if _, _, err := m.For("r1", step); err != nil {
		t.Fatalf("first For: %v", err)
	}
	dir, _, err := m.For("r1", step)
	if err != nil {
		t.Fatalf("second For (resume) should succeed, got %v", err)
	}
	if br := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); br != "step/a" {
		t.Errorf("re-created worktree branch = %q, want step/a", br)
	}
}

func TestGitManagerTeardownNoRepoIsNoop(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	if err := m.TeardownRun("never-started"); err != nil {
		t.Errorf("teardown of an unknown run should be a no-op, got %v", err)
	}
}

func TestGitManagerForHealsUnbornHead(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	base := filepath.Join(root, "r1", "base")
	// Simulate a crash after `git init` but before the base commit: .git exists, HEAD unborn.
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", base, "init").CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
	m := &GitManager{Root: root}
	dir, _, err := m.For("r1", &flow.Step{ID: "a", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatalf("For should heal an unborn HEAD, got %v", err)
	}
	if br := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); br != "step/a" {
		t.Errorf("branch = %q, want step/a", br)
	}
}

func TestGitManagerConcurrentIsolatedFor(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = m.For("r1", &flow.Step{ID: fmt.Sprintf("s%d", i), Workspace: flow.WSIsolated})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent For[%d]: %v", i, err)
		}
	}
}

func TestGitManagerTeardownRemovesAllWorktrees(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	m := &GitManager{Root: root}
	var dirs []string
	for _, id := range []string{"a", "b", "c"} {
		d, _, err := m.For("r1", &flow.Step{ID: id, Workspace: flow.WSIsolated})
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, d)
	}
	if err := m.TeardownRun("r1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	for _, d := range dirs {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("worktree %s should be removed, stat err=%v", d, err)
		}
	}
}

func TestGitManagerCommitRecordsWork(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	dir, _, err := m.For("r1", &flow.Step{ID: "a", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	branch, commit, err := m.Commit("r1", &flow.Step{ID: "a", Workspace: flow.WSIsolated}, dir)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if branch != "step/a" {
		t.Errorf("branch = %q, want step/a", branch)
	}
	if commit == "" || commit != gitOut(t, dir, "rev-parse", "HEAD") {
		t.Errorf("commit sha = %q, want HEAD", commit)
	}
	if status := gitOut(t, dir, "status", "--porcelain"); status != "" {
		t.Errorf("worktree should be clean after commit, got %q", status)
	}
}

func TestGitManagerCommitAllowsEmpty(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	dir, _, err := m.For("r1", &flow.Step{ID: "a", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatal(err)
	}
	if _, commit, err := m.Commit("r1", &flow.Step{ID: "a", Workspace: flow.WSIsolated}, dir); err != nil || commit == "" {
		t.Fatalf("commit of a no-file step should still produce a commit, got commit=%q err=%v", commit, err)
	}
}

var _ core.Workspace = (*GitManager)(nil)
