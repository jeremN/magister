package workspace

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// ResolveRemote resolves the push destination for a run's source repo, read-only:
//   - remote == ""        → the source's `origin` URL
//   - remote is a URL     → returned as-is (git validates/authenticates it)
//   - remote is a name    → the source's URL for that remote
//
// The source repo's refs/working tree are never written. ctx cancels the
// underlying git subprocess.
func ResolveRemote(ctx context.Context, sourceRepo, remote string) (string, error) {
	if sourceRepo == "" {
		return "", fmt.Errorf("empty source repo path")
	}
	if !filepath.IsAbs(sourceRepo) {
		return "", fmt.Errorf("source repo path must be absolute: %q", sourceRepo)
	}
	if looksLikeURL(remote) {
		return remote, nil
	}
	name := remote
	if name == "" {
		name = "origin"
	}
	if !safeRemoteName(name) {
		return "", fmt.Errorf("invalid remote name %q", name)
	}
	out, err := gitRead(ctx, sourceRepo, "remote", "get-url", name)
	if err != nil {
		return "", fmt.Errorf("no remote %q in %q", name, sourceRepo)
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("remote %q has no url", name)
	}
	return url, nil
}

// PushBranch pushes srcBranch from the scratch clone to destBranch on remoteURL.
// Without force, git refuses a non-fast-forward overwrite of an existing ref; a
// new branch always succeeds. The combined git output rides on the error so push
// failures (auth/network/non-fast-forward) surface the remote's message. Credentials
// come from the ambient git environment — none are handled here. ctx cancels the
// underlying git subprocess (e.g. on a hung network push).
func PushBranch(ctx context.Context, scratchBase, remoteURL, srcBranch, destBranch string, force bool) error {
	if scratchBase == "" || !filepath.IsAbs(scratchBase) {
		return fmt.Errorf("scratch base path must be absolute: %q", scratchBase)
	}
	if !safeRef(srcBranch) {
		return fmt.Errorf("invalid source branch %q", srcBranch)
	}
	if !safeRef(destBranch) {
		return fmt.Errorf("invalid destination branch %q", destBranch)
	}
	args := []string{"push"}
	if force {
		args = append(args, "--force")
	}
	// -- separates the <repository> <refspec> operands from options, so a "-"-leading
	// remote/branch can't be parsed as a flag (the refs are also safeRef-validated).
	args = append(args, "--", remoteURL, srcBranch+":refs/heads/"+destBranch)
	// #nosec G204 -- git push without a shell; refs validated; operands after --.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = scratchBase
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// safeRef accepts a conservative branch-name charset (rejecting a leading "-",
// "..", a trailing "." or ".lock", and anything outside [A-Za-z0-9/._-]) so a name
// cannot smuggle a flag or corrupt the src:refs/heads/dest refspec. These also match
// git's own check-ref-format rules, so a valid safeRef won't be rejected downstream.
func safeRef(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.Contains(s, "..") ||
		strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// looksLikeURL reports whether s is a git URL (scheme://… or scp-like user@host:path)
// rather than a remote name.
func looksLikeURL(s string) bool {
	if strings.Contains(s, "://") {
		return true
	}
	if i := strings.IndexByte(s, ':'); i > 0 && !strings.Contains(s[:i], "/") {
		return true // user@host:path
	}
	return false
}

// safeRemoteName accepts a conservative remote-name charset ([A-Za-z0-9._-]) and
// rejects a leading "-", so a flag-like name can't be smuggled into
// `git remote get-url`. Slash is excluded (more conservative than git allows); use
// a flat remote name, or pass the remote's URL directly.
func safeRemoteName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}
