package executor

import (
	"bytes"
	"slices"
	"testing"

	"concentus/internal/event"
)

func TestCodexSpecArgs(t *testing.T) {
	got := CodexSpec{}.Args("gpt-5-codex", "do the thing")
	want := []string{"exec", "--json", "-s", "workspace-write", "--skip-git-repo-check", "-m", "gpt-5-codex", "do the thing"}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %v, want %v", got, want)
	}
}

func TestCodexSpecArgsOmitsEmptyModel(t *testing.T) {
	got := CodexSpec{}.Args("", "do the thing")
	want := []string{"exec", "--json", "-s", "workspace-write", "--skip-git-repo-check", "do the thing"}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %v, want %v (no -m when model empty)", got, want)
	}
}

func TestCodexSpecParseConcatenatesMessagesAndEmitsMilestones(t *testing.T) {
	stream := `{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"I'll create hello.txt."}}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/zsh -lc ls","status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"/bin/zsh -lc ls","aggregated_output":"hello.txt\n","exit_code":0,"status":"completed"}}
{"type":"item.started","item":{"id":"item_2","type":"file_change","changes":[{"path":"/tmp/x/hello.txt","kind":"add"}],"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_2","type":"file_change","changes":[{"path":"/tmp/x/hello.txt","kind":"add"}],"status":"completed"}}
{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}`
	var got []event.Event
	emit := func(e event.Event) { got = append(got, e) }
	summary, cost, err := CodexSpec{}.Parse(bytes.NewReader([]byte(stream)), emit)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if summary != "I'll create hello.txt.\ndone" {
		t.Errorf("summary = %q, want %q (messages concatenated newline-joined)", summary, "I'll create hello.txt.\ndone")
	}
	if cost != 0 {
		t.Errorf("cost = %v, want 0 (codex reports no USD)", cost)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 milestones (one per tool item.started, no double-emit on completed), got %d: %+v", len(got), got)
	}
	if got[0].Kind != event.AgentTool || got[0].Summary != "command_execution: /bin/zsh -lc ls" {
		t.Errorf("milestone[0] = %+v, want agent.tool \"command_execution: /bin/zsh -lc ls\"", got[0])
	}
	if got[1].Kind != event.AgentTool || got[1].Summary != "file_change: add hello.txt" {
		t.Errorf("milestone[1] = %+v, want agent.tool \"file_change: add hello.txt\"", got[1])
	}
}

func TestRenderCodexItemFallsBackToType(t *testing.T) {
	if got := renderCodexItem(&codexItem{Type: "web_search"}); got != "web_search" {
		t.Errorf("renderCodexItem = %q, want bare \"web_search\"", got)
	}
}

func TestCodexSpecParseFailureWithoutMessageStillErrors(t *testing.T) {
	// A turn.failed with no error payload must still fail — the failure fact is
	// independent of whether a message was extracted.
	stream := `{"type":"turn.started"}
{"type":"turn.failed"}`
	if _, _, err := (CodexSpec{}).Parse(bytes.NewReader([]byte(stream)), noEmit); err == nil {
		t.Fatal("expected an error for turn.failed even with no error message")
	}
}

func TestCodexSpecParseCompletedSupersedesEarlierFailure(t *testing.T) {
	// A later turn.completed wins over an earlier turn.failed (last terminal event).
	stream := `{"type":"error","message":"transient"}
{"type":"turn.failed","error":{"message":"transient"}}
{"type":"item.completed","item":{"type":"agent_message","text":"recovered"}}
{"type":"turn.completed","usage":{"input_tokens":1}}`
	summary, _, err := CodexSpec{}.Parse(bytes.NewReader([]byte(stream)), noEmit)
	if err != nil {
		t.Fatalf("a completed turn should supersede an earlier failure, got err: %v", err)
	}
	if summary != "recovered" {
		t.Errorf("summary = %q, want %q", summary, "recovered")
	}
}

func TestCodexSpecParseTurnFailed(t *testing.T) {
	stream := `{"type":"turn.started"}
{"type":"error","message":"bad model"}
{"type":"turn.failed","error":{"message":"bad model"}}`
	if _, _, err := (CodexSpec{}).Parse(bytes.NewReader([]byte(stream)), noEmit); err == nil {
		t.Fatal("expected an error when the stream reports turn.failed")
	}
}

func TestCodexSpecParseNoTurnCompleted(t *testing.T) {
	stream := `{"type":"item.completed","item":{"type":"agent_message","text":"hi"}}`
	if _, _, err := (CodexSpec{}).Parse(bytes.NewReader([]byte(stream)), noEmit); err == nil {
		t.Fatal("expected an error when the stream has no turn.completed")
	}
}
