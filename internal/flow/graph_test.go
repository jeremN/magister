package flow

import "testing"

func TestTerminalStepsFanIn(t *testing.T) {
	f := &Flow{Steps: []*Step{
		{ID: "a"},
		{ID: "b"},
		{ID: "c", Needs: []string{"a", "b"}},
	}}
	terms := TerminalSteps(f)
	if len(terms) != 1 || terms[0].ID != "c" {
		t.Fatalf("terminals = %v, want [c]", ids(terms))
	}
}

func TestTerminalStepsMultipleLeaves(t *testing.T) {
	f := &Flow{Steps: []*Step{
		{ID: "root"},
		{ID: "x", Needs: []string{"root"}},
		{ID: "y", Needs: []string{"root"}},
	}}
	terms := TerminalSteps(f)
	if len(terms) != 2 || terms[0].ID != "x" || terms[1].ID != "y" {
		t.Fatalf("terminals = %v, want [x y]", ids(terms))
	}
}

func TestTerminalStepsLinear(t *testing.T) {
	f := &Flow{Steps: []*Step{
		{ID: "a"},
		{ID: "b", Needs: []string{"a"}},
		{ID: "c", Needs: []string{"b"}},
	}}
	terms := TerminalSteps(f)
	if len(terms) != 1 || terms[0].ID != "c" {
		t.Fatalf("terminals = %v, want [c]", ids(terms))
	}
}

func TestTerminalStepsSingle(t *testing.T) {
	f := &Flow{Steps: []*Step{{ID: "only"}}}
	terms := TerminalSteps(f)
	if len(terms) != 1 || terms[0].ID != "only" {
		t.Fatalf("terminals = %v, want [only]", ids(terms))
	}
}

func TestTerminalStepsEmpty(t *testing.T) {
	if got := TerminalSteps(&Flow{}); len(got) != 0 {
		t.Fatalf("terminals = %v, want none", ids(got))
	}
}

func TestTerminalStepsDanglingNeed(t *testing.T) {
	// A Need referencing a non-existent ID must not break leaf detection: the
	// unknown ID matches no real step, so the only step stays terminal.
	f := &Flow{Steps: []*Step{{ID: "a", Needs: []string{"ghost"}}}}
	terms := TerminalSteps(f)
	if len(terms) != 1 || terms[0].ID != "a" {
		t.Fatalf("terminals = %v, want [a]", ids(terms))
	}
}

func ids(steps []*Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.ID
	}
	return out
}
