// Package join combines a fan-in step's upstream artifacts into one result.
package join

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Strategy combines a fan-in step's inputs.
type Strategy interface {
	Join(ctx context.Context, s *flow.Step, inputs []core.Artifact, workDir string) (core.Result, error)
}

// Registry maps a strategy name to its implementation.
type Registry map[flow.JoinStrategy]Strategy

// Default registers only merge. select/synthesize (which need an arbiter agent)
// arrive in M5; until then an unregistered strategy fails at runtime with a
// clear "not implemented yet" message from the engine.
func Default() Registry {
	return Registry{flow.JoinMerge: Merge{}}
}

// Merge writes a manifest listing every upstream artifact. With real worktrees
// (M4) this becomes a git merge; the manifest keeps the pipeline observable now.
type Merge struct{}

func (Merge) Join(_ context.Context, s *flow.Step, inputs []core.Artifact, workDir string) (core.Result, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# merge: %s\n", s.ID)
	for _, in := range inputs {
		fmt.Fprintf(&b, "- %s -> %s\n", in.StepID, in.Path)
	}
	manifest := filepath.Join(workDir, s.ID+".merge.md")
	if err := os.WriteFile(manifest, []byte(b.String()), 0o644); err != nil {
		return core.Result{}, err
	}
	return core.Result{
		StepID:    s.ID,
		Summary:   fmt.Sprintf("merged %d branch(es)", len(inputs)),
		Artifacts: []core.Artifact{{StepID: s.ID, Path: manifest}},
	}, nil
}
