package flow

import "testing"

func baseFlow() *Flow {
	return &Flow{
		Name: "f",
		Steps: []*Step{
			{ID: "a", Agent: "m", Gate: Gate{Policy: GateManual}},
			{ID: "b", Needs: []string{"a"}, Agent: "m",
				Gate: Gate{Policy: GateAuto, Verifier: &Verifier{Command: "true"}}},
		},
	}
}

func TestValidateAcceptsGoodFlow(t *testing.T) {
	if err := Validate(baseFlow()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejections(t *testing.T) {
	cases := map[string]func(*Flow){
		"no name":             func(f *Flow) { f.Name = "" },
		"no steps":            func(f *Flow) { f.Steps = nil },
		"dup id":              func(f *Flow) { f.Steps[1].ID = "a" },
		"unknown dep":         func(f *Flow) { f.Steps[1].Needs = []string{"ghost"} },
		"self dep":            func(f *Flow) { f.Steps[0].Needs = []string{"a"} },
		"no agent or join":    func(f *Flow) { f.Steps[0].Agent = "" },
		"agent and join":      func(f *Flow) { f.Steps[0].Join = &Join{Strategy: JoinMerge} },
		"auto without verify": func(f *Flow) { f.Steps[1].Gate.Verifier = nil },
		"bad gate policy":     func(f *Flow) { f.Steps[0].Gate.Policy = "weird" },
		"cond without expr":   func(f *Flow) { f.Steps[0].Gate.Policy = GateConditional },
		"select without agent": func(f *Flow) {
			f.Steps[1].Agent = ""
			f.Steps[1].Join = &Join{Strategy: JoinSelect}
		},
		"retry max zero": func(f *Flow) { f.Steps[0].Retry = &RetryPolicy{Max: 0} },
		"onfail retry without policy": func(f *Flow) {
			f.Steps[0].Gate.OnFail = FailRetry
		},
		"negative concurrency": func(f *Flow) { f.Concurrency = -1 },
		"bad join on_conflict": func(f *Flow) {
			f.Steps[1].Agent = ""
			f.Steps[1].Gate = Gate{Policy: GateManual}
			f.Steps[1].Join = &Join{Strategy: JoinMerge, OnConflict: "bogus"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := baseFlow()
			mutate(f)
			if err := Validate(f); err == nil {
				t.Fatalf("%s: expected error, got nil", name)
			}
		})
	}
}

func TestValidateRejectsUnsafeStepID(t *testing.T) {
	for _, bad := range []string{"a/b", "has space", "..", ".", "-leading", "weird*char"} {
		f := &Flow{Name: "f", Steps: []*Step{
			{ID: bad, Agent: "mock", Gate: Gate{Policy: GateManual}},
		}}
		if err := Validate(f); err == nil {
			t.Errorf("step id %q should be rejected", bad)
		}
	}
}

func TestValidateAcceptsSlugStepIDs(t *testing.T) {
	for _, ok := range []string{"a", "plan", "impl-api", "w0", "step_1", "v1.2"} {
		f := &Flow{Name: "f", Steps: []*Step{
			{ID: ok, Agent: "mock", Gate: Gate{Policy: GateManual}},
		}}
		if err := Validate(f); err != nil {
			t.Errorf("step id %q should be accepted, got %v", ok, err)
		}
	}
}

func TestValidateDetectsCycle(t *testing.T) {
	f := &Flow{Name: "f", Steps: []*Step{
		{ID: "a", Needs: []string{"b"}, Agent: "m"},
		{ID: "b", Needs: []string{"a"}, Agent: "m"},
	}}
	if err := Validate(f); err == nil {
		t.Fatal("expected cycle error, got nil")
	}
}
