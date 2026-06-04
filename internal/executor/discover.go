package executor

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"concentus/internal/core"
)

// discoverGit lists files changed in workDir via `git status --porcelain`, as
// absolute-path artifacts (StepID is filled in by CLIAgent). It is CLIAgent's
// default discoverer; a non-nil error is treated as non-fatal by the caller.
func discoverGit(workDir string) ([]core.Artifact, error) {
	// #nosec G204 -- fixed git subcommand in an operator-controlled workdir; no shell.
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	var arts []core.Artifact
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 4 { // "XY <path>"
			continue
		}
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 { // rename: take the new path
			path = path[i+4:]
		}
		path = strings.Trim(path, `"`) // porcelain quotes paths with special chars
		arts = append(arts, core.Artifact{Path: filepath.Join(workDir, path)})
	}
	return arts, nil
}
