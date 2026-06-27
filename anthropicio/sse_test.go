package anthropicio

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sausheong/harness/llm"
)

// bufWriter is a test SSEWriter capturing everything written.
type bufWriter struct{ b strings.Builder }

func (w *bufWriter) Write(p []byte) (int, error) { return w.b.Write(p) }
func (w *bufWriter) Flush()                       {}

// parseSSE returns the ordered list of event types from an SSE payload.
func parseSSE(s string) []string {
	var types []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "event: ") {
			types = append(types, strings.TrimPrefix(line, "event: "))
		}
	}
	return types
}

func feed(evs ...llm.ChatEvent) <-chan llm.ChatEvent {
	ch := make(chan llm.ChatEvent, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch
}

func TestEncodeTextStream(t *testing.T) {
	w := &bufWriter{}
	err := EncodeSSE(w, "anthropic/opus", feed(
		llm.ChatEvent{Type: llm.EventTextDelta, Text: "Hel"},
		llm.ChatEvent{Type: llm.EventTextDelta, Text: "lo"},
		llm.ChatEvent{Type: llm.EventDone, StopReason: "end_turn",
			Usage: &llm.Usage{InputTokens: 10, OutputTokens: 2}},
	))
	if err != nil {
		t.Fatalf("EncodeSSE: %v", err)
	}
	out := w.b.String()
	got := parseSSE(out)
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event seq = %v\nwant %v\n---\n%s", got, want, out)
	}
	if !strings.Contains(out, `"text":"Hel"`) || !strings.Contains(out, `"text":"lo"`) {
		t.Errorf("missing text deltas in:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("missing stop_reason in:\n%s", out)
	}
}

func TestEncodeToolUse(t *testing.T) {
	w := &bufWriter{}
	err := EncodeSSE(w, "anthropic/opus", feed(
		llm.ChatEvent{Type: llm.EventToolCallStart, ToolCall: &llm.ToolCall{ID: "toolu_1", Name: "get"}},
		llm.ChatEvent{Type: llm.EventToolCallDone, ToolCall: &llm.ToolCall{ID: "toolu_1", Name: "get", Input: json.RawMessage(`{"q":"x"}`)}},
		llm.ChatEvent{Type: llm.EventDone, StopReason: "tool_use", Usage: &llm.Usage{InputTokens: 5, OutputTokens: 3}},
	))
	if err != nil {
		t.Fatalf("EncodeSSE: %v", err)
	}
	out := w.b.String()
	got := parseSSE(out)
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event seq = %v\nwant %v\n---\n%s", got, want, out)
	}
	if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, `"name":"get"`) {
		t.Errorf("missing tool_use start in:\n%s", out)
	}
	if !strings.Contains(out, `"type":"input_json_delta"`) || !strings.Contains(out, `{\"q\":\"x\"}`) {
		t.Errorf("missing input_json_delta with full input in:\n%s", out)
	}
}

func TestEncodeTextThenTool(t *testing.T) {
	// Text block must be closed before the tool block opens; indices increment.
	w := &bufWriter{}
	_ = EncodeSSE(w, "m", feed(
		llm.ChatEvent{Type: llm.EventTextDelta, Text: "thinking"},
		llm.ChatEvent{Type: llm.EventToolCallStart, ToolCall: &llm.ToolCall{ID: "t1", Name: "f"}},
		llm.ChatEvent{Type: llm.EventToolCallDone, ToolCall: &llm.ToolCall{ID: "t1", Name: "f", Input: json.RawMessage(`{}`)}},
		llm.ChatEvent{Type: llm.EventDone, StopReason: "tool_use", Usage: &llm.Usage{OutputTokens: 1}},
	))
	out := w.b.String()
	got := parseSSE(out)
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event seq = %v\nwant %v\n---\n%s", got, want, out)
	}
	if !strings.Contains(out, `"index":0`) || !strings.Contains(out, `"index":1`) {
		t.Errorf("expected indices 0 and 1 in:\n%s", out)
	}
}

func TestEncodeErrorMidStream(t *testing.T) {
	w := &bufWriter{}
	_ = EncodeSSE(w, "m", feed(
		llm.ChatEvent{Type: llm.EventTextDelta, Text: "partial"},
		llm.ChatEvent{Type: llm.EventError, Error: errString("boom")},
	))
	out := w.b.String()
	if !strings.Contains(out, "event: error") {
		t.Errorf("expected SSE error event in:\n%s", out)
	}
	if !strings.Contains(out, "event: message_stop") {
		t.Errorf("expected message_stop after error in:\n%s", out)
	}
}
