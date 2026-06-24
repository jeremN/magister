package workspace

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// ResolveBase validates that repoDir is a git repo and resolves ref (default
// "HEAD") to a concrete commit SHA, reading the source repo only — it never
// writes. Used at submit time so a bad repo/base fails the request, not a running
// step. The returned SHA is what gets pinned and cloned, so a later resume forks
// from the same commit even if the source branch advanced. ctx cancels the
// underlying git subprocesses.
func ResolveBase(ctx context.Context, repoDir, ref string) (string, error) {
	if repoDir == "" {
		return "", fmt.Errorf("empty repo path")
	}
	if !filepath.IsAbs(repoDir) {
		return "", fmt.Errorf("repo path must be absolute: %q", repoDir)
	}
	if _, err := gitRead(ctx, repoDir, "rev-parse", "--git-dir"); err != nil {
		return "", fmt.Errorf("not a git repo: %q", repoDir)
	}
	if ref == "" {
		ref = "HEAD"
	}
	// ref is user-supplied: --end-of-options ensures a "-"-leading ref (e.g.
	// "--upload-pack=...") is treated as an operand, never a git flag, so it cannot
	// smuggle an option. An unresolvable ref then simply fails verification below.
	out, err := gitRead(ctx, repoDir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("base %q does not resolve in %q", ref, repoDir)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitRead runs a read-only git command in dir and returns the raw combined output.
// It is deliberately a "quiet" helper: it does NOT wrap the error with git's output
// (unlike GitManager.run) because its callers craft their own user-facing messages
// from the high-level failure, not git's stderr. No shell is involved. ctx cancels
// the subprocess.
func gitRead(ctx context.Context, dir string, args ...string) ([]byte, error) {
	// #nosec G204 -- read-only git (rev-parse / remote get-url) without a shell;
	// user-supplied args are guarded at each call site (--end-of-options for a ref,
	// safeRemoteName for a remote name).
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
