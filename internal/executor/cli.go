package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/logctx"
)

// CLISpec adapts one coding-agent CLI's invocation and output schema for CLIAgent.
// ClaudeSpec, GeminiSpec, and CodexSpec implement it. Parse
// consumes the CLI's stdout stream, emitting milestone events via emit as they
// arrive, and returns the final summary+cost. A non-nil Parse error means the agent
// ran but failed (e.g. is_error / non-success subtype / no result) — distinct from a
// process/exec failure, which CLIAgent surfaces itself. emit is never nil.
type CLISpec interface {
	Args(model, prompt string) []string
	Parse(stdout io.Reader, emit func(event.Event)) (summary string, costUSD float64, err error)
}

// CLIAgent is a core.Executor that runs a coding-agent CLI in the step's WorkDir,
// passes the prompt, parses cost+summary via Spec, and discovers changed files.
type CLIAgent struct {
	Bin      string                                                             // e.g. "claude"
	Model    string                                                             // "opus" / "sonnet"
	Spec     CLISpec                                                            // ClaudeSpec{}
	Env      []string                                                           // nil ⇒ os.Environ() (carries ANTHROPIC_API_KEY)
	Discover func(ctx context.Context, workDir string) ([]core.Artifact, error) // nil ⇒ discoverGit
	Log      *slog.Logger                                                       // nil ⇒ context logger (non-fatal discovery errors)
}

var _ core.Executor = (*CLIAgent)(nil)

func (a *CLIAgent) logger(ctx context.Context) *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return logctx.From(ctx)
}

func (a *CLIAgent) Run(ctx context.Context, t core.Task) (core.Result, error) {
	// #nosec G204 -- Bin + args are operator-controlled (daemon registry + flow YAML);
	// no shell. Running a coding-agent CLI is the intended capability.
	cmd := exec.CommandContext(ctx, a.Bin, a.Spec.Args(a.Model, promptWithFeedback(t.Prompt, t.Feedback))...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Dir = t.WorkDir
	if a.Env != nil {
		cmd.Env = a.Env
	} else {
		cmd.Env = os.Environ()
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return core.Result{}, fmt.Errorf("%s: stdout pipe: %w", a.Bin, err)
	}

	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return core.Result{}, fmt.Errorf("agent binary %q not found", a.Bin)
		}
		return core.Result{}, fmt.Errorf("%s: start: %w", a.Bin, err)
	}

	emit := t.Emit
	if emit == nil {
		emit = func(event.Event) {}
	}
	summary, cost, perr := a.Spec.Parse(stdout, emit)
	_, _ = io.Copy(io.Discard, stdout) // drain any unread tail so Wait can't block

	if werr := cmd.Wait(); werr != nil {
		// A non-zero exit (or kill via ctx) is a hard failure even if a result line
		// was parsed mid-stream — it takes precedence, surfacing trimmed stderr.
		return core.Result{}, fmt.Errorf("%s: %w: %s", a.Bin, werr, truncate(stderr.Bytes(), 500))
	}
	if perr != nil {
		return core.Result{}, perr
	}

	discover := a.Discover
	if discover == nil {
		discover = discoverGit
	}
	arts, derr := discover(ctx, t.WorkDir)
	if derr != nil {
		a.logger(ctx).Warn("artifact discovery failed", "step", t.StepID, "err", derr)
		arts = nil
	}
	for i := range arts {
		arts[i].StepID = t.StepID
	}
	return core.Result{StepID: t.StepID, Summary: summary, Artifacts: arts, CostUSD: cost}, nil
}

// promptWithFeedback appends the previous attempt's verifier output to the prompt
// on a retry, so the agent can fix the specific failure. Empty feedback (the
// first attempt) returns the prompt unchanged.
func promptWithFeedback(prompt, feedback string) string {
	if feedback == "" {
		return prompt
	}
	return prompt +
		"\n\n## Previous attempt failed verification\n" +
		"The verifier for this step failed. Fix the problems shown below, then redo the work.\n\n" +
		"```\n" + feedback + "\n```\n"
}
