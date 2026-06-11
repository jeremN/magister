// Package join combines a fan-in step's upstream artifacts into one result.
package join

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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

// Default registers all three strategies: merge, select, and synthesize.
func Default() Registry {
	return Registry{
		flow.JoinMerge:      Merge{},
		flow.JoinSelect:     Select{},
		flow.JoinSynthesize: Synthesize{},
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

// Merge does a real git merge of the upstream branches into the join's worktree.
// A clean merge commits and returns the merged tree; a conflict is dispositioned
// by on_conflict — escalate surfaces a *ConflictError (the engine runs the
// resolve-then-approve ladder), anything else aborts the merge and fails.
type Merge struct{}

func (Merge) Join(_ context.Context, s *flow.Step, inputs []core.Artifact, workDir string, _ RunAgent) (core.Result, error) {
	branches := upstreamBranches(inputs)
	if len(branches) == 0 {
		return core.Result{}, fmt.Errorf("merge: no branch-backed inputs")
	}
	for _, br := range branches {
		if _, err := gitCmd(workDir, "merge", "--no-edit", br); err != nil {
			conflicted := conflictedPaths(workDir)
			if len(conflicted) == 0 {
				return core.Result{}, fmt.Errorf("merge %s: %w", br, err)
			}
			if s.Join.OnConflict == flow.FailEscalate {
				return core.Result{}, &ConflictError{Branch: br, Paths: conflicted, WorkDir: workDir}
			}
			_, _ = gitCmd(workDir, "merge", "--abort")
			return core.Result{}, fmt.Errorf("merge conflict in %v", conflicted)
		}
	}
	res, err := CommittedResult(workDir, s)
	if err != nil {
		return core.Result{}, err
	}
	res.Summary = fmt.Sprintf("merged %d branch(es)", len(branches))
	return res, nil
}
