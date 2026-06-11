package join

import (
	"errors"
	"path/filepath"
	"testing"

	"concentus/internal/flow"
)

func TestCommittedResultEnumeratesTree(t *testing.T) {
	joinDir, _ := setupJoinRepo(t, false)
	// Put two tracked files in the join worktree and commit them.
	writeFile(t, joinDir, "x.txt", "x")
	writeFile(t, joinDir, "y.txt", "y")
	gitX(t, joinDir, "add", "-A")
	gitX(t, joinDir, "commit", "-m", "work")

	res, err := CommittedResult(joinDir, &flow.Step{ID: "integrate"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 2 {
		t.Fatalf("want 2 artifacts (x.txt, y.txt), got %+v", res.Artifacts)
	}
	if res.Artifacts[0].Branch != "step/integrate" || res.Artifacts[0].Commit == "" {
		t.Errorf("artifact refs = %+v, want branch step/integrate + a sha", res.Artifacts[0])
	}
	for _, a := range res.Artifacts {
		if a.StepID != "integrate" || a.Branch != "step/integrate" {
			t.Errorf("artifact not tagged with the join: %+v", a)
		}
		if filepath.Dir(a.Path) != joinDir {
			t.Errorf("artifact path %q not under join dir %q", a.Path, joinDir)
		}
	}
}

func TestCommittedResultEmptyTree(t *testing.T) {
	joinDir, _ := setupJoinRepo(t, false) // join worktree off empty base, no tracked files
	res, err := CommittedResult(joinDir, &flow.Step{ID: "integrate"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 0 {
		t.Errorf("empty worktree should yield 0 artifacts, got %+v", res.Artifacts)
	}
}

func TestCommittedResultHandlesSpecialPaths(t *testing.T) {
	joinDir, _ := setupJoinRepo(t, false)
	// A filename with a space and a non-ASCII char would be quoted/escaped by
	// git's default output; -z must round-trip it as the real path.
	name := "ré sumé.txt"
	writeFile(t, joinDir, name, "x")
	gitX(t, joinDir, "add", "-A")
	gitX(t, joinDir, "commit", "-m", "work")

	res, err := CommittedResult(joinDir, &flow.Step{ID: "integrate"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 1 || filepath.Base(res.Artifacts[0].Path) != name {
		t.Fatalf("special-char path not round-tripped: %+v", res.Artifacts)
	}
}

func TestConflictErrorIs(t *testing.T) {
	err := error(&ConflictError{Branch: "step/b", Paths: []string{"shared.txt"}, WorkDir: "/w"})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Paths[0] != "shared.txt" {
		t.Fatalf("ConflictError should unwrap via errors.As, got %v", err)
	}
}
