package join

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"concentus/internal/flow"
)

func TestMergeCombinesBranches(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, false)
	res, err := Merge{}.Join(context.Background(), joinStep(flow.JoinMerge, flow.FailAbort), inputs, joinDir, nil)
	if err != nil {
		t.Fatalf("clean merge: %v", err)
	}
	got := map[string]bool{}
	for _, a := range res.Artifacts {
		got[filepath.Base(a.Path)] = true
	}
	if !got["a.txt"] || !got["b.txt"] {
		t.Fatalf("merged tree missing a.txt/b.txt: %+v", res.Artifacts)
	}
	if res.Artifacts[0].Branch != "step/integrate" {
		t.Errorf("result not on the join branch: %+v", res.Artifacts[0])
	}
}

func TestMergeConflictAbortFails(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, true)
	_, err := Merge{}.Join(context.Background(), joinStep(flow.JoinMerge, flow.FailAbort), inputs, joinDir, nil)
	if err == nil {
		t.Fatal("expected a conflict error with on_conflict=abort")
	}
	var ce *ConflictError
	if errors.As(err, &ce) {
		t.Fatal("abort should NOT surface a ConflictError (that is escalate-only)")
	}
	if len(conflictedPaths(context.Background(), joinDir)) != 0 {
		t.Error("abort should leave no conflict markers (git merge --abort)")
	}
}

func TestMergeConflictEscalateReturnsConflictError(t *testing.T) {
	joinDir, inputs := setupJoinRepo(t, true)
	_, err := Merge{}.Join(context.Background(), joinStep(flow.JoinMerge, flow.FailEscalate), inputs, joinDir, nil)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("escalate should surface a *ConflictError, got %v", err)
	}
	found := false
	for _, p := range ce.Paths {
		if p == "shared.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("ConflictError.Paths = %v, want shared.txt", ce.Paths)
	}
}

func TestDefaultRegistryHasAllStrategies(t *testing.T) {
	r := Default()
	if _, ok := r[flow.JoinMerge]; !ok {
		t.Error("merge should be registered")
	}
	if _, ok := r[flow.JoinSelect]; !ok {
		t.Error("select should be registered")
	}
	if _, ok := r[flow.JoinSynthesize]; !ok {
		t.Error("synthesize should be registered")
	}
}
