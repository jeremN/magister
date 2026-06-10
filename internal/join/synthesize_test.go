package join

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

func synthesizeStep() *flow.Step {
	return &flow.Step{ID: "merge", Needs: []string{"a", "b"},
		Join: &flow.Join{Strategy: flow.JoinSynthesize, Agent: "arbiter"}}
}

func TestSynthesizeReturnsArbiterOutput(t *testing.T) {
	dir := t.TempDir()
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A")
	out := filepath.Join(dir, "synthesis.md")
	// The arbiter "writes" synthesis.md; the stub reports it as its artifact.
	run := func(_ context.Context, _, _, wd string, _ []core.Artifact) (core.Result, error) {
		if err := os.WriteFile(out, []byte("merged"), 0o644); err != nil {
			return core.Result{}, err
		}
		return core.Result{Summary: "synthesized", Artifacts: []core.Artifact{{StepID: "merge", Path: out}}, CostUSD: 0.05}, nil
	}
	res, err := Synthesize{}.Join(context.Background(), synthesizeStep(), []core.Artifact{in}, dir, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Path != out {
		t.Fatalf("result artifacts = %+v, want only synthesis.md", res.Artifacts)
	}
	if res.StepID != "merge" || res.CostUSD != 0.05 {
		t.Errorf("result = %+v, want StepID=merge cost=0.05", res)
	}
}

func TestSynthesizeExcludesStagedCandidates(t *testing.T) {
	dir := t.TempDir()
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A")
	out := filepath.Join(dir, "synthesis.md")
	staged := filepath.Join(dir, ".candidates", "a", "a.out.md")
	// The stub reports BOTH the real output and a staged candidate path (as a
	// real agent's discoverGit would). Only the real output must survive.
	run := func(_ context.Context, _, _, wd string, _ []core.Artifact) (core.Result, error) {
		_ = os.WriteFile(out, []byte("merged"), 0o644)
		return core.Result{Summary: "ok", Artifacts: []core.Artifact{
			{StepID: "merge", Path: staged},
			{StepID: "merge", Path: out},
		}}, nil
	}
	res, err := Synthesize{}.Join(context.Background(), synthesizeStep(), []core.Artifact{in}, dir, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Path != out {
		t.Fatalf("result artifacts = %+v, want the staged .candidates path excluded", res.Artifacts)
	}
}

func TestSynthesizeEmptyOutputErrors(t *testing.T) {
	dir := t.TempDir()
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A")
	// Arbiter produced nothing outside .candidates -> error.
	run := stubRun(core.Result{Summary: "done", Artifacts: nil}, nil)
	_, err := Synthesize{}.Join(context.Background(), synthesizeStep(), []core.Artifact{in}, dir, run)
	if err == nil {
		t.Fatal("expected an error when the arbiter produced no synthesized output")
	}
}
