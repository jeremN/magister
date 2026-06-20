// Package executor holds implementations of core.Executor.
package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"concentus/internal/core"
)

// Mock simulates an executor so flows run end-to-end with no real CLIs or API
// keys. It writes a small artifact and reports a fixed cost.
type Mock struct {
	Name  string
	Delay time.Duration
}

func (m Mock) Run(ctx context.Context, t core.Task) (core.Result, error) {
	if err := ctx.Err(); err != nil {
		return core.Result{}, err
	}
	if m.Delay > 0 {
		select {
		case <-time.After(m.Delay):
		case <-ctx.Done():
			return core.Result{}, ctx.Err()
		}
	}

	body := fmt.Sprintf("# %s\nexecutor: %s\nrole: %s\ninputs: %d\n",
		t.StepID, m.Name, t.Role, len(t.Inputs))
	outPath := filepath.Join(t.WorkDir, t.StepID+".out.md")
	if err := os.WriteFile(outPath, []byte(body), 0o644); err != nil {
		return core.Result{}, err
	}
	return core.Result{
		StepID:    t.StepID,
		Summary:   fmt.Sprintf("%s done by %s", t.StepID, m.Name),
		Artifacts: []core.Artifact{{StepID: t.StepID, Path: outPath}},
		CostUSD:   0.01,
	}, nil
}
