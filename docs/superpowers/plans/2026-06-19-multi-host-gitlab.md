# Multi-host delivery (GitLab via glab) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the PR/MR-opening step host-pluggable and add GitLab (gitlab.com) delivery via the `glab` CLI, so `cm pr`/`cm ship` work on GitLab remotes (push already does).

**Architecture:** A small `host.Host` interface with two CLI-backed adapters — `GH` (gh, GitHub) and a new `GL` (glab, GitLab) — selected by a `host.For(hostname)` factory. `ParseRemote` recognizes both canonical hosts. The supervisor parses the remote, picks the host, and calls the interface — no GitHub-specific knowledge remains in the supervisor. `cm push` is untouched (already host-agnostic).

**Tech Stack:** Go 1.22 stdlib only; the external `glab` CLI (v1.101.0, like `gh` — not a Go dependency); ambient CLI auth (zero token handling); env-driven fake-CLI stubs for offline tests.

## Global Constraints

- Go 1.22; **stdlib-only, no new Go dependency** (`glab` is an external binary like `gh`); **no DB migration; no new HTTP route; no engine change.**
- **GitLab via `glab`, gitlab.com only**, **2-segment `owner/repo` only** (subgroups, self-hosted, Bitbucket are out of scope — noted follow-ups).
- Zero token handling — ambient `gh`/`glab` auth only.
- The PR step stays **post-run, store-driven**, touching no scratch clone (the probe-gated scratch fallback is NOT implemented here; it's a follow-up only if the live proof forces it).
- User-facing output stays **host-neutral** (`cm pr`/`cm ship` print the URL; no per-host wording).
- Hardening posture (both adapters): neutral `os.TempDir` cwd, separate stdout/stderr, single-token `--flag=value`, `safeSeg`-guarded owner/repo, `#nosec G204` on the exec.
- Commits: single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. `gofmt`/`go vet`/`go test -race ./...` clean before merge.

## File Structure

- `internal/host/gh.go` — **modify**: widen `ParseRemote` to `{github.com, gitlab.com}`; rename the gh adapter `Runner`→`GH`.
- `internal/host/host.go` — **create**: the `Host` interface + the `For(hostname)` factory.
- `internal/host/gl.go` — **create**: the `GL` (glab) adapter.
- `internal/host/gh_test.go` — **modify**: flip gitlab `ParseRemote` cases to accepted; move the unsupported probe to `bitbucket.org`; `Runner`→`GH`.
- `internal/host/gl_test.go` — **create**: `GL` adapter unit tests against the fake-glab stub.
- `internal/host/testdata/fake-glab` — **create** (mode 0755): env-driven glab stub.
- `internal/supervisor/supervisor.go` — **modify**: `Host *host.Runner` → `HostFor func(host string) (host.Host, error)`; `hostRunner()` → `hostFor(host)`.
- `internal/supervisor/pr.go` — **modify**: `prCore` uses the parsed host → `hostFor(host)`.
- `internal/supervisor/pr_test.go` — **modify**: migrate the `Host` injection to `HostFor` (a `stubHostFor` helper); flip `TestPRUnsupportedHost` to `bitbucket.org`; add the GitLab integration test + a `glabStub` helper.
- `internal/supervisor/ship_test.go` — **modify**: migrate the one `sup.Host = …` injection to `HostFor`.

---

### Task 1: `ParseRemote` accepts gitlab.com

**Files:**
- Modify: `internal/host/gh.go` (the host-validity gate in `ParseRemote`)
- Modify: `internal/host/gh_test.go` (`TestParseRemote`, `TestParseRemoteStripsPort`)
- Modify: `internal/supervisor/pr_test.go` (`TestPRUnsupportedHost`)

**Interfaces:**
- Consumes: nothing.
- Produces: `host.ParseRemote(url)` now returns `(host, owner, repo, nil)` for `gitlab.com` URLs (host = `"gitlab.com"`), and still errors for any host other than github.com/gitlab.com. Signature unchanged: `ParseRemote(remoteURL string) (host, owner, repo string, err error)`.

- [ ] **Step 1: Update the host-validity gate**

In `internal/host/gh.go`, replace this block in `ParseRemote`:

```go
	if host != "github.com" {
		return "", "", "", fmt.Errorf("unsupported host %q (only github.com is supported)", host)
	}
	return host, owner, repo, nil
```

with:

```go
	if !supportedHost(host) {
		return "", "", "", fmt.Errorf("unsupported host %q (supported: github.com, gitlab.com)", host)
	}
	return host, owner, repo, nil
```

Then add this helper near `safeSeg` in the same file:

```go
// supportedHost reports whether the host has a delivery adapter (host.For).
func supportedHost(host string) bool {
	return host == "github.com" || host == "gitlab.com"
}
```

- [ ] **Step 2: Update the host-package ParseRemote tests**

In `internal/host/gh_test.go`, in `TestParseRemote`, change the gitlab case from rejected to accepted and add a genuinely-unsupported host. Replace this line:

```go
		{"https://gitlab.com/o/r.git", "", "", false}, // unsupported host
```

with:

```go
		{"https://gitlab.com/o/r.git", "o", "r", true},        // gitlab now supported
		{"git@gitlab.com:o/r.git", "o", "r", true},            // gitlab scp-form
		{"ssh://git@gitlab.com/o/r.git", "o", "r", true},      // gitlab ssh-form
		{"https://bitbucket.org/o/r.git", "", "", false},      // still unsupported host
```

In `TestParseRemoteStripsPort`, change the gitlab+port case from rejected to accepted and add a still-rejected host+port. Replace:

```go
		{"ssh://git@gitlab.com:22/o/r.git", "", "", false}, // other host + port still rejected
```

with:

```go
		{"ssh://git@gitlab.com:22/o/r.git", "o", "r", true},        // gitlab + port now supported
		{"ssh://git@bitbucket.org:22/o/r.git", "", "", false},      // unsupported host + port still rejected
```

- [ ] **Step 3: Flip the supervisor's unsupported-host test**

In `internal/supervisor/pr_test.go`, `TestPRUnsupportedHost` currently uses a gitlab.com origin (now supported). Change its origin to a still-unsupported host. Replace:

```go
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://gitlab.com/o/r.git"))
```

with:

```go
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://bitbucket.org/o/r.git"))
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/host/ ./internal/supervisor/`
Expected: PASS. (`ParseRemote` accepts gitlab.com; the supervisor's unsupported-host test now uses bitbucket.org. A gitlab PR still routes to the gh runner at this point — no test exercises that path, so the suite is green.)

- [ ] **Step 5: Commit**

```bash
git add internal/host/gh.go internal/host/gh_test.go internal/supervisor/pr_test.go
git commit -m "feat(host): ParseRemote recognizes gitlab.com"
```

---

### Task 2: `Host` interface + `GH` rename + `host.For` factory + supervisor `HostFor` rewire

**Files:**
- Create: `internal/host/host.go` (the `Host` interface + `For` factory)
- Modify: `internal/host/gh.go` (rename `Runner`→`GH`)
- Modify: `internal/host/gh_test.go` (`&Runner{`→`&GH{`)
- Modify: `internal/supervisor/supervisor.go` (`Host` field → `HostFor`; `hostRunner`→`hostFor`)
- Modify: `internal/supervisor/pr.go` (`prCore` uses the parsed host)
- Modify: `internal/supervisor/pr_test.go` (`stubHostFor` helper; migrate `newPRSup`)
- Modify: `internal/supervisor/ship_test.go` (migrate the one `sup.Host` injection)

**Interfaces:**
- Consumes: `host.ParseRemote` (Task 1).
- Produces:
  - `host.Host` interface: `ExistingOpenPR(ctx, owner, repo, head string) (url string, exists bool, err error)`, `CreatePR(ctx context.Context, o CreateOpts) (url string, err error)`, `BranchExists(ctx context.Context, owner, repo, branch string) bool`.
  - `host.GH` struct (the former `Runner`); `host.New() *GH` unchanged in behavior.
  - `host.For(host string) (Host, error)` — `github.com`→`New()`; any other host (incl. gitlab.com **for now** — `GL` is added in Task 3)→`fmt.Errorf("unsupported host %q", host)`.
  - `Supervisor.HostFor func(host string) (host.Host, error)` field (nil → `host.For`); `(*Supervisor).hostFor(host string) (host.Host, error)`.

- [ ] **Step 1: Create the `Host` interface and `For` factory**

Create `internal/host/host.go`:

```go
package host

import (
	"context"
	"fmt"
)

// Host opens and inspects change requests (PRs/MRs) on a git host via its CLI.
// Implemented by GH (GitHub, gh) and GL (GitLab, glab). Auth is ambient (the CLI's
// own credential store); no token is handled here.
type Host interface {
	// ExistingOpenPR returns the URL of an open change request whose head is `head`,
	// if one exists.
	ExistingOpenPR(ctx context.Context, owner, repo, head string) (url string, exists bool, err error)
	// CreatePR opens a change request and returns its URL.
	CreatePR(ctx context.Context, o CreateOpts) (url string, err error)
	// BranchExists reports whether branch exists on owner/repo. Any failure reads as
	// "absent"; it only refines a CreatePR failure into a "run cm push first" message.
	BranchExists(ctx context.Context, owner, repo, branch string) bool
}

// For returns the delivery adapter for a host (as returned by ParseRemote). Only
// the canonical hosts are supported; ParseRemote already rejects anything else, so
// this is a defensive belt-and-suspenders.
func For(host string) (Host, error) {
	switch host {
	case "github.com":
		return New(), nil
	default:
		return nil, fmt.Errorf("unsupported host %q (supported: github.com, gitlab.com)", host)
	}
}
```

(Note: `gitlab.com` falls through to the error here; Task 3 adds the `gitlab.com → NewGitLab()` case once `GL` exists. No test sends a gitlab URL through `For` until Task 4.)

- [ ] **Step 2: Rename the gh adapter `Runner` → `GH`**

In `internal/host/gh.go`, rename the struct and its receivers. Replace:

```go
// Runner shells the gh CLI. Bin is the command name (default "gh"); tests set it to an
// absolute path to a stub. All methods use exec.CommandContext (no shell) and the
// single-token --flag=value form so a value can never be parsed as a flag.
type Runner struct {
	Bin string
}

// New returns a Runner backed by the gh CLI on PATH.
func New() *Runner { return &Runner{Bin: "gh"} }

func (r *Runner) bin() string {
```

with:

```go
// GH shells the gh CLI (GitHub adapter). Bin is the command name (default "gh"); tests
// set it to an absolute path to a stub. All methods use exec.CommandContext (no shell)
// and the single-token --flag=value form so a value can never be parsed as a flag.
type GH struct {
	Bin string
}

// compile-time check that GH satisfies the Host interface.
var _ Host = (*GH)(nil)

// New returns a GH adapter backed by the gh CLI on PATH.
func New() *GH { return &GH{Bin: "gh"} }

func (r *GH) bin() string {
```

Then change the remaining four method receivers in `gh.go` from `(r *Runner)` to `(r *GH)`: `run`, `ExistingOpenPR`, `CreatePR`, `BranchExists`. (Use find-and-replace `(r *Runner)` → `(r *GH)` within `gh.go`.)

- [ ] **Step 3: Update the gh adapter tests for the rename**

In `internal/host/gh_test.go`, replace every `&Runner{Bin: …}` with `&GH{Bin: …}` (4 occurrences: `TestRunnerCreatePR`, `TestRunnerCreatePROmitsBaseWhenEmpty`, `TestRunnerCreatePRFailureSurfacesStderr`, `TestRunnerExistingOpenPR`, `TestRunnerBranchExists` — find-and-replace `&Runner{` → `&GH{`).

- [ ] **Step 4: Run the host package to confirm the rename + interface compile**

Run: `go test ./internal/host/`
Expected: PASS (rename is mechanical; `GH` satisfies `Host`; `For` compiles).

- [ ] **Step 5: Rewire the supervisor from `Host` to `HostFor`**

In `internal/supervisor/supervisor.go`, replace the field:

```go
	// Host is the gh-backed PR client; nil → a default host.New() (the gh CLI on PATH).
	Host *host.Runner
```

with (note: the func-type field has **no parameter name** — naming it `host` would shadow the `host` package in the `host.Host` return type and fail to compile):

```go
	// HostFor returns the delivery adapter for a host (github.com→gh, gitlab.com→glab).
	// nil → the default host.For (the real CLIs on PATH). Tests inject fake-CLI adapters.
	HostFor func(string) (host.Host, error)
```

Then replace the accessor (the method parameter is named `hostName`, not `host`, so it does not shadow the package):

```go
func (s *Supervisor) hostRunner() *host.Runner {
	if s.Host != nil {
		return s.Host
	}
	return host.New()
}
```

with:

```go
func (s *Supervisor) hostFor(hostName string) (host.Host, error) {
	if s.HostFor != nil {
		return s.HostFor(hostName)
	}
	return host.For(hostName)
}
```

The `"concentus/internal/host"` import stays unaliased; no other change to `supervisor.go` is needed.

- [ ] **Step 6: Use the parsed host in `prCore`**

In `internal/supervisor/pr.go`, in `prCore`, replace:

```go
	_, owner, repo, err := host.ParseRemote(remoteURL)
	if err != nil {
		return PRResult{}, false, prErr(http.StatusBadRequest, "%v", err)
	}
```

with:

```go
	hostName, owner, repo, err := host.ParseRemote(remoteURL)
	if err != nil {
		return PRResult{}, false, prErr(http.StatusBadRequest, "%v", err)
	}
```

Then replace:

```go
	runner := s.hostRunner()
```

with:

```go
	runner, err := s.hostFor(hostName)
	if err != nil {
		return PRResult{}, false, prErr(http.StatusBadRequest, "%v", err)
	}
```

(`pr.go` still imports `concentus/internal/host` for `host.ParseRemote`/`host.CreateOpts`; that's unaffected by the `supervisor.go` alias — Go import names are per-file.)

- [ ] **Step 7: Migrate the test injections to `HostFor`**

In `internal/supervisor/pr_test.go`, add a helper (near `ghStub`):

```go
// stubHostFor routes a Supervisor's HostFor to fake-CLI adapters per host. github.com
// uses the fake-gh stub; any other host falls through to the real host.For (which
// errors for unsupported hosts). Task 4 extends this with gitlab.com → the glab stub.
func stubHostFor(t *testing.T) func(string) (host.Host, error) {
	t.Helper()
	gh := &host.GH{Bin: ghStub(t)}
	return func(h string) (host.Host, error) {
		switch h {
		case "github.com":
			return gh, nil
		default:
			return host.For(h)
		}
	}
}
```

Then in `newPRSup`, replace:

```go
	sup.Host = &host.Runner{Bin: ghStub(t)}
```

with:

```go
	sup.HostFor = stubHostFor(t)
```

In `internal/supervisor/ship_test.go`, replace the one direct injection:

```go
	sup.Host = &host.Runner{Bin: ghStub(t)}
```

with:

```go
	sup.HostFor = stubHostFor(t)
```

- [ ] **Step 8: Run the supervisor + host packages**

Run: `go test ./internal/host/ ./internal/supervisor/`
Expected: PASS. The GitHub PR/Ship behavior is byte-for-byte unchanged (same fake-gh stub, same argv); only the injection seam moved from a struct field to a factory func.

- [ ] **Step 9: Build the whole tree (the rename touches a public type)**

Run: `go build ./... && go vet ./...`
Expected: build + vet clean (no other package references `host.Runner`).

- [ ] **Step 10: Commit**

```bash
git add internal/host/host.go internal/host/gh.go internal/host/gh_test.go internal/supervisor/supervisor.go internal/supervisor/pr.go internal/supervisor/pr_test.go internal/supervisor/ship_test.go
git commit -m "refactor(host): Host interface + For factory; supervisor picks host by remote"
```

---

### Task 3: `GL` glab adapter + fake-glab stub + wire into `For`

**Files:**
- Create: `internal/host/gl.go` (the `GL` adapter)
- Create: `internal/host/testdata/fake-glab` (mode 0755)
- Create: `internal/host/gl_test.go`
- Modify: `internal/host/host.go` (wire `gitlab.com → NewGitLab()`)

**Interfaces:**
- Consumes: `host.Host`, `host.CreateOpts`, `lastURL` (Task 2 / existing in gh.go).
- Produces: `host.GL` struct + `host.NewGitLab() *GL`, satisfying `Host`; `host.For("gitlab.com")` now returns a `*GL`.

- [ ] **Step 1: Write the fake-glab stub**

Create `internal/host/testdata/fake-glab` (it must be committed executable — `chmod 0755` after creating):

```sh
#!/bin/sh
# fake-glab — offline glab stub for tests. Behavior is env-driven (FAKE_GLAB_*).
# Records argv (one arg per line, '---' per call) to $FAKE_GLAB_ARGV_FILE.
if [ -n "$FAKE_GLAB_ARGV_FILE" ]; then
  for a in "$@"; do printf '%s\n' "$a"; done >> "$FAKE_GLAB_ARGV_FILE"
  printf -- '---\n' >> "$FAKE_GLAB_ARGV_FILE"
fi
case "$1" in
  mr)
    case "$2" in
      list)
        if [ -n "$FAKE_GLAB_EXISTING_MR" ]; then
          printf '[{"web_url":"%s"}]\n' "$FAKE_GLAB_EXISTING_MR"
        else
          printf '[]\n'
        fi
        ;;
      create)
        if [ -n "$FAKE_GLAB_CREATE_FAIL" ]; then
          printf '%s\n' "$FAKE_GLAB_CREATE_FAIL" 1>&2
          exit 1
        fi
        printf '%s\n' "${FAKE_GLAB_MR_URL:-https://gitlab.com/o/r/-/merge_requests/1}"
        ;;
      *) printf 'fake-glab: unexpected mr subcommand %s\n' "$2" 1>&2; exit 2 ;;
    esac
    ;;
  api)
    if [ -n "$FAKE_GLAB_BRANCH_MISSING" ]; then
      printf 'glab: 404 Not Found\n' 1>&2
      exit 1
    fi
    ;;
  *) printf 'fake-glab: unexpected command %s\n' "$1" 1>&2; exit 2 ;;
esac
```

Then make it executable and confirm:

```bash
chmod 0755 internal/host/testdata/fake-glab
test -x internal/host/testdata/fake-glab && echo OK
```

- [ ] **Step 2: Write the failing GL adapter tests**

Create `internal/host/gl_test.go`:

```go
package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGLCreatePR(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_GLAB_ARGV_FILE", argv)
	t.Setenv("FAKE_GLAB_MR_URL", "https://gitlab.com/o/r/-/merge_requests/9")
	r := &GL{Bin: stubPath(t, "fake-glab")}
	url, err := r.CreatePR(context.Background(), CreateOpts{
		Owner: "o", Repo: "r", Head: "magister/x", Base: "main",
		Title: "the title", Body: "the body", Draft: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if url != "https://gitlab.com/o/r/-/merge_requests/9" {
		t.Errorf("url = %q", url)
	}
	got, _ := os.ReadFile(argv)
	for _, want := range []string{
		"mr", "create", "--repo=o/r", "--source-branch=magister/x",
		"--target-branch=main", "--title=the title", "--description=the body",
		"--draft", "--yes",
	} {
		if !strings.Contains(string(got), want+"\n") {
			t.Errorf("argv missing %q; got:\n%s", want, got)
		}
	}
	// must NOT push or auto-fill — the branch was already delivered by cm push.
	for _, bad := range []string{"--fill", "--push"} {
		if strings.Contains(string(got), bad+"\n") {
			t.Errorf("argv must not contain %q; got:\n%s", bad, got)
		}
	}
}

func TestGLCreatePROmitsBaseWhenEmpty(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_GLAB_ARGV_FILE", argv)
	r := &GL{Bin: stubPath(t, "fake-glab")}
	if _, err := r.CreatePR(context.Background(), CreateOpts{Owner: "o", Repo: "r", Head: "h", Title: "t", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(argv); strings.Contains(string(got), "--target-branch=") {
		t.Errorf("expected no --target-branch; got:\n%s", got)
	}
}

func TestGLCreatePRFailureSurfacesStderr(t *testing.T) {
	t.Setenv("FAKE_GLAB_CREATE_FAIL", "boom: bad target")
	r := &GL{Bin: stubPath(t, "fake-glab")}
	if _, err := r.CreatePR(context.Background(), CreateOpts{Owner: "o", Repo: "r", Head: "h", Title: "t", Body: "b"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want failure surfacing stderr, got %v", err)
	}
}

func TestGLExistingOpenMR(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_GLAB_ARGV_FILE", argv)
	r := &GL{Bin: stubPath(t, "fake-glab")}
	if url, ok, err := r.ExistingOpenPR(context.Background(), "o", "r", "magister/x"); err != nil || ok || url != "" {
		t.Fatalf("want none, got url=%q ok=%v err=%v", url, ok, err)
	}
	t.Setenv("FAKE_GLAB_EXISTING_MR", "https://gitlab.com/o/r/-/merge_requests/3")
	url, ok, err := r.ExistingOpenPR(context.Background(), "o", "r", "magister/x")
	if err != nil || !ok || url != "https://gitlab.com/o/r/-/merge_requests/3" {
		t.Fatalf("want existing, got url=%q ok=%v err=%v", url, ok, err)
	}
	// the list call must scope by repo and source branch.
	got, _ := os.ReadFile(argv)
	for _, want := range []string{"mr", "list", "--repo=o/r", "--source-branch=magister/x", "--output=json"} {
		if !strings.Contains(string(got), want+"\n") {
			t.Errorf("argv missing %q; got:\n%s", want, got)
		}
	}
}

func TestGLBranchExists(t *testing.T) {
	r := &GL{Bin: stubPath(t, "fake-glab")}
	if !r.BranchExists(context.Background(), "o", "r", "magister/x") {
		t.Error("want exists")
	}
	t.Setenv("FAKE_GLAB_BRANCH_MISSING", "1")
	if r.BranchExists(context.Background(), "o", "r", "magister/x") {
		t.Error("want missing")
	}
}

func TestForRoutesGitLab(t *testing.T) {
	h, err := For("gitlab.com")
	if err != nil {
		t.Fatalf("For(gitlab.com): %v", err)
	}
	if _, ok := h.(*GL); !ok {
		t.Errorf("For(gitlab.com) = %T, want *GL", h)
	}
}
```

(`stubPath` already exists in `gh_test.go`, same `package host`.)

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/host/ -run 'GL|ForRoutesGitLab'`
Expected: FAIL — `GL` and `NewGitLab` undefined (compile error).

- [ ] **Step 4: Implement the GL adapter**

Create `internal/host/gl.go`:

```go
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

// GL shells the glab CLI (GitLab adapter). It mirrors GH's hardening: neutral cwd,
// separate stdout/stderr, single-token --flag=value, no shell. Auth is ambient
// (glab's own token store); no token is handled here. GitLab calls a PR a "merge
// request" (mr); the Host method names stay "PR" for a host-neutral interface.
type GL struct {
	Bin string
}

// compile-time check that GL satisfies the Host interface.
var _ Host = (*GL)(nil)

// NewGitLab returns a GL adapter backed by the glab CLI on PATH.
func NewGitLab() *GL { return &GL{Bin: "glab"} }

func (r *GL) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return "glab"
}

// run executes glab from a neutral working directory (so an ambient repo's git config
// cannot influence glab) and returns stdout and stderr separately.
func (r *GL) run(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	// #nosec G204 -- glab + args are built from charset-guarded owner/repo, ref-shaped
	// head/base, and --flag=value single tokens; no shell.
	cmd := exec.CommandContext(ctx, r.bin(), args...)
	cmd.Dir = os.TempDir()
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err = cmd.Run()
	return so.String(), se.String(), err
}

// ExistingOpenPR returns the URL of an open merge request whose source branch is
// `head`, if one exists.
func (r *GL) ExistingOpenPR(ctx context.Context, owner, repo, head string) (string, bool, error) {
	so, se, err := r.run(ctx, "mr", "list",
		"--repo="+owner+"/"+repo, "--source-branch="+head, "--output=json")
	if err != nil {
		return "", false, fmt.Errorf("glab mr list: %w: %s", err, strings.TrimSpace(se))
	}
	var mrs []struct {
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(so)), &mrs); err != nil {
		return "", false, fmt.Errorf("glab mr list: parse json: %w", err)
	}
	if len(mrs) == 0 {
		return "", false, nil
	}
	return mrs[0].WebURL, true, nil
}

// CreatePR opens a merge request and returns its URL. It never passes --fill/--push:
// the source branch was already delivered by cm push. --yes skips glab's confirmation
// prompt (glab is interactive by default, unlike gh).
func (r *GL) CreatePR(ctx context.Context, o CreateOpts) (string, error) {
	args := []string{"mr", "create",
		"--repo=" + o.Owner + "/" + o.Repo,
		"--source-branch=" + o.Head,
		"--title=" + o.Title,
		"--description=" + o.Body,
		"--yes",
	}
	if o.Base != "" {
		args = append(args, "--target-branch="+o.Base)
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
		return "", fmt.Errorf("glab mr create: %w: %s", err, out)
	}
	url := lastURL(so)
	if url == "" {
		return "", fmt.Errorf("glab mr create: no MR url in output: %s", strings.TrimSpace(so))
	}
	return url, nil
}

// BranchExists reports whether branch exists on owner/repo (via the GitLab API). The
// project is addressed by its URL-encoded full path (owner%2Frepo) and a branch's
// slashes are likewise encoded. Any glab failure (incl. 404) reads as "absent" — it
// only refines a CreatePR failure into a "run cm push first" message.
func (r *GL) BranchExists(ctx context.Context, owner, repo, branch string) bool {
	project := owner + "%2F" + repo
	br := strings.ReplaceAll(branch, "/", "%2F")
	_, _, err := r.run(ctx, "api", "projects/"+project+"/repository/branches/"+br)
	return err == nil
}
```

- [ ] **Step 5: Wire gitlab.com into `For`**

In `internal/host/host.go`, add the gitlab case to the `For` switch:

```go
	switch host {
	case "github.com":
		return New(), nil
	case "gitlab.com":
		return NewGitLab(), nil
	default:
		return nil, fmt.Errorf("unsupported host %q (supported: github.com, gitlab.com)", host)
	}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/host/ -run 'GL|ForRoutesGitLab'`
Expected: PASS.

- [ ] **Step 7: Run the whole host package**

Run: `go test -race ./internal/host/`
Expected: PASS (GH unchanged, GL added, `For` routes both).

- [ ] **Step 8: Commit**

```bash
git add internal/host/gl.go internal/host/gl_test.go internal/host/testdata/fake-glab internal/host/host.go
git commit -m "feat(host): GL adapter opens GitLab merge requests via glab"
```

---

### Task 4: Supervisor GitLab integration test

**Files:**
- Modify: `internal/supervisor/pr_test.go` (extend `stubHostFor` to route gitlab.com; add a `glabStub` helper + the GitLab PR test)

**Interfaces:**
- Consumes: `Supervisor.PR` / `prCore` (Task 2), `host.GL` + the fake-glab stub (Task 3).
- Produces: a regression test proving a gitlab-remote run routes through `prCore` to the glab adapter and yields the MR URL.

- [ ] **Step 1: Add a glab-stub path helper and route it in `stubHostFor`**

In `internal/supervisor/pr_test.go`, add a `glabStub` helper next to `ghStub`:

```go
// glabStub returns the absolute path to the shared fake-glab stub in internal/host.
func glabStub(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "host", "testdata", "fake-glab"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("fake-glab stub missing: %v", err)
	}
	return abs
}
```

Then extend `stubHostFor` (from Task 2) to also route gitlab.com:

```go
func stubHostFor(t *testing.T) func(string) (host.Host, error) {
	t.Helper()
	gh := &host.GH{Bin: ghStub(t)}
	gl := &host.GL{Bin: glabStub(t)}
	return func(h string) (host.Host, error) {
		switch h {
		case "github.com":
			return gh, nil
		case "gitlab.com":
			return gl, nil
		default:
			return host.For(h)
		}
	}
}
```

- [ ] **Step 2: Write the failing GitLab integration test**

In `internal/supervisor/pr_test.go`, add:

```go
func TestPROpensMergeRequestGitLab(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_GLAB_ARGV_FILE", argv)
	t.Setenv("FAKE_GLAB_MR_URL", "https://gitlab.com/test-owner/test-repo/-/merge_requests/7")
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://gitlab.com/test-owner/test-repo.git"))

	res, err := sup.PR(context.Background(), "r1", PROpts{})
	if err != nil {
		t.Fatalf("pr: %v", err)
	}
	if res.URL != "https://gitlab.com/test-owner/test-repo/-/merge_requests/7" {
		t.Errorf("url = %q", res.URL)
	}
	if res.Repo != "test-owner/test-repo" {
		t.Errorf("repo = %q", res.Repo)
	}
	if res.Head != "magister/r1" {
		t.Errorf("head = %q", res.Head)
	}
	got, _ := os.ReadFile(argv)
	for _, want := range []string{"mr", "create", "--repo=test-owner/test-repo", "--source-branch=magister/r1"} {
		if !strings.Contains(string(got), want+"\n") {
			t.Errorf("argv missing %q; got:\n%s", want, got)
		}
	}
}
```

(`srcWithGHOrigin` just sets an origin URL — the name is GitHub-historical; it works for any URL.)

- [ ] **Step 3: Run the test to verify it fails, then passes**

Run: `go test ./internal/supervisor/ -run TestPROpensMergeRequestGitLab`
Expected: PASS (the routing + adapter already exist from Tasks 2–3; this test exercises them end-to-end through `prCore`). If it fails, the routing is wrong — fix before proceeding.

- [ ] **Step 4: Run the full suite + vet + gofmt**

Run: `gofmt -l internal && go vet ./... && go test -race ./...`
Expected: `gofmt -l` empty; vet clean; all packages PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/supervisor/pr_test.go
git commit -m "test(supervisor): GitLab run routes prCore to the glab adapter"
```

---

## Self-Review

**1. Spec coverage** (against `docs/superpowers/specs/2026-06-19-multi-host-gitlab-design.md`):
- Host interface + per-host adapter + `For` factory → Tasks 2 (interface/factory/GH) + 3 (GL). ✓
- `ParseRemote` widened to `{github.com, gitlab.com}`, 2-segment only → Task 1 (`supportedHost`; `splitOwnerRepo` untouched). ✓
- GH adapter renamed `GH`, behavior unchanged → Task 2. ✓
- GL adapter: `mr list -F json`/`mr create --yes` no `--fill`/`--push`/`api …%2F…/branches` → Task 3. ✓
- Supervisor `HostFor` injection, daemon needs no wiring (nil→`host.For`), `prCore` uses parsed host → Task 2. ✓
- User-facing output host-neutral (no cm/handler/DTO change) → no task touches `cmd/cm` or the API; ✓ by omission.
- Error handling: unsupported host 400 (ParseRemote), CLI failure 502, existing-MR 409/idempotent, branch-not-pushed 409 → unchanged `prCore` paths + Task 1's message; ✓.
- Testing: ParseRemote gitlab cases, fake-glab stub, GL unit tests, supervisor gitlab integration → Tasks 1, 3, 4. ✓
- Global constraints (no new dep, no migration, no route, no engine change, stdlib, ambient auth) → held; no task adds a dep/migration/route. ✓
- Out of scope (Bitbucket, self-hosted, subgroups, scratch fallback) → not implemented. ✓

**2. Placeholder scan:** No TBD/TODO; every code step has complete code. The two build-driven notes (the `hostpkg` import alias in Task 2 Step 5; `chmod 0755` on the stub in Task 3 Step 1) are concrete instructions, not placeholders. ✓

**3. Type consistency:** `Host` interface methods (`ExistingOpenPR`/`CreatePR`/`BranchExists`) are identical across the interface (Task 2), `GH` (renamed, Task 2), and `GL` (Task 3). `CreateOpts` is reused unchanged. `host.For(host string) (Host, error)` matches its call in `hostFor` (Task 2) and the `For` definition (Tasks 2→3). `Supervisor.HostFor func(host string) (host.Host, error)` matches `hostFor`, `stubHostFor`, and the test injections (Tasks 2, 4). `NewGitLab() *GL` and `New() *GH` match their `For` call sites. The fake-glab `FAKE_GLAB_*` env names match between the stub (Task 3) and the GL tests (Task 3) and the supervisor test (Task 4). ✓
