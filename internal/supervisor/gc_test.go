package supervisor

import (
	"context"
	"errors"
	"net/http"
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

	// Each reclaimed run is now marked, so the store no longer selects it: the
	// second sweep queries zero rows and reports 0 reclaimed.
	n, err = sup.SweepScratch(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("second SweepScratch: %v", err)
	}
	if n != 0 {
		t.Errorf("second sweep reclaimed = %d, want 0 (dirs already gone)", n)
	}
}

func TestSweepScratchMarksReclaimed(t *testing.T) {
	root := t.TempDir()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	gm := &workspace.GitManager{Root: root}
	sup := New(testEngine(t, st, reg, gm), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	ctx := context.Background()

	if err := st.CreateRun(ctx, core.RunState{ID: "done", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunStatus(ctx, "done", core.RunSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "done", "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.SweepScratch(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The reclaimed run is now marked, so the store no longer selects it.
	left, err := st.ReclaimableRuns(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("after sweep, ReclaimableRuns = %v, want none (run marked)", left)
	}
}

func assertReclaimStatus(t *testing.T, err error, want int) {
	t.Helper()
	var re *ReclaimError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v, want *ReclaimError", err)
	}
	if re.Status != want {
		t.Errorf("status = %d, want %d", re.Status, want)
	}
}

func newReclaimSup(t *testing.T) (*Supervisor, *store.Mem, string) {
	t.Helper()
	root := t.TempDir()
	st := store.NewMem()
	reg := NewApprovalRegistry()
	sup := New(testEngine(t, st, reg, &workspace.GitManager{Root: root}), st, reg)
	t.Cleanup(func() { sup.Shutdown(time.Second) })
	return sup, st, root
}

func TestReclaimRunRemovesScratchAndIsIdempotent(t *testing.T) {
	sup, st, root := newReclaimSup(t)
	ctx := context.Background()
	if err := st.CreateRun(ctx, core.RunState{ID: "done", Status: core.RunSucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "done", "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	removed, err := sup.ReclaimRun(ctx, "done")
	if err != nil {
		t.Fatalf("ReclaimRun: %v", err)
	}
	if !removed {
		t.Error("removed = false, want true")
	}
	if _, err := os.Stat(filepath.Join(root, "done")); !os.IsNotExist(err) {
		t.Error("scratch not removed")
	}
	// Idempotent: the dir is gone, so the second call returns removed=false, no error.
	removed, err = sup.ReclaimRun(ctx, "done")
	if err != nil {
		t.Fatalf("second ReclaimRun: %v", err)
	}
	if removed {
		t.Error("second removed = true, want false (already gone)")
	}
}

func TestReclaimRunUnknownIs404(t *testing.T) {
	sup, _, _ := newReclaimSup(t)
	_, err := sup.ReclaimRun(context.Background(), "nope")
	assertReclaimStatus(t, err, http.StatusNotFound)
}

func TestReclaimRunActiveIs409(t *testing.T) {
	sup, st, _ := newReclaimSup(t)
	ctx := context.Background()
	if err := st.CreateRun(ctx, core.RunState{ID: "r", Status: core.RunFailed}); err != nil {
		t.Fatal(err)
	}
	sup.mu.Lock()
	sup.runs["r"] = func() {} // simulate an active run registered in the run map
	sup.mu.Unlock()
	_, err := sup.ReclaimRun(ctx, "r")
	assertReclaimStatus(t, err, http.StatusConflict)
}

func TestReclaimRunNonTerminalIs409(t *testing.T) {
	sup, st, _ := newReclaimSup(t)
	ctx := context.Background()
	if err := st.CreateRun(ctx, core.RunState{ID: "r", Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}
	_, err := sup.ReclaimRun(ctx, "r")
	assertReclaimStatus(t, err, http.StatusConflict)
}
