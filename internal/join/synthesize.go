package join

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Synthesize asks the arbiter agent to read all candidates and write one
// reconciled result into the join workdir; that written output is the result.
type Synthesize struct{}

func (Synthesize) Join(ctx context.Context, s *flow.Step, inputs []core.Artifact, workDir string, run RunAgent) (core.Result, error) {
	staged, err := stageCandidates(inputs, workDir)
	if err != nil {
		return core.Result{}, err
	}
	res, err := run(ctx, s.Join.Agent, synthesizePrompt(s, staged), workDir, inputs)
	if err != nil {
		return core.Result{}, fmt.Errorf("synthesize: arbiter failed: %w", err)
	}
	// Keep only the arbiter's new writes; the .candidates/ dir is reserved for
	// staged inputs, so any artifact written there is intentionally dropped.
	var artifacts []core.Artifact
	for _, a := range res.Artifacts {
		if !underCandidates(a.Path, workDir) {
			artifacts = append(artifacts, core.Artifact{StepID: s.ID, Path: a.Path})
		}
	}
	if len(artifacts) == 0 {
		return core.Result{}, fmt.Errorf("synthesize: arbiter produced no output")
	}
	return core.Result{StepID: s.ID, Summary: res.Summary, Artifacts: artifacts, CostUSD: res.CostUSD}, nil
}

// underCandidates reports whether path is inside <workDir>/.candidates (a staged input).
func underCandidates(path, workDir string) bool {
	rel, err := filepath.Rel(filepath.Join(workDir, ".candidates"), path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// synthesizePrompt lists each candidate's staged files and asks for a merged result.
func synthesizePrompt(s *flow.Step, staged map[string][]string) string {
	var b strings.Builder
	b.WriteString("You are reconciling multiple candidate results into one.\n\n")
	for _, dep := range s.Needs {
		fmt.Fprintf(&b, "Candidate %s:\n", dep)
		for _, p := range staged[dep] {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	b.WriteString("\nRead all candidates and write a single reconciled result into the current directory.\n")
	return b.String()
}
