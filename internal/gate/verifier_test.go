package gate

import (
	"context"
	"strings"
	"testing"
)

func TestCommandVerifierPassesWithNoOutput(t *testing.T) {
	ok, out, err := CommandVerifier{}.Verify(context.Background(), "true", t.TempDir())
	if err != nil || !ok || out != "" {
		t.Fatalf("Verify(true) = (%v, %q, %v), want (true, \"\", nil)", ok, out, err)
	}
}

func TestCommandVerifierEmptyCommandPasses(t *testing.T) {
	ok, out, err := CommandVerifier{}.Verify(context.Background(), "", t.TempDir())
	if err != nil || !ok || out != "" {
		t.Fatalf("Verify(\"\") = (%v, %q, %v), want (true, \"\", nil)", ok, out, err)
	}
}

func TestCommandVerifierCapturesFailureOutput(t *testing.T) {
	ok, out, err := CommandVerifier{}.Verify(context.Background(), `echo "boom: bad"; exit 1`, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	if ok {
		t.Fatal("want ok=false on non-zero exit")
	}
	if !strings.Contains(out, "boom: bad") {
		t.Errorf("output = %q, want it to contain the command's stdout", out)
	}
}

func TestCommandVerifierTailCapsLargeOutput(t *testing.T) {
	// Emit ~25 KiB then a tail marker; the captured feedback is capped near
	// maxFeedbackBytes and keeps the tail.
	cmd := `i=0; while [ $i -lt 5000 ]; do echo LINE; i=$((i+1)); done; echo TAILMARK; exit 1`
	ok, out, err := CommandVerifier{}.Verify(context.Background(), cmd, t.TempDir())
	if err != nil || ok {
		t.Fatalf("Verify = (%v, %v), want (false, nil)", ok, err)
	}
	if len(out) > maxFeedbackBytes+64 {
		t.Errorf("output len %d exceeds cap %d (+marker slack)", len(out), maxFeedbackBytes)
	}
	if !strings.Contains(out, "TAILMARK") {
		t.Error("tail (with TAILMARK) must be kept")
	}
	if !strings.Contains(out, "truncated") {
		t.Error("truncation marker missing")
	}
}
