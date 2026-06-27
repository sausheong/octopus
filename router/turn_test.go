package router

import (
	"testing"

	"github.com/sausheong/harness/llm"
)

func TestLastUserTurnPlain(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply"},
		{Role: "user", Content: "second"},
	}
	got, ok := LastUserTurn(msgs)
	if !ok || got.Content != "second" {
		t.Fatalf("got %q ok=%v, want \"second\"", got.Content, ok)
	}
}

func TestLastUserTurnSkipsToolResult(t *testing.T) {
	// A tool_result continuation: latest message is a tool_result (user role
	// with ToolCallID). The genuine user turn is the earlier "do the thing".
	msgs := []llm.Message{
		{Role: "user", Content: "do the thing"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "t1", Name: "x"}}},
		{Role: "user", ToolCallID: "t1", Content: "tool output"},
	}
	got, ok := LastUserTurn(msgs)
	if !ok || got.Content != "do the thing" {
		t.Fatalf("got %q ok=%v, want \"do the thing\"", got.Content, ok)
	}
}

func TestLastUserTurnMultipleToolResults(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "real turn"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "t1"}, {ID: "t2"}}},
		{Role: "user", ToolCallID: "t1", Content: "out1"},
		{Role: "user", ToolCallID: "t2", Content: "out2"},
	}
	got, ok := LastUserTurn(msgs)
	if !ok || got.Content != "real turn" {
		t.Fatalf("got %q ok=%v, want \"real turn\"", got.Content, ok)
	}
}

func TestLastUserTurnNone(t *testing.T) {
	msgs := []llm.Message{{Role: "assistant", Content: "hi"}}
	if _, ok := LastUserTurn(msgs); ok {
		t.Fatal("expected ok=false when no genuine user turn")
	}
}
