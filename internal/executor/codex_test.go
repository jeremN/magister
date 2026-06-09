package executor

import (
	"slices"
	"testing"
)

func TestCodexSpecArgs(t *testing.T) {
	got := CodexSpec{}.Args("gpt-5-codex", "do the thing")
	want := []string{"exec", "--json", "-s", "workspace-write", "--skip-git-repo-check", "-m", "gpt-5-codex", "do the thing"}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %v, want %v", got, want)
	}
}

func TestCodexSpecArgsOmitsEmptyModel(t *testing.T) {
	got := CodexSpec{}.Args("", "do the thing")
	want := []string{"exec", "--json", "-s", "workspace-write", "--skip-git-repo-check", "do the thing"}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %v, want %v (no -m when model empty)", got, want)
	}
}
