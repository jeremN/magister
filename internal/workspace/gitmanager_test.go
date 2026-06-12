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

// setupSourceRepo builds a committed fixture repo and returns its dir + HEAD sha.
func setupSourceRepo(t *testing.T) (string, string) {
	t.Helper()
	src := t.TempDir()
	gitOut(t, src, "init")
	gitOut(t, src, "config", "user.name", "fix")
	gitOut(t, src, "config", "user.email", "fix@example.com")
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("base content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, src, "add", "-A")
	gitOut(t, src, "commit", "-m", "base")
	return src, gitOut(t, src, "rev-parse", "HEAD")
}

func TestGitManagerProvisionClonesRealRepo(t *testing.T) {
	requireGit(t)
	src, sha := setupSourceRepo(t)

	m := &GitManager{Root: t.TempDir()}
	if err := m.Provision("r1", src, sha); err != nil {
		t.Fatalf("provision: %v", err)
	}
	wt, _, err := m.For("r1", &flow.Step{ID: "build", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "hello.txt"))
	if err != nil {
		t.Fatalf("base file missing in worktree (clone did not fork from base): %v", err)
	}
	if string(got) != "base content\n" {
		t.Errorf("base content = %q, want %q", got, "base content\n")
	}
	// The step branch forks from the cloned base, so its parent is the base sha.
	if parent := gitOut(t, wt, "rev-parse", "HEAD"); parent != sha {
		t.Errorf("worktree HEAD = %q, want pinned base %q", parent, sha)
	}
}

func TestIsHexSHA(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"abc123", false}, // too short (6)
		{"abc1234", true}, // min length (7)
		{"0123456789abcdef0123456789abcdef01234567", true},                           // full sha-1 (40)
		{"DEADBEEFdeadbeef", true},                                                   // mixed case
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true},   // 64 (sha-256)
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0", false}, // 65, too long
		{"--upload-pack=x", false},                                                   // flag-like
		{"main", false},                                                              // ref name
		{"ghijklm", false},                                                           // non-hex letters
	}
	for _, c := range cases {
		if got := isHexSHA(c.in); got != c.want {
			t.Errorf("isHexSHA(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestGitManagerProvisionRejectsFlaglikeBase(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t)
	m := &GitManager{Root: t.TempDir()}
	// A non-hex, "-"-leading base must be rejected before it reaches git, so it
	// cannot smuggle a flag (e.g. --upload-pack=...) into `checkout`.
	if err := m.Provision("r1", src, "--upload-pack=touch pwned"); err != nil {
		t.Fatalf("provision records the spec, should not error: %v", err)
	}
	if _, _, err := m.For("r1", &flow.Step{ID: "build", Workspace: flow.WSIsolated}); err == nil {
		t.Fatal("For should reject a non-hex/flag-like base")
	}
}

func TestGitManagerNoRepoUsesEmptyBase(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	// No Provision at all => synthetic empty base (today's behavior).
	wt, _, err := m.For("r1", &flow.Step{ID: "build", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected empty base, found a stray file (stat err=%v)", err)
	}
}

func TestGitManagerProvisionEmptyRepoUsesEmptyBase(t *testing.T) {
	requireGit(t)
	m := &GitManager{Root: t.TempDir()}
	if err := m.Provision("r1", "", ""); err != nil {
		t.Fatalf("provision empty: %v", err)
	}
	wt, _, err := m.For("r1", &flow.Step{ID: "build", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("empty repo should select the empty base, found a stray file (stat err=%v)", err)
	}
}

func TestGitManagerBasePath(t *testing.T) {
	root := t.TempDir()
	m := &GitManager{Root: root}
	got := m.BasePath("run-7")
	if got != filepath.Join(root, "run-7", "base") {
		t.Errorf("BasePath = %q, want %q", got, filepath.Join(root, "run-7", "base"))
	}
}

func TestManagerBasePath(t *testing.T) {
	root := t.TempDir()
	m := &Manager{Root: root}
	got := m.BasePath("run-7")
	if got != filepath.Join(root, "run-7") {
		t.Errorf("BasePath = %q, want %q", got, filepath.Join(root, "run-7"))
	}
}

var _ core.Workspace = (*GitManager)(nil)
