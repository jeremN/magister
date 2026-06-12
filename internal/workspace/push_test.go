package workspace

import "testing"

func TestResolveRemoteOriginDefault(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t)
	bare := t.TempDir()
	gitOut(t, bare, "init", "--bare")
	gitOut(t, src, "remote", "add", "origin", bare)

	got, err := ResolveRemote(src, "")
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

	got, err := ResolveRemote(src, "upstream")
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
		got, err := ResolveRemote("/abs/src", url)
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
	if _, err := ResolveRemote(src, ""); err == nil {
		t.Error("expected error when origin is absent")
	}
}

func TestResolveRemoteRejectsBadName(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t)
	if _, err := ResolveRemote(src, "--upload-pack=x"); err == nil {
		t.Error("expected error for a flag-like remote name")
	}
}

func TestResolveRemoteRejectsRelativeSource(t *testing.T) {
	requireGit(t)
	if _, err := ResolveRemote("relative/path", ""); err == nil {
		t.Error("expected error for a relative source path")
	}
}
