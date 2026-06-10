package join

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// writeArtifact creates a file and returns it as a StepID-tagged artifact.
func writeArtifact(t *testing.T, dir, stepID, name, body string) core.Artifact {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return core.Artifact{StepID: stepID, Path: p}
}

// stubRun returns a fixed result, ignoring the prompt (no real agent).
func stubRun(res core.Result, err error) RunAgent {
	return func(context.Context, string, string, string, []core.Artifact) (core.Result, error) {
		return res, err
	}
}

func selectStep() *flow.Step {
	return &flow.Step{ID: "pick", Needs: []string{"a", "b"},
		Join: &flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter"}}
}

func TestSelectForwardsWinnerArtifacts(t *testing.T) {
	dir := t.TempDir()
	srcA, srcB := t.TempDir(), t.TempDir()
	inA := writeArtifact(t, srcA, "a", "a.out.md", "A")
	inB := writeArtifact(t, srcB, "b", "b.out.md", "B")
	run := stubRun(core.Result{Summary: "B is cleaner\nSELECTED: b", CostUSD: 0.02}, nil)

	res, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{inA, inB}, dir, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "b" || res.Artifacts[0].Path != inB.Path {
		t.Fatalf("result artifacts = %+v, want only b's original artifact", res.Artifacts)
	}
	if res.StepID != "pick" || res.CostUSD != 0.02 {
		t.Errorf("result = %+v, want StepID=pick cost=0.02", res)
	}
}

func TestSelectNoTokenErrors(t *testing.T) {
	dir := t.TempDir()
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A")
	run := stubRun(core.Result{Summary: "I cannot decide"}, nil)
	_, err := Select{}.Join(context.Background(), selectStep(), ([]core.Artifact{in}), dir, run)
	if err == nil {
		t.Fatal("expected an error when the arbiter emits no SELECTED token")
	}
}

func TestSelectUnknownWinnerErrors(t *testing.T) {
	dir := t.TempDir()
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A")
	run := stubRun(core.Result{Summary: "SELECTED: zzz"}, nil) // zzz is not a dependency
	_, err := Select{}.Join(context.Background(), selectStep(), ([]core.Artifact{in}), dir, run)
	if err == nil {
		t.Fatal("expected an error when the chosen step is not a dependency")
	}
}

func TestStageCandidatesCopies(t *testing.T) {
	dir := t.TempDir()
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "hello")
	staged, err := stageCandidates([]core.Artifact{in}, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, ".candidates", "a", "a.out.md")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected staged copy at %s: %v", want, err)
	}
	if len(staged["a"]) != 1 {
		t.Fatalf("staged[a] = %v, want one entry", staged["a"])
	}
}

func TestSelectForwardsAllWinnerArtifacts(t *testing.T) {
	dir := t.TempDir()
	srcA, srcB := t.TempDir(), t.TempDir()
	inA := writeArtifact(t, srcA, "a", "a.out.md", "A")
	inB1 := writeArtifact(t, srcB, "b", "b1.out.md", "B1")
	inB2 := writeArtifact(t, srcB, "b", "b2.out.md", "B2")
	run := stubRun(core.Result{Summary: "SELECTED: b"}, nil)
	res, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{inA, inB1, inB2}, dir, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 2 {
		t.Fatalf("result artifacts = %+v, want both of b's artifacts forwarded", res.Artifacts)
	}
}

func TestSelectParsesTokenWithoutSpace(t *testing.T) {
	dir := t.TempDir()
	inA := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A")
	inB := writeArtifact(t, t.TempDir(), "b", "b.out.md", "B")
	run := stubRun(core.Result{Summary: "SELECTED:b"}, nil) // no space after colon
	res, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{inA, inB}, dir, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "b" {
		t.Fatalf("result artifacts = %+v, want b (token without a space must parse)", res.Artifacts)
	}
}

func TestSelectLastTokenWins(t *testing.T) {
	dir := t.TempDir()
	inA := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A")
	inB := writeArtifact(t, t.TempDir(), "b", "b.out.md", "B")
	run := stubRun(core.Result{Summary: "SELECTED: a\non reflection\nSELECTED: b"}, nil)
	res, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{inA, inB}, dir, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "b" {
		t.Fatalf("result artifacts = %+v, want b (last SELECTED token wins)", res.Artifacts)
	}
}

func TestStageCandidatesDisambiguatesCollision(t *testing.T) {
	dir := t.TempDir()
	src1, src2 := t.TempDir(), t.TempDir()
	// Same step "a", two artifacts with the SAME basename from different source dirs.
	in1 := writeArtifact(t, src1, "a", "main.go", "one")
	in2 := writeArtifact(t, src2, "a", "main.go", "two")
	staged, err := stageCandidates([]core.Artifact{in1, in2}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(staged["a"]) != 2 {
		t.Fatalf("staged[a] = %v, want two distinct staged paths (no overwrite)", staged["a"])
	}
	if staged["a"][0] == staged["a"][1] {
		t.Fatalf("staged paths collided: %v", staged["a"])
	}
	// Both staged files must exist with their original contents preserved.
	bodies := map[string]bool{}
	for _, rel := range staged["a"] {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		bodies[string(b)] = true
	}
	if !bodies["one"] || !bodies["two"] {
		t.Fatalf("staged contents = %v, want both 'one' and 'two' preserved", bodies)
	}
}
