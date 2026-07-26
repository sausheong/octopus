package openaiio

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sausheong/harness/llm"
)

// ---- Decode tests -----------------------------------------------------------

func TestDecodeSimpleMessage(t *testing.T) {
	body := `{"model":"gpt-4o","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`
	chat, stream, model, err := Decode([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stream {
		t.Error("stream should be false")
	}
	if model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", model)
	}
	if len(chat.Messages) != 1 || chat.Messages[0].Content != "hello" {
		t.Errorf("unexpected messages: %+v", chat.Messages)
	}
	if chat.MaxTokens != 100 {
		t.Errorf("max_tokens = %d, want 100", chat.MaxTokens)
	}
}

func TestDecodeStreaming(t *testing.T) {
	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	_, stream, _, err := Decode([]byte(body))
	if err != nil || !stream {
		t.Errorf("stream = %v, err = %v", stream, err)
	}
}

func TestDecodeEmptyMessages(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[]}`
	if _, _, _, err := Decode([]byte(body)); err == nil {
		t.Error("expected error for empty messages")
	}
}

func TestDecodeBadJSON(t *testing.T) {
	if _, _, _, err := Decode([]byte("{bad")); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestDecodeRejectsInvalidRequests(t *testing.T) {
	tests := []string{
		`{"max_tokens":0,"messages":[{"role":"user","content":"x"}]}`,
		`{"temperature":2.1,"messages":[{"role":"user","content":"x"}]}`,
		`{"messages":[{"role":"bogus","content":"x"}]}`,
		`{"messages":[{"role":"tool","content":"x"}]}`,
		`{"tools":[{"type":"function","function":{"name":""}}],"messages":[{"role":"user","content":"x"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"audio"}]}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"{"}}]}]}`,
	}
	for _, body := range tests {
		if _, _, _, err := Decode([]byte(body)); err == nil {
			t.Errorf("Decode accepted invalid request: %s", body)
		}
	}
}

func TestDecodeDeveloperMessageAsSystem(t *testing.T) {
	chat, _, _, err := Decode([]byte(`{"messages":[{"role":"developer","content":"policy"},{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	system, rest := ExtractSystem(chat.Messages)
	if system != "policy" || len(rest) != 1 {
		t.Fatalf("system=%q rest=%+v", system, rest)
	}
}

func TestDecodeSystemMessage(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"system","content":"you are helpful"},
		{"role":"user","content":"hi"}
	]}`
	chat, _, _, err := Decode([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// system message is preserved as-is; ExtractSystem handles it separately
	sys, rest := ExtractSystem(chat.Messages)
	if sys != "you are helpful" {
		t.Errorf("system = %q, want 'you are helpful'", sys)
	}
	if len(rest) != 1 || rest[0].Content != "hi" {
		t.Errorf("unexpected rest: %+v", rest)
	}
}

func TestDecodeMultiPartContent(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"look at this"},
		{"type":"text","text":" image"}
	]}]}`
	chat, _, _, err := Decode([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chat.Messages[0].Content != "look at this image" {
		t.Errorf("content = %q", chat.Messages[0].Content)
	}
}

func TestDecodeImageDataURI(t *testing.T) {
	// 1x1 white PNG in base64
	b64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwADhQGAWjR9awAAAABJRU5ErkJggg=="
	body := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}}
	]}]}`
	chat, _, _, err := Decode([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chat.Messages[0].Images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(chat.Messages[0].Images))
	}
	if chat.Messages[0].Images[0].MimeType != "image/png" {
		t.Errorf("mime = %q", chat.Messages[0].Images[0].MimeType)
	}
}

func TestDecodePlainImageURLRejected(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"https://example.com/img.png"}}
	]}]}`
	if _, _, _, err := Decode([]byte(body)); err == nil {
		t.Error("expected error for plain image URL")
	}
}

func TestDecodeToolDefinitions(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"go"}],
		"tools":[{"type":"function","function":{"name":"search","description":"web search","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`
	chat, _, _, err := Decode([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Name != "search" {
		t.Errorf("unexpected tools: %+v", chat.Tools)
	}
}

func TestDecodeAssistantToolCalls(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","tool_calls":[{"id":"tc1","type":"function","function":{"name":"search","arguments":"{\"q\":\"test\"}"}}]},
		{"role":"tool","tool_call_id":"tc1","content":"result"}
	]}`
	chat, _, _, err := Decode([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// assistant message should carry the tool call
	var asst llm.Message
	for _, m := range chat.Messages {
		if m.Role == "assistant" {
			asst = m
		}
	}
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "tc1" {
		t.Errorf("unexpected tool calls: %+v", asst.ToolCalls)
	}
	if string(asst.ToolCalls[0].Input) != `{"q":"test"}` {
		t.Errorf("tool input = %s", asst.ToolCalls[0].Input)
	}
	// tool result becomes a user message with ToolCallID
	var toolResult llm.Message
	for _, m := range chat.Messages {
		if m.ToolCallID == "tc1" {
			toolResult = m
		}
	}
	if toolResult.Content != "result" {
		t.Errorf("tool result content = %q", toolResult.Content)
	}
}

func TestDecodeTemperature(t *testing.T) {
	body := `{"model":"m","temperature":0.3,"messages":[{"role":"user","content":"hi"}]}`
	chat, _, _, err := Decode([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chat.Temperature != 0.3 {
		t.Errorf("temperature = %v, want 0.3", chat.Temperature)
	}
}

// ---- ExtractSystem tests ----------------------------------------------------

func TestExtractSystemMultiple(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "line one"},
		{Role: "system", Content: "line two"},
		{Role: "user", Content: "hi"},
	}
	sys, rest := ExtractSystem(msgs)
	if sys != "line one\nline two" {
		t.Errorf("system = %q", sys)
	}
	if len(rest) != 1 {
		t.Errorf("rest len = %d", len(rest))
	}
}

func TestExtractSystemNone(t *testing.T) {
	msgs := []llm.Message{{Role: "user", Content: "hi"}}
	sys, rest := ExtractSystem(msgs)
	if sys != "" || len(rest) != 1 {
		t.Errorf("sys=%q rest=%v", sys, rest)
	}
}

// ---- CollectCompletion tests ------------------------------------------------

func events(evs ...llm.ChatEvent) <-chan llm.ChatEvent {
	ch := make(chan llm.ChatEvent, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch
}

func TestCollectCompletionText(t *testing.T) {
	ch := events(
		llm.ChatEvent{Type: llm.EventTextDelta, Text: "hello "},
		llm.ChatEvent{Type: llm.EventTextDelta, Text: "world"},
		llm.ChatEvent{Type: llm.EventDone, StopReason: "end_turn", Usage: &llm.Usage{InputTokens: 5, OutputTokens: 2}},
	)
	out, err := CollectCompletion("gpt-4o", ch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object = %v", resp["object"])
	}
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hello world" {
		t.Errorf("content = %v", msg["content"])
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v", choices[0].(map[string]any)["finish_reason"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 5 {
		t.Errorf("prompt_tokens = %v", usage["prompt_tokens"])
	}
}

func TestCollectCompletionToolCall(t *testing.T) {
	ch := events(
		llm.ChatEvent{Type: llm.EventToolCallDone, ToolCall: &llm.ToolCall{ID: "tc1", Name: "search", Input: json.RawMessage(`{"q":"test"}`)}},
		llm.ChatEvent{Type: llm.EventDone, StopReason: "tool_use"},
	)
	out, err := CollectCompletion("gpt-4o", ch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices := resp["choices"].([]any)
	if choices[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", choices[0].(map[string]any)["finish_reason"])
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	tcs := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "tc1" {
		t.Errorf("tool_call id = %v", tc["id"])
	}
}

func TestCollectCompletionError(t *testing.T) {
	ch := events(llm.ChatEvent{Type: llm.EventError, Error: errString("boom")})
	if _, err := CollectCompletion("m", ch); err == nil {
		t.Error("expected error from EventError")
	}
}

func TestCollectCompletionMaxTokens(t *testing.T) {
	ch := events(
		llm.ChatEvent{Type: llm.EventTextDelta, Text: "truncated"},
		llm.ChatEvent{Type: llm.EventDone, StopReason: "max_tokens"},
	)
	out, _ := CollectCompletion("m", ch)
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices := resp["choices"].([]any)
	if choices[0].(map[string]any)["finish_reason"] != "length" {
		t.Errorf("finish_reason = %v", choices[0].(map[string]any)["finish_reason"])
	}
}

// ---- EncodeSSE tests --------------------------------------------------------

type bufWriter struct{ bytes.Buffer }

func (b *bufWriter) Flush() {}

func TestEncodeSSEText(t *testing.T) {
	ch := events(
		llm.ChatEvent{Type: llm.EventTextDelta, Text: "hello"},
		llm.ChatEvent{Type: llm.EventDone, StopReason: "end_turn"},
	)
	w := &bufWriter{}
	if err := EncodeSSE(w, "gpt-4o", ch); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := w.String()
	if !strings.Contains(out, `"hello"`) {
		t.Errorf("missing text delta in SSE: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing [DONE]: %s", out)
	}
	// every line must be "data: ..." or blank
	for _, line := range strings.Split(out, "\n") {
		if line != "" && !strings.HasPrefix(line, "data: ") {
			t.Errorf("unexpected SSE line: %q", line)
		}
	}
}

func TestEncodeSSERoleHeader(t *testing.T) {
	ch := events(llm.ChatEvent{Type: llm.EventDone, StopReason: "end_turn"})
	w := &bufWriter{}
	EncodeSSE(w, "m", ch)
	if !strings.Contains(w.String(), `"role":"assistant"`) {
		t.Errorf("missing role header chunk: %s", w.String())
	}
}

func TestEncodeSSEToolCallDoneWithoutStart(t *testing.T) {
	ch := events(
		llm.ChatEvent{Type: llm.EventToolCallDone, ToolCall: &llm.ToolCall{ID: "tc1", Name: "fn", Input: json.RawMessage(`{"a":1}`)}},
		llm.ChatEvent{Type: llm.EventDone, StopReason: "tool_use"},
	)
	w := &bufWriter{}
	if err := EncodeSSE(w, "m", ch); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := w.String()
	if !strings.Contains(out, `"tc1"`) {
		t.Errorf("missing tool call id in SSE: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing [DONE]: %s", out)
	}
}

func TestEncodeSSEToolCallStartThenDoneIncludesArguments(t *testing.T) {
	ch := events(
		llm.ChatEvent{Type: llm.EventToolCallStart, ToolCall: &llm.ToolCall{ID: "tc1", Name: "fn"}},
		llm.ChatEvent{Type: llm.EventToolCallDone, ToolCall: &llm.ToolCall{ID: "tc1", Name: "fn", Input: json.RawMessage(`{"a":1}`)}},
		llm.ChatEvent{Type: llm.EventDone, StopReason: "tool_use"},
	)
	w := &bufWriter{}
	if err := EncodeSSE(w, "m", ch); err != nil {
		t.Fatalf("EncodeSSE: %v", err)
	}
	if !strings.Contains(w.String(), `"arguments":"{\"a\":1}"`) {
		t.Fatalf("full arguments were lost after Start/Done: %s", w.String())
	}
}

func TestEncodeSSEMultipleStartedToolCallsKeepIndices(t *testing.T) {
	ch := events(
		llm.ChatEvent{Type: llm.EventToolCallStart, ToolCall: &llm.ToolCall{ID: "tc1", Name: "one"}},
		llm.ChatEvent{Type: llm.EventToolCallStart, ToolCall: &llm.ToolCall{ID: "tc2", Name: "two"}},
		llm.ChatEvent{Type: llm.EventToolCallDone, ToolCall: &llm.ToolCall{ID: "tc1", Name: "one", Input: json.RawMessage(`{"a":1}`)}},
		llm.ChatEvent{Type: llm.EventToolCallDone, ToolCall: &llm.ToolCall{ID: "tc2", Name: "two", Input: json.RawMessage(`{"b":2}`)}},
		llm.ChatEvent{Type: llm.EventDone, StopReason: "tool_use"},
	)
	w := &bufWriter{}
	if err := EncodeSSE(w, "m", ch); err != nil {
		t.Fatalf("EncodeSSE: %v", err)
	}
	out := w.String()
	for _, want := range []string{`"index":0`, `"index":1`, `"arguments":"{\"a\":1}"`, `"arguments":"{\"b\":2}"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
}

func TestEncodeSSEError(t *testing.T) {
	ch := events(llm.ChatEvent{Type: llm.EventError, Error: errString("boom")})
	w := &bufWriter{}
	EncodeSSE(w, "m", ch)
	out := w.String()
	if !strings.Contains(out, "boom") {
		t.Errorf("missing error message: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing [DONE] after error: %s", out)
	}
}

func TestCollectCompletionRejectsMissingDone(t *testing.T) {
	_, err := CollectCompletion("m", events(llm.ChatEvent{Type: llm.EventTextDelta, Text: "partial"}))
	if err == nil {
		t.Fatal("expected premature channel closure to fail")
	}
}

func TestEncodeSSEPrematureClosureEmitsError(t *testing.T) {
	w := &bufWriter{}
	if err := EncodeSSE(w, "m", events(llm.ChatEvent{Type: llm.EventTextDelta, Text: "partial"})); err != nil {
		t.Fatalf("EncodeSSE: %v", err)
	}
	out := w.String()
	if !strings.Contains(out, "provider stream closed without terminal event") || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("premature closure was not surfaced as an error: %s", out)
	}
}

func TestDecodeTagsUserAsNonExplicitSession(t *testing.T) {
	chat, _, _, err := Decode([]byte(`{"model":"m","user":"bob",
		"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if chat.SessionID != "user:bob" {
		t.Errorf("SessionID = %q, want %q", chat.SessionID, "user:bob")
	}
}

func TestDecodeLeavesSessionEmptyWithoutUser(t *testing.T) {
	chat, _, _, err := Decode([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if chat.SessionID != "" {
		t.Errorf("SessionID = %q, want empty", chat.SessionID)
	}
}
