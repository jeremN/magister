package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"concentus/internal/core"
	"concentus/internal/engine"
	"concentus/internal/flow"
	"concentus/internal/host"
	"concentus/internal/workspace"
)

// tracer is shared by the delivery spans in pr.go and ship.go too.
var tracer = otel.Tracer("concentus")

// Supervisor owns all active runs: it persists+starts new ones, cancels them,
// routes gate approvals, resumes incomplete runs on startup, and drains on
// shutdown. The engine is stateless config shared across runs.
type Supervisor struct {
	engine *engine.Engine
	store  core.Store
	reg    *ApprovalRegistry

	// Log records non-fatal resume issues; nil = discard. The daemon wires a real one.
	Log *slog.Logger

	// Host is the gh-backed PR client; nil → a default host.New() (the gh CLI on PATH).
	Host *host.Runner

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
	if err := s.engine.Provision(ctx, id, repo, base); err != nil {
		return "", fmt.Errorf("provision run: %w", err)
	}
	s.start(ctx, id, func(runCtx context.Context) error { return s.engine.Run(runCtx, id, f) })
	return id, nil
}

// start launches a run goroutine under a cancelable context registered for
// cancellation and shutdown. The base context is derived from context.Background()
// so a run outlives the HTTP request that submitted it. When parent carries a valid
// span, its span context is propagated via trace.ContextWithRemoteSpanContext so the
// run-root span is a child of the submit span — carrying the trace without the
// request's cancellation.
func (s *Supervisor) start(parent context.Context, id core.RunID, run func(context.Context) error) {
	base := context.Background()
	if sc := trace.SpanContextFromContext(parent); sc.IsValid() {
		base = trace.ContextWithRemoteSpanContext(base, sc)
	}
	runCtx, cancel := context.WithCancel(base)
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

// CancelError carries an HTTP status so the API layer maps cancel failures without
// string-matching. Status 404 = unknown run; 409 = run is known but not active.
type CancelError struct {
	Status int
	Msg    string
}

func (e *CancelError) Error() string { return e.Msg }

// Cancel cancels an active run. Returns nil on success, *CancelError(404) for an
// unknown run, *CancelError(409) for a known-but-terminal (non-active) run, and a
// plain (non-*CancelError) error on a store-load failure → the handler maps it to 500.
func (s *Supervisor) Cancel(ctx context.Context, id core.RunID) error {
	s.mu.Lock()
	cancel, ok := s.runs[id]
	s.mu.Unlock()
	if ok {
		cancel()
		return nil
	}
	// Not in the active map: distinguish unknown vs already-terminal via the store.
	if _, err := s.store.GetRun(ctx, id); err != nil {
		if errors.Is(err, core.ErrRunNotFound) {
			return &CancelError{Status: http.StatusNotFound, Msg: "unknown run"}
		}
		// A transient store failure is NOT a missing run: surface it as a plain error
		// so the handler's non-*CancelError fallback writes 500 (mirrors Retry/ReclaimRun).
		return fmt.Errorf("load run %q: %w", id, err)
	}
	return &CancelError{Status: http.StatusConflict, Msg: "run not active"}
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

func (s *Supervisor) hostRunner() *host.Runner {
	if s.Host != nil {
		return s.Host
	}
	return host.New()
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
		if err := s.resumeRun(context.Background(), rs, f); err != nil {
			s.logger().Error("resume: provision run", "run", rs.ID, "err", err)
			continue
		}
	}
	return nil
}

// resumeRun resets the run's non-succeeded steps to pending, re-provisions its
// scratch spec, and starts engine.Resume under the run's own id. Shared by
// ResumeAll (startup reconciliation) and Retry (on-demand), so the two resume
// paths cannot drift. Returns a non-nil error only when provisioning fails; the
// caller decides whether that is fatal.
func (s *Supervisor) resumeRun(ctx context.Context, rs core.RunState, f *flow.Flow) error {
	s.resetIncompleteSteps(ctx, rs)
	if err := s.engine.Provision(ctx, rs.ID, rs.Repo, rs.Base); err != nil {
		return fmt.Errorf("provision run: %w", err)
	}
	s.start(ctx, rs.ID, func(runCtx context.Context) error { return s.engine.Resume(runCtx, rs, f) })
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

// PushOpts configures Push. Zero values mean: origin remote, magister/<runID>
// destination, the unique terminal step, no force.
type PushOpts struct {
	Remote string // "" → source's origin; a remote name or a URL otherwise
	As     string // "" → magister/<runID>
	Step   string // "" → the unique terminal step
	Force  bool
}

// PushResult is returned by Push on success.
type PushResult struct {
	Remote       string
	Branch       string // destination branch on the remote
	SourceBranch string // the run's result branch that was pushed
	Commit       string
}

// PushError carries an HTTP status so the API layer maps failures without
// string-matching.
type PushError struct {
	Status int
	Msg    string
}

func (e *PushError) Error() string { return e.Msg }

func pushErr(status int, format string, a ...any) *PushError {
	return &PushError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// Push delivers a succeeded external-repo run's result branch to a remote. It is a
// post-run, store-driven operation (the engine lifecycle is untouched): it reads the
// run, identifies the result step (the unique terminal, or opts.Step), reads that
// step's persisted branch, resolves the remote, and pushes from the scratch clone.
// Errors are *PushError with an HTTP status (see the slice-2 spec).
func (s *Supervisor) Push(ctx context.Context, runID core.RunID, opts PushOpts) (_ PushResult, err error) {
	ctx, span := tracer.Start(ctx, "push "+string(runID))
	defer span.End()
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}()
	rs, err := s.store.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, core.ErrRunNotFound) {
			return PushResult{}, pushErr(http.StatusNotFound, "unknown run %q", runID)
		}
		return PushResult{}, pushErr(http.StatusInternalServerError, "load run %q: %v", runID, err)
	}
	if rs.Repo == "" {
		return PushResult{}, pushErr(http.StatusBadRequest, "run %q is not an external-repo run (no --repo)", runID)
	}
	if rs.Status != core.RunSucceeded {
		return PushResult{}, pushErr(http.StatusConflict, "run %q is %s, not succeeded", runID, rs.Status)
	}
	f, err := flow.ParseBytes([]byte(rs.FlowYAML))
	if err != nil {
		return PushResult{}, pushErr(http.StatusInternalServerError, "parse stored flow: %v", err)
	}
	step, perr := pickResultStep(f, opts.Step)
	if perr != nil {
		return PushResult{}, perr
	}
	branch, commit := stepBranch(rs, step.ID)
	if branch == "" {
		return PushResult{}, pushErr(http.StatusBadRequest, "step %q has no branch (not an isolated/committed step)", step.ID)
	}
	remoteURL, err := workspace.ResolveRemote(ctx, rs.Repo, opts.Remote)
	if err != nil {
		return PushResult{}, pushErr(http.StatusBadRequest, "remote: %v", err)
	}
	dest := opts.As
	if dest == "" {
		dest = "magister/" + string(runID)
	}
	base := s.engine.BasePath(runID)
	if base == "" || !dirHasGit(base) {
		return PushResult{}, pushErr(http.StatusNotFound, "scratch repo for run %q not found (reclaimed?)", runID)
	}
	if err := workspace.PushBranch(ctx, base, remoteURL, branch, dest, opts.Force); err != nil {
		return PushResult{}, pushErr(http.StatusBadGateway, "%v", err)
	}
	return PushResult{Remote: remoteURL, Branch: dest, SourceBranch: branch, Commit: commit}, nil
}

// pickResultStep selects the step whose branch to push: opts.Step if given, else
// the unique terminal step; zero/ambiguous → error.
func pickResultStep(f *flow.Flow, stepID string) (*flow.Step, *PushError) {
	if stepID != "" {
		for _, st := range f.Steps {
			if st.ID == stepID {
				return st, nil
			}
		}
		return nil, pushErr(http.StatusBadRequest, "unknown step %q", stepID)
	}
	terms := flow.TerminalSteps(f)
	switch len(terms) {
	case 1:
		return terms[0], nil
	case 0:
		return nil, pushErr(http.StatusBadRequest, "flow has no terminal step")
	default:
		ids := make([]string, len(terms))
		for i, t := range terms {
			ids[i] = t.ID
		}
		return nil, pushErr(http.StatusBadRequest, "ambiguous result: %d terminal steps %v; use --step", len(terms), ids)
	}
}

// stepBranch returns the persisted branch+commit a step committed to (carried on
// each of its artifacts); empty branch if the step committed nothing.
func stepBranch(rs core.RunState, stepID string) (branch, commit string) {
	for _, st := range rs.Steps {
		if st.StepID != stepID {
			continue
		}
		for _, a := range st.Artifacts {
			if a.Branch != "" {
				return a.Branch, a.Commit
			}
		}
	}
	return "", ""
}

func dirHasGit(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
