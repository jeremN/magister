package gate

import (
	"context"
	"errors"
	"os/exec"
)

// Verifier resolves an auto gate by running a check.
type Verifier interface {
	Verify(ctx context.Context, command, workDir string) (bool, error)
}

// CommandVerifier runs a shell command in the step's workspace; exit 0 = pass.
// Any command ("go test ./...", "tsc --noEmit", a reviewer CLI) uses this one
// path, so no per-type verifier registry is needed.
//
// Security: command comes from the operator-controlled flow YAML (flow.Verifier.Command),
// which is a trusted config boundary — not end-user input. Running arbitrary shell
// is the intended capability (operators supply verification scripts). If this
// package is ever extended to accept untrusted input, sanitize or re-evaluate.
type CommandVerifier struct{}

func (CommandVerifier) Verify(ctx context.Context, command, workDir string) (bool, error) {
	if command == "" {
		return true, nil
	}
	// #nosec G204 -- command is operator-supplied config (flow YAML), not user input.
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = workDir
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return false, nil // non-zero exit = check failed, not an infra error
		}
		return false, err
	}
	return true, nil
}
