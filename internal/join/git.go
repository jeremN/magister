package join

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// gitCmd runs git in workDir and returns combined output. Args are orchestrator-
// controlled (fixed subcommands, validated branch names); no shell is involved.
// Mirrors executor.discoverGit's direct-exec pattern.
func gitCmd(workDir string, args ...string) ([]byte, error) {
	// #nosec G204 -- fixed git subcommands in an operator-controlled worktree; no shell.
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(out))
	}
	return out, nil
}

// ConflictError reports that a git merge left unresolved conflicts. The engine
// distinguishes it (via errors.As) from an arbiter failure to drive the
// on_conflict=escalate resolve-then-approve ladder.
type ConflictError struct {
	Branch  string
	Paths   []string
	WorkDir string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("merge conflict in %s (merging %s)", strings.Join(e.Paths, ", "), e.Branch)
}

// nulFields splits NUL-terminated git output (the `-z` form) into its non-empty
// fields. `-z` emits raw bytes, so unlike the default output it never quotes or
// octal-escapes paths with spaces or non-ASCII characters.
func nulFields(out []byte) []string {
	var fields []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			fields = append(fields, f)
		}
	}
	return fields
}

// upstreamBranches returns the distinct, first-seen-ordered branch refs carried
// by the inputs. An input with no branch (a shared step / mock) is skipped.
func upstreamBranches(inputs []core.Artifact) []string {
	var brs []string
	seen := map[string]bool{}
	for _, in := range inputs {
		if in.Branch != "" && !seen[in.Branch] {
			seen[in.Branch] = true
			brs = append(brs, in.Branch)
		}
	}
	return brs
}

// conflictedPaths returns the worktree's unmerged paths (relative to workDir).
// A git error (an unexpected worktree state) is treated as "no conflicts": this
// helper is only consulted after a merge that already exited non-zero, so the
// caller's own error carries the real failure.
func conflictedPaths(workDir string) []string {
	out, err := gitCmd(workDir, "diff", "--name-only", "-z", "--diff-filter=U")
	if err != nil {
		return nil
	}
	return nulFields(out)
}

// CommittedResult builds the result of a committed join worktree: its branch
// (the worktree's current branch), HEAD sha, and every tracked file as an
// artifact. Used by merge's clean path, synthesize, and the engine's escalate-
// finalize so all three enumerate artifacts identically.
func CommittedResult(workDir string, s *flow.Step) (core.Result, error) {
	branchOut, err := gitCmd(workDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return core.Result{}, err
	}
	branch := strings.TrimSpace(string(branchOut))
	shaOut, err := gitCmd(workDir, "rev-parse", "HEAD")
	if err != nil {
		return core.Result{}, err
	}
	sha := strings.TrimSpace(string(shaOut))
	filesOut, err := gitCmd(workDir, "ls-files", "-z")
	if err != nil {
		return core.Result{}, err
	}
	var artifacts []core.Artifact
	for _, rel := range nulFields(filesOut) {
		artifacts = append(artifacts, core.Artifact{
			StepID: s.ID, Path: filepath.Join(workDir, rel), Branch: branch, Commit: sha,
		})
	}
	// core.Result has no ref fields; branch/commit ride on each artifact.
	return core.Result{StepID: s.ID, Artifacts: artifacts}, nil
}

// ResolveConflictPrompt asks the arbiter to resolve every conflict marker in the
// listed files (in its current working directory) and leave a clean tree. Shared
// by synthesize and the engine's merge-escalate ladder.
func ResolveConflictPrompt(paths []string) string {
	var b strings.Builder
	b.WriteString("A git merge left conflicts. Resolve every <<<<<<< / ======= / >>>>>>> marker ")
	b.WriteString("in these files, keeping the best of both sides, and leave a clean tree:\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "  - %s\n", p)
	}
	return b.String()
}
