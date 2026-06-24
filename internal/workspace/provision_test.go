package workspace

import (
	"context"
	"testing"
)

func TestResolveBaseDefaultsToHEAD(t *testing.T) {
	requireGit(t)
	src, sha := setupSourceRepo(t)
	got, err := ResolveBase(context.Background(), src, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != sha {
		t.Errorf("ResolveBase(HEAD) = %q, want %q", got, sha)
	}
}

func TestResolveBasePinsExplicitCommit(t *testing.T) {
	requireGit(t)
	src, sha := setupSourceRepo(t)
	// An explicit commit-ish (the SHA itself) resolves through the ^{commit}
	// peeling, a distinct path from the HEAD default.
	got, err := ResolveBase(context.Background(), src, sha)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != sha {
		t.Errorf("ResolveBase(sha) = %q, want %q", got, sha)
	}
}

func TestResolveBaseRejectsFlaglikeRef(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t)
	// A "-"-leading ref must not be parsed as a git flag (--end-of-options guard).
	if _, err := ResolveBase(context.Background(), src, "--upload-pack=touch pwned"); err == nil {
		t.Error("expected error for a flag-like ref")
	}
}

func TestResolveBaseRejectsNonRepo(t *testing.T) {
	requireGit(t)
	if _, err := ResolveBase(context.Background(), t.TempDir(), ""); err == nil {
		t.Error("expected error for a non-git directory")
	}
}

func TestResolveBaseRejectsUnknownRef(t *testing.T) {
	requireGit(t)
	src, _ := setupSourceRepo(t)
	if _, err := ResolveBase(context.Background(), src, "no-such-branch"); err == nil {
		t.Error("expected error for an unresolvable ref")
	}
}

func TestResolveBaseRejectsRelativePath(t *testing.T) {
	requireGit(t)
	if _, err := ResolveBase(context.Background(), "relative/path", ""); err == nil {
		t.Error("expected error for a relative repo path")
	}
}
