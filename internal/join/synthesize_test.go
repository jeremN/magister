package join

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

func TestSynthesizeAutoMergesWithoutArbiter(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, false) // disjoint files → no conflict
	run := func(context.Context, string, string, string, []core.Artifact) (core.Result, error) {
		t.Fatal("arbiter must not be called when the merge has no conflicts")
		return core.Result{}, nil
	}
	res, err := Synthesize{}.Join(context.Background(), joinStep(flow.JoinSynthesize, ""), inputs, joinDir, run)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	got := map[string]bool{}
	for _, a := range res.Artifacts {
		got[filepath.Base(a.Path)] = true
	}
	if !got["a.txt"] || !got["b.txt"] {
		t.Fatalf("auto-merged tree missing files: %+v", res.Artifacts)
	}
}

func TestSynthesizeArbiterResolvesConflict(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, true) // both write shared.txt → conflict
	run := func(_ context.Context, _, _, wd string, _ []core.Artifact) (core.Result, error) {
		if err := os.WriteFile(filepath.Join(wd, "shared.txt"), []byte("reconciled"), 0o644); err != nil {
			t.Fatal(err)
		}
		return core.Result{Summary: "resolved", CostUSD: 0.03}, nil
	}
	res, err := Synthesize{}.Join(context.Background(), joinStep(flow.JoinSynthesize, ""), inputs, joinDir, run)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(joinDir, "shared.txt"))
	if err != nil || string(body) != "reconciled" {
		t.Fatalf("arbiter resolution not committed: body=%q err=%v", body, err)
	}
	if res.CostUSD != 0.03 {
		t.Errorf("arbiter cost not propagated: %v", res.CostUSD)
	}
}

func TestSynthesizeArbiterResolutionWithTrailingWhitespace(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, true)
	// A real arbiter may emit trailing whitespace; that must NOT be misread as a
	// leftover conflict marker (the --check must ignore whitespace, only flag markers).
	run := func(_ context.Context, _, _, wd string, _ []core.Artifact) (core.Result, error) {
		if err := os.WriteFile(filepath.Join(wd, "shared.txt"), []byte("resolved   \nline\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return core.Result{Summary: "resolved"}, nil
	}
	_, err := Synthesize{}.Join(context.Background(), joinStep(flow.JoinSynthesize, ""), inputs, joinDir, run)
	if err != nil {
		t.Fatalf("a whitespace-laden but marker-free resolution must succeed, got: %v", err)
	}
}

func TestSynthesizeArbiterLeavesMarkersFails(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, true)
	run := func(context.Context, string, string, string, []core.Artifact) (core.Result, error) {
		return core.Result{Summary: "did nothing"}, nil // leaves markers
	}
	_, err := Synthesize{}.Join(context.Background(), joinStep(flow.JoinSynthesize, ""), inputs, joinDir, run)
	if err == nil {
		t.Fatal("expected an error when the arbiter leaves unresolved conflicts")
	}
}

// TestSynthesizeAbortOnCommitFailure verifies that when the post-resolution
// commit fails, the worktree is NOT left in MERGING state. A pre-commit hook
// is installed to make git commit fail deterministically after EnsureResolved
// has already staged the resolved tree.
func TestSynthesizeAbortOnCommitFailure(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, true)

	// Locate the common git dir for the join worktree (it's a gitfile pointing
	// into the base repo's worktrees directory). We need to install a failing
	// pre-commit hook on the base repo so it applies to the join worktree.
	// Use core.hooksPath so we don't touch the main hooks dir.
	hooksDir := t.TempDir()
	hook := filepath.Join(hooksDir, "pre-commit")
	// Write a hook that immediately exits non-zero to fail every commit.
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Find the base repo: the join worktree's .git is a file whose gitdir points
	// into base/.git/worktrees/… — walk up 3 dirs to reach the base repo root.
	gitFileData, readErr := os.ReadFile(filepath.Join(joinDir, ".git"))
	if readErr != nil {
		t.Fatalf("read joinDir/.git: %v", readErr)
	}
	// gitdir line: "gitdir: /abs/path/base/.git/worktrees/wt-join"
	gitdirPath := strings.TrimPrefix(strings.TrimSpace(string(gitFileData)), "gitdir: ")
	// base/.git/worktrees/wt-join → parent×3 = base
	baseRepo := filepath.Dir(filepath.Dir(filepath.Dir(gitdirPath)))
	gitX(t, baseRepo, "config", "core.hooksPath", hooksDir)
	// Restore the config after the test so it doesn't pollute other tests.
	t.Cleanup(func() {
		gitX(t, baseRepo, "config", "--unset", "core.hooksPath")
	})

	// The arbiter correctly resolves the conflict; EnsureResolved succeeds.
	// But git commit then fails (the hook exits 1).
	run := func(_ context.Context, _, _, wd string, _ []core.Artifact) (core.Result, error) {
		if err := os.WriteFile(filepath.Join(wd, "shared.txt"), []byte("resolved"), 0o644); err != nil {
			t.Fatal(err)
		}
		return core.Result{Summary: "resolved"}, nil
	}

	_, err := Synthesize{}.Join(context.Background(), joinStep(flow.JoinSynthesize, ""), inputs, joinDir, run)
	if err == nil {
		t.Fatal("expected an error when git commit fails")
	}

	// The worktree must NOT be left in MERGING state: MERGE_HEAD must be absent.
	// MERGE_HEAD lives inside the worktree's git dir (gitdirPath), not in .git/.
	// If merge --abort was NOT called, MERGE_HEAD would still exist.
	mergeHead := filepath.Join(gitdirPath, "MERGE_HEAD")
	if _, statErr := os.Stat(mergeHead); statErr == nil {
		t.Error("MERGE_HEAD still exists: worktree left in MERGING state after commit failure")
	}

	// Also verify that a subsequent clean merge isn't rejected with "you have not
	// concluded your merge": attempt another git operation on the worktree.
	if _, gitErr := gitCmd(context.Background(), joinDir, "status"); gitErr != nil {
		t.Errorf("git status after abort failed: %v (worktree may still be mid-merge)", gitErr)
	}
}
