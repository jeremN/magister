package engine

import (
	"fmt"
	"strings"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// promptFor builds the prompt handed to an executor. An explicit step.Prompt
// wins; otherwise a default is assembled from the role and upstream artifacts.
func promptFor(s *flow.Step, inputs []core.Artifact) string {
	if s.Prompt != "" {
		return s.Prompt
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Role: %s\nStep: %s\n", s.Role, s.ID)
	if len(inputs) > 0 {
		b.WriteString("Upstream artifacts:\n")
		for _, in := range inputs {
			fmt.Fprintf(&b, "- %s: %s\n", in.StepID, in.Path)
		}
	}
	return b.String()
}
