package executor

// CLISpec adapts one coding-agent CLI's invocation and output schema for CLIAgent.
// ClaudeSpec implements it now; CodexSpec/GeminiSpec arrive in a later slice. A
// non-nil Parse error means the agent ran but failed (e.g. is_error / non-success
// subtype) — distinct from a process/exec failure, which CLIAgent surfaces itself.
type CLISpec interface {
	Args(model, prompt string) []string
	Parse(stdout []byte) (summary string, costUSD float64, err error)
}
