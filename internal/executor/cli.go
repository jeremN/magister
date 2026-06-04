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
)

// CLISpec adapts one coding-agent CLI's invocation and output schema for CLIAgent.
// ClaudeSpec implements it now; CodexSpec/GeminiSpec arrive in a later slice. A
// non-nil Parse error means the agent ran but failed (e.g. is_error / non-success
// subtype) — distinct from a process/exec failure, which CLIAgent surfaces itself.
type CLISpec interface {
	Args(model, prompt string) []string
	Parse(stdout []byte) (summary string, costUSD float64, err error)
}

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// CLIAgent is a core.Executor that runs a coding-agent CLI in the step's WorkDir,
// passes the prompt, parses cost+summary via Spec, and discovers changed files.
type CLIAgent struct {
	Bin      string                                        // e.g. "claude"
	Model    string                                        // "opus" / "sonnet"
	Spec     CLISpec                                       // ClaudeSpec{}
	Env      []string                                      // nil ⇒ os.Environ() (carries ANTHROPIC_API_KEY)
	Discover func(workDir string) ([]core.Artifact, error) // nil ⇒ discoverGit
	Log      *slog.Logger                                  // nil ⇒ discard (non-fatal discovery errors)
}

var _ core.Executor = (*CLIAgent)(nil)

func (a *CLIAgent) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return discardLogger
}

func (a *CLIAgent) Run(ctx context.Context, t core.Task) (core.Result, error) {
	// #nosec G204 -- Bin + args are operator-controlled (daemon registry + flow YAML);
	// no shell. Running a coding-agent CLI is the intended capability.
	cmd := exec.CommandContext(ctx, a.Bin, a.Spec.Args(a.Model, t.Prompt)...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Dir = t.WorkDir
	if a.Env != nil {
		cmd.Env = a.Env
	} else {
		cmd.Env = os.Environ()
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return core.Result{}, fmt.Errorf("agent binary %q not found", a.Bin)
		}
		return core.Result{}, fmt.Errorf("%s: %w: %s", a.Bin, err, truncate(stderr.Bytes(), 500))
	}

	summary, cost, err := a.Spec.Parse(stdout.Bytes())
	if err != nil {
		return core.Result{}, err
	}

	discover := a.Discover
	if discover == nil {
		discover = discoverGit
	}
	arts, derr := discover(t.WorkDir)
	if derr != nil {
		a.logger().Warn("artifact discovery failed", "step", t.StepID, "err", derr)
		arts = nil
	}
	for i := range arts {
		arts[i].StepID = t.StepID
	}
	return core.Result{StepID: t.StepID, Summary: summary, Artifacts: arts, CostUSD: cost}, nil
}
