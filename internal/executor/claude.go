package executor

import (
	"encoding/json"
	"fmt"
	"strings"
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

// claudeResult mirrors the fields CLIAgent needs from claude's JSON result object.
// Unknown fields are ignored (forward-compatible).
type claudeResult struct {
	Subtype      string   `json:"subtype"`
	IsError      bool     `json:"is_error"`
	Result       string   `json:"result"`
	TotalCostUSD float64  `json:"total_cost_usd"`
	Errors       []string `json:"errors"`
}

func (ClaudeSpec) Parse(stdout []byte) (string, float64, error) {
	var r claudeResult
	if err := json.Unmarshal(stdout, &r); err != nil {
		return "", 0, fmt.Errorf("parse claude output: %w (got: %s)", err, truncate(stdout, 200))
	}
	if r.IsError || (r.Subtype != "" && r.Subtype != "success") {
		msg := r.Subtype
		if len(r.Errors) > 0 {
			msg += ": " + strings.Join(r.Errors, "; ")
		}
		return "", 0, fmt.Errorf("claude agent failed (%s)", msg)
	}
	return r.Result, r.TotalCostUSD, nil
}

// truncate returns a trimmed, length-capped string for error diagnostics.
func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
