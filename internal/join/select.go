package join

import (
	"context"
	"fmt"
	"strings"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Select asks the arbiter agent to choose the single best candidate among the
// fan-in inputs, then forwards that candidate's artifacts (by reference).
type Select struct{}

func (Select) Join(ctx context.Context, s *flow.Step, inputs []core.Artifact, workDir string, run RunAgent) (core.Result, error) {
	staged, err := stageCandidates(inputs, workDir)
	if err != nil {
		return core.Result{}, err
	}
	res, err := run(ctx, s.Join.Agent, selectPrompt(s, staged), workDir, inputs)
	if err != nil {
		return core.Result{}, fmt.Errorf("select: arbiter failed: %w", err)
	}
	winner, ok := parseSelected(res.Summary)
	if !ok {
		return core.Result{}, fmt.Errorf("select: no SELECTED token in arbiter output")
	}
	if !isDependency(s, winner) {
		return core.Result{}, fmt.Errorf("select: chosen step %q is not a dependency", winner)
	}
	// Forward the winner's original artifacts by reference. A winner that produced
	// no artifacts forwards an empty set; the arbiter's rationale still rides on Summary.
	var artifacts []core.Artifact
	for _, in := range inputs {
		if in.StepID == winner {
			artifacts = append(artifacts, in)
		}
	}
	return core.Result{StepID: s.ID, Summary: res.Summary, Artifacts: artifacts, CostUSD: res.CostUSD}, nil
}

// parseSelected returns the step id from the last `SELECTED: <id>` line in text.
func parseSelected(text string) (string, bool) {
	winner, ok := "", false
	for _, line := range strings.Split(text, "\n") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(line), "SELECTED:"); found {
			if id := strings.TrimSpace(rest); id != "" {
				winner, ok = id, true
			}
		}
	}
	return winner, ok
}

func isDependency(s *flow.Step, id string) bool {
	for _, dep := range s.Needs {
		if dep == id {
			return true
		}
	}
	return false
}

// selectPrompt lists each candidate's staged files and asks for a SELECTED token.
func selectPrompt(s *flow.Step, staged map[string][]string) string {
	var b strings.Builder
	b.WriteString("You are choosing the single best candidate implementation.\n\n")
	for _, dep := range s.Needs {
		fmt.Fprintf(&b, "Candidate %s:\n", dep)
		for _, p := range staged[dep] {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	b.WriteString("\nRead each candidate's files, decide which is best, explain briefly, ")
	b.WriteString("and end your reply with a line:\nSELECTED: <step-id>\n")
	return b.String()
}
