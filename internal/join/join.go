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

// RunAgent runs the named agent with prompt in workDir and returns its result.
// The engine binds it to the same executor path a normal step uses (Task + the
// persist-then-publish Emit closure), so an arbiter streams agent.tool milestones
// and its artifacts are discovered exactly like a normal step's. inputs are the
// fan-in artifacts (passed through to the agent's Task).
type RunAgent func(ctx context.Context, agentName, prompt, workDir string, inputs []core.Artifact) (core.Result, error)

// Strategy combines a fan-in step's inputs. Strategies that need an arbiter agent
// (select/synthesize) invoke it via run; merge ignores run.
type Strategy interface {
	Join(ctx context.Context, s *flow.Step, inputs []core.Artifact, workDir string, run RunAgent) (core.Result, error)
}

// Registry maps a strategy name to its implementation.
type Registry map[flow.JoinStrategy]Strategy

// Default registers merge and select. synthesize (which also needs an arbiter
// agent) arrives next; until then an unregistered strategy fails at runtime
// with a clear "not implemented yet" message from the engine.
func Default() Registry {
	return Registry{
		flow.JoinMerge:  Merge{},
		flow.JoinSelect: Select{},
	}
}

// stageCandidates copies each input artifact into <workDir>/.candidates/<stepID>/<base>
// so the arbiter can read every candidate from within its own workspace, and returns
// the staged relative paths grouped by source step (for the prompt). select uses these
// read-only; synthesize excludes the .candidates dir from its result.
func stageCandidates(inputs []core.Artifact, workDir string) (map[string][]string, error) {
	staged := make(map[string][]string)
	used := make(map[string]bool) // dest paths written this call, to avoid basename collisions
	for _, in := range inputs {
		destDir := filepath.Join(workDir, ".candidates", in.StepID)
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(in.Path)
		if err != nil {
			return nil, fmt.Errorf("stage candidate %s: %w", in.Path, err)
		}
		base := filepath.Base(in.Path)
		dest := filepath.Join(destDir, base)
		for i := 1; used[dest]; i++ {
			dest = filepath.Join(destDir, fmt.Sprintf("%d-%s", i, base))
		}
		used[dest] = true
		// staging copies are read-only context for the arbiter; source file mode is not preserved.
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(workDir, dest)
		if err != nil {
			return nil, err
		}
		staged[in.StepID] = append(staged[in.StepID], rel)
	}
	return staged, nil
}

// Merge writes a manifest listing every upstream artifact. With real worktrees
// (M4) this becomes a git merge; the manifest keeps the pipeline observable now.
type Merge struct{}

func (Merge) Join(_ context.Context, s *flow.Step, inputs []core.Artifact, workDir string, _ RunAgent) (core.Result, error) {
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
