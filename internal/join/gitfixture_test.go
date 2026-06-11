package join

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func gitX(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setupJoinRepo builds a base repo with two committed branches (step/a, step/b)
// and a join worktree on step/integrate off the empty base. With conflict=true
// both branches write shared.txt differently (a merge conflict); otherwise they
// touch disjoint files (a clean merge). Returns the join worktree dir and the
// branch-backed inputs a join would receive.
func setupJoinRepo(t *testing.T, conflict bool) (string, []core.Artifact) {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	base := filepath.Join(root, "base")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	gitX(t, base, "init")
	gitX(t, base, "config", "user.name", "test")
	gitX(t, base, "config", "user.email", "test@test")
	gitX(t, base, "commit", "--allow-empty", "-m", "base")

	wtA := filepath.Join(root, "wt-a")
	gitX(t, base, "worktree", "add", wtA, "-b", "step/a", "HEAD")
	writeFile(t, wtA, "a.txt", "from A")
	if conflict {
		writeFile(t, wtA, "shared.txt", "A version")
	}
	gitX(t, wtA, "add", "-A")
	gitX(t, wtA, "commit", "-m", "a")

	wtB := filepath.Join(root, "wt-b")
	gitX(t, base, "worktree", "add", wtB, "-b", "step/b", "HEAD")
	writeFile(t, wtB, "b.txt", "from B")
	if conflict {
		writeFile(t, wtB, "shared.txt", "B version")
	}
	gitX(t, wtB, "add", "-A")
	gitX(t, wtB, "commit", "-m", "b")

	joinDir := filepath.Join(root, "wt-join")
	gitX(t, base, "worktree", "add", joinDir, "-b", "step/integrate", "HEAD")

	inputs := []core.Artifact{
		{StepID: "a", Branch: "step/a", Commit: gitX(t, base, "rev-parse", "step/a"), Path: filepath.Join(wtA, "a.txt")},
		{StepID: "b", Branch: "step/b", Commit: gitX(t, base, "rev-parse", "step/b"), Path: filepath.Join(wtB, "b.txt")},
	}
	return joinDir, inputs
}

func joinStep(strategy flow.JoinStrategy, onConflict flow.FailPolicy) *flow.Step {
	return &flow.Step{ID: "integrate", Needs: []string{"a", "b"}, Workspace: flow.WSIsolated,
		Join: &flow.Join{Strategy: strategy, Agent: "arbiter", OnConflict: onConflict}}
}
