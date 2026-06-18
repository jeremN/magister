# external-repo Slice 3 (open a PR) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an explicit `cm pr <run>` / `POST /v1/runs/{id}/pr` that opens a GitHub Pull Request on the pushed `magister/<runID>` branch of a succeeded external-repo run, via the `gh` CLI.

**Architecture:** A new bounded `internal/host` package parses `owner/repo` from the source remote URL and shells `gh` (injectable `Bin`, ambient auth, no token handling). `Supervisor.PR` is a post-run, store-driven orchestrator (engine untouched, no scratch clone needed) that validates the run, builds the PR metadata from store data, and calls the `gh` runner; typed `*PRError{Status,Msg}` maps to HTTP. A thin `api` handler and `cm pr` verb mirror Slice 2's push layering.

**Tech Stack:** Go 1.22 stdlib (`os/exec`, `encoding/json`, `net/http`); the `gh` CLI as an external tool; shell-script stubs for offline tests.

## Global Constraints

- **Go 1.22; no new Go dependencies** — stdlib + the existing module only.
- **Zero token handling** — `gh` authenticates via its own keyring/`GH_TOKEN`; never read/store/pass a credential.
- **GitHub-only** — a non-`github.com` remote is a clear error; no GitLab/other host, no cross-repo/fork PRs.
- **Argv hardening** — `owner`/`repo` charset-guarded; head/base ref-shaped; all `gh` args use the single-token `--flag=value` form; `exec.Command*`, no shell; `// #nosec G204` with rationale (as in `internal/executor/cli.go`).
- **PR touches no scratch clone and no git refs** — store + a read-only `remote get-url` on the source + `gh`. (Verified: `gh pr create --repo … --head … --base …` is git-context-free.)
- **gofmt is NOT hook-enforced** — run `gofmt -l .` yourself; the pre-existing dirty `internal/executor/gemini.go` is NOT ours — do not touch or reformat it.
- **Commits:** one conventional-commit subject line, **no body, no `Co-Authored-By` trailer**, never `--no-verify`. `rtk` is not installed — run `go`/`git`/`gofmt`/`gh` directly.
- Branch: `slice3-open-pr` (already created; the spec commit `b2f569a` is on it).

---

### Task 1: `host.ParseRemote` — owner/repo from a remote URL

**Files:**
- Create: `internal/host/gh.go`
- Test: `internal/host/gh_test.go`

**Interfaces:**
- Produces: `func ParseRemote(remoteURL string) (host, owner, repo string, err error)` — accepts `https://…`, scp-like `git@host:owner/repo`, and `ssh://…` forms; strips a trailing `.git`; charset-guards owner/repo; errors for a non-`github.com` host.

- [ ] **Step 1: Write the failing test**

```go
package host

import "testing"

func TestParseRemote(t *testing.T) {
	cases := []struct {
		in, owner, repo string
		ok              bool
	}{
		{"https://github.com/o/r", "o", "r", true},
		{"https://github.com/o/r.git", "o", "r", true},
		{"git@github.com:o/r.git", "o", "r", true},
		{"git@github.com:o/r", "o", "r", true},
		{"ssh://git@github.com/o/r.git", "o", "r", true},
		{"https://x-access-token:TOK@github.com/o/r.git", "o", "r", true},
		{"https://gitlab.com/o/r.git", "", "", false}, // unsupported host
		{"https://github.com/only-one", "", "", false},
		{"git@github.com:-flag/r.git", "", "", false}, // flag-like owner
		{"not a url", "", "", false},
	}
	for _, c := range cases {
		_, owner, repo, err := ParseRemote(c.in)
		if c.ok {
			if err != nil || owner != c.owner || repo != c.repo {
				t.Errorf("ParseRemote(%q) = (%q,%q,%v), want (%q,%q,nil)", c.in, owner, repo, err, c.owner, c.repo)
			}
		} else if err == nil {
			t.Errorf("ParseRemote(%q): want error, got (%q,%q)", c.in, owner, repo)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/host/ -run TestParseRemote`
Expected: FAIL — `undefined: ParseRemote` (package may not compile yet).

- [ ] **Step 3: Write minimal implementation**

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/host/ -run TestParseRemote -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/host/gh.go internal/host/gh_test.go
git add internal/host/gh.go internal/host/gh_test.go
git commit -m "feat(host): ParseRemote extracts owner/repo from a git remote URL"
```

---

### Task 2: `host.Runner` — open/look-up PRs via the `gh` CLI

**Files:**
- Modify: `internal/host/gh.go`
- Create: `internal/host/testdata/fake-gh` (executable shell stub)
- Test: `internal/host/gh_test.go`

**Interfaces:**
- Consumes: `ParseRemote` (Task 1) — not directly; Runner takes owner/repo as args.
- Produces:
  - `type Runner struct { Bin string }` and `func New() *Runner` (Bin `"gh"`).
  - `type CreateOpts struct { Owner, Repo, Head, Base, Title, Body string; Draft bool }`
  - `func (r *Runner) ExistingOpenPR(ctx context.Context, owner, repo, head string) (url string, exists bool, err error)`
  - `func (r *Runner) CreatePR(ctx context.Context, o CreateOpts) (url string, err error)`
  - `func (r *Runner) BranchExists(ctx context.Context, owner, repo, branch string) bool`

**fake-gh env contract** (used by this task and the supervisor/api tests):
- `FAKE_GH_ARGV_FILE` — if set, append the invocation's argv (one arg per line, then a `---` line).
- `FAKE_GH_EXISTING_PR` — if set, `gh pr list` prints `[{"url":"<val>"}]`; else `[]`.
- `FAKE_GH_PR_URL` — `gh pr create` prints this URL (default a canned URL).
- `FAKE_GH_CREATE_FAIL` — if set, `gh pr create` prints it to stderr and exits 1.
- `FAKE_GH_BRANCH_MISSING` — if set, `gh api …/branches/…` exits 1 (simulated 404).

- [ ] **Step 1: Create the stub `internal/host/testdata/fake-gh`**

```sh
#!/bin/sh
# fake-gh — offline gh stub for tests. Behavior is env-driven (see FAKE_GH_* in the
# slice-3 plan). Records argv (one arg per line, '---' per call) to $FAKE_GH_ARGV_FILE.
if [ -n "$FAKE_GH_ARGV_FILE" ]; then
  for a in "$@"; do printf '%s\n' "$a"; done >> "$FAKE_GH_ARGV_FILE"
  printf -- '---\n' >> "$FAKE_GH_ARGV_FILE"
fi
case "$1" in
  pr)
    case "$2" in
      list)
        if [ -n "$FAKE_GH_EXISTING_PR" ]; then
          printf '[{"url":"%s"}]\n' "$FAKE_GH_EXISTING_PR"
        else
          printf '[]\n'
        fi
        ;;
      create)
        if [ -n "$FAKE_GH_CREATE_FAIL" ]; then
          printf '%s\n' "$FAKE_GH_CREATE_FAIL" 1>&2
          exit 1
        fi
        printf '%s\n' "${FAKE_GH_PR_URL:-https://github.com/o/r/pull/1}"
        ;;
      *) printf 'fake-gh: unexpected pr subcommand %s\n' "$2" 1>&2; exit 2 ;;
    esac
    ;;
  api)
    if [ -n "$FAKE_GH_BRANCH_MISSING" ]; then
      printf 'gh: Not Found (HTTP 404)\n' 1>&2
      exit 1
    fi
    ;;
  *) printf 'fake-gh: unexpected command %s\n' "$1" 1>&2; exit 2 ;;
esac
```

Then make it executable (git preserves the mode):

```bash
chmod +x internal/host/testdata/fake-gh
```

- [ ] **Step 2: Write the failing tests**

```go
// append to internal/host/gh_test.go
import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stubPath(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stub %s missing: %v", name, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("stub %s is not executable — chmod +x it", name)
	}
	return abs
}

func TestRunnerCreatePR(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_GH_ARGV_FILE", argv)
	t.Setenv("FAKE_GH_PR_URL", "https://github.com/o/r/pull/9")
	r := &Runner{Bin: stubPath(t, "fake-gh")}
	url, err := r.CreatePR(context.Background(), CreateOpts{
		Owner: "o", Repo: "r", Head: "magister/x", Base: "main",
		Title: "the title", Body: "the body", Draft: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if url != "https://github.com/o/r/pull/9" {
		t.Errorf("url = %q", url)
	}
	got, _ := os.ReadFile(argv)
	for _, want := range []string{"pr", "create", "--repo=o/r", "--head=magister/x", "--base=main", "--title=the title", "--body=the body", "--draft"} {
		if !strings.Contains(string(got), want+"\n") {
			t.Errorf("argv missing %q; got:\n%s", want, got)
		}
	}
}

func TestRunnerCreatePROmitsBaseWhenEmpty(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_GH_ARGV_FILE", argv)
	r := &Runner{Bin: stubPath(t, "fake-gh")}
	if _, err := r.CreatePR(context.Background(), CreateOpts{Owner: "o", Repo: "r", Head: "h", Title: "t", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(argv); strings.Contains(string(got), "--base=") {
		t.Errorf("expected no --base; got:\n%s", got)
	}
}

func TestRunnerCreatePRFailureSurfacesStderr(t *testing.T) {
	t.Setenv("FAKE_GH_CREATE_FAIL", "boom: bad base")
	r := &Runner{Bin: stubPath(t, "fake-gh")}
	if _, err := r.CreatePR(context.Background(), CreateOpts{Owner: "o", Repo: "r", Head: "h", Title: "t", Body: "b"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want failure surfacing stderr, got %v", err)
	}
}

func TestRunnerExistingOpenPR(t *testing.T) {
	r := &Runner{Bin: stubPath(t, "fake-gh")}
	if url, ok, err := r.ExistingOpenPR(context.Background(), "o", "r", "magister/x"); err != nil || ok || url != "" {
		t.Fatalf("want none, got url=%q ok=%v err=%v", url, ok, err)
	}
	t.Setenv("FAKE_GH_EXISTING_PR", "https://github.com/o/r/pull/3")
	url, ok, err := r.ExistingOpenPR(context.Background(), "o", "r", "magister/x")
	if err != nil || !ok || url != "https://github.com/o/r/pull/3" {
		t.Fatalf("want existing, got url=%q ok=%v err=%v", url, ok, err)
	}
}

func TestRunnerBranchExists(t *testing.T) {
	r := &Runner{Bin: stubPath(t, "fake-gh")}
	if !r.BranchExists(context.Background(), "o", "r", "magister/x") {
		t.Error("want exists")
	}
	t.Setenv("FAKE_GH_BRANCH_MISSING", "1")
	if r.BranchExists(context.Background(), "o", "r", "magister/x") {
		t.Error("want missing")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/host/ -run TestRunner`
Expected: FAIL — `undefined: Runner` / `CreateOpts`.

- [ ] **Step 4: Write the implementation (append to `internal/host/gh.go`)**

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	// (fmt, strings already imported in this file)
)

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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/host/ -v`
Expected: PASS (all `TestParseRemote` + `TestRunner*`).

- [ ] **Step 6: Commit**

```bash
gofmt -w internal/host/gh.go internal/host/gh_test.go
git add internal/host/gh.go internal/host/gh_test.go internal/host/testdata/fake-gh
git commit -m "feat(host): gh Runner opens and looks up PRs via the gh CLI"
```

---

### Task 3: `Supervisor.PR` — open a PR (happy path + validation)

**Files:**
- Modify: `internal/supervisor/supervisor.go` (add `Host` field + `hostRunner()` accessor + `host` import)
- Create: `internal/supervisor/pr.go`
- Test: `internal/supervisor/pr_test.go`

**Interfaces:**
- Consumes: `host.Runner`/`New`/`CreateOpts`/`ExistingOpenPR`/`CreatePR`/`BranchExists` (Task 2); `host.ParseRemote` (Task 1); `workspace.ResolveRemote` (Slice 2); `pickResultStep`/`stepBranch` (existing, `supervisor.go`); `flow.ParseBytes`.
- Produces:
  - `type PROpts struct { Remote, As, Step, Base, Title, Body string; Draft bool }`
  - `type PRResult struct { URL, Repo, Head, Base string; Draft bool }`
  - `type PRError struct { Status int; Msg string }` + `func (e *PRError) Error() string` + `prErr(...)`
  - `func (s *Supervisor) PR(ctx context.Context, runID core.RunID, opts PROpts) (PRResult, error)`
  - `func defaultPRTitle(rs core.RunState) string`, `func generatePRBody(rs core.RunState, term *flow.Step) string`
  - `Supervisor.Host *host.Runner` field, `func (s *Supervisor) hostRunner() *host.Runner`

- [ ] **Step 1: Add the `Host` field + accessor to `supervisor.go`**

In the import block add `"concentus/internal/host"`. In the `Supervisor` struct add (next to `Log`):

```go
	// Host is the gh-backed PR client; nil → a default host.New() (the gh CLI on PATH).
	Host *host.Runner
```

Add the accessor (near `logger()`):

```go
func (s *Supervisor) hostRunner() *host.Runner {
	if s.Host != nil {
		return s.Host
	}
	return host.New()
}
```

- [ ] **Step 2: Write the failing tests `internal/supervisor/pr_test.go`**

```go
package supervisor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"concentus/internal/core"
	"concentus/internal/host"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

// ghStub returns the absolute path to the shared fake-gh stub in internal/host/testdata.
func ghStub(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "host", "testdata", "fake-gh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("fake-gh stub missing: %v", err)
	}
	return abs
}

// srcWithGHOrigin builds a git repo whose origin is the given (never-fetched) URL.
func srcWithGHOrigin(t *testing.T, url string) string {
	t.Helper()
	src := t.TempDir()
	gitS(t, src, "init")
	gitS(t, src, "remote", "add", "origin", url)
	return src
}

// seedExtRun persists a succeeded external-repo run with a single terminal step.
func seedExtRun(t *testing.T, st core.Store, id core.RunID, repo string) {
	t.Helper()
	err := st.CreateRun(context.Background(), core.RunState{
		ID: id, Name: "demo", Repo: repo, Status: core.RunSucceeded,
		FlowYAML: "name: demo\nsteps:\n  - id: integrate\n    agent: mock\n",
		Steps: []core.StepState{{
			RunID: id, StepID: "integrate", Status: core.StepSucceeded,
			Artifacts: []core.Artifact{{StepID: "integrate", Branch: "step/integrate", Commit: "abcdef1234567"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func prErrStatus(t *testing.T, err error) int {
	t.Helper()
	var pe *PRError
	if !errors.As(err, &pe) {
		t.Fatalf("want *PRError, got %v", err)
	}
	return pe.Status
}

func newPRSup(t *testing.T, st core.Store) *Supervisor {
	t.Helper()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.Manager{Root: t.TempDir()}), st, reg)
	sup.Host = &host.Runner{Bin: ghStub(t)}
	return sup
}

func TestPROpensPullRequest(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_GH_ARGV_FILE", argv)
	t.Setenv("FAKE_GH_PR_URL", "https://github.com/test-owner/test-repo/pull/7")
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))

	res, err := sup.PR(context.Background(), "r1", PROpts{})
	if err != nil {
		t.Fatalf("pr: %v", err)
	}
	if res.URL != "https://github.com/test-owner/test-repo/pull/7" {
		t.Errorf("url = %q", res.URL)
	}
	if res.Repo != "test-owner/test-repo" {
		t.Errorf("repo = %q", res.Repo)
	}
	if res.Head != "magister/r1" {
		t.Errorf("head = %q", res.Head)
	}
	got, _ := os.ReadFile(argv)
	for _, want := range []string{"create", "--repo=test-owner/test-repo", "--head=magister/r1"} {
		if !strings.Contains(string(got), want+"\n") {
			t.Errorf("argv missing %q; got:\n%s", want, got)
		}
	}
}

func TestPRUnknownRun404(t *testing.T) {
	sup := newPRSup(t, store.NewMem())
	_, err := sup.PR(context.Background(), "nope", PROpts{})
	if got := prErrStatus(t, err); got != http.StatusNotFound {
		t.Errorf("status = %d, want 404", got)
	}
}

func TestPRRejectsNonExternalRepo(t *testing.T) {
	st := store.NewMem()
	sup := newPRSup(t, st)
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Status: core.RunSucceeded,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n",
	})
	_, err := sup.PR(context.Background(), "r1", PROpts{})
	if got := prErrStatus(t, err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
}

func TestPRRejectsUnsucceededRun(t *testing.T) {
	st := store.NewMem()
	sup := newPRSup(t, st)
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Repo: "/abs/proj", Status: core.RunRunning,
		FlowYAML: "name: f\nsteps:\n  - id: a\n    agent: mock\n",
	})
	_, err := sup.PR(context.Background(), "r1", PROpts{})
	if got := prErrStatus(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409", got)
	}
}

func TestPRUnsupportedHost(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://gitlab.com/o/r.git"))
	_, err := sup.PR(context.Background(), "r1", PROpts{})
	if got := prErrStatus(t, err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (unsupported host)", got)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/supervisor/ -run TestPR`
Expected: FAIL — `undefined: PROpts` / `Supervisor.PR`.

- [ ] **Step 4: Write `internal/supervisor/pr.go`**

```go
package supervisor

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"concentus/internal/core"
	"concentus/internal/flow"
	"concentus/internal/host"
	"concentus/internal/workspace"
)

// PROpts configures PR. Zero values mean: origin remote, magister/<runID> head, the
// unique terminal step (for the body summary), the repo's default base branch,
// generated title/body, not a draft.
type PROpts struct {
	Remote, As, Step, Base, Title, Body string
	Draft                               bool
}

// PRResult is returned by PR on success.
type PRResult struct {
	URL, Repo, Head, Base string
	Draft                 bool
}

// PRError carries an HTTP status so the API maps failures without string-matching.
type PRError struct {
	Status int
	Msg    string
}

func (e *PRError) Error() string { return e.Msg }

func prErr(status int, format string, a ...any) *PRError {
	return &PRError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// PR opens a pull request on the host repo for a succeeded external-repo run. It is a
// post-run, store-driven operation (engine untouched, no scratch clone): it reads the
// run, derives owner/repo from the source remote, builds the PR metadata from store
// data, and shells gh. The head branch is the push destination (magister/<runID> by
// default), so the run must already have been pushed (cm push). Errors are *PRError
// with an HTTP status (see the slice-3 spec).
func (s *Supervisor) PR(ctx context.Context, runID core.RunID, opts PROpts) (PRResult, error) {
	rs, err := s.store.GetRun(ctx, runID)
	if err != nil {
		// TODO: no store not-found sentinel; a genuine storage error reads as 404 (as in Push).
		return PRResult{}, prErr(http.StatusNotFound, "unknown run %q", runID)
	}
	if rs.Repo == "" {
		return PRResult{}, prErr(http.StatusBadRequest, "run %q is not an external-repo run (no --repo)", runID)
	}
	if rs.Status != core.RunSucceeded {
		return PRResult{}, prErr(http.StatusConflict, "run %q is %s, not succeeded", runID, rs.Status)
	}
	head := opts.As
	if head == "" {
		head = "magister/" + string(runID)
	}
	remoteURL, err := workspace.ResolveRemote(rs.Repo, opts.Remote)
	if err != nil {
		return PRResult{}, prErr(http.StatusBadRequest, "remote: %v", err)
	}
	_, owner, repo, err := host.ParseRemote(remoteURL)
	if err != nil {
		return PRResult{}, prErr(http.StatusBadRequest, "%v", err)
	}
	f, err := flow.ParseBytes([]byte(rs.FlowYAML))
	if err != nil {
		return PRResult{}, prErr(http.StatusInternalServerError, "parse stored flow: %v", err)
	}
	term, perr := pickResultStep(f, opts.Step)
	if perr != nil {
		return PRResult{}, prErr(perr.Status, "%s", perr.Msg)
	}
	title := opts.Title
	if title == "" {
		title = defaultPRTitle(rs)
	}
	body := opts.Body
	if body == "" {
		body = generatePRBody(rs, term)
	}

	runner := s.hostRunner()
	url, err := runner.CreatePR(ctx, host.CreateOpts{
		Owner: owner, Repo: repo, Head: head, Base: opts.Base,
		Title: title, Body: body, Draft: opts.Draft,
	})
	if err != nil {
		return PRResult{}, prErr(http.StatusBadGateway, "%v", err)
	}
	return PRResult{URL: url, Repo: owner + "/" + repo, Head: head, Base: opts.Base, Draft: opts.Draft}, nil
}

// defaultPRTitle builds the title when --title is not given: the flow name + a short id.
func defaultPRTitle(rs core.RunState) string {
	id := string(rs.ID)
	short := id
	if len(id) > 8 {
		short = id[len(id)-8:]
	}
	name := rs.Name
	if name == "" {
		name = "run"
	}
	return fmt.Sprintf("magister: %s (%s)", name, short)
}

// generatePRBody builds the body when --body is not given: a small run summary from
// store data (flow name, run id, the result step + its commit, the step list).
func generatePRBody(rs core.RunState, term *flow.Step) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## magister run `%s` — %s\n\n", rs.ID, rs.Name)
	b.WriteString("Delivered by concentus-magister.\n\n")
	if term != nil {
		if _, commit := stepBranch(rs, term.ID); commit != "" {
			fmt.Fprintf(&b, "**Result:** step `%s` (commit %s)\n\n", term.ID, shortSHA(commit))
		} else {
			fmt.Fprintf(&b, "**Result:** step `%s`\n\n", term.ID)
		}
	}
	b.WriteString("**Steps:**\n")
	for _, st := range rs.Steps {
		fmt.Fprintf(&b, "- `%s` %s\n", st.StepID, statusMark(st.Status))
	}
	return b.String()
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func statusMark(s core.StepStatus) string {
	if s == core.StepSucceeded {
		return "✓"
	}
	return string(s)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/supervisor/ -run TestPR -v`
Expected: PASS (5 tests; git-needing ones skip if git is absent).

- [ ] **Step 6: Commit**

```bash
gofmt -w internal/supervisor/supervisor.go internal/supervisor/pr.go internal/supervisor/pr_test.go
git add internal/supervisor/supervisor.go internal/supervisor/pr.go internal/supervisor/pr_test.go
git commit -m "feat(supervisor): PR opens a pull request for a succeeded external-repo run"
```

---

### Task 4: `Supervisor.PR` — existing-PR 409 + unpushed-branch diagnosis

**Files:**
- Modify: `internal/supervisor/pr.go`
- Test: `internal/supervisor/pr_test.go`

**Interfaces:**
- Consumes: `runner.ExistingOpenPR`, `runner.BranchExists` (Task 2); the `PR` method (Task 3).
- Produces: no new exported names — refines `PR`'s behavior (a pre-check before create, and a diagnosis after a create failure).

- [ ] **Step 1: Write the failing tests (append to `internal/supervisor/pr_test.go`)**

```go
func TestPRExistingOpenPRReturns409(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	t.Setenv("FAKE_GH_EXISTING_PR", "https://github.com/test-owner/test-repo/pull/2")
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))
	_, err := sup.PR(context.Background(), "r1", PROpts{})
	if got := prErrStatus(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409", got)
	}
	var pe *PRError
	errors.As(err, &pe)
	if !strings.Contains(pe.Msg, "pull/2") {
		t.Errorf("message should carry the existing PR URL, got %q", pe.Msg)
	}
}

func TestPRUnpushedBranchSaysPushFirst(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	t.Setenv("FAKE_GH_CREATE_FAIL", "GraphQL: Head sha can't be blank")
	t.Setenv("FAKE_GH_BRANCH_MISSING", "1")
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))
	_, err := sup.PR(context.Background(), "r1", PROpts{})
	if got := prErrStatus(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409", got)
	}
	var pe *PRError
	errors.As(err, &pe)
	if !strings.Contains(pe.Msg, "cm push") {
		t.Errorf("message should tell the user to push first, got %q", pe.Msg)
	}
}

func TestPRCreateFailureWithExistingBranchIs502(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	t.Setenv("FAKE_GH_CREATE_FAIL", "GraphQL: base branch nonsense") // branch exists (no FAKE_GH_BRANCH_MISSING)
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))
	_, err := sup.PR(context.Background(), "r1", PROpts{})
	if got := prErrStatus(t, err); got != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/supervisor/ -run 'TestPRExisting|TestPRUnpushed|TestPRCreateFailure'`
Expected: FAIL — existing-PR returns 502 (not 409), unpushed returns 502 with no "cm push", etc.

- [ ] **Step 3: Refine `PR` in `internal/supervisor/pr.go`**

Replace the `runner := s.hostRunner()` … create block with:

```go
	runner := s.hostRunner()
	if url, exists, err := runner.ExistingOpenPR(ctx, owner, repo, head); err != nil {
		return PRResult{}, prErr(http.StatusBadGateway, "%v", err)
	} else if exists {
		return PRResult{}, prErr(http.StatusConflict, "PR already exists for %s: %s", head, url)
	}

	url, err := runner.CreatePR(ctx, host.CreateOpts{
		Owner: owner, Repo: repo, Head: head, Base: opts.Base,
		Title: title, Body: body, Draft: opts.Draft,
	})
	if err != nil {
		if !runner.BranchExists(ctx, owner, repo, head) {
			return PRResult{}, prErr(http.StatusConflict, "branch %q not on remote; run `cm push %s` first", head, runID)
		}
		return PRResult{}, prErr(http.StatusBadGateway, "%v", err)
	}
	return PRResult{URL: url, Repo: owner + "/" + repo, Head: head, Base: opts.Base, Draft: opts.Draft}, nil
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/supervisor/ -run TestPR -v`
Expected: PASS (all 8 PR tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/supervisor/pr.go internal/supervisor/pr_test.go
git add internal/supervisor/pr.go internal/supervisor/pr_test.go
git commit -m "feat(supervisor): PR returns 409 on existing PR or unpushed branch"
```

---

### Task 5: `POST /v1/runs/{id}/pr` endpoint

**Files:**
- Modify: `internal/api/handlers.go` (`handlePR`), `internal/api/router.go` (route), `internal/api/dto.go` (`prRequest`/`prResponse`)
- Test: `internal/api/handlers_test.go`

**Interfaces:**
- Consumes: `supervisor.PR`/`PROpts`/`PRResult`/`PRError` (Tasks 3-4).
- Produces: `prRequest{Remote,As,Step,Base,Title,Body string; Draft bool}` (JSON), `prResponse{URL,Repo,Head,Base,Draft}`, route `POST /v1/runs/{id}/pr`.

- [ ] **Step 1: Write the failing tests (append to `internal/api/handlers_test.go`)**

```go
// ghAPIStub returns the absolute path to the shared fake-gh stub.
func ghAPIStub(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "host", "testdata", "fake-gh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("fake-gh stub missing: %v", err)
	}
	return abs
}

func TestPREndpointOpensPR(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src := t.TempDir()
	runGit(t, src, "init")
	runGit(t, src, "remote", "add", "origin", "https://github.com/o/r.git")

	st := store.NewMem()
	reg := supervisor.NewApprovalRegistry()
	bus := event.NewBus()
	eng := &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}},
		WS:    &workspace.Manager{Root: t.TempDir()},
		Gate:  &gate.Evaluator{Approver: &supervisor.RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: core.SystemClock{},
	}
	sup := supervisor.New(eng, st, reg)
	sup.Host = &host.Runner{Bin: ghAPIStub(t)}
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	t.Setenv("FAKE_GH_PR_URL", "https://github.com/o/r/pull/5")
	st.CreateRun(context.Background(), core.RunState{
		ID: "r1", Name: "demo", Repo: src, Status: core.RunSucceeded,
		FlowYAML: "name: demo\nsteps:\n  - id: integrate\n    agent: mock\n",
		Steps: []core.StepState{{
			RunID: "r1", StepID: "integrate", Status: core.StepSucceeded,
			Artifacts: []core.Artifact{{StepID: "integrate", Branch: "step/integrate", Commit: "abc"}},
		}},
	})
	srv := &Server{Sup: sup, Store: st, Bus: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ShutdownTimeout: time.Second}
	hs := httptest.NewServer(srv.Router(""))
	t.Cleanup(hs.Close)

	resp, err := http.Post(hs.URL+"/v1/runs/r1/pr", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("pr = %d, want 200: %s", resp.StatusCode, b)
	}
	var pr prResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pr.URL != "https://github.com/o/r/pull/5" || pr.Repo != "o/r" || pr.Head != "magister/r1" {
		t.Errorf("response = %+v", pr)
	}
}

func TestPREndpointUnknownRun404(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs/nope/pr", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
```

Note: ensure `internal/api/handlers_test.go` imports include `"os"`, `"path/filepath"`, and `"concentus/internal/host"` (add any missing). `runGit`, `testServer`, `store`, `event`, `engine`, `executor`, `gate`, `join`, `workspace`, `slog`, `io`, `httptest`, `strings`, `time` are already used elsewhere in this test file.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestPREndpoint`
Expected: FAIL — `undefined: prResponse` / no route (404 for the happy path too).

- [ ] **Step 3: Add the DTOs to `internal/api/dto.go`**

```go
// prRequest is the JSON body of POST /v1/runs/{id}/pr. All fields optional.
type prRequest struct {
	Remote string `json:"remote,omitempty"`
	As     string `json:"as,omitempty"`
	Step   string `json:"step,omitempty"`
	Base   string `json:"base,omitempty"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
}

// prResponse is returned from POST /v1/runs/{id}/pr.
type prResponse struct {
	URL   string `json:"url"`
	Repo  string `json:"repo"`
	Head  string `json:"head"`
	Base  string `json:"base,omitempty"`
	Draft bool   `json:"draft,omitempty"`
}
```

- [ ] **Step 4: Add the handler to `internal/api/handlers.go`**

```go
func (s *Server) handlePR(w http.ResponseWriter, r *http.Request) {
	var req prRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.Sup.PR(r.Context(), core.RunID(r.PathValue("id")), supervisor.PROpts{
		Remote: req.Remote, As: req.As, Step: req.Step, Base: req.Base,
		Title: req.Title, Body: req.Body, Draft: req.Draft,
	})
	if err != nil {
		var pe *supervisor.PRError
		if errors.As(err, &pe) {
			writeError(w, pe.Status, pe.Msg)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, prResponse{
		URL: res.URL, Repo: res.Repo, Head: res.Head, Base: res.Base, Draft: res.Draft,
	})
}
```

- [ ] **Step 5: Register the route in `internal/api/router.go`**

After the `/push` line:

```go
	v1.HandleFunc("POST /v1/runs/{id}/pr", s.handlePR)
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/api/ -run TestPREndpoint -v`
Expected: PASS (happy path + 404).

- [ ] **Step 7: Commit**

```bash
gofmt -w internal/api/handlers.go internal/api/router.go internal/api/dto.go internal/api/handlers_test.go
git add internal/api/handlers.go internal/api/router.go internal/api/dto.go internal/api/handlers_test.go
git commit -m "feat(api): POST /v1/runs/{id}/pr opens a pull request"
```

---

### Task 6: `cm pr <run>` CLI verb

**Files:**
- Modify: `cmd/cm/main.go` (dispatch case + usage line + `pr` method)
- Test: `cmd/cm/main_test.go`

**Interfaces:**
- Consumes: the `POST /v1/runs/{id}/pr` endpoint (Task 5); the existing `printErr` helper.
- Produces: `func (c *client) pr(args []string, out io.Writer) int`; a `pr` case in `dispatch`.

- [ ] **Step 1: Write the failing tests (append to `cmd/cm/main_test.go`)**

Add `"encoding/json"` to the test file's imports. Then:

```go
func TestPRSendsJSONBody(t *testing.T) {
	var got http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeBody(w, `{"url":"https://github.com/o/r/pull/3","repo":"o/r","head":"magister/r1"}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	code := dispatch([]string{"pr", "r1", "--title", "My PR", "--base", "main", "--draft"}, srv.URL, &out)
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v1/runs/r1/pr" {
		t.Errorf("request = %s %s, want POST /v1/runs/r1/pr", got.Method, got.URL.Path)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if sent["title"] != "My PR" || sent["base"] != "main" || sent["draft"] != true {
		t.Errorf("body = %v, want title/base/draft set", sent)
	}
	if !strings.Contains(out.String(), "https://github.com/o/r/pull/3") {
		t.Errorf("output missing PR url: %q", out.String())
	}
}

func TestPRRequiresRun(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"pr"}, "http://x", &out); code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}

func TestPRNon200PrintsError(t *testing.T) {
	var got http.Request
	srv := fakeAPI(t, http.StatusConflict, `{"error":"PR already exists for magister/r1: https://github.com/o/r/pull/9"}`, &got)
	defer srv.Close()
	var out bytes.Buffer
	code := dispatch([]string{"pr", "r1"}, srv.URL, &out)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "pull/9") {
		t.Errorf("output should surface the existing PR url, got %q", out.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/cm/ -run TestPR`
Expected: FAIL — unknown command `pr` (exit 2 for the happy path; body never sent).

- [ ] **Step 3: Add the dispatch case + usage in `cmd/cm/main.go`**

Update the usage line (line ~33) to include `pr`:

```go
	fmt.Fprintln(out, "usage: cm <run|ls|get|watch|approve|reject|cancel|push|pr> ...")
```

Add to the `switch args[0]` (after the `push` case):

```go
	case "pr":
		return c.pr(args[1:], out)
```

- [ ] **Step 4: Add the `pr` method in `cmd/cm/main.go`**

```go
func (c *client) pr(args []string, out io.Writer) int {
	var run string
	body := map[string]any{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--draft":
			body["draft"] = true
		case "--remote", "--as", "--step", "--base", "--title", "--body":
			flag := args[i]
			i++
			if i >= len(args) {
				fmt.Fprintf(out, "usage: %s requires a value\n", flag)
				return 2
			}
			body[flag[2:]] = args[i] // strip "--"
		default:
			run = args[i]
		}
	}
	if run == "" {
		fmt.Fprintln(out, "usage: cm pr <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft]")
		return 2
	}
	payload, _ := json.Marshal(body)
	resp, err := c.http.Post(c.base+"/v1/runs/"+run+"/pr", "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintln(out, "pr:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	var pr struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		fmt.Fprintln(out, "pr: decode response:", err)
		return 1
	}
	fmt.Fprintln(out, "opened", pr.URL)
	return 0
}
```

(`body[flag[2:]]` strips the leading `--` without needing the `strings` package.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./cmd/cm/ -run TestPR -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Commit**

```bash
gofmt -w cmd/cm/main.go cmd/cm/main_test.go
git add cmd/cm/main.go cmd/cm/main_test.go
git commit -m "feat(cm): pr <run> opens a pull request for a finished run"
```

---

### Task 7: Doc note + full-suite verification + manual proof

**Files:**
- Modify: `.claude/skills/running-the-orchestrator/SKILL.md` (a `cm pr` note)

No daemon change is required: `Supervisor.hostRunner()` defaults to `host.New()` (the `gh` CLI on PATH), so the real `magisterd` uses `gh` with zero wiring — mirroring how `cm push` needed no special construction. Tests inject `sup.Host`.

- [ ] **Step 1: Add a `cm pr` note to the run skill**

In `.claude/skills/running-the-orchestrator/SKILL.md`, next to the `cm push` documentation, add a short note:

```markdown
- `cm pr <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft]`
  opens a GitHub Pull Request on the pushed `magister/<runID>` branch of a succeeded
  external-repo run (run `cm push <run>` first). Uses the `gh` CLI with ambient auth
  (no token handling); `owner/repo` is parsed from the source's origin remote. A PR
  that already exists is reported as a 409 with its URL.
```

- [ ] **Step 2: Format check (ours only — do NOT touch gemini.go)**

Run: `gofmt -l internal/host internal/supervisor internal/api cmd/cm`
Expected: no output (the pre-existing dirty `internal/executor/gemini.go` is unrelated and out of scope).

- [ ] **Step 3: Vet**

Run: `go vet ./...`
Expected: clean (no output).

- [ ] **Step 4: Full race suite**

Run: `go test -race ./...`
Expected: PASS across all packages (the new `internal/host` plus the modified `supervisor`/`api`/`cmd/cm`). Git-needing tests skip gracefully where git is absent.

- [ ] **Step 5: Commit the doc note**

```bash
git add .claude/skills/running-the-orchestrator/SKILL.md
git commit -m "docs(external-repo): cm pr note in run skill"
```

- [ ] **Step 6: Manual proof (human-run; requires a throwaway GitHub repo, `gh` authed as `jeremN`)**

This is the end-to-end confirmation against a real host. Run by the operator:

```bash
# 0. A throwaway repo you own, cloned locally as the source; its origin = the GitHub repo.
#    Start the daemon (see the run skill) and export MAGISTER_ADDR=http://127.0.0.1:8080
cm run flows/external-repo.yaml --repo /abs/path/to/throwaway-clone --base HEAD   # → <run>
cm get <run>            # wait for status: succeeded
cm push <run>           # → pushed step/integrate → magister/<run> on origin
cm pr <run>             # → opened https://github.com/<you>/<repo>/pull/N
cm pr <run>             # → 409: PR already exists for magister/<run>: https://…/pull/N
```

Expected: the first `cm pr` opens a PR whose head is `magister/<run>`, base is the repo's default branch, and the body is the generated run summary; the second reports the existing PR's URL as a conflict. (No checkbox — this is operator verification, not an automated test.)

---

## Self-Review

**Spec coverage** (each spec section → task):
- §Surface (`cm pr`, `POST .../pr`, success output) → Tasks 5, 6.
- §`internal/host` (`ParseRemote`, `Runner` with 3 ops, `--flag=value` hardening, default `gh`) → Tasks 1, 2.
- §`Supervisor.PR` (validate → head → ParseRemote → parse flow + pickResultStep → title/body → existing-PR 409 → create → diagnose) → Tasks 3, 4; `generatePRBody`/`defaultPRTitle` → Task 3.
- §API (handler, route, DTOs, status mapping) → Task 5.
- §CLI → Task 6.
- §Daemon wiring → Task 7 (resolved by the defaulting accessor — no daemon change).
- §Testing (host table + stub argv; supervisor seeded + error paths; handler; cm; manual proof) → Tasks 1-7.
- §Security (ambient auth, argv hardening, no SSRF, read-only source, no scratch dep) → Global Constraints + Tasks 2-3.
- §Carried (30s timeout wraps `/pr`) → noted in spec; no code (accepted follow-up).

**Placeholder scan:** none — every step has concrete code/commands.

**Type consistency:** `PROpts`/`PRResult`/`PRError`/`prErr` defined in Task 3, used in Tasks 4-5; `host.Runner`/`CreateOpts`/`ExistingOpenPR`/`CreatePR`/`BranchExists`/`ParseRemote`/`New` defined in Tasks 1-2, used in Tasks 3-5; `prRequest`/`prResponse` defined and used in Task 5; `Supervisor.Host`/`hostRunner()` defined in Task 3, defaulted in Task 7. `pickResultStep`/`stepBranch` are existing `supervisor.go` symbols reused unchanged. Consistent.
