package openaiio

import (
	"encoding/json"
	"fmt"

	"github.com/sausheong/harness/llm"
)

// SSEWriter is the minimal sink EncodeSSE needs.
type SSEWriter interface {
	Write([]byte) (int, error)
	Flush()
}

// EncodeSSE drains events and writes OpenAI-format SSE chunks to w.
func EncodeSSE(w SSEWriter, model string, events <-chan llm.ChatEvent) error {
	id := newChatID()
	// active tool call being accumulated
	var activeTCIdx int
	activeTC := false

	emit := func(delta map[string]any, finishReason *string) error {
		choice := map[string]any{"index": 0, "delta": delta}
		if finishReason != nil {
			choice["finish_reason"] = *finishReason
		} else {
			choice["finish_reason"] = nil
		}
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []any{choice},
		}
		data, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		w.Flush()
		return nil
	}

	// role header chunk
	if err := emit(map[string]any{"role": "assistant", "content": ""}, nil); err != nil {
		return err
	}

	for ev := range events {
		switch ev.Type {
		case llm.EventTextDelta:
			if err := emit(map[string]any{"content": ev.Text}, nil); err != nil {
				return err
			}

		case llm.EventToolCallStart:
			if ev.ToolCall == nil {
				continue
			}
			delta := map[string]any{
				"tool_calls": []any{map[string]any{
					"index": activeTCIdx,
					"id":    ev.ToolCall.ID,
					"type":  "function",
					"function": map[string]any{
						"name":      ev.ToolCall.Name,
						"arguments": "",
					},
				}},
			}
			if err := emit(delta, nil); err != nil {
				return err
			}
			activeTC = true

		case llm.EventToolCallDelta:
			if ev.ToolCall == nil || !activeTC {
				continue
			}
			delta := map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    activeTCIdx,
					"function": map[string]any{"arguments": string(ev.ToolCall.Input)},
				}},
			}
			if err := emit(delta, nil); err != nil {
				return err
			}

		case llm.EventToolCallDone:
			if ev.ToolCall == nil {
				continue
			}
			if !activeTC {
				// Provider sent Done without Start — emit a combined chunk.
				input := ev.ToolCall.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				delta := map[string]any{
					"tool_calls": []any{map[string]any{
						"index": activeTCIdx,
						"id":    ev.ToolCall.ID,
						"type":  "function",
						"function": map[string]any{
							"name":      ev.ToolCall.Name,
							"arguments": string(input),
						},
					}},
				}
				if err := emit(delta, nil); err != nil {
					return err
				}
			}
			activeTCIdx++
			activeTC = false

		case llm.EventError:
			msg := "stream error"
			if ev.Error != nil {
				msg = ev.Error.Error()
			}
			errChunk := map[string]any{
				"id":     id,
				"object": "chat.completion.chunk",
				"model":  model,
				"error":  map[string]any{"message": msg, "type": "api_error"},
			}
			data, _ := json.Marshal(errChunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			w.Flush()
			_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
			w.Flush()
			return nil

		case llm.EventDone:
			fr := mapStopReason(ev.StopReason)
			if activeTCIdx > 0 {
				fr = "tool_calls"
			}
			if err := emit(map[string]any{}, &fr); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
			w.Flush()
			return nil
		}
	}

	// Channel closed without EventDone.
	fr := "stop"
	_ = emit(map[string]any{}, &fr)
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	w.Flush()
	return nil
}
