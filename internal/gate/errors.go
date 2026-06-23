package gate

import "fmt"

// VerifierFailure is the error a failed auto gate returns. Output carries the
// verifier's captured stdout/stderr tail so the engine can feed it to the next
// attempt's agent. Output is deliberately not in Error() — it can be large; the
// run's recorded Err and logs stay concise.
type VerifierFailure struct {
	Command string
	Output  string
}

func (f *VerifierFailure) Error() string {
	return fmt.Sprintf("gate failed (policy=%q, command=%q)", "auto", f.Command)
}
