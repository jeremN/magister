package join

import (
	"context"
	"fmt"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// Synthesize merges every upstream branch into the join worktree, asking the
// arbiter to resolve only the true conflicts (non-conflicting changes merge
// automatically). The committed merged tree is the result.
type Synthesize struct{}

func (Synthesize) Join(ctx context.Context, s *flow.Step, inputs []core.Artifact, workDir string, run RunAgent) (core.Result, error) {
	branches := upstreamBranches(inputs)
	if len(branches) == 0 {
		return core.Result{}, fmt.Errorf("synthesize: no branch-backed inputs")
	}
	var cost float64
	for _, br := range branches {
		if _, err := gitCmd(ctx, workDir, "merge", "--no-edit", br); err == nil {
			continue // clean merge auto-committed
		}
		conflicted := conflictedPaths(ctx, workDir)
		if len(conflicted) == 0 {
			return core.Result{}, fmt.Errorf("synthesize: merge %s failed without conflicts", br)
		}
		ares, aerr := run(ctx, s.Join.Agent, ResolveConflictPrompt(conflicted), workDir, inputs)
		if aerr != nil {
			_, _ = gitCmd(ctx, workDir, "merge", "--abort")
			return core.Result{}, fmt.Errorf("synthesize: arbiter failed: %w", aerr)
		}
		cost += ares.CostUSD
		// Stage the arbiter's writes and confirm no conflict markers remain before
		// concluding the merge. (conflictedPaths is index-state-based, so it can't
		// recheck a working-tree resolution until `git add`; EnsureResolved stages
		// then scans the staged tree.)
		if err := EnsureResolved(ctx, workDir); err != nil {
			_, _ = gitCmd(ctx, workDir, "merge", "--abort")
			return core.Result{}, fmt.Errorf("synthesize: arbiter left conflicts in %v: %w", conflicted, err)
		}
		if _, err := gitCmd(ctx, workDir, "commit", "--no-edit"); err != nil {
			_, _ = gitCmd(ctx, workDir, "merge", "--abort")
			return core.Result{}, fmt.Errorf("synthesize: commit after resolving %s: %w", br, err)
		}
	}
	res, err := CommittedResult(ctx, workDir, s)
	if err != nil {
		return core.Result{}, err
	}
	res.Summary = fmt.Sprintf("synthesized %d branch(es)", len(branches))
	res.CostUSD = cost
	return res, nil
}
