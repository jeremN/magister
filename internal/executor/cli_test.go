package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/event"
)

func stubPath(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stub %s missing: %v", name, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("stub %s is not executable — chmod +x it", name)
	}
	return abs
}

func TestCLIAgentRunSuccess(t *testing.T) {
	dir := initGitRepo(t) // from discover_test.go; skips if git absent
	a := &CLIAgent{Bin: stubPath(t, "fake-claude-ok"), Model: "opus", Spec: ClaudeSpec{}}
	res, err := a.Run(context.Background(), core.Task{StepID: "s1", Prompt: "go", WorkDir: dir})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Summary != "stub done" || res.CostUSD != 0.02 {
		t.Errorf("summary=%q cost=%v, want \"stub done\"/0.02", res.Summary, res.CostUSD)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "s1" ||
		filepath.Base(res.Artifacts[0].Path) != "agent-output.txt" {
		t.Errorf("artifacts = %+v, want one agent-output.txt attributed to s1", res.Artifacts)
	}
}

func TestCLIAgentRunNonZeroExit(t *testing.T) {
	a := &CLIAgent{Bin: stubPath(t, "fake-claude-fail"), Model: "opus", Spec: ClaudeSpec{}}
	_, err := a.Run(context.Background(), core.Task{StepID: "s1", Prompt: "go", WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should surface stderr, got: %v", err)
	}
}

func TestCLIAgentRunBinaryNotFound(t *testing.T) {
	a := &CLIAgent{Bin: "definitely-not-a-real-binary-xyz", Model: "opus", Spec: ClaudeSpec{}}
	_, err := a.Run(context.Background(), core.Task{StepID: "s1", Prompt: "go", WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected a not-found error, got: %v", err)
	}
}

func TestCLIAgentDiscoveryFailureIsNonFatal(t *testing.T) {
	// WorkDir is NOT a git repo, so discoverGit fails — but the step still succeeds
	// (the agent produced a result); artifacts are just empty.
	a := &CLIAgent{Bin: stubPath(t, "fake-claude-ok"), Model: "opus", Spec: ClaudeSpec{}}
	res, err := a.Run(context.Background(), core.Task{StepID: "s1", Prompt: "go", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("discovery failure must not fail the step, got: %v", err)
	}
	if res.Summary != "stub done" || len(res.Artifacts) != 0 {
		t.Errorf("want summary kept + no artifacts, got %+v", res)
	}
}

func TestCLIAgentRunStreamsMilestones(t *testing.T) {
	dir := initGitRepo(t) // from discover_test.go; skips if git absent
	var got []event.Event
	a := &CLIAgent{Bin: stubPath(t, "fake-claude-stream"), Model: "opus", Spec: ClaudeSpec{}}
	res, err := a.Run(context.Background(), core.Task{
		StepID: "s1", Prompt: "go", WorkDir: dir,
		Emit: func(e event.Event) { got = append(got, e) },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Summary != "stream done" || res.CostUSD != 0.03 {
		t.Errorf("summary=%q cost=%v, want \"stream done\"/0.03", res.Summary, res.CostUSD)
	}
	if len(got) != 1 || got[0].Kind != event.AgentTool || got[0].Summary != "Edit: out.txt" {
		t.Fatalf("milestones = %+v, want one agent.tool \"Edit: out.txt\"", got)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].StepID != "s1" ||
		filepath.Base(res.Artifacts[0].Path) != "out.txt" {
		t.Errorf("artifacts = %+v, want one out.txt attributed to s1", res.Artifacts)
	}
}

func TestCLIAgentRunDrainsStdoutOnParseError(t *testing.T) {
	// The stub emits a malformed line (Parse bails immediately) then floods >64KB to
	// stdout that nobody reads. Without the io.Copy drain before Wait, the child blocks
	// writing to a full OS pipe buffer and Run deadlocks. Run it in a goroutine with a
	// timeout so a regression fails cleanly instead of hanging the whole suite.
	a := &CLIAgent{Bin: stubPath(t, "fake-claude-flood"), Model: "opus", Spec: ClaudeSpec{}}
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), core.Task{StepID: "s1", Prompt: "go", WorkDir: t.TempDir()})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a non-nil error from the malformed stream")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run deadlocked — stdout was not drained before Wait")
	}
}
