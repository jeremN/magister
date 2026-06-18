// Package host opens pull requests on a git host via the `gh` CLI. It parses the
// owner/repo from a remote URL and shells `gh` with an injectable command name so
// tests substitute a stub. Auth is ambient (gh's own keyring/GH_TOKEN); no token is
// ever handled here.
package host

import (
	"fmt"
	"strings"
)

// ParseRemote extracts the host, owner, and repo from a git remote URL. It accepts
// https (https://github.com/owner/repo[.git], optionally with credentials), scp-like
// (git@github.com:owner/repo[.git]), and ssh:// forms. Only github.com is supported.
func ParseRemote(remoteURL string) (host, owner, repo string, err error) {
	s := strings.TrimSpace(remoteURL)
	switch {
	case strings.Contains(s, "://"):
		rest := s[strings.Index(s, "://")+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 && !strings.Contains(rest[:at], "/") {
			rest = rest[at+1:]
		}
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return "", "", "", fmt.Errorf("cannot parse remote %q", remoteURL)
		}
		host = rest[:slash]
		owner, repo, err = splitOwnerRepo(rest[slash+1:])
	case strings.Contains(s, "@") && strings.Contains(s, ":"):
		hostPath := s[strings.IndexByte(s, '@')+1:]
		colon := strings.IndexByte(hostPath, ':')
		if colon < 0 {
			return "", "", "", fmt.Errorf("cannot parse remote %q", remoteURL)
		}
		host = hostPath[:colon]
		owner, repo, err = splitOwnerRepo(hostPath[colon+1:])
	default:
		return "", "", "", fmt.Errorf("cannot parse remote %q", remoteURL)
	}
	if err != nil {
		return "", "", "", err
	}
	if host != "github.com" {
		return "", "", "", fmt.Errorf("unsupported host %q (only github.com is supported)", host)
	}
	return host, owner, repo, nil
}

func splitOwnerRepo(p string) (owner, repo string, err error) {
	p = strings.TrimSuffix(strings.TrimPrefix(p, "/"), ".git")
	parts := strings.Split(p, "/")
	if len(parts) != 2 || !safeSeg(parts[0]) || !safeSeg(parts[1]) {
		return "", "", fmt.Errorf("cannot parse owner/repo from %q", p)
	}
	return parts[0], parts[1], nil
}

// safeSeg guards an owner/repo segment: non-empty, no leading "-", charset
// [A-Za-z0-9._-] so it cannot smuggle a flag into the gh argv.
func safeSeg(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}
