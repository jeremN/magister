package join

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

func writeArtifact(t *testing.T, dir, stepID, name, body, branch string) core.Artifact {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return core.Artifact{StepID: stepID, Path: p, Branch: branch, Commit: "sha-" + stepID}
}

func stubRun(res core.Result, err error) RunAgent {
	return func(context.Context, string, string, string, []core.Artifact) (core.Result, error) {
		return res, err
	}
}

func selectStep() *flow.Step {
	return &flow.Step{ID: "pick", Needs: []string{"a", "b"}, Workspace: flow.WSIsolated,
		Join: &flow.Join{Strategy: flow.JoinSelect, Agent: "arbiter"}}
}

func TestSelectForwardsWinnerByRef(t *testing.T) {
	dir := t.TempDir()
	inA := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A", "step/a")
	inB := writeArtifact(t, t.TempDir(), "b", "b.out.md", "B", "step/b")
	run := stubRun(core.Result{Summary: "B is cleaner\nSELECTED: b", CostUSD: 0.02}, nil)

	res, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{inA, inB}, dir, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "b" ||
		res.Artifacts[0].Branch != "step/b" || res.Artifacts[0].Commit != "sha-b" {
		t.Fatalf("result = %+v, want b's artifact forwarded with its branch+commit", res.Artifacts)
	}
	if res.StepID != "pick" || res.CostUSD != 0.02 {
		t.Errorf("result = %+v, want StepID=pick cost=0.02", res)
	}
}

func TestSelectForwardsAllWinnerArtifacts(t *testing.T) {
	dir := t.TempDir()
	srcB := t.TempDir()
	inA := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A", "step/a")
	inB1 := writeArtifact(t, srcB, "b", "b1.out.md", "B1", "step/b")
	inB2 := writeArtifact(t, srcB, "b", "b2.out.md", "B2", "step/b")
	run := stubRun(core.Result{Summary: "SELECTED: b"}, nil)

	res, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{inA, inB1, inB2}, dir, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 2 {
		t.Fatalf("result artifacts = %+v, want both of b's artifacts forwarded", res.Artifacts)
	}
}

func TestSelectNoTokenErrors(t *testing.T) {
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A", "step/a")
	run := stubRun(core.Result{Summary: "I cannot decide"}, nil)
	_, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{in}, t.TempDir(), run)
	if err == nil {
		t.Fatal("expected an error when the arbiter emits no SELECTED token")
	}
}

func TestSelectUnknownWinnerErrors(t *testing.T) {
	in := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A", "step/a")
	run := stubRun(core.Result{Summary: "SELECTED: zzz"}, nil)
	_, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{in}, t.TempDir(), run)
	if err == nil {
		t.Fatal("expected an error when the chosen step is not a dependency")
	}
}

func TestSelectLastTokenWins(t *testing.T) {
	inA := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A", "step/a")
	inB := writeArtifact(t, t.TempDir(), "b", "b.out.md", "B", "step/b")
	run := stubRun(core.Result{Summary: "SELECTED: a\non reflection\nSELECTED: b"}, nil)
	res, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{inA, inB}, t.TempDir(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "b" {
		t.Fatalf("result artifacts = %+v, want b (last SELECTED token wins)", res.Artifacts)
	}
}

func TestSelectParsesTokenWithoutSpace(t *testing.T) {
	inA := writeArtifact(t, t.TempDir(), "a", "a.out.md", "A", "step/a")
	inB := writeArtifact(t, t.TempDir(), "b", "b.out.md", "B", "step/b")
	run := stubRun(core.Result{Summary: "SELECTED:b"}, nil)
	res, err := Select{}.Join(context.Background(), selectStep(), []core.Artifact{inA, inB}, t.TempDir(), run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "b" {
		t.Fatalf("result artifacts = %+v, want b", res.Artifacts)
	}
}
