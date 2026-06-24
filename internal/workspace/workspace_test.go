package workspace

import (
	"context"
	"os"
	"testing"

	"concentus/internal/core"
	"concentus/internal/flow"
)

// compile-time assertion that Manager satisfies the (extended) Workspace port.
var _ core.Workspace = (*Manager)(nil)

func TestManagerTeardownRunIsNoop(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	if err := m.TeardownRun(context.Background(), "run1"); err != nil {
		t.Fatalf("plain Manager TeardownRun should be a no-op, got %v", err)
	}
}

func TestSharedReusesRunRoot(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	d1, _, err := m.For("run1", &flow.Step{ID: "a", Workspace: flow.WSShared})
	if err != nil {
		t.Fatal(err)
	}
	d2, _, err := m.For("run1", &flow.Step{ID: "b", Workspace: flow.WSShared})
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Errorf("shared steps should share a dir: %q vs %q", d1, d2)
	}
	if _, err := os.Stat(d1); err != nil {
		t.Errorf("dir not created: %v", err)
	}
}

func TestIsolatedGetsOwnDir(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	shared, _, _ := m.For("run1", &flow.Step{ID: "a", Workspace: flow.WSShared})
	iso, cleanup, err := m.For("run1", &flow.Step{ID: "b", Workspace: flow.WSIsolated})
	if err != nil {
		t.Fatal(err)
	}
	if iso == shared {
		t.Errorf("isolated step should get its own dir")
	}
	if err := cleanup(); err != nil {
		t.Errorf("cleanup: %v", err)
	}
}
