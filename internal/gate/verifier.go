package gate

import (
	"context"
	"errors"
	"os/exec"
)

// Verifier resolves an auto gate by running a check.
type Verifier interface {
	Verify(ctx context.Context, command, workDir string) (ok bool, output string, err error)
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

const maxFeedbackBytes = 8 << 10 // 8 KiB cap on verifier output fed back to the agent

func (CommandVerifier) Verify(ctx context.Context, command, workDir string) (bool, string, error) {
	if command == "" {
		return true, "", nil
	}
	// #nosec G204 -- command is operator-supplied config (flow YAML), not user input.
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			// Killed by the step timeout / cancellation — an infra error, not a
			// gate verdict. The engine treats it as a retryable failure.
			return false, "", ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return false, tailBytes(out, maxFeedbackBytes), nil // non-zero exit = check failed
		}
		return false, "", err
	}
	return true, "", nil
}

// tailBytes returns the last n bytes of b as a string. Verifier/test output
// prints its summary at the end, so the tail is the useful part; when b is
// longer than n a single truncation marker is prefixed.
func tailBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "…(truncated)\n" + string(b[len(b)-n:])
}
