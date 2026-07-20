package anthropicio

import (
	"encoding/json"
	"testing"

	"github.com/sausheong/harness/llm"
)

func TestCollectText(t *testing.T) {
	out, err := CollectMessage("anthropic/opus", feed(
		llm.ChatEvent{Type: llm.EventTextDelta, Text: "Hel"},
		llm.ChatEvent{Type: llm.EventTextDelta, Text: "lo"},
		llm.ChatEvent{Type: llm.EventDone, StopReason: "end_turn", Usage: &llm.Usage{InputTokens: 4, OutputTokens: 2}},
	))
	if err != nil {
		t.Fatalf("CollectMessage: %v", err)
	}
	var m struct {
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if m.Role != "assistant" || m.Model != "anthropic/opus" {
		t.Errorf("role/model = %q/%q", m.Role, m.Model)
	}
	if len(m.Content) != 1 || m.Content[0].Type != "text" || m.Content[0].Text != "Hello" {
		t.Fatalf("content = %+v", m.Content)
	}
	if m.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", m.StopReason)
	}
	if m.Usage.InputTokens != 4 || m.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v", m.Usage)
	}
}

func TestCollectToolUse(t *testing.T) {
	out, err := CollectMessage("m", feed(
		llm.ChatEvent{Type: llm.EventTextDelta, Text: "calling"},
		llm.ChatEvent{Type: llm.EventToolCallDone, ToolCall: &llm.ToolCall{ID: "t1", Name: "get", Input: json.RawMessage(`{"q":"x"}`)}},
		llm.ChatEvent{Type: llm.EventDone, StopReason: "tool_use", Usage: &llm.Usage{OutputTokens: 3}},
	))
	if err != nil {
		t.Fatalf("CollectMessage: %v", err)
	}
	var m struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m.Content) != 2 {
		t.Fatalf("content len = %d, want 2: %s", len(m.Content), out)
	}
	if m.Content[0].Type != "text" || m.Content[0].Text != "calling" {
		t.Errorf("content[0] = %+v", m.Content[0])
	}
	if m.Content[1].Type != "tool_use" || m.Content[1].Name != "get" || string(m.Content[1].Input) != `{"q":"x"}` {
		t.Errorf("content[1] = %+v", m.Content[1])
	}
}

func TestCollectError(t *testing.T) {
	_, err := CollectMessage("m", feed(
		llm.ChatEvent{Type: llm.EventError, Error: errString("boom")},
	))
	if err == nil {
		t.Fatal("expected error from EventError")
	}
}

func TestCollectRejectsMissingDone(t *testing.T) {
	_, err := CollectMessage("m", feed(llm.ChatEvent{Type: llm.EventTextDelta, Text: "partial"}))
	if err == nil {
		t.Fatal("expected premature channel closure to fail")
	}
}
