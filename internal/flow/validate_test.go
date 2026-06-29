package flow

import "testing"

func baseFlow() *Flow {
	return &Flow{
		Name: "f",
		Steps: []*Step{
			{ID: "a", Agent: "m", Prompt: "do the task", Gate: Gate{Policy: GateManual}},
			{ID: "b", Needs: []string{"a"}, Agent: "m", Prompt: "do the task",
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
		"cond bad expr": func(f *Flow) {
			f.Steps[0].Gate = Gate{Policy: GateConditional, Condition: &Condition{Expr: "not valid +++"}}
		},
		"select without agent": func(f *Flow) {
			f.Steps[0].Workspace = WSIsolated
			f.Steps[1].Workspace = WSIsolated
			f.Steps[1].Agent = ""
			f.Steps[1].Join = &Join{Strategy: JoinSelect}
		},
		"retry max zero": func(f *Flow) { f.Steps[0].Retry = &RetryPolicy{Max: 0} },
		"onfail retry without policy": func(f *Flow) {
			f.Steps[0].Gate.OnFail = FailRetry
		},
		"negative concurrency": func(f *Flow) { f.Concurrency = -1 },
		"bad join on_conflict": func(f *Flow) {
			f.Steps[0].Workspace = WSIsolated
			f.Steps[1].Workspace = WSIsolated
			f.Steps[1].Agent = ""
			f.Steps[1].Gate = Gate{Policy: GateManual}
			f.Steps[1].Join = &Join{Strategy: JoinMerge, OnConflict: "bogus"}
		},
		// Gap 1: duplicate needs entry
		"duplicate needs": func(f *Flow) {
			f.Steps[1].Needs = []string{"a", "a"}
		},
		// Gap 2: agent step with empty role and empty prompt
		"agent empty role and prompt": func(f *Flow) {
			f.Steps[0].Role = ""
			f.Steps[0].Prompt = ""
		},
		// Gap 3: negative retry backoff
		"negative retry backoff": func(f *Flow) {
			f.Steps[0].Retry = &RetryPolicy{Max: 1, Backoff: -1}
		},
		// Gap 4: unknown workspace value on a non-join step
		"unknown workspace": func(f *Flow) {
			f.Steps[0].Workspace = "bogus"
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

func TestValidateJoinRequiresIsolatedUpstreams(t *testing.T) {
	f := &Flow{Name: "f", Steps: []*Step{
		{ID: "a", Agent: "m", Prompt: "p", Workspace: WSShared, Gate: Gate{Policy: GateAuto, Verifier: &Verifier{Command: "true"}}},
		{ID: "j", Needs: []string{"a"}, Workspace: WSIsolated, Join: &Join{Strategy: JoinMerge}},
	}}
	if err := Validate(f); err == nil {
		t.Fatal("a join over a shared upstream must be rejected")
	}
}

func TestValidateJoinMustBeIsolated(t *testing.T) {
	f := &Flow{Name: "f", Steps: []*Step{
		{ID: "a", Agent: "m", Prompt: "p", Workspace: WSIsolated, Gate: Gate{Policy: GateAuto, Verifier: &Verifier{Command: "true"}}},
		{ID: "j", Needs: []string{"a"}, Workspace: WSShared, Join: &Join{Strategy: JoinMerge}},
	}}
	if err := Validate(f); err == nil {
		t.Fatal("a join step itself must be isolated")
	}
}

func TestValidateMergeEscalateRequiresAgent(t *testing.T) {
	f := &Flow{Name: "f", Steps: []*Step{
		{ID: "a", Agent: "m", Prompt: "p", Workspace: WSIsolated, Gate: Gate{Policy: GateAuto, Verifier: &Verifier{Command: "true"}}},
		{ID: "j", Needs: []string{"a"}, Workspace: WSIsolated, Join: &Join{Strategy: JoinMerge, OnConflict: FailEscalate}},
	}}
	if err := Validate(f); err == nil {
		t.Fatal("merge + on_conflict=escalate requires an arbiter agent")
	}
}

func TestValidateJoinRetryRequiresRetryPolicy(t *testing.T) {
	f := &Flow{Name: "f", Steps: []*Step{
		{ID: "a", Agent: "m", Prompt: "p", Workspace: WSIsolated, Gate: Gate{Policy: GateAuto, Verifier: &Verifier{Command: "true"}}},
		{ID: "j", Needs: []string{"a"}, Workspace: WSIsolated, Join: &Join{Strategy: JoinMerge, OnConflict: FailRetry}},
	}}
	if err := Validate(f); err == nil {
		t.Fatal("on_conflict=retry requires a retry policy")
	}
}

func TestValidateAcceptsIsolatedJoin(t *testing.T) {
	f := &Flow{Name: "f", Steps: []*Step{
		{ID: "a", Agent: "m", Prompt: "p", Workspace: WSIsolated, Gate: Gate{Policy: GateAuto, Verifier: &Verifier{Command: "true"}}},
		{ID: "b", Agent: "m", Prompt: "p", Workspace: WSIsolated, Gate: Gate{Policy: GateAuto, Verifier: &Verifier{Command: "true"}}},
		{ID: "j", Needs: []string{"a", "b"}, Workspace: WSIsolated,
			Join: &Join{Strategy: JoinMerge, Agent: "m", OnConflict: FailEscalate}, Gate: Gate{Policy: GateManual}},
	}}
	if err := Validate(f); err != nil {
		t.Fatalf("a well-formed isolated merge+escalate join must be accepted, got: %v", err)
	}
}

func TestValidateRejectsUnsafeStepID(t *testing.T) {
	for _, bad := range []string{"a/b", "has space", "..", ".", "-leading", "weird*char"} {
		f := &Flow{Name: "f", Steps: []*Step{
			{ID: bad, Agent: "mock", Prompt: "p", Gate: Gate{Policy: GateManual}},
		}}
		if err := Validate(f); err == nil {
			t.Errorf("step id %q should be rejected", bad)
		}
	}
}

func TestValidateAcceptsSlugStepIDs(t *testing.T) {
	for _, ok := range []string{"a", "plan", "impl-api", "w0", "step_1", "v1.2"} {
		f := &Flow{Name: "f", Steps: []*Step{
			{ID: ok, Agent: "mock", Prompt: "p", Gate: Gate{Policy: GateManual}},
		}}
		if err := Validate(f); err != nil {
			t.Errorf("step id %q should be accepted, got %v", ok, err)
		}
	}
}

func TestValidateDetectsCycle(t *testing.T) {
	f := &Flow{Name: "f", Steps: []*Step{
		{ID: "a", Needs: []string{"b"}, Agent: "m", Prompt: "p"},
		{ID: "b", Needs: []string{"a"}, Agent: "m", Prompt: "p"},
	}}
	if err := Validate(f); err == nil {
		t.Fatal("expected cycle error, got nil")
	}
}

func TestValidateCompilesGoodCondition(t *testing.T) {
	f := baseFlow()
	f.Steps[0].Gate = Gate{Policy: GateConditional, Condition: &Condition{Expr: "result.cost_usd < 1.0"}}
	if err := Validate(f); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ok, err := f.Steps[0].Gate.Condition.Eval(GateEnv{Result: GateResult{CostUSD: 0.1}})
	if err != nil || !ok {
		t.Errorf("post-validate eval = %v,%v want true,nil (Validate should have compiled the expr)", ok, err)
	}
}

// Positive cases: the four gap checks must not reject legitimately valid flows.
func TestValidateGapPositiveCases(t *testing.T) {
	t.Run("single valid needs", func(t *testing.T) {
		// step b needs a exactly once — must pass
		f := baseFlow()
		if err := Validate(f); err != nil {
			t.Fatalf("single valid needs should be accepted: %v", err)
		}
	})

	t.Run("agent with prompt", func(t *testing.T) {
		f := baseFlow()
		f.Steps[0].Prompt = "do something"
		if err := Validate(f); err != nil {
			t.Fatalf("agent step with prompt should be accepted: %v", err)
		}
	})

	t.Run("agent with role only", func(t *testing.T) {
		f := baseFlow()
		f.Steps[0].Role = "senior engineer"
		f.Steps[0].Prompt = "" // genuinely role-only: baseFlow() sets a prompt; clear it so this exercises role-without-prompt
		if err := Validate(f); err != nil {
			t.Fatalf("agent step with role should be accepted: %v", err)
		}
	})

	t.Run("workspace empty (default)", func(t *testing.T) {
		f := baseFlow()
		f.Steps[0].Workspace = ""
		if err := Validate(f); err != nil {
			t.Fatalf("empty workspace should be accepted: %v", err)
		}
	})

	t.Run("workspace shared", func(t *testing.T) {
		f := baseFlow()
		f.Steps[0].Workspace = WSShared
		if err := Validate(f); err != nil {
			t.Fatalf("workspace=shared should be accepted: %v", err)
		}
	})

	t.Run("workspace isolated non-join", func(t *testing.T) {
		f := baseFlow()
		f.Steps[0].Workspace = WSIsolated
		if err := Validate(f); err != nil {
			t.Fatalf("workspace=isolated on non-join should be accepted: %v", err)
		}
	})

	t.Run("retry backoff zero", func(t *testing.T) {
		f := baseFlow()
		f.Steps[0].Retry = &RetryPolicy{Max: 1, Backoff: 0}
		if err := Validate(f); err != nil {
			t.Fatalf("retry.backoff=0 should be accepted: %v", err)
		}
	})

	t.Run("retry backoff positive", func(t *testing.T) {
		f := baseFlow()
		f.Steps[0].Retry = &RetryPolicy{Max: 2, Backoff: 5}
		if err := Validate(f); err != nil {
			t.Fatalf("retry.backoff=5 should be accepted: %v", err)
		}
	})
}
