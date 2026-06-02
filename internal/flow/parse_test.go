package flow

import "testing"

func TestParseExampleFlow(t *testing.T) {
	f, err := Parse("../../flows/feature-flow.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Name != "feature-flow" {
		t.Errorf("name = %q, want feature-flow", f.Name)
	}
	if f.Concurrency != 4 {
		t.Errorf("concurrency = %d, want 4", f.Concurrency)
	}
	if len(f.Steps) != 4 {
		t.Fatalf("steps = %d, want 4", len(f.Steps))
	}
	if f.Steps[3].Join == nil || f.Steps[3].Join.Strategy != JoinMerge {
		t.Errorf("step 3 should be a merge join")
	}
	if err := Validate(f); err != nil {
		t.Fatalf("example flow should validate: %v", err)
	}
}

func TestParseBytesRejectsUnknownKey(t *testing.T) {
	_, err := ParseBytes([]byte("name: x\nbogus: 1\nsteps: [{id: a, agent: m}]\n"))
	if err == nil {
		t.Fatal("expected strict-decode error for unknown key, got nil")
	}
}
