// Package store holds implementations of core.Store. Mem is the in-memory one
// used by M1 tests and the keyless demo; SQLite replaces it in M2 behind the
// same interface.
package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"concentus/internal/core"
	"concentus/internal/event"
)

var _ core.Store = (*Mem)(nil)

type Mem struct {
	mu        sync.Mutex
	runs      map[core.RunID]*core.RunState
	events    map[core.RunID][]event.Event
	createdAt map[core.RunID]time.Time
	updatedAt map[core.RunID]time.Time
	reclaimed map[core.RunID]bool
	seq       int64
}

func NewMem() *Mem {
	return &Mem{
		runs:      make(map[core.RunID]*core.RunState),
		events:    make(map[core.RunID][]event.Event),
		createdAt: make(map[core.RunID]time.Time),
		updatedAt: make(map[core.RunID]time.Time),
		reclaimed: make(map[core.RunID]bool),
	}
}

func (m *Mem) CreateRun(_ context.Context, r core.RunState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[r.ID]; ok {
		return fmt.Errorf("run %q already exists", r.ID)
	}
	cp := r
	m.runs[r.ID] = &cp
	now := time.Now()
	m.createdAt[r.ID] = now
	m.updatedAt[r.ID] = now
	return nil
}

func (m *Mem) SaveStepTransition(_ context.Context, st core.StepState, evs []event.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[st.RunID]
	if !ok {
		return fmt.Errorf("unknown run %q", st.RunID)
	}
	// Own our copy of the artifacts: the engine keeps this slice after persisting
	// it and stamps branch/commit onto it in place (commitIsolated) once a blocking
	// gate passes. Without this copy that in-place write would race a concurrent
	// GetRun→cloneRun. Mirrors the read-side deep copy.
	st.Artifacts = cloneArtifacts(st.Artifacts)
	found := false
	for i := range r.Steps {
		if r.Steps[i].StepID == st.StepID {
			r.Steps[i] = st
			found = true
			break
		}
	}
	if !found {
		r.Steps = append(r.Steps, st)
	}
	for _, e := range evs {
		m.seq++
		e.Seq = m.seq
		m.events[st.RunID] = append(m.events[st.RunID], e)
	}
	return nil
}

func (m *Mem) SetRunStatus(_ context.Context, id core.RunID, status core.RunStatus, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return fmt.Errorf("unknown run %q", id)
	}
	r.Status = status
	r.Err = errMsg
	m.updatedAt[id] = time.Now()
	return nil
}

func (m *Mem) AppendEvents(_ context.Context, id core.RunID, evs []event.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[id]; !ok {
		return fmt.Errorf("unknown run %q", id)
	}
	for _, e := range evs {
		m.seq++
		e.Seq = m.seq
		m.events[id] = append(m.events[id], e)
	}
	return nil
}

// LoadIncompleteRuns returns every run still pending or running, deep-copied,
// so it mirrors the SQLite store and stays a faithful test double for resume.
func (m *Mem) LoadIncompleteRuns(context.Context) ([]core.RunState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.RunState
	for _, r := range m.runs {
		if r.Status == core.RunPending || r.Status == core.RunRunning {
			out = append(out, cloneRun(r))
		}
	}
	return out, nil
}

func (m *Mem) GetRun(_ context.Context, id core.RunID) (core.RunState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return core.RunState{}, fmt.Errorf("unknown run %q: %w", id, core.ErrRunNotFound)
	}
	return cloneRun(r), nil
}

// cloneRun returns a deep copy whose Steps/Artifacts slices share nothing with
// the store's internal arrays, so callers can read concurrently with execution
// without racing (spec §17).
func cloneRun(r *core.RunState) core.RunState {
	out := *r
	out.Steps = make([]core.StepState, len(r.Steps))
	for i, st := range r.Steps {
		st.Artifacts = cloneArtifacts(st.Artifacts)
		out.Steps[i] = st
	}
	return out
}

// cloneArtifacts copies an artifact slice so the store and its callers never
// share a backing array (used on both the write and read paths).
func cloneArtifacts(a []core.Artifact) []core.Artifact {
	return append([]core.Artifact(nil), a...)
}

func (m *Mem) ListRuns(_ context.Context, f core.Filter) ([]core.RunSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.RunSummary
	for _, r := range m.runs {
		if f.Status != "" && r.Status != f.Status {
			continue
		}
		out = append(out, core.RunSummary{ID: r.ID, Name: r.Name, Status: r.Status})
	}
	// Mirror SQLite's ORDER BY created_at DESC, id (ASC) so the in-memory double
	// returns results in the same deterministic order as the production store.
	sort.Slice(out, func(i, j int) bool {
		ci, cj := m.createdAt[out[i].ID], m.createdAt[out[j].ID]
		if !ci.Equal(cj) {
			return ci.After(cj) // created_at DESC
		}
		return out[i].ID < out[j].ID // id ASC (tie-break)
	})
	return out, nil
}

func (m *Mem) EventsSince(_ context.Context, id core.RunID, seq int64) ([]event.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []event.Event
	for _, e := range m.events[id] {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	return out, nil
}

func isTerminal(s core.RunStatus) bool {
	return s == core.RunSucceeded || s == core.RunFailed || s == core.RunCanceled
}

func (m *Mem) MarkReclaimed(_ context.Context, id core.RunID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Mirror SQLite's behaviour: an UPDATE on a missing id affects 0 rows and
	// returns no error. Mark only if the run is present; otherwise no-op.
	if _, ok := m.runs[id]; ok {
		m.reclaimed[id] = true
	}
	return nil
}

func (m *Mem) ReclaimableRuns(_ context.Context, before time.Time) ([]core.RunID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.RunID
	for id, r := range m.runs {
		if !isTerminal(r.Status) || m.reclaimed[id] {
			continue
		}
		if u, ok := m.updatedAt[id]; ok && u.Before(before) {
			out = append(out, id)
		}
	}
	return out, nil
}

// Ping always succeeds: the in-memory store is reachable whenever the process is.
func (m *Mem) Ping(_ context.Context) error {
	return nil
}
