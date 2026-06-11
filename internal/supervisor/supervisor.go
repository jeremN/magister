package supervisor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"concentus/internal/core"
	"concentus/internal/engine"
	"concentus/internal/flow"
)

// Supervisor owns all active runs: it persists+starts new ones, cancels them,
// routes gate approvals, resumes incomplete runs on startup, and drains on
// shutdown. The engine is stateless config shared across runs.
type Supervisor struct {
	engine *engine.Engine
	store  core.Store
	reg    *ApprovalRegistry

	// Log records non-fatal resume issues; nil = discard. The daemon wires a real one.
	Log *slog.Logger

	mu   sync.Mutex
	runs map[core.RunID]context.CancelFunc
	wg   sync.WaitGroup
}

func New(eng *engine.Engine, store core.Store, reg *ApprovalRegistry) *Supervisor {
	return &Supervisor{
		engine: eng, store: store, reg: reg,
		runs: make(map[core.RunID]context.CancelFunc),
	}
}

// NewRunID returns a fresh sortable run ID.
func NewRunID() core.RunID { return core.RunID(ulid.Make().String()) }

// Submit persists a pending run, provisions its workspace (repo+base; empty repo
// = synthetic base), and starts it. Validating the flow and the repo/base is the
// caller's job (the API handler does it before calling Submit).
func (s *Supervisor) Submit(ctx context.Context, f *flow.Flow, flowYAML, repo, base string) (core.RunID, error) {
	id := NewRunID()
	if err := s.store.CreateRun(ctx, core.RunState{
		ID: id, Name: f.Name, FlowYAML: flowYAML, Status: core.RunPending,
		Concurrency: f.Concurrency, Repo: repo, Base: base,
	}); err != nil {
		return "", fmt.Errorf("create run: %w", err)
	}
	if err := s.engine.Provision(id, repo, base); err != nil {
		return "", fmt.Errorf("provision run: %w", err)
	}
	s.start(id, func(runCtx context.Context) error { return s.engine.Run(runCtx, id, f) })
	return id, nil
}

// start launches a run goroutine under a cancelable context registered for
// cancellation and shutdown. The context is derived from context.Background(),
// not any request context, so a run outlives the HTTP request that submitted it.
func (s *Supervisor) start(id core.RunID, run func(context.Context) error) {
	runCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.runs[id] = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.runs, id)
			s.mu.Unlock()
			cancel()
		}()
		_ = run(runCtx) // terminal status is persisted by the engine
	}()
}

// Cancel cancels an active run. Returns false if the run isn't active.
func (s *Supervisor) Cancel(id core.RunID) bool {
	s.mu.Lock()
	cancel, ok := s.runs[id]
	s.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// Approve resolves a pending manual gate. Returns false if nothing is awaiting.
func (s *Supervisor) Approve(id core.RunID, stepID string, approved bool, reason string) bool {
	return s.reg.Resolve(id, stepID, Decision{Approved: approved, Reason: reason})
}

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func (s *Supervisor) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return discardLogger
}

// resetIncompleteSteps marks every non-succeeded step of a resumed run as pending,
// so observers don't see a stale actionable status (e.g. awaiting_gate) before the
// engine re-runs the step. Succeeded steps are left intact — they seed downstream
// inputs (spec §7). Startup reconciliation, so no event is emitted.
func (s *Supervisor) resetIncompleteSteps(ctx context.Context, rs core.RunState) {
	for _, st := range rs.Steps {
		if st.Status == core.StepSucceeded {
			continue
		}
		reset := core.StepState{RunID: rs.ID, StepID: st.StepID, Status: core.StepPending}
		if err := s.store.SaveStepTransition(ctx, reset, nil); err != nil {
			// Non-fatal: the engine re-runs the step regardless; only the visible
			// status stays stale. Log and continue.
			s.logger().Error("resume: reset step to pending", "run", rs.ID, "step", st.StepID, "err", err)
		}
	}
}

// ResumeAll loads incomplete runs from the store and resumes each (startup). A run
// with an unparseable/invalid flow is skipped (logged), not fatal to the others.
func (s *Supervisor) ResumeAll(ctx context.Context) error {
	runs, err := s.store.LoadIncompleteRuns(ctx)
	if err != nil {
		return fmt.Errorf("load incomplete runs: %w", err)
	}
	for _, rs := range runs {
		f, err := flow.ParseBytes([]byte(rs.FlowYAML))
		if err != nil {
			s.logger().Error("resume: skip run with unparseable flow", "run", rs.ID, "err", err)
			continue
		}
		if err := flow.Validate(f); err != nil {
			s.logger().Error("resume: skip run with invalid flow", "run", rs.ID, "err", err)
			continue
		}
		s.resetIncompleteSteps(ctx, rs)
		rs := rs
		if err := s.engine.Provision(rs.ID, rs.Repo, rs.Base); err != nil {
			s.logger().Error("resume: provision run", "run", rs.ID, "err", err)
			continue
		}
		s.start(rs.ID, func(runCtx context.Context) error { return s.engine.Resume(runCtx, rs, f) })
	}
	return nil
}

// Shutdown cancels all active runs and waits for them to unwind, up to timeout.
func (s *Supervisor) Shutdown(timeout time.Duration) {
	s.mu.Lock()
	for _, cancel := range s.runs {
		cancel()
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}
