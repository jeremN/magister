package executor

import (
	"bytes"
	"slices"
	"testing"

	"concentus/internal/event"
)

// noEmit is a no-op milestone sink for parser tests that don't assert emissions.
func noEmit(event.Event) {}

func TestClaudeSpecArgs(t *testing.T) {
	got := ClaudeSpec{}.Args("opus", "do the thing")
	want := []string{"-p", "do the thing", "--model", "opus", "--output-format", "json", "--permission-mode", "acceptEdits"}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %v, want %v", got, want)
	}
}

func TestClaudeSpecParseSuccess(t *testing.T) {
	out := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"all done","total_cost_usd":0.0123,"usage":{"input_tokens":5}}`)
	summary, cost, err := ClaudeSpec{}.Parse(bytes.NewReader(out), noEmit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if summary != "all done" {
		t.Errorf("summary = %q, want %q", summary, "all done")
	}
	if cost != 0.0123 {
		t.Errorf("cost = %v, want 0.0123", cost)
	}
}

func TestClaudeSpecParseErrorResult(t *testing.T) {
	out := []byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"","total_cost_usd":0.5,"errors":["hit max turns"]}`)
	if _, _, err := (ClaudeSpec{}).Parse(bytes.NewReader(out), noEmit); err == nil {
		t.Fatal("expected error for is_error/non-success result")
	}
}

func TestClaudeSpecParseMalformed(t *testing.T) {
	if _, _, err := (ClaudeSpec{}).Parse(bytes.NewReader([]byte("not json at all")), noEmit); err == nil {
		t.Fatal("expected a parse error for non-JSON output")
	}
}
