package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"concentus/internal/store/sqldb"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// SQLite is a durable core.Store backed by one SQLite database file in WAL mode.
// SQLite allows a single writer at a time, so writes go through a dedicated
// handle capped at one connection (w); reads use a separate pool (r) that WAL
// lets run concurrently with the writer. Pragmas are set per-connection via the
// DSN so every pooled connection — reader or writer — enforces foreign keys.
type SQLite struct {
	w  *sql.DB
	r  *sql.DB
	qw *sqldb.Queries // bound to the writer handle
	qr *sqldb.Queries // bound to the reader pool
}

// pragmas run on every connection: bounded wait on the single writer lock, WAL
// journaling (persists in the file), and enforced foreign keys.
const pragmas = "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"

// Open opens (creating if absent) the database at path, applies all migrations,
// and returns a ready store. Not safe to call concurrently with itself: goose's
// legacy API uses package globals. A daemon opens exactly one store at startup.
func Open(path string) (*SQLite, error) {
	dsn := "file:" + path + "?" + pragmas
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open writer: %w", err)
	}
	w.SetMaxOpenConns(1) // SQLite has a single writer; serialize writes here.

	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("open reader: %w", err)
	}

	if err := migrate(w); err != nil {
		_ = w.Close()
		_ = r.Close()
		return nil, err
	}
	return &SQLite{w: w, r: r, qw: sqldb.New(w), qr: sqldb.New(r)}, nil
}

func migrate(db *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Close releases both connection pools.
func (s *SQLite) Close() error {
	return errors.Join(s.w.Close(), s.r.Close())
}
