package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"concentus/internal/core"
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

func (s *SQLite) CreateRun(ctx context.Context, r core.RunState) error {
	return s.qw.CreateRun(ctx, sqldb.CreateRunParams{
		ID:          string(r.ID),
		Name:        r.Name,
		FlowYaml:    r.FlowYAML,
		Status:      string(r.Status),
		Concurrency: int64(r.Concurrency),
		Error:       r.Err,
	})
}

func (s *SQLite) SetRunStatus(ctx context.Context, id core.RunID, status core.RunStatus, errMsg string) error {
	return s.qw.SetRunStatus(ctx, sqldb.SetRunStatusParams{
		Status: string(status),
		Error:  errMsg,
		ID:     string(id),
	})
}

func (s *SQLite) GetRun(ctx context.Context, id core.RunID) (core.RunState, error) {
	row, err := s.qr.GetRun(ctx, string(id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.RunState{}, fmt.Errorf("unknown run %q", id)
		}
		return core.RunState{}, err
	}
	steps, err := s.loadSteps(ctx, id)
	if err != nil {
		return core.RunState{}, err
	}
	return core.RunState{
		ID:          core.RunID(row.ID),
		Name:        row.Name,
		FlowYAML:    row.FlowYaml,
		Status:      core.RunStatus(row.Status),
		Concurrency: int(row.Concurrency),
		Err:         row.Error,
		Steps:       steps,
	}, nil
}

func (s *SQLite) ListRuns(ctx context.Context, f core.Filter) ([]core.RunSummary, error) {
	if f.Status != "" {
		rows, err := s.qr.ListRunsByStatus(ctx, string(f.Status))
		if err != nil {
			return nil, err
		}
		out := make([]core.RunSummary, 0, len(rows))
		for _, r := range rows {
			out = append(out, core.RunSummary{ID: core.RunID(r.ID), Name: r.Name, Status: core.RunStatus(r.Status)})
		}
		return out, nil
	}
	rows, err := s.qr.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.RunSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, core.RunSummary{ID: core.RunID(r.ID), Name: r.Name, Status: core.RunStatus(r.Status)})
	}
	return out, nil
}

// loadSteps reads a run's steps and attaches each step's artifacts. Returns
// fresh slices, so GetRun/LoadIncompleteRuns satisfy the deep-copy contract
// naturally (spec §17).
func (s *SQLite) loadSteps(ctx context.Context, id core.RunID) ([]core.StepState, error) {
	rows, err := s.qr.ListSteps(ctx, string(id))
	if err != nil {
		return nil, err
	}
	arts, err := s.qr.ListArtifactsForRun(ctx, string(id))
	if err != nil {
		return nil, err
	}
	byStep := make(map[string][]core.Artifact, len(rows))
	for _, a := range arts {
		byStep[a.StepID] = append(byStep[a.StepID], core.Artifact{StepID: a.StepID, Path: a.Path})
	}
	steps := make([]core.StepState, 0, len(rows))
	for _, r := range rows {
		steps = append(steps, core.StepState{
			RunID:     core.RunID(r.RunID),
			StepID:    r.ID,
			Status:    core.StepStatus(r.Status),
			Attempt:   int(r.Attempt),
			Summary:   r.Summary,
			CostUSD:   r.CostUsd,
			WorkDir:   r.Workdir,
			Err:       r.Error,
			Artifacts: byStep[r.ID],
		})
	}
	return steps, nil
}
