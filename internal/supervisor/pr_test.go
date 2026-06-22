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
func TestPRStorageError500(t *testing.T) {
	sup := newPRSup(t, getErrStore{store.NewMem()})
	_, err := sup.PR(context.Background(), "r1", PROpts{})
	if got := prErrStatus(t, err); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
}

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

func TestPRRejectsUnsafeHeadOrBase(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))
	if _, err := sup.PR(context.Background(), "r1", PROpts{As: "../evil"}); prErrStatus(t, err) != http.StatusBadRequest {
		t.Error("unsafe --as should be 400")
	}
	if _, err := sup.PR(context.Background(), "r1", PROpts{Base: "foo bar"}); prErrStatus(t, err) != http.StatusBadRequest {
		t.Error("unsafe --base should be 400")
	}
}

func TestPRExistingOpenPRReturns409(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	t.Setenv("FAKE_GH_EXISTING_PR", "https://github.com/test-owner/test-repo/pull/2")
	t.Setenv("FAKE_GH_EXISTING_PR_OWNER", "test-owner") // same-repo head owner == base owner
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))
	_, err := sup.PR(context.Background(), "r1", PROpts{})
	if got := prErrStatus(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409", got)
	}
	var pe *PRError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PRError, got %T", err)
	}
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
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PRError, got %T", err)
	}
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

func TestPRCoreReportsExistingPR(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	t.Setenv("FAKE_GH_EXISTING_PR", "https://github.com/test-owner/test-repo/pull/8")
	t.Setenv("FAKE_GH_EXISTING_PR_OWNER", "test-owner") // same-repo head owner == base owner
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))

	res, existed, err := sup.prCore(context.Background(), "r1", PROpts{})
	if err != nil {
		t.Fatalf("prCore: %v", err)
	}
	if !existed {
		t.Fatal("existed = false, want true")
	}
	if res.URL != "https://github.com/test-owner/test-repo/pull/8" {
		t.Errorf("url = %q, want the existing PR url", res.URL)
	}
}

func TestPRFromForkExistingReturns409(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_GH_ARGV_FILE", argv)
	t.Setenv("FAKE_GH_EXISTING_PR", "https://github.com/test-owner/test-repo/pull/5")
	t.Setenv("FAKE_GH_EXISTING_PR_OWNER", "fork-owner") // the open PR's head lives on the fork
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))

	_, err := sup.PR(context.Background(), "r1", PROpts{HeadRepo: "https://github.com/fork-owner/test-repo.git"})
	if got := prErrStatus(t, err); got != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (cross-fork PR already exists)", got)
	}
	var pe *PRError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PRError, got %T", err)
	}
	if !strings.Contains(pe.Msg, "pull/5") {
		t.Errorf("message should carry the existing PR URL, got %q", pe.Msg)
	}
	// The existing-PR lookup must query the BARE branch — `gh pr list --head` does not
	// match the owner:branch form (regression: a cross-fork dup used to slip past the
	// pre-check and 502 on create).
	got, _ := os.ReadFile(argv)
	if !strings.Contains(string(got), "--head=magister/r1\n") {
		t.Errorf("pr list should query the bare branch --head=magister/r1; argv:\n%s", got)
	}
}

func TestPRFromForkComposesCrossForkHead(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_GH_ARGV_FILE", argv)
	t.Setenv("FAKE_GH_PR_URL", "https://github.com/test-owner/test-repo/pull/9")
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))

	res, err := sup.PR(context.Background(), "r1", PROpts{HeadRepo: "https://github.com/fork-owner/test-repo.git"})
	if err != nil {
		t.Fatalf("pr: %v", err)
	}
	// Base repo stays the upstream; head is the cross-fork owner:branch form.
	if res.Repo != "test-owner/test-repo" {
		t.Errorf("repo = %q, want upstream test-owner/test-repo", res.Repo)
	}
	if res.Head != "fork-owner:magister/r1" {
		t.Errorf("head = %q, want fork-owner:magister/r1", res.Head)
	}
	got, _ := os.ReadFile(argv)
	for _, want := range []string{"create", "--repo=test-owner/test-repo", "--head=fork-owner:magister/r1"} {
		if !strings.Contains(string(got), want+"\n") {
			t.Errorf("argv missing %q; got:\n%s", want, got)
		}
	}
}

func TestPRFromForkChecksBranchOnFork(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_GH_ARGV_FILE", argv)
	t.Setenv("FAKE_GH_CREATE_FAIL", "GraphQL: Head sha can't be blank")
	t.Setenv("FAKE_GH_BRANCH_MISSING", "1")
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))

	_, err := sup.PR(context.Background(), "r1", PROpts{HeadRepo: "https://github.com/fork-owner/test-repo.git"})
	if got := prErrStatus(t, err); got != http.StatusConflict {
		t.Fatalf("status = %d, want 409", got)
	}
	var pe *PRError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PRError, got %T", err)
	}
	if !strings.Contains(pe.Msg, "cm push") {
		t.Errorf("message should tell the user to push first, got %q", pe.Msg)
	}
	// The branch-existence check must target the FORK owner, not the upstream.
	got, _ := os.ReadFile(argv)
	if !strings.Contains(string(got), "repos/fork-owner/test-repo/branches/magister/r1\n") {
		t.Errorf("BranchExists should query the fork; argv:\n%s", got)
	}
}

func TestPRFromForkRejectsNonGitHubHeadRepo(t *testing.T) {
	requireGitS(t)
	st := store.NewMem()
	sup := newPRSup(t, st)
	seedExtRun(t, st, "r1", srcWithGHOrigin(t, "https://github.com/test-owner/test-repo.git"))
	_, err := sup.PR(context.Background(), "r1", PROpts{HeadRepo: "https://gitlab.com/fork-owner/test-repo.git"})
	if got := prErrStatus(t, err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (non-github head-repo)", got)
	}
}
