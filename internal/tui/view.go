package tui

import (
	"fmt"
	"strings"
)

func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) > w {
		if w <= 1 {
			return string(runes[:w])
		}
		return string(runes[:w-1]) + "…"
	}
	return s + strings.Repeat(" ", w-len(runes))
}

func view(m model, w, h int) string {
	rows := make([]string, 0, h)

	// Header.
	conn := "connected"
	if !m.conn {
		conn = "disconnected"
	}
	title := "cm tui — magisterd"
	if d, ok := m.focusedRun(); ok {
		title = fmt.Sprintf("cm tui — %s %s [%s]", d.Name, d.ID, d.Status)
	}
	rows = append(rows, clip(fmt.Sprintf("%s    (%s)", title, conn), w))

	// Body: left runs column (width ~24), right detail/log.
	left := w / 3
	if left < 16 {
		left = 16
	}
	if left > 30 {
		left = 30
	}
	right := w - left - 1
	if right < 1 {
		right = 1
	}

	bodyH := h - 2 // minus header + key bar
	if bodyH < 1 {
		bodyH = 1
	}

	// Left lines.
	leftLines := make([]string, 0, bodyH)
	leftLines = append(leftLines, clip("RUNS", left))
	for i, r := range m.runs {
		cursor := "  "
		if i == m.sel {
			cursor = "> "
		}
		name := r.Name
		if name == "" {
			name = r.ID
		}
		leftLines = append(leftLines, clip(fmt.Sprintf("%s%s %s", cursor, name, r.Status), left))
	}

	// Right lines: steps then a separator then the log tail.
	rightLines := make([]string, 0, bodyH)
	if d, ok := m.focusedRun(); ok {
		rightLines = append(rightLines, clip("STEPS", right))
		for _, s := range d.Steps {
			mark := ""
			if s.Status == statusAwaitingGate {
				mark = "  <-- approve?"
			}
			rightLines = append(rightLines, clip(fmt.Sprintf("%-12s %s%s", s.ID, s.Status, mark), right))
		}
		rightLines = append(rightLines, clip("EVENTS", right))
		start := 0
		remaining := bodyH - len(rightLines)
		if len(m.log) > remaining && remaining > 0 {
			start = len(m.log) - remaining
		}
		for _, e := range m.log[start:] {
			rightLines = append(rightLines, clip(fmt.Sprintf("%s %s %s", e.At.Format("15:04:05"), e.Kind, e.StepID), right))
		}
	} else {
		rightLines = append(rightLines, clip("(select a run and press enter)", right))
	}

	for i := 0; i < bodyH; i++ {
		l := clip("", left)
		if i < len(leftLines) {
			l = leftLines[i]
		}
		r := clip("", right)
		if i < len(rightLines) {
			r = rightLines[i]
		}
		rows = append(rows, clip(l+" "+r, w))
	}

	// Status / key bar. Modal prompts take over the bar.
	bar := "j/k select · enter open · a approve · r reject · c cancel · R retry · q quit"
	switch m.mode {
	case modeConfirmCancel:
		bar = fmt.Sprintf("cancel run %s? (y/n)", m.focus)
	case modeReason:
		bar = "reject reason: " + m.reasonBuf + "_"
	default:
		if m.status != "" {
			bar = "! " + m.status
		}
	}
	rows = append(rows, clip(bar, w))

	// Force exactly h rows.
	for len(rows) < h {
		rows = append(rows, clip("", w))
	}
	if len(rows) > h {
		rows = rows[:h]
	}
	return strings.Join(rows, "\n")
}
