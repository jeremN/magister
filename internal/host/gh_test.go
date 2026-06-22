package host

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
	if url, ok, err := r.ExistingOpenPR(context.Background(), "o", "r", "magister/x", "o"); err != nil || ok || url != "" {
		t.Fatalf("want none, got url=%q ok=%v err=%v", url, ok, err)
	}
	t.Setenv("FAKE_GH_EXISTING_PR", "https://github.com/o/r/pull/3")
	t.Setenv("FAKE_GH_EXISTING_PR_OWNER", "o")
	url, ok, err := r.ExistingOpenPR(context.Background(), "o", "r", "magister/x", "o")
	if err != nil || !ok || url != "https://github.com/o/r/pull/3" {
		t.Fatalf("want existing, got url=%q ok=%v err=%v", url, ok, err)
	}
	// A PR whose head lives on a different owner (e.g. another fork with the same
	// branch name) must NOT be mistaken for ours.
	if url, ok, err := r.ExistingOpenPR(context.Background(), "o", "r", "magister/x", "someone-else"); err != nil || ok || url != "" {
		t.Fatalf("want no match for a different head owner, got url=%q ok=%v err=%v", url, ok, err)
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

func TestParseRemoteStripsPort(t *testing.T) {
	cases := []struct {
		url, owner, repo string
		ok               bool
	}{
		{"ssh://git@github.com:22/test-owner/test-repo.git", "test-owner", "test-repo", true},
		{"https://github.com:443/o/r.git", "o", "r", true},
		{"ssh://git@gitlab.com:22/o/r.git", "", "", false}, // other host + port still rejected
	}
	for _, c := range cases {
		_, owner, repo, err := ParseRemote(c.url)
		if c.ok {
			if err != nil || owner != c.owner || repo != c.repo {
				t.Errorf("ParseRemote(%q) = %q/%q/%v, want %q/%q/nil", c.url, owner, repo, err, c.owner, c.repo)
			}
		} else if err == nil {
			t.Errorf("ParseRemote(%q) = nil error, want unsupported-host error", c.url)
		}
	}
}
