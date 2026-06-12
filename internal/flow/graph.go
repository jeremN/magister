package flow

// TerminalSteps returns the steps that no other step depends on (nothing lists
// them in its Needs) — the leaves of the DAG. Order follows the flow's step order.
// For a fan-in flow this is the single final join; a flow with independent leaves
// returns several.
func TerminalSteps(f *Flow) []*Step {
	needed := make(map[string]bool)
	for _, s := range f.Steps {
		for _, dep := range s.Needs {
			needed[dep] = true
		}
	}
	var terms []*Step
	for _, s := range f.Steps {
		if !needed[s.ID] {
			terms = append(terms, s)
		}
	}
	return terms
}
