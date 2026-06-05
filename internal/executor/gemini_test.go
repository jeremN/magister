package executor

import (
	"bytes"
	"slices"
	"testing"

	"concentus/internal/event"
)

func TestGeminiSpecArgs(t *testing.T) {
	got := GeminiSpec{}.Args("gemini-2.5-pro", "do the thing")
	want := []string{"-p", "do the thing", "-m", "gemini-2.5-pro", "-o", "stream-json", "--approval-mode", "yolo", "--skip-trust"}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %v, want %v", got, want)
	}
}

func TestGeminiSpecParseConcatenatesDeltasAndEmitsMilestones(t *testing.T) {
	stream := `{"type":"init","model":"gemini-2.5-flash"}
{"type":"message","role":"user","content":"go"}
{"type":"tool_use","tool_name":"update_topic","parameters":{"strategic_intent":"x"}}
{"type":"tool_use","tool_name":"write_file","parameters":{"file_path":"report.txt","content":"hi"}}
{"type":"tool_result","tool_id":"x","status":"success"}
{"type":"message","role":"assistant","content":"I created","delta":true}
{"type":"message","role":"assistant","content":" report.txt.","delta":true}
{"type":"result","status":"success","stats":{"total_tokens":10}}`
	var got []event.Event
	emit := func(e event.Event) { got = append(got, e) }
	summary, cost, err := GeminiSpec{}.Parse(bytes.NewReader([]byte(stream)), emit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if summary != "I created report.txt." {
		t.Errorf("summary = %q, want %q (deltas concatenated)", summary, "I created report.txt.")
	}
	if cost != 0 {
		t.Errorf("cost = %v, want 0 (gemini reports no USD)", cost)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 milestone (update_topic skipped), got %d: %+v", len(got), got)
	}
	if got[0].Kind != event.AgentTool || got[0].Summary != "write_file: report.txt" {
		t.Errorf("milestone = %+v, want agent.tool \"write_file: report.txt\"", got[0])
	}
}

func TestGeminiSpecParseRendersCommandTool(t *testing.T) {
	stream := `{"type":"tool_use","tool_name":"run_shell_command","parameters":{"command":"go test ./..."}}
{"type":"result","status":"success"}`
	var got []event.Event
	_, _, err := GeminiSpec{}.Parse(bytes.NewReader([]byte(stream)), func(e event.Event) { got = append(got, e) })
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 || got[0].Summary != "run_shell_command: go test ./..." {
		t.Fatalf("milestone = %+v, want \"run_shell_command: go test ./...\"", got)
	}
}

func TestGeminiSpecParseErrorStatus(t *testing.T) {
	stream := `{"type":"result","status":"error"}`
	if _, _, err := (GeminiSpec{}).Parse(bytes.NewReader([]byte(stream)), noEmit); err == nil {
		t.Fatal("expected an error for a non-success result status")
	}
}

func TestGeminiSpecParseNoResultLine(t *testing.T) {
	stream := `{"type":"message","role":"assistant","content":"hi","delta":true}`
	if _, _, err := (GeminiSpec{}).Parse(bytes.NewReader([]byte(stream)), noEmit); err == nil {
		t.Fatal("expected an error when the stream has no result line")
	}
}

func TestRenderGeminiToolFallsBackToName(t *testing.T) {
	if got := renderGeminiTool("search_web", []byte(`{"query":"golang"}`)); got != "search_web" {
		t.Errorf("renderGeminiTool = %q, want bare \"search_web\"", got)
	}
}
