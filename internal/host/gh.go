// Package host opens pull requests on a git host via the `gh` CLI. It parses the
// owner/repo from a remote URL and shells `gh` with an injectable command name so
// tests substitute a stub. Auth is ambient (gh's own keyring/GH_TOKEN); no token is
// ever handled here.
package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
		if c := strings.IndexByte(host, ':'); c >= 0 {
			host = host[:c] // drop an explicit :port (e.g. ssh://git@github.com:22/o/r)
		}
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

// Runner shells the gh CLI. Bin is the command name (default "gh"); tests set it to an
// absolute path to a stub. All methods use exec.CommandContext (no shell) and the
// single-token --flag=value form so a value can never be parsed as a flag.
type Runner struct {
	Bin string
}

// New returns a Runner backed by the gh CLI on PATH.
func New() *Runner { return &Runner{Bin: "gh"} }

func (r *Runner) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return "gh"
}

// CreateOpts are the inputs to CreatePR. Base "" omits --base (gh uses the repo default).
type CreateOpts struct {
	Owner, Repo, Head, Base, Title, Body string
	Draft                                bool
}

// run executes gh from a neutral working directory (so an ambient repo's git config —
// e.g. gh-merge-base — cannot influence gh) and returns stdout and stderr separately.
func (r *Runner) run(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	// #nosec G204 -- gh + args are built from charset-guarded owner/repo, ref-shaped
	// head/base, and --flag=value single tokens; no shell.
	cmd := exec.CommandContext(ctx, r.bin(), args...)
	cmd.Dir = os.TempDir()
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err = cmd.Run()
	return so.String(), se.String(), err
}

// ExistingOpenPR returns the URL of an open PR whose head is `head`, if one exists.
func (r *Runner) ExistingOpenPR(ctx context.Context, owner, repo, head string) (string, bool, error) {
	so, se, err := r.run(ctx, "pr", "list",
		"--repo="+owner+"/"+repo, "--head="+head, "--state=open", "--json=url")
	if err != nil {
		return "", false, fmt.Errorf("gh pr list: %w: %s", err, strings.TrimSpace(se))
	}
	var prs []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(so)), &prs); err != nil {
		return "", false, fmt.Errorf("gh pr list: parse json: %w", err)
	}
	if len(prs) == 0 {
		return "", false, nil
	}
	return prs[0].URL, true, nil
}

// CreatePR opens a pull request and returns its URL.
func (r *Runner) CreatePR(ctx context.Context, o CreateOpts) (string, error) {
	args := []string{"pr", "create",
		"--repo=" + o.Owner + "/" + o.Repo,
		"--head=" + o.Head,
		"--title=" + o.Title,
		"--body=" + o.Body,
	}
	if o.Base != "" {
		args = append(args, "--base="+o.Base)
	}
	if o.Draft {
		args = append(args, "--draft")
	}
	so, se, err := r.run(ctx, args...)
	if err != nil {
		out := strings.TrimSpace(se)
		if out == "" {
			out = strings.TrimSpace(so)
		}
		return "", fmt.Errorf("gh pr create: %w: %s", err, out)
	}
	url := lastURL(so)
	if url == "" {
		return "", fmt.Errorf("gh pr create: no PR url in output: %s", strings.TrimSpace(so))
	}
	return url, nil
}

// BranchExists reports whether branch exists on owner/repo (via the GitHub API). Any
// gh failure (incl. a 404) reads as "absent" — it is only used to refine a CreatePR
// failure into a helpful "run cm push first" message.
func (r *Runner) BranchExists(ctx context.Context, owner, repo, branch string) bool {
	_, _, err := r.run(ctx, "api", "--silent", "repos/"+owner+"/"+repo+"/branches/"+branch)
	return err == nil
}

// lastURL returns the last http(s) line in s (gh prints the PR URL on its own line).
func lastURL(s string) string {
	var last string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "http://") || strings.HasPrefix(ln, "https://") {
			last = ln
		}
	}
	return last
}
