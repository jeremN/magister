package join

import (
	"context"
	"os"
	"path/filepath"
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
