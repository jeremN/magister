package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRemoteOriginDefault(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t)
	bare := t.TempDir()
	gitOut(t, bare, "init", "--bare")
	gitOut(t, src, "remote", "add", "origin", bare)

	got, err := ResolveRemote(context.Background(), src, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != bare {
		t.Errorf("ResolveRemote origin = %q, want %q", got, bare)
	}
}

func TestResolveRemoteByName(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t)
	bare := t.TempDir()
	gitOut(t, bare, "init", "--bare")
	gitOut(t, src, "remote", "add", "upstream", bare)

	got, err := ResolveRemote(context.Background(), src, "upstream")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != bare {
		t.Errorf("ResolveRemote upstream = %q, want %q", got, bare)
	}
}

func TestResolveRemoteURLPassthrough(t *testing.T) {
	// A URL short-circuits before any git call, so no fixture/git is needed.
	for _, url := range []string{"https://example.com/me/x.git", "git@github.com:me/x.git"} {
		got, err := ResolveRemote(context.Background(), "/abs/src", url)
		if err != nil {
			t.Fatalf("resolve %q: %v", url, err)
		}
		if got != url {
			t.Errorf("ResolveRemote(%q) = %q, want passthrough", url, got)
		}
	}
}

func TestResolveRemoteMissing(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t) // no remotes configured
	if _, err := ResolveRemote(context.Background(), src, ""); err == nil {
		t.Error("expected error when origin is absent")
	}
}

func TestResolveRemoteRejectsBadName(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t)
	if _, err := ResolveRemote(context.Background(), src, "--upload-pack=x"); err == nil {
		t.Error("expected error for a flag-like remote name")
	}
}

func TestResolveRemoteRejectsRelativeSource(t *testing.T) {
	requireGit(t)
	if _, err := ResolveRemote(context.Background(), "relative/path", ""); err == nil {
		t.Error("expected error for a relative source path")
	}
}

// setupScratchWithBranch builds a scratch repo with a committed branch and returns
// (dir, sha). The branch carries one file so the commit is non-empty.
func setupScratchWithBranch(t *testing.T, branch string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	gitOut(t, dir, "init")
	gitOut(t, dir, "config", "user.name", "fix")
	gitOut(t, dir, "config", "user.email", "fix@example.com")
	gitOut(t, dir, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "-A")
	gitOut(t, dir, "commit", "-m", "work")
	return dir, gitOut(t, dir, "rev-parse", "HEAD")
}

func TestPushBranchNewBranch(t *testing.T) {
	requireGit(t)
	scratch, sha := setupScratchWithBranch(t, "step/integrate")
	bare := t.TempDir()
	gitOut(t, bare, "init", "--bare")

	if err := PushBranch(context.Background(), scratch, bare, "step/integrate", "magister/run-1", false); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := gitOut(t, bare, "rev-parse", "magister/run-1"); got != sha {
		t.Errorf("remote ref = %q, want %q", got, sha)
	}
}

func TestPushBranchRefusesNonFastForwardWithoutForce(t *testing.T) {
	requireGit(t)
	scratch, _ := setupScratchWithBranch(t, "step/integrate")
	bare := t.TempDir()
	gitOut(t, bare, "init", "--bare")
	if err := PushBranch(context.Background(), scratch, bare, "step/integrate", "magister/run-1", false); err != nil {
		t.Fatalf("first push: %v", err)
	}
	// Rewrite the branch to a different history (non-fast-forward).
	gitOut(t, scratch, "commit", "--amend", "-m", "rewritten")
	if err := PushBranch(context.Background(), scratch, bare, "step/integrate", "magister/run-1", false); err == nil {
		t.Error("expected non-fast-forward push to be refused without --force")
	}
	if err := PushBranch(context.Background(), scratch, bare, "step/integrate", "magister/run-1", true); err != nil {
		t.Errorf("force push should succeed: %v", err)
	}
}

func TestPushBranchRejectsFlaglikeBranch(t *testing.T) {
	requireGit(t)
	scratch, _ := setupScratchWithBranch(t, "step/integrate")
	bare := t.TempDir()
	gitOut(t, bare, "init", "--bare")
	if err := PushBranch(context.Background(), scratch, bare, "step/integrate", "--force", false); err == nil {
		t.Error("expected a flag-like destination branch to be rejected")
	}
	if err := PushBranch(context.Background(), scratch, bare, "--upload-pack=x", "magister/run-1", false); err == nil {
		t.Error("expected a flag-like source branch to be rejected")
	}
}
