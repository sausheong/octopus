package openaiio

import (
	"encoding/json"
	"strings"

	"github.com/sausheong/harness/llm"
)

// CollectCompletion drains events into a single OpenAI ChatCompletion JSON for
// non-streaming responses.
func CollectCompletion(model string, events <-chan llm.ChatEvent) ([]byte, error) {
	var text strings.Builder
	var toolCalls []oaiToolCall
	finishReason := "stop"
	var usage *llm.Usage

	for ev := range events {
		switch ev.Type {
		case llm.EventTextDelta:
			text.WriteString(ev.Text)
		case llm.EventToolCallDone:
			if ev.ToolCall != nil {
				input := ev.ToolCall.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				toolCalls = append(toolCalls, oaiToolCall{
					ID:   ev.ToolCall.ID,
					Type: "function",
					Function: oaiToolCallFunction{
						Name:      ev.ToolCall.Name,
						Arguments: string(input),
					},
				})
			}
		case llm.EventError:
			if ev.Error != nil {
				return nil, ev.Error
			}
			return nil, errString("stream error")
		case llm.EventDone:
			if ev.StopReason != "" {
				finishReason = mapStopReason(ev.StopReason)
			}
			usage = ev.Usage
		}
	}

	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	msg := map[string]any{
		"role":    "assistant",
		"content": nil,
	}
	if text.Len() > 0 {
		msg["content"] = text.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	resp := map[string]any{
		"id":      newChatID(),
		"object":  "chat.completion",
		"model":   model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finishReason}},
	}
	if usage != nil {
		resp["usage"] = map[string]any{
			"prompt_tokens":     usage.InputTokens,
			"completion_tokens": usage.OutputTokens,
			"total_tokens":      usage.InputTokens + usage.OutputTokens,
		}
	}
	return json.Marshal(resp)
}

type oaiToolCall struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Function oaiToolCallFunction `json:"function"`
}

type oaiToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// mapStopReason converts harness/Anthropic stop reasons to OpenAI finish_reason.
func mapStopReason(r string) string {
	switch r {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

type errString string

func (e errString) Error() string { return string(e) }
