package executor

import (
	"fmt"
	"io"

	"concentus/internal/event"
)

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

// Parse is implemented in a later task (Task 2). Stub satisfies CLISpec so the
// constructor compiles; calling it before Task 2 lands returns an error.
func (CodexSpec) Parse(_ io.Reader, _ func(event.Event)) (string, float64, error) {
	return "", 0, fmt.Errorf("codex: Parse not yet implemented")
}

// Codex returns a CLIAgent backed by the `codex` CLI. An empty model uses codex's
// account-default. Env defaults to os.Environ() (carries codex's login / OPENAI_API_KEY).
func Codex(model string) *CLIAgent {
	return &CLIAgent{Bin: "codex", Model: model, Spec: CodexSpec{}}
}
