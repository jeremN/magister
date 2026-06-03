package store

import (
	"context"
	"path/filepath"
	"testing"
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
