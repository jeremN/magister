package tui

import (
	"strings"
	"testing"

	"concentus/internal/event"
)

func TestViewShowsRunsAndKeyBar(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Name: "feature", Status: "running"}})
	out := view(m, 80, 24)
	if !strings.Contains(out, "feature") || !strings.Contains(out, "running") {
		t.Fatalf("runs not rendered:\n%s", out)
	}
	if !strings.Contains(out, "approve") || !strings.Contains(out, "quit") {
		t.Fatalf("key bar missing:\n%s", out)
	}
	if got := strings.Count(out, "\n") + 1; got != 24 {
		t.Fatalf("want 24 rows, got %d", got)
	}
}

func TestViewHighlightsGateAndLog(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Name: "feature", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Name: "feature", Status: "running", Steps: []StepView{{ID: "plan", Status: "awaiting_gate"}}})
	m, _ = update(m, sseEvent(event.Event{Seq: 1, RunID: "a1", StepID: "plan", Kind: event.GateAwaiting}))
	out := view(m, 100, 24)
	if !strings.Contains(out, "plan") || !strings.Contains(out, "awaiting_gate") {
		t.Fatalf("gate step not shown:\n%s", out)
	}
	if !strings.Contains(out, "approve?") {
		t.Fatalf("gate highlight marker not shown:\n%s", out)
	}
	if !strings.Contains(out, "gate.awaiting") {
		t.Fatalf("event log not shown:\n%s", out)
	}
}

func TestViewDisconnectedBanner(t *testing.T) {
	m := initialModel()
	m, _ = update(m, connMsg(false))
	if !strings.Contains(view(m, 80, 24), "disconnected") {
		t.Fatal("want a disconnected indicator")
	}
}

func TestViewShowsCancelConfirm(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running"})
	m, _ = update(m, keyMsg('c'))
	if !strings.Contains(view(m, 80, 24), "(y/n)") {
		t.Fatal("want a cancel confirm prompt in the status bar")
	}
}

func TestViewShowsReasonPrompt(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Status: "running", Steps: []StepView{{ID: "plan", Status: "awaiting_gate"}}})
	m, _ = update(m, keyMsg('r'))
	m, _ = update(m, keyMsg('n'))
	m, _ = update(m, keyMsg('o'))
	out := view(m, 80, 24)
	if !strings.Contains(out, "reason") || !strings.Contains(out, "no") {
		t.Fatalf("want a reason editor showing the typed text:\n%s", out)
	}
}

func TestViewFitsNarrowTerminal(t *testing.T) {
	m := initialModel()
	m, _ = update(m, runsLoaded{{ID: "a1", Name: "feature", Status: "running"}})
	m, _ = update(m, keyMsg('\r'))
	m, _ = update(m, runSnapshot{ID: "a1", Name: "feature", Status: "running", Steps: []StepView{{ID: "plan", Status: "awaiting_gate"}, {ID: "build", Status: "running"}}})
	m, _ = update(m, sseEvent(event.Event{Seq: 1, RunID: "a1", StepID: "plan", Kind: event.GateAwaiting}))

	for _, tc := range []struct{ w, h int }{{10, 5}, {1, 3}} {
		out := view(m, tc.w, tc.h) // must not panic
		rows := strings.Split(out, "\n")
		if len(rows) != tc.h {
			t.Fatalf("w=%d h=%d: want %d rows, got %d:\n%s", tc.w, tc.h, tc.h, len(rows), out)
		}
		for i, row := range rows {
			if n := len([]rune(row)); n > tc.w {
				t.Fatalf("w=%d h=%d: row %d is %d runes (> w):\n%q", tc.w, tc.h, i, n, row)
			}
		}
	}
}

func TestClipNegativeWidthIsSafe(t *testing.T) {
	// clip must be total: a negative or zero width returns "" without panicking.
	if got := clip("abc", -1); got != "" {
		t.Fatalf("clip(_, -1) = %q, want \"\"", got)
	}
	if got := clip("abc", 0); got != "" {
		t.Fatalf("clip(_, 0) = %q, want \"\"", got)
	}
}
