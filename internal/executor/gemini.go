package executor

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"concentus/internal/event"
)

var _ CLISpec = GeminiSpec{}

// GeminiSpec runs the `gemini` CLI (Google Gemini CLI) headless in `-o stream-json`
// mode and parses its NDJSON stream: top-level tool_use lines become agent.tool
// milestones; role:"assistant" message deltas are concatenated into the summary; the
// final result line carries status. Gemini reports token stats but no USD, so cost is
// always 0.
type GeminiSpec struct{}

func (GeminiSpec) Args(model, prompt string) []string {
	return []string{
		"-p", prompt,
		"-m", model,
		"-o", "stream-json",
		"--approval-mode", "yolo", // auto-approve all tools; headless can't prompt
		"--skip-trust",            // else a workspace-trust prompt hangs the headless run
	}
}

// geminiLine is one NDJSON object from the `gemini` CLI. Fields are the union of what
// we read across line types; unknown fields/types are ignored (forward-compatible).
// tool_use lines are top-level (unlike claude's nested content blocks); role-tagged
// message lines carry assistant text as incremental deltas; the result line carries
// status (no summary text, no USD).
type geminiLine struct {
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolName   string          `json:"tool_name"`
	Parameters json.RawMessage `json:"parameters"`
	Status     string          `json:"status"`
}

// Parse reads the gemini CLI's stdout as a stream of NDJSON objects (json.Decoder
// imposes no line-size cap). It emits one agent.tool milestone per tool_use line
// (except gemini's internal update_topic tracker), concatenates the assistant message
// deltas into the summary, and fails on a missing or non-success result line. cost is
// always 0 — gemini reports token stats but no USD. emit is never nil.
func (GeminiSpec) Parse(stdout io.Reader, emit func(event.Event)) (string, float64, error) {
	dec := json.NewDecoder(stdout)
	var summary strings.Builder
	sawResult := false
	status := ""
	for dec.More() {
		var line geminiLine
		if err := dec.Decode(&line); err != nil {
			return "", 0, fmt.Errorf("parse gemini output: %w", err)
		}
		switch line.Type {
		case "tool_use":
			if line.ToolName == "update_topic" {
				continue // gemini's internal intent/UI tracker, not a real action
			}
			emit(event.Event{Kind: event.AgentTool, Summary: renderGeminiTool(line.ToolName, line.Parameters)})
		case "message":
			if line.Role == "assistant" {
				summary.WriteString(line.Content) // deltas are incremental chunks
			}
		case "result":
			sawResult = true
			status = line.Status
		}
	}
	if !sawResult {
		return "", 0, fmt.Errorf("gemini output ended with no result")
	}
	if status != "success" {
		return "", 0, fmt.Errorf("gemini agent failed (status: %s)", status)
	}
	return summary.String(), 0, nil
}

// renderGeminiTool produces a short human label for a gemini tool_use line:
// "<tool_name>: <salient parameter>" (file path / command / pattern), or just the
// tool name when no known parameter is present. Best-effort: unknown shapes render as
// the name alone.
func renderGeminiTool(toolName string, params json.RawMessage) string {
	var p struct {
		FilePath string `json:"file_path"`
		Command  string `json:"command"`
		Pattern  string `json:"pattern"`
	}
	_ = json.Unmarshal(params, &p)
	switch {
	case p.FilePath != "":
		return toolName + ": " + p.FilePath
	case p.Command != "":
		return toolName + ": " + truncate([]byte(p.Command), 80)
	case p.Pattern != "":
		return toolName + ": " + p.Pattern
	default:
		return toolName
	}
}

// Gemini returns a CLIAgent backed by the `gemini` CLI for the given model
// (e.g. "gemini-2.5-pro"). Env defaults to os.Environ() (carries gemini's auth).
func Gemini(model string) *CLIAgent {
	return &CLIAgent{Bin: "gemini", Model: model, Spec: GeminiSpec{}}
}
