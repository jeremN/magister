package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/store"
	"concentus/internal/workspace"
)

func TestSweepScratchReclaimsTerminalAgedRuns(t *testing.T) {
	root := t.TempDir()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	gm := &workspace.GitManager{Root: root}
	sup := New(testEngine(t, st, reg, gm), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	ctx := context.Background()

	mkRun := func(id core.RunID, status core.RunStatus) {
		if err := st.CreateRun(ctx, core.RunState{ID: id, Status: core.RunPending}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetRunStatus(ctx, id, status, ""); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, string(id), "base"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkRun("done1", core.RunSucceeded)
	mkRun("done2", core.RunFailed)
	mkRun("active", core.RunRunning)

	n, err := sup.SweepScratch(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SweepScratch: %v", err)
	}
	if n != 2 {
		t.Errorf("reclaimed = %d, want 2", n)
	}
	for _, id := range []string{"done1", "done2"} {
		if _, err := os.Stat(filepath.Join(root, id)); !os.IsNotExist(err) {
			t.Errorf("%s scratch not reclaimed", id)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "active")); err != nil {
		t.Errorf("active scratch wrongly reclaimed: %v", err)
	}
}
