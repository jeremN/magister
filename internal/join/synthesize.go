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
		if _, err := gitCmd(workDir, "merge", "--no-edit", br); err == nil {
			continue // clean merge auto-committed
		}
		conflicted := conflictedPaths(workDir)
		if len(conflicted) == 0 {
			return core.Result{}, fmt.Errorf("synthesize: merge %s failed without conflicts", br)
		}
		ares, aerr := run(ctx, s.Join.Agent, ResolveConflictPrompt(conflicted), workDir, inputs)
		if aerr != nil {
			_, _ = gitCmd(workDir, "merge", "--abort")
			return core.Result{}, fmt.Errorf("synthesize: arbiter failed: %w", aerr)
		}
		cost += ares.CostUSD
		// Stage the arbiter's writes, then scan the staged content for leftover
		// conflict markers. The unmerged index is checked via the *staged* tree
		// (conflictedPaths is index-state-based, so it would still report the path
		// until `git add`). Whitespace rules are disabled so only genuine conflict
		// markers fail the check — not trailing whitespace an arbiter may emit.
		if _, err := gitCmd(workDir, "add", "-A"); err != nil {
			_, _ = gitCmd(workDir, "merge", "--abort")
			return core.Result{}, fmt.Errorf("synthesize: stage resolution of %s: %w", br, err)
		}
		if _, err := gitCmd(workDir,
			"-c", "core.whitespace=-trailing-space,-blank-at-eol,-space-before-tab,-blank-at-eof",
			"diff", "--cached", "--check"); err != nil {
			_, _ = gitCmd(workDir, "merge", "--abort")
			return core.Result{}, fmt.Errorf("synthesize: arbiter left unresolved conflicts in %v", conflicted)
		}
		if _, err := gitCmd(workDir, "commit", "--no-edit"); err != nil {
			return core.Result{}, fmt.Errorf("synthesize: commit after resolving %s: %w", br, err)
		}
	}
	res, err := CommittedResult(workDir, s)
	if err != nil {
		return core.Result{}, err
	}
	res.Summary = fmt.Sprintf("synthesized %d branch(es)", len(branches))
	res.CostUSD = cost
	return res, nil
}
