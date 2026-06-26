package tui

import (
	"concentus/internal/event"
)

// ---- messages (driver -> reducer) ----
type runsLoaded []RunSummary
type runSnapshot RunDetail
type sseEvent event.Event
type keyMsg rune
type actionResult struct{ Err error }
type connMsg bool
type redrawMsg struct{} // force a render without mutating state (e.g. on resize)

// ---- commands (reducer -> driver) ----
type cmdFocus string   // snapshot + (re)open SSE for this run id (on enter)
type cmdRefresh string // snapshot-only refresh, no SSE change (on lifecycle event)
type cmdApprove struct {
	ID, Step string
	OK       bool
	Reason   string
}
type cmdCancel string
type cmdRetry string
type cmdQuit struct{}

const logCap = 500

// statusAwaitingGate mirrors core.StepAwaitingGate (daemon wire value).
// A step blocked at a gate is emitted as "awaiting_gate" in the run-detail DTO.
// "gate.awaiting" is an event Kind, never a step status — do not use it here.
const statusAwaitingGate = "awaiting_gate"

// inputMode is the keyboard interaction mode: normal navigation, a y/n confirm
// before a destructive cancel, or a one-line reason editor before a reject.
type inputMode int

const (
	modeNormal inputMode = iota
	modeConfirmCancel
	modeReason
)

type model struct {
	runs      []RunSummary
	sel       int       // index into runs
	focus     string    // focused run id ("" = none)
	detail    RunDetail // focused run snapshot
	log       []event.Event
	lastSeq   int64
	conn      bool      // daemon reachable
	status    string    // transient status-bar message (e.g. an action error)
	mode      inputMode // normal / confirm-cancel / reason-input
	reasonBuf string    // accumulates the reject reason while in modeReason
}

func initialModel() model { return model{conn: true} }

func (m model) focusedRun() (RunDetail, bool) {
	if m.focus == "" || m.detail.ID != m.focus {
		return RunDetail{}, false
	}
	return m.detail, true
}

// gateStep returns the focused run's step awaiting a gate, if any.
func (m model) gateStep() (StepView, bool) {
	for _, s := range m.detail.Steps {
		if s.Status == statusAwaitingGate {
			return s, true
		}
	}
	return StepView{}, false
}

func isTerminal(status string) bool {
	return status == "failed" || status == "canceled" || status == "succeeded"
}

func isLifecycle(k event.Kind) bool {
	switch k {
	case event.StepStarted, event.StepDone, event.StepFailed, event.StepRetrying, event.GateAwaiting, event.RunDone:
		return true
	}
	return false
}

func (m *model) appendLog(e event.Event) {
	m.log = append(m.log, e)
	if len(m.log) > logCap {
		m.log = m.log[len(m.log)-logCap:]
	}
}

// update is the pure reducer: it maps the current model + a message to the next
// model and a list of side-effect commands for the driver to run.
func update(m model, ms any) (model, []any) {
	switch v := ms.(type) {
	case runsLoaded:
		m.runs = []RunSummary(v)
		m.conn = true
		if m.sel >= len(m.runs) {
			m.sel = max(0, len(m.runs)-1)
		}
		return m, nil

	case runSnapshot:
		if RunDetail(v).ID == m.focus {
			m.detail = RunDetail(v)
		}
		return m, nil

	case sseEvent:
		e := event.Event(v)
		if e.RunID != m.focus {
			return m, nil
		}
		m.appendLog(e)
		if e.Seq > m.lastSeq {
			m.lastSeq = e.Seq
		}
		if isLifecycle(e.Kind) {
			return m, []any{cmdRefresh(m.focus)} // snapshot-only refresh (keeps the SSE stream)
		}
		return m, nil

	case connMsg:
		m.conn = bool(v)
		return m, nil

	case redrawMsg:
		return m, nil

	case actionResult:
		if v.Err != nil {
			m.status = v.Err.Error()
		} else {
			m.status = ""
		}
		return m, nil

	case keyMsg:
		return updateKey(m, rune(v))
	}
	return m, nil
}

func updateKey(m model, r rune) (model, []any) {
	// Modal keys take precedence so 'q'/'c'/'r' don't leak into a prompt.
	switch m.mode {
	case modeConfirmCancel:
		if r == 'y' || r == 'Y' {
			m.mode = modeNormal
			return m, []any{cmdCancel(m.focus)}
		}
		m.mode = modeNormal // any other key aborts
		return m, nil
	case modeReason:
		switch r {
		case '\r', '\n': // submit the reject with the typed reason
			step, ok := m.gateStep()
			reason := m.reasonBuf
			m.mode = modeNormal
			m.reasonBuf = ""
			if ok {
				return m, []any{cmdApprove{ID: m.focus, Step: step.ID, OK: false, Reason: reason}}
			}
			return m, nil
		case 27: // esc aborts
			m.mode = modeNormal
			m.reasonBuf = ""
			return m, nil
		case 127, 8: // backspace / delete
			if n := len(m.reasonBuf); n > 0 {
				m.reasonBuf = m.reasonBuf[:n-1]
			}
			return m, nil
		default:
			if r >= 32 && r < 127 { // printable ASCII only
				m.reasonBuf += string(r)
			}
			return m, nil
		}
	}

	// modeNormal:
	switch r {
	case 'q', 3: // q or Ctrl-C
		return m, []any{cmdQuit{}}
	case 'j', 14: // down
		if m.sel < len(m.runs)-1 {
			m.sel++
		}
		return m, nil
	case 'k', 16: // up
		if m.sel > 0 {
			m.sel--
		}
		return m, nil
	case '\r', '\n': // enter -> focus selected
		if m.sel < len(m.runs) {
			m.focus = m.runs[m.sel].ID
			m.detail = RunDetail{}
			m.log = nil
			m.lastSeq = 0
			return m, []any{cmdFocus(m.focus)}
		}
		return m, nil
	case 'a': // approve gate
		if s, ok := m.gateStep(); ok {
			return m, []any{cmdApprove{ID: m.focus, Step: s.ID, OK: true}}
		}
		return m, nil
	case 'r': // reject gate -> open the reason editor (no command yet)
		if _, ok := m.gateStep(); ok {
			m.mode = modeReason
			m.reasonBuf = ""
		}
		return m, nil
	case 'c': // cancel active run -> arm the y/n confirm (no command yet)
		if d, ok := m.focusedRun(); ok && !isTerminal(d.Status) {
			m.mode = modeConfirmCancel
		}
		return m, nil
	case 'R': // retry terminal run
		if d, ok := m.focusedRun(); ok && isTerminal(d.Status) {
			return m, []any{cmdRetry(m.focus)}
		}
		return m, nil
	}
	return m, nil
}
