package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/event"
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

func TestSQLiteSaveStepTransitionAndEvents(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	if err := s.CreateRun(ctx, core.RunState{ID: "r1", Name: "f", FlowYAML: "x", Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	// running transition: no artifacts yet, one started event.
	if err := s.SaveStepTransition(ctx,
		core.StepState{RunID: "r1", StepID: "a", Status: core.StepRunning, Attempt: 1},
		[]event.Event{{RunID: "r1", StepID: "a", Kind: event.StepStarted, Attempt: 1, At: now}}); err != nil {
		t.Fatal(err)
	}
	// succeeded transition: upserts the same step, adds an artifact + a done event.
	if err := s.SaveStepTransition(ctx,
		core.StepState{RunID: "r1", StepID: "a", Status: core.StepSucceeded, Attempt: 1,
			Summary: "ok", CostUSD: 0.02, WorkDir: "/w",
			Artifacts: []core.Artifact{{StepID: "a", Path: "/w/a.md"}}},
		[]event.Event{{RunID: "r1", StepID: "a", Kind: event.StepDone, Summary: "ok", CostUSD: 0.02, Attempt: 1, At: now}}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Steps) != 1 {
		t.Fatalf("want 1 step (upsert, not insert), got %d", len(got.Steps))
	}
	st := got.Steps[0]
	if st.Status != core.StepSucceeded || st.Summary != "ok" || st.CostUSD != 0.02 || st.WorkDir != "/w" {
		t.Errorf("step not upserted correctly: %+v", st)
	}
	if len(st.Artifacts) != 1 || st.Artifacts[0].Path != "/w/a.md" {
		t.Errorf("artifacts not persisted: %+v", st.Artifacts)
	}

	evs, err := s.EventsSince(ctx, "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	if evs[0].Seq != 1 || evs[1].Seq != 2 {
		t.Errorf("seq not autoincremented: %d, %d", evs[0].Seq, evs[1].Seq)
	}
	if !evs[1].At.Equal(now) {
		t.Errorf("event timestamp round-trip: got %v want %v", evs[1].At, now)
	}
	// cursor: only events after seq 1.
	after, err := s.EventsSince(ctx, "r1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Kind != event.StepDone {
		t.Errorf("EventsSince cursor wrong: %+v", after)
	}
}

func TestSQLiteRejectsEventForUnknownRun(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	// No CreateRun: the steps.run_id foreign key rejects the orphan transition
	// (during UpsertStep, before any event is written) — nothing is committed.
	err := s.SaveStepTransition(ctx,
		core.StepState{RunID: "ghost", StepID: "a", Status: core.StepRunning},
		[]event.Event{{RunID: "ghost", StepID: "a", Kind: event.StepStarted, At: time.Now()}})
	if err == nil {
		t.Fatal("expected FK violation for unknown run, got nil")
	}
}

func TestSQLiteArtifactRefsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)
	if err := s.CreateRun(ctx, core.RunState{ID: "r1", Name: "f", FlowYAML: "x", Status: core.RunRunning}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveStepTransition(ctx,
		core.StepState{RunID: "r1", StepID: "a", Status: core.StepSucceeded, Attempt: 1,
			Artifacts: []core.Artifact{{StepID: "a", Path: "/w/a.md", Branch: "step/a", Commit: "deadbeef"}}},
		nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	arts := got.Steps[0].Artifacts
	if len(arts) != 1 || arts[0].StepID != "a" || arts[0].Path != "/w/a.md" ||
		arts[0].Branch != "step/a" || arts[0].Commit != "deadbeef" {
		t.Fatalf("artifact did not round-trip: %+v", arts)
	}
}

func TestRunRepoBaseRoundTrip(t *testing.T) {
	st := tempDB(t)
	ctx := context.Background()

	want := core.RunState{
		ID: "r1", Name: "f", FlowYAML: "name: f\n", Status: core.RunPending,
		Concurrency: 1, Repo: "/abs/path/proj", Base: "abc123def",
	}
	if err := st.CreateRun(ctx, want); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.GetRun(ctx, "r1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Repo != want.Repo || got.Base != want.Base {
		t.Errorf("GetRun repo/base = %q/%q, want %q/%q", got.Repo, got.Base, want.Repo, want.Base)
	}

	inc, err := st.LoadIncompleteRuns(ctx)
	if err != nil {
		t.Fatalf("load incomplete: %v", err)
	}
	if len(inc) != 1 || inc[0].Repo != want.Repo || inc[0].Base != want.Base {
		t.Errorf("LoadIncompleteRuns repo/base = %+v, want repo/base %q/%q", inc, want.Repo, want.Base)
	}
}

func TestSQLiteReclaimableRuns(t *testing.T) {
	st := tempDB(t)
	ctx := context.Background()

	if err := st.CreateRun(ctx, core.RunState{ID: "done", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunStatus(ctx, "done", core.RunSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(ctx, core.RunState{ID: "active", Status: core.RunPending}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunStatus(ctx, "active", core.RunRunning, ""); err != nil {
		t.Fatal(err)
	}

	got, err := st.ReclaimableRuns(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !sameRunIDSet(got, []core.RunID{"done"}) {
		t.Errorf("future cutoff = %v, want [done]", got)
	}
	got, err = st.ReclaimableRuns(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("past cutoff = %v, want none", got)
	}
}

func TestSQLiteLoadIncompleteRuns(t *testing.T) {
	ctx := context.Background()
	s := tempDB(t)

	// r1: running, with a succeeded step (+artifact) and a running step.
	if err := s.CreateRun(ctx, core.RunState{ID: "r1", Name: "f", FlowYAML: "yaml1", Status: core.RunRunning, Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveStepTransition(ctx,
		core.StepState{RunID: "r1", StepID: "a", Status: core.StepSucceeded, Attempt: 1,
			Artifacts: []core.Artifact{{StepID: "a", Path: "/w/a.md"}}},
		nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveStepTransition(ctx,
		core.StepState{RunID: "r1", StepID: "b", Status: core.StepRunning, Attempt: 1}, nil); err != nil {
		t.Fatal(err)
	}
	// r2: succeeded — must NOT be returned.
	if err := s.CreateRun(ctx, core.RunState{ID: "r2", Name: "done", FlowYAML: "yaml2", Status: core.RunSucceeded}); err != nil {
		t.Fatal(err)
	}

	inc, err := s.LoadIncompleteRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inc) != 1 || inc[0].ID != "r1" {
		t.Fatalf("want only r1 incomplete, got %+v", inc)
	}
	r := inc[0]
	if r.FlowYAML != "yaml1" || r.Concurrency != 2 {
		t.Errorf("run fields not loaded: %+v", r)
	}
	if len(r.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(r.Steps))
	}
	var a core.StepState
	for _, st := range r.Steps {
		if st.StepID == "a" {
			a = st
		}
	}
	if a.Status != core.StepSucceeded || len(a.Artifacts) != 1 || a.Artifacts[0].Path != "/w/a.md" {
		t.Errorf("succeeded step's artifacts not loaded for resume: %+v", a)
	}
}

func TestSQLitePing(t *testing.T) {
	s := tempDB(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("SQLite.Ping = %v, want nil", err)
	}
}
