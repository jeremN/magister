// Package store holds implementations of core.Store. Mem is the in-memory one
// used by M1 tests and the keyless demo; SQLite replaces it in M2 behind the
// same interface.
package store

import (
	"context"
	"fmt"
	"sync"

	"concentus/internal/core"
	"concentus/internal/event"
)

var _ core.Store = (*Mem)(nil)

type Mem struct {
	mu     sync.Mutex
	runs   map[core.RunID]*core.RunState
	events map[core.RunID][]event.Event
	seq    int64
}

func NewMem() *Mem {
	return &Mem{
		runs:   make(map[core.RunID]*core.RunState),
		events: make(map[core.RunID][]event.Event),
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
	return nil
}

func (m *Mem) SaveStepTransition(_ context.Context, st core.StepState, evs []event.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[st.RunID]
	if !ok {
		return fmt.Errorf("unknown run %q", st.RunID)
	}
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
	return nil
}

func (m *Mem) LoadIncompleteRuns(context.Context) ([]core.RunState, error) {
	// M1 is single-process and non-resuming; nothing to load. Resume lands in M2.
	return nil, nil
}

func (m *Mem) GetRun(_ context.Context, id core.RunID) (core.RunState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return core.RunState{}, fmt.Errorf("unknown run %q", id)
	}
	out := *r
	out.Steps = make([]core.StepState, len(r.Steps))
	for i, st := range r.Steps {
		st.Artifacts = append([]core.Artifact(nil), st.Artifacts...)
		out.Steps[i] = st
	}
	return out, nil
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
