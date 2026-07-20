package anthropicio

import (
	"encoding/json"
	"strings"

	"github.com/sausheong/harness/llm"
)

// CollectMessage drains events into a single Anthropic Message JSON for
// non-streaming responses. An EventError aborts with an error so the caller
// can return a proper HTTP error status (nothing has been written yet).
func CollectMessage(model string, events <-chan llm.ChatEvent) ([]byte, error) {
	var text strings.Builder
	type toolBlock struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	var tools []toolBlock
	type thinkingBlock struct {
		Type      string `json:"type"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature,omitempty"`
	}
	var thinking []thinkingBlock
	stopReason := "end_turn"
	in, out, cacheCreation, cacheRead := 0, 0, 0, 0
	done := false

eventLoop:
	for ev := range events {
		switch ev.Type {
		case llm.EventThinkingBlock:
			if ev.ThinkingBlock != nil {
				thinking = append(thinking, thinkingBlock{Type: "thinking", Thinking: ev.ThinkingBlock.Thinking, Signature: ev.ThinkingBlock.Signature})
			}
		case llm.EventTextDelta:
			text.WriteString(ev.Text)
		case llm.EventToolCallDone:
			if ev.ToolCall != nil {
				input := ev.ToolCall.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				tools = append(tools, toolBlock{
					Type: "tool_use", ID: ev.ToolCall.ID, Name: ev.ToolCall.Name, Input: input,
				})
			}
		case llm.EventError:
			if ev.Error != nil {
				return nil, ev.Error
			}
			return nil, errString("stream error")
		case llm.EventDone:
			done = true
			if ev.StopReason != "" {
				stopReason = ev.StopReason
			}
			if ev.Usage != nil {
				in = ev.Usage.InputTokens
				out = ev.Usage.OutputTokens
				cacheCreation = ev.Usage.CacheCreationInputTokens
				cacheRead = ev.Usage.CacheReadInputTokens
			}
			break eventLoop
		}
	}
	if !done {
		return nil, errString("provider stream closed without terminal event")
	}

	var content []any
	for _, tb := range thinking {
		content = append(content, tb)
	}
	if text.Len() > 0 {
		content = append(content, map[string]any{"type": "text", "text": text.String()})
	}
	for _, tb := range tools {
		content = append(content, tb)
	}
	if content == nil {
		content = []any{}
	}

	usage := map[string]any{"input_tokens": in, "output_tokens": out}
	if cacheCreation > 0 {
		usage["cache_creation_input_tokens"] = cacheCreation
	}
	if cacheRead > 0 {
		usage["cache_read_input_tokens"] = cacheRead
	}
	msg := map[string]any{
		"id":            newMessageID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usage,
	}
	return json.Marshal(msg)
}

// errString is a tiny error type so this file has no extra deps. It mirrors
// the one in the test; defined here for production use.
type errString string

func (e errString) Error() string { return string(e) }
