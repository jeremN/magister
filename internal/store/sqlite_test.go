package store

import (
	"context"
	"path/filepath"
	"testing"

	"concentus/internal/core"
)

func tempDB(t *testing.T) *SQLite {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenAppliesMigrationsAndPragmas(t *testing.T) {
	s := tempDB(t)

	// WAL + foreign_keys must actually be on (per-connection pragmas via DSN).
	var jmode string
	if err := s.w.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&jmode); err != nil {
		t.Fatal(err)
	}
	if jmode != "wal" {
		t.Errorf("journal_mode = %q, want wal", jmode)
	}
	var fk int
	if err := s.r.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}

	// migrations applied: the runs table exists.
	var n int
	if err := s.r.QueryRowContext(context.Background(),
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='runs'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("runs table not found (n=%d)", n)
	}
}

func TestOpenIsIdempotentAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(path) // re-applying migrations on an existing DB must no-op
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s2.Close()
}

func TestSQLiteCreateGetSetListRuns(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)

	if err := s.CreateRun(ctx, core.RunState{
		ID: "r1", Name: "feature", FlowYAML: "name: feature\n", Status: core.RunPending, Concurrency: 4,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "feature" || got.FlowYAML != "name: feature\n" || got.Concurrency != 4 || got.Status != core.RunPending {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if len(got.Steps) != 0 {
		t.Errorf("new run should have no steps, got %d", len(got.Steps))
	}

	if err := s.SetRunStatus(ctx, "r1", core.RunRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, core.RunState{ID: "r2", Name: "other", FlowYAML: "x", Status: core.RunSucceeded}); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListRuns(ctx, core.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 runs, got %d", len(all))
	}
	running, err := s.ListRuns(ctx, core.Filter{Status: core.RunRunning})
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0].ID != "r1" {
		t.Fatalf("filter by status failed: %+v", running)
	}

	if _, err := s.GetRun(ctx, "nope"); err == nil {
		t.Error("GetRun of unknown id should error")
	}
}
