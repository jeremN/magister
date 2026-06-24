package flow

import "fmt"

// Validate enforces the invariants the engine relies on. By the time a flow
// passes Validate, the engine can treat it as total.
func Validate(f *Flow) error {
	if f == nil {
		return fmt.Errorf("flow is nil")
	}
	if f.Name == "" {
		return fmt.Errorf("flow has no name")
	}
	if len(f.Steps) == 0 {
		return fmt.Errorf("flow has no steps")
	}
	if f.Concurrency < 0 {
		return fmt.Errorf("flow concurrency must be >= 0, got %d", f.Concurrency)
	}

	byID := make(map[string]*Step, len(f.Steps))
	for _, s := range f.Steps {
		if s.ID == "" {
			return fmt.Errorf("a step has no id")
		}
		if !validStepID(s.ID) {
			return fmt.Errorf("step id %q must match [A-Za-z0-9._-], not start with '-', and not be '.'/'..'", s.ID)
		}
		if _, dup := byID[s.ID]; dup {
			return fmt.Errorf("duplicate step id %q", s.ID)
		}
		byID[s.ID] = s
	}

	for _, s := range f.Steps {
		seen := make(map[string]bool, len(s.Needs))
		for _, dep := range s.Needs {
			if dep == s.ID {
				return fmt.Errorf("step %q needs itself", s.ID)
			}
			if _, ok := byID[dep]; !ok {
				return fmt.Errorf("step %q needs unknown step %q", s.ID, dep)
			}
			if seen[dep] {
				return fmt.Errorf("step %q: duplicate needs entry %q", s.ID, dep)
			}
			seen[dep] = true
		}
		if s.Join == nil && s.Agent == "" {
			return fmt.Errorf("step %q has neither an agent nor a join", s.ID)
		}
		if s.Join != nil && s.Agent != "" {
			return fmt.Errorf("step %q has both an agent and a join (pick one)", s.ID)
		}
		if s.Agent != "" && s.Join == nil && s.Role == "" && s.Prompt == "" {
			return fmt.Errorf("step %q: agent step needs a role or a prompt", s.ID)
		}
		switch s.Workspace {
		case "", WSShared, WSIsolated:
			// valid
		default:
			return fmt.Errorf("step %q: unknown workspace %q", s.ID, s.Workspace)
		}
		if err := validateGate(s); err != nil {
			return err
		}
		if err := validateJoin(s, byID); err != nil {
			return err
		}
		if s.Retry != nil && s.Retry.Max < 1 {
			return fmt.Errorf("step %q: retry.max must be >= 1, got %d", s.ID, s.Retry.Max)
		}
		if s.Retry != nil && s.Retry.Backoff < 0 {
			return fmt.Errorf("step %q: retry.backoff must be >= 0", s.ID)
		}
		if s.Timeout < 0 {
			return fmt.Errorf("step %q: timeout must be >= 0", s.ID)
		}
	}

	if bad := findCycle(f, byID); bad != "" {
		return fmt.Errorf("flow has a cycle involving step %q", bad)
	}
	return nil
}

func validateGate(s *Step) error {
	switch s.Gate.Policy {
	case "", GateManual:
		// default is manual
	case GateAuto:
		if s.Gate.Verifier == nil || s.Gate.Verifier.Command == "" {
			return fmt.Errorf("step %q: auto gate requires a verifier command", s.ID)
		}
	case GateConditional:
		if s.Gate.Condition == nil || s.Gate.Condition.Expr == "" {
			return fmt.Errorf("step %q: conditional gate requires a condition expr", s.ID)
		}
		if err := s.Gate.Condition.Compile(); err != nil {
			return fmt.Errorf("step %q: invalid condition expr: %w", s.ID, err)
		}
	default:
		return fmt.Errorf("step %q: unknown gate policy %q", s.ID, s.Gate.Policy)
	}

	switch s.Gate.OnFail {
	case "", FailAbort, FailRetry, FailEscalate:
		// ok
	default:
		return fmt.Errorf("step %q: unknown on_fail %q", s.ID, s.Gate.OnFail)
	}
	if s.Gate.OnFail == FailRetry && s.Retry == nil {
		return fmt.Errorf("step %q: on_fail=retry requires a retry policy", s.ID)
	}
	return nil
}

func validateJoin(s *Step, byID map[string]*Step) error {
	if s.Join == nil {
		return nil
	}
	if s.Workspace != WSIsolated {
		return fmt.Errorf("step %q: a join step must be workspace: isolated", s.ID)
	}
	for _, dep := range s.Needs {
		if up, ok := byID[dep]; ok && up.Workspace != WSIsolated {
			return fmt.Errorf("step %q: join upstream %q must be workspace: isolated", s.ID, dep)
		}
	}
	switch s.Join.Strategy {
	case JoinMerge:
		if s.Join.OnConflict == FailEscalate && s.Join.Agent == "" {
			return fmt.Errorf("step %q: merge with on_conflict=escalate requires an arbiter agent", s.ID)
		}
	case JoinSelect, JoinSynthesize:
		if s.Join.Agent == "" {
			return fmt.Errorf("step %q: %q join requires an arbiter agent", s.ID, s.Join.Strategy)
		}
	default:
		return fmt.Errorf("step %q: unknown join strategy %q", s.ID, s.Join.Strategy)
	}
	switch s.Join.OnConflict {
	case "", FailAbort, FailRetry, FailEscalate:
		// ok
	default:
		return fmt.Errorf("step %q: unknown join on_conflict %q", s.ID, s.Join.OnConflict)
	}
	if s.Join.OnConflict == FailRetry && s.Retry == nil {
		return fmt.Errorf("step %q: on_conflict=retry requires a retry policy", s.ID)
	}
	if len(s.Needs) == 0 {
		return fmt.Errorf("step %q: join step must depend on at least one step", s.ID)
	}
	return nil
}

// validStepID reports whether id is safe as a filesystem path segment and a git
// branch name: [A-Za-z0-9._-], not a leading '-', and not "." or "..".
func validStepID(id string) bool {
	if id == "." || id == ".." || id[0] == '-' {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// findCycle runs a white/gray/black DFS over the needs graph and returns a step
// that participates in a cycle, or "" if the graph is acyclic.
func findCycle(f *Flow, byID map[string]*Step) string {
	const (
		white = iota
		gray
		black
	)
	color := make(map[string]int, len(f.Steps))
	var bad string

	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = gray
		for _, dep := range byID[id].Needs {
			switch color[dep] {
			case gray:
				bad = dep
				return true
			case white:
				if visit(dep) {
					return true
				}
			}
		}
		color[id] = black
		return false
	}

	for _, s := range f.Steps {
		if color[s.ID] == white && visit(s.ID) {
			return bad
		}
	}
	return ""
}
