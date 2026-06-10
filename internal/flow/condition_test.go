package flow

import "testing"

func TestConditionCompileRejectsBadExpr(t *testing.T) {
	c := &Condition{Expr: "this is not valid +++"}
	if err := c.Compile(); err == nil {
		t.Fatal("expected a compile error for a malformed expr")
	}
}

func TestConditionCompileRejectsNonBool(t *testing.T) {
	c := &Condition{Expr: "result.summary"} // a string, not a bool — AsBool must reject
	if err := c.Compile(); err == nil {
		t.Fatal("expected a compile error for a non-bool expr")
	}
}

func TestConditionCompileAcceptsGoodExpr(t *testing.T) {
	c := &Condition{Expr: `result.cost_usd < 1.0 && result.summary contains "OK"`}
	if err := c.Compile(); err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
}

func TestConditionEvalTrueFalse(t *testing.T) {
	c := &Condition{Expr: "result.cost_usd < 1.0"}
	if err := c.Compile(); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.Eval(GateEnv{Result: GateResult{CostUSD: 0.5}}); err != nil || !ok {
		t.Errorf("eval(0.5) = %v,%v want true,nil", ok, err)
	}
	if ok, err := c.Eval(GateEnv{Result: GateResult{CostUSD: 2.0}}); err != nil || ok {
		t.Errorf("eval(2.0) = %v,%v want false,nil", ok, err)
	}
}

func TestConditionEvalUncompiledErrors(t *testing.T) {
	c := &Condition{Expr: "true"}
	if _, err := c.Eval(GateEnv{}); err == nil {
		t.Fatal("expected an error evaluating an uncompiled condition")
	}
}

// TestConditionEvalRuntimeNonBoolErrors pins the runtime-error branch of Eval.
// A ternary whose branches mix static types (`false ? true : "x"`) passes
// expr.AsBool()'s static check — AsBool lets untyped/mixed exprs compile — yet
// at runtime resolves to a string. expr.Run surfaces that as an error rather
// than letting a non-bool reach the `out.(bool)` assertion, so Eval must return
// (false, err): a regression here (e.g. an expr-lang upgrade) would otherwise
// panic on the assertion instead of erroring cleanly.
func TestConditionEvalRuntimeNonBoolErrors(t *testing.T) {
	c := &Condition{Expr: `false ? true : "x"`}
	if err := c.Compile(); err != nil {
		t.Fatalf("expected expr to compile (AsBool passes untyped exprs): %v", err)
	}
	ok, err := c.Eval(GateEnv{})
	if err == nil {
		t.Fatal("expected a runtime error for an expr that yields a non-bool value")
	}
	if ok {
		t.Errorf("eval = %v, want false on runtime error", ok)
	}
}
