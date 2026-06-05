package executor

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"concentus/internal/event"
)

var _ CLISpec = ClaudeSpec{}

// ClaudeSpec runs the `claude` CLI (Claude Code) headless and parses its single
// `--output-format json` result object.
type ClaudeSpec struct{}

func (ClaudeSpec) Args(model, prompt string) []string {
	return []string{
		"-p", prompt,
		"--model", model,
		"--output-format", "json",
		"--permission-mode", "acceptEdits",
	}
}

// streamLine is one NDJSON object from the `claude` CLI's output. Fields are the
// union of what we read across line types; unknown fields/types are ignored
// (forward-compatible). For type=="result", the flat result fields apply; the
// Message.Content blocks (type=="assistant") carry tool_use milestones (used from
// the next task on).
type streamLine struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
	Subtype      string   `json:"subtype"`
	IsError      bool     `json:"is_error"`
	Result       string   `json:"result"`
	TotalCostUSD float64  `json:"total_cost_usd"`
	Errors       []string `json:"errors"`
}

// Parse reads the claude CLI's stdout as a stream of NDJSON objects (json.Decoder
// imposes no line-size cap, so a multi-MB tool_result line cannot overflow it),
// returning the final result object's summary+cost. emit is reserved for milestone
// events (wired in from the next task); a missing result line is an error.
func (ClaudeSpec) Parse(stdout io.Reader, emit func(event.Event)) (string, float64, error) {
	dec := json.NewDecoder(stdout)
	var result *streamLine
	for dec.More() {
		var line streamLine
		if err := dec.Decode(&line); err != nil {
			return "", 0, fmt.Errorf("parse claude output: %w", err)
		}
		if line.Type == "result" {
			r := line
			result = &r
		}
	}
	if result == nil {
		return "", 0, fmt.Errorf("claude output ended with no result")
	}
	if result.IsError || (result.Subtype != "" && result.Subtype != "success") {
		msg := result.Subtype
		if len(result.Errors) > 0 {
			msg += ": " + strings.Join(result.Errors, "; ")
		}
		return "", 0, fmt.Errorf("claude agent failed (%s)", msg)
	}
	return result.Result, result.TotalCostUSD, nil
}

// truncate returns a trimmed, length-capped string for error diagnostics.
func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Claude returns a CLIAgent backed by the `claude` CLI for the given model alias
// (e.g. "opus", "sonnet"). Env defaults to os.Environ() (carries ANTHROPIC_API_KEY).
func Claude(model string) *CLIAgent {
	return &CLIAgent{Bin: "claude", Model: model, Spec: ClaudeSpec{}}
}
