package supervisor

import (
	"context"
	"testing"
	"time"

	"concentus/internal/core"
	"concentus/internal/flow"
	"concentus/internal/gate"
)

var _ gate.Approver = (*RegistryApprover)(nil)

func TestRegistryApproverBlocksUntilResolved(t *testing.T) {
	reg := NewApprovalRegistry()
	a := &RegistryApprover{Reg: reg}
	step := &flow.Step{ID: "s"}

	res := make(chan bool, 1)
	go func() {
		ok, err := a.Approve(context.Background(), "r1", step, core.Result{})
		if err != nil {
			t.Errorf("approve: %v", err)
		}
		res <- ok
	}()

	// give the goroutine time to register, then resolve
	waitFor(t, func() bool { return reg.Resolve("r1", "s", Decision{Approved: true}) })
	select {
	case ok := <-res:
		if !ok {
			t.Error("expected approval")
		}
	case <-time.After(time.Second):
		t.Fatal("Approve did not return after Resolve")
	}
}

func TestRegistryApproverContextCancelUnblocks(t *testing.T) {
	a := &RegistryApprover{Reg: NewApprovalRegistry()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Approve(ctx, "r1", &flow.Step{ID: "s"}, core.Result{}); err == nil {
		t.Error("expected context error when canceled")
	}
}

// waitFor retries fn until true or a timeout (fn has a side effect we want once).
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}
