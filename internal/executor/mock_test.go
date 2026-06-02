package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"concentus/internal/core"
)

func TestMockWritesArtifact(t *testing.T) {
	dir := t.TempDir()
	m := Mock{Name: "sonnet"}
	res, err := m.Run(context.Background(), core.Task{StepID: "impl", Role: "impl", WorkDir: dir})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(res.Artifacts))
	}
	if _, err := os.Stat(res.Artifacts[0].Path); err != nil {
		t.Fatalf("artifact not written: %v", err)
	}
	if filepath.Dir(res.Artifacts[0].Path) != dir {
		t.Errorf("artifact not in workdir")
	}
	if res.CostUSD == 0 {
		t.Errorf("expected a nonzero mock cost")
	}
}

func TestMockHonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := Mock{Name: "x", Delay: 5 /* ns; tiny */}
	if _, err := m.Run(ctx, core.Task{StepID: "s", WorkDir: t.TempDir()}); err == nil {
		t.Fatal("expected context error")
	}
}
