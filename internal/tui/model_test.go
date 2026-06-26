package tui

import (
	"testing"

	"concentus/internal/event"
)

func hasCmd[T any](cmds []any) bool {
	for _, c := range cmds {
		if _, ok := c.(T); ok {
			return true
		}
	}
	return false
}

func TestRunsLoadedPopulatesListAndKeepsSelection(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Name: "f", Status: "running"}, {ID: "b2", Name: "g", Status: "done"}})
	if len(m.runs) != 2 || m.sel != 0 {
		t.Fatalf("runs=%d sel=%d", len(m.runs), m.sel)
	}
}

func TestEnterFocusesSelectedRun(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, cmds := update(m, keyMsg('\r')) // enter
	if m.focus != "a1" {
		t.Fatalf("focus=%q", m.focus)
	}
	if !hasCmd[cmdFocus](cmds) {
		t.Fatalf("want a cmdFocus, got %T", cmds)
	}
}

func TestGateAwaitingEnablesApprove(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running", Steps: []StepView{{ID: "plan", Status: "awaiting_gate"}}})
	step, ok := m.gateStep()
	if !ok || step.ID != "plan" {
		t.Fatalf("gateStep=%+v ok=%v", step, ok)
	}
	// 'a' approve -> a cmdApprove for the gate step
	_, cmds := update(m, keyMsg('a'))
	if !hasCmd[cmdApprove](cmds) {
		t.Fatalf("want cmdApprove, got %#v", cmds)
	}
}

func TestApproveIgnoredWhenNoGate(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running", Steps: []StepView{{ID: "plan", Status: "running"}}})
	_, cmds := update(m, keyMsg('a'))
	if hasCmd[cmdApprove](cmds) {
		t.Fatal("approve must be a no-op when no gate is awaiting")
	}
}

func TestRetryOnlyWhenTerminal(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running"})
	if _, cmds := update(m, keyMsg('R')); hasCmd[cmdRetry](cmds) {
		t.Fatal("retry must be a no-op on a running run")
	}
	m, _ = update(m, runSnapshot{ID: "a1", Status: "failed"})
	if _, cmds := update(m, keyMsg('R')); !hasCmd[cmdRetry](cmds) {
		t.Fatal("retry must fire on a failed run")
	}
}

func TestSseEventAppendsToLogAndRefreshes(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	before := len(m.log)
	m, cmds := update(m, sseEvent(event.Event{Seq: 7, RunID: "a1", StepID: "plan", Kind: event.GateAwaiting}))
	if len(m.log) != before+1 {
		t.Fatalf("log not appended: %d->%d", before, len(m.log))
	}
	if m.lastSeq != 7 {
		t.Fatalf("lastSeq=%d", m.lastSeq)
	}
	if !hasCmd[cmdRefresh](cmds) { // lifecycle event triggers a snapshot-only refresh
		t.Fatalf("want a cmdRefresh after a lifecycle event")
	}
	if hasCmd[cmdFocus](cmds) { // ...and must NOT re-focus (that would reopen the SSE stream)
		t.Fatal("lifecycle refresh must not emit cmdFocus (would tear down the SSE stream)")
	}
}

func TestCancelRequiresConfirm(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running"})
	// 'c' must arm a confirm, NOT cancel yet.
	m, cmds := update(m, keyMsg('c'))
	if hasCmd[cmdCancel](cmds) {
		t.Fatal("cancel must not fire before confirm")
	}
	if m.mode != modeConfirmCancel {
		t.Fatalf("mode=%v, want modeConfirmCancel", m.mode)
	}
	// 'y' confirms and fires the cancel, returning to normal mode.
	m, cmds = update(m, keyMsg('y'))
	if !hasCmd[cmdCancel](cmds) {
		t.Fatal("y must confirm the cancel")
	}
	if m.mode != modeNormal {
		t.Fatalf("mode=%v, want modeNormal after confirm", m.mode)
	}
}

func TestCancelAbortedByOtherKey(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running"})
	m, _ = update(m, keyMsg('c'))
	m, cmds := update(m, keyMsg('n')) // any non-y key aborts
	if hasCmd[cmdCancel](cmds) || m.mode != modeNormal {
		t.Fatalf("n must abort the cancel confirm (mode=%v)", m.mode)
	}
}

func TestReasonInputThenReject(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running", Steps: []StepView{{ID: "plan", Status: "awaiting_gate"}}})
	// 'r' enters reason-input mode (no command yet).
	m, cmds := update(m, keyMsg('r'))
	if hasCmd[cmdApprove](cmds) || m.mode != modeReason {
		t.Fatalf("r must enter reason mode without rejecting (mode=%v)", m.mode)
	}
	for _, ch := range "no" { // type the reason
		m, _ = update(m, keyMsg(ch))
	}
	if m.reasonBuf != "no" {
		t.Fatalf("reasonBuf=%q, want \"no\"", m.reasonBuf)
	}
	// Enter submits reject with the typed reason.
	m, cmds = update(m, keyMsg('\r'))
	if m.mode != modeNormal {
		t.Fatalf("mode=%v, want modeNormal after submit", m.mode)
	}
	for _, c := range cmds {
		if ap, ok := c.(cmdApprove); ok {
			if ap.OK != false || ap.Reason != "no" {
				t.Fatalf("got %+v, want reject with reason \"no\"", ap)
			}
			return
		}
	}
	t.Fatal("expected a cmdApprove on reason submit")
}

func TestReasonInputEscAborts(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running", Steps: []StepView{{ID: "plan", Status: "awaiting_gate"}}})
	m, _ = update(m, keyMsg('r'))
	m, _ = update(m, keyMsg('x'))
	m, cmds := update(m, keyMsg(27)) // esc
	if hasCmd[cmdApprove](cmds) || m.mode != modeNormal || m.reasonBuf != "" {
		t.Fatalf("esc must abort reason input (mode=%v buf=%q)", m.mode, m.reasonBuf)
	}
}

func TestQuitKey(t *testing.T) {
	_, cmds := update(initialModel(), keyMsg('q'))
	if !hasCmd[cmdQuit](cmds) {
		t.Fatal("q must request quit")
	}
}

// A redraw (e.g. on terminal resize) must force a render without touching the
// connection indicator — only the poll loop owns conn. Regression guard for the
// SIGWINCH-sends-connMsg(true) false-"connected" flip.
func TestRedrawMsgPreservesConnAndEmitsNoCommands(t *testing.T) {
	m := model{conn: false}
	got, cmds := update(m, redrawMsg{})
	if got.conn {
		t.Fatalf("redrawMsg changed conn to true, want false")
	}
	if cmds != nil {
		t.Fatalf("redrawMsg emitted commands %v, want none", cmds)
	}
}
