package workspace

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ResolveRemote resolves the push destination for a run's source repo, read-only:
//   - remote == ""        → the source's `origin` URL
//   - remote is a URL     → returned as-is (git validates/authenticates it)
//   - remote is a name    → the source's URL for that remote
//
// The source repo's refs/working tree are never written.
func ResolveRemote(sourceRepo, remote string) (string, error) {
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
	out, err := gitRead(sourceRepo, "remote", "get-url", name)
	if err != nil {
		return "", fmt.Errorf("no remote %q in %q", name, sourceRepo)
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("remote %q has no url", name)
	}
	return url, nil
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
