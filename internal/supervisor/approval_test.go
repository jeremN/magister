package supervisor

import "testing"

func TestApprovalRegistryResolveDeliversDecision(t *testing.T) {
	r := NewApprovalRegistry()
	ch := r.Await("run1", "stepA")

	if !r.Resolve("run1", "stepA", Decision{Approved: true, Reason: "ok"}) {
		t.Fatal("Resolve should find the pending approval")
	}
	d := <-ch
	if !d.Approved || d.Reason != "ok" {
		t.Fatalf("wrong decision: %+v", d)
	}
	// resolving again finds nothing (it was consumed)
	if r.Resolve("run1", "stepA", Decision{Approved: true}) {
		t.Error("second Resolve should report no pending approval")
	}
}

func TestApprovalRegistryResolveUnknownReturnsFalse(t *testing.T) {
	r := NewApprovalRegistry()
	if r.Resolve("nope", "nope", Decision{Approved: true}) {
		t.Error("Resolve of an unregistered key must return false")
	}
}

func TestApprovalRegistryCancelRemoves(t *testing.T) {
	r := NewApprovalRegistry()
	_ = r.Await("r", "s")
	r.Cancel("r", "s")
	if r.Resolve("r", "s", Decision{Approved: true}) {
		t.Error("a canceled approval must not resolve")
	}
}
