package join

import (
	"context"
	"os"
	"strings"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

func TestMergeWritesManifest(t *testing.T) {
	dir := t.TempDir()
	inputs := []core.Artifact{
		{StepID: "a", Path: "/tmp/a.md"},
		{StepID: "b", Path: "/tmp/b.md"},
	}
	res, err := Merge{}.Join(context.Background(), &flow.Step{ID: "integrate"}, inputs, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("want 1 manifest artifact, got %d", len(res.Artifacts))
	}
	data, err := os.ReadFile(res.Artifacts[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a -> /tmp/a.md", "b -> /tmp/b.md"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("manifest missing %q:\n%s", want, data)
		}
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
