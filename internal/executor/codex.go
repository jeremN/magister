package executor

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"concentus/internal/event"
)

var _ CLISpec = CodexSpec{}

// CodexSpec runs the OpenAI `codex` CLI headless (`codex exec --json`) and parses its
// JSONL event stream. Codex wraps tool calls in item.started/item.completed lifecycle
// events (a third dialect, distinct from claude's content blocks and gemini's flat
// lines): command_execution and file_change items become agent.tool milestones; the
// one-or-more agent_message items are concatenated into the summary. Codex reports
// token usage but no USD, so cost is always 0.
type CodexSpec struct{}

// Args builds `codex exec --json -s workspace-write --skip-git-repo-check [-m <model>]
// <prompt>`. The sandbox is workspace-write (codex exec is non-interactive and does not
// prompt under it); -m is omitted when model is empty so codex resolves its
// account-default model (an explicit model is validated by codex against the account's
// allowed set). The prompt is a positional arg, not a flag.
func (CodexSpec) Args(model, prompt string) []string {
	args := []string{"exec", "--json", "-s", "workspace-write", "--skip-git-repo-check"}
	if model != "" {
		args = append(args, "-m", model)
	}
	return append(args, prompt)
}

// codexEvent is one top-level JSONL object from `codex exec --json`. Fields are the
// union read across event types; unknown fields/types are ignored (forward-compatible).
// The item.* lifecycle nests a codexItem; turn.failed nests an error; a top-level error
// event carries message directly.
type codexEvent struct {
	Type    string      `json:"type"`
	Item    *codexItem  `json:"item"`
	Message string      `json:"message"` // top-level "error" event
	Error   *codexError `json:"error"`   // "turn.failed" event
}

type codexItem struct {
	Type    string        `json:"type"`
	Text    string        `json:"text"`    // agent_message
	Command string        `json:"command"` // command_execution
	Changes []codexChange `json:"changes"` // file_change
}

type codexChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type codexError struct {
	Message string `json:"message"`
}

// Parse reads codex's stdout as a stream of JSONL events (json.Decoder imposes no
// line-size cap). It emits one agent.tool milestone per command_execution/file_change
// item.started (the earliest live signal; item.completed for tool items does not
// re-emit), concatenates the agent_message texts (newline-joined) into the summary, and
// fails on a turn.failed/error event or a stream that never reaches turn.completed. cost
// is always 0 — codex reports token usage but no USD. emit is never nil.
func (CodexSpec) Parse(stdout io.Reader, emit func(event.Event)) (string, float64, error) {
	dec := json.NewDecoder(stdout)
	var summary strings.Builder
	sawCompleted := false
	failed := false
	failMsg := ""
	for dec.More() {
		var ev codexEvent
		if err := dec.Decode(&ev); err != nil {
			return "", 0, fmt.Errorf("parse codex output: %w", err)
		}
		switch ev.Type {
		case "item.started":
			if ev.Item != nil && (ev.Item.Type == "command_execution" || ev.Item.Type == "file_change") {
				emit(event.Event{Kind: event.AgentTool, Summary: renderCodexItem(ev.Item)})
			}
		case "item.completed":
			if ev.Item != nil && ev.Item.Type == "agent_message" {
				if summary.Len() > 0 {
					summary.WriteByte('\n')
				}
				summary.WriteString(ev.Item.Text)
			}
		case "turn.completed":
			sawCompleted = true
			failed = false // a completed turn supersedes any earlier failure (last terminal event wins)
			failMsg = ""
		case "turn.failed":
			failed = true
			if ev.Error != nil && ev.Error.Message != "" {
				failMsg = ev.Error.Message
			}
		case "error":
			failed = true
			if ev.Message != "" {
				failMsg = ev.Message
			}
		}
	}
	if failed {
		if failMsg == "" {
			failMsg = "codex reported a failure"
		}
		return "", 0, fmt.Errorf("codex agent failed: %s", failMsg)
	}
	if !sawCompleted {
		return "", 0, fmt.Errorf("codex output ended with no turn.completed")
	}
	return summary.String(), 0, nil
}

// renderCodexItem produces a short human label for a tool item:
// "command_execution: <command truncated to 80>" or "file_change: <kind> <basename>".
// Unknown/empty item types render as the bare type (forward-compatible fallback).
func renderCodexItem(item *codexItem) string {
	switch item.Type {
	case "command_execution":
		return "command_execution: " + truncate([]byte(item.Command), 80)
	case "file_change":
		if len(item.Changes) > 0 {
			return "file_change: " + item.Changes[0].Kind + " " + filepath.Base(item.Changes[0].Path)
		}
		return "file_change"
	default:
		return item.Type
	}
}

// Codex returns a CLIAgent backed by the `codex` CLI. An empty model uses codex's
// account-default. Env defaults to os.Environ() (carries codex's login / OPENAI_API_KEY).
func Codex(model string) *CLIAgent {
	return &CLIAgent{Bin: "codex", Model: model, Spec: CodexSpec{}}
}
