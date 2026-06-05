package executor

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"concentus/internal/event"
)

// noEmit is a no-op milestone sink for parser tests that don't assert emissions.
func noEmit(event.Event) {}

func TestClaudeSpecArgs(t *testing.T) {
	got := ClaudeSpec{}.Args("opus", "do the thing")
	want := []string{"-p", "do the thing", "--model", "opus", "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits"}
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

func TestClaudeSpecParseEmitsToolMilestones(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"working"},{"type":"tool_use","name":"Edit","input":{"file_path":"src/foo.go"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.04}`
	var got []event.Event
	emit := func(e event.Event) { got = append(got, e) }
	summary, cost, err := ClaudeSpec{}.Parse(bytes.NewReader([]byte(stream)), emit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if summary != "done" || cost != 0.04 {
		t.Errorf("summary=%q cost=%v, want done/0.04", summary, cost)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 tool milestones, got %d: %+v", len(got), got)
	}
	if got[0].Kind != event.AgentTool || got[0].Summary != "Edit: src/foo.go" {
		t.Errorf("milestone[0] = %+v, want agent.tool \"Edit: src/foo.go\"", got[0])
	}
	if got[1].Kind != event.AgentTool || got[1].Summary != "Bash: go test ./..." {
		t.Errorf("milestone[1] = %+v, want agent.tool \"Bash: go test ./...\"", got[1])
	}
}

func TestClaudeSpecParseNoResultLine(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"x"}}]}}`
	if _, _, err := (ClaudeSpec{}).Parse(bytes.NewReader([]byte(stream)), noEmit); err == nil {
		t.Fatal("expected error when the stream has no result line")
	}
}

func TestClaudeSpecParseLargeToolResultLine(t *testing.T) {
	big := strings.Repeat("x", 200_000) // > bufio.Scanner's 64KB default token cap
	stream := `{"type":"user","message":{"content":[{"type":"tool_result","content":"` + big + `"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"ok","total_cost_usd":0.01}`
	summary, _, err := ClaudeSpec{}.Parse(bytes.NewReader([]byte(stream)), noEmit)
	if err != nil {
		t.Fatalf("a >64KB NDJSON line must decode (json.Decoder has no line cap), got: %v", err)
	}
	if summary != "ok" {
		t.Errorf("summary = %q, want ok", summary)
	}
}

func TestClaudeSpecParseErrorMessageNeverEmpty(t *testing.T) {
	out := []byte(`{"type":"result","is_error":true,"subtype":"","result":"","total_cost_usd":0}`)
	_, _, err := (ClaudeSpec{}).Parse(bytes.NewReader(out), noEmit)
	if err == nil {
		t.Fatal("expected an error for an is_error result")
	}
	if strings.Contains(err.Error(), "()") {
		t.Errorf("failure message has empty parens: %v", err)
	}
	if !strings.Contains(err.Error(), "is_error") {
		t.Errorf("want a non-empty reason in the message, got: %v", err)
	}
}
