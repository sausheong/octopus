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
	type toolState struct {
		index   int
		id      string
		name    string
		deltaed bool
		done    bool
	}
	var toolCalls []*toolState
	nextToolIndex := 0
	toolSeen := false
	findTool := func(tc *llm.ToolCall) *toolState {
		if tc != nil {
			for _, state := range toolCalls {
				if !state.done && tc.ID != "" && state.id == tc.ID {
					return state
				}
			}
			for _, state := range toolCalls {
				if !state.done && tc.Name != "" && state.name == tc.Name {
					return state
				}
			}
		}
		// Argument deltas often have no ID. Associate those with the most
		// recently opened call, which matches provider event ordering.
		for i := len(toolCalls) - 1; i >= 0; i-- {
			if !toolCalls[i].done {
				return toolCalls[i]
			}
		}
		return nil
	}

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
	emitError := func(msg string) error {
		errChunk := map[string]any{
			"id": id, "object": "chat.completion.chunk", "model": model,
			"error": map[string]any{"message": msg, "type": "api_error"},
		}
		data, err := json.Marshal(errChunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		w.Flush()
		if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
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
			state := &toolState{index: nextToolIndex, id: ev.ToolCall.ID, name: ev.ToolCall.Name}
			nextToolIndex++
			toolCalls = append(toolCalls, state)
			toolSeen = true
			delta := map[string]any{
				"tool_calls": []any{map[string]any{
					"index": state.index,
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

		case llm.EventToolCallDelta:
			if ev.ToolCall == nil {
				continue
			}
			state := findTool(ev.ToolCall)
			if state == nil {
				continue
			}
			delta := map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    state.index,
					"function": map[string]any{"arguments": string(ev.ToolCall.Input)},
				}},
			}
			if err := emit(delta, nil); err != nil {
				return err
			}
			state.deltaed = true

		case llm.EventToolCallDone:
			if ev.ToolCall == nil {
				continue
			}
			state := findTool(ev.ToolCall)
			if state == nil {
				// Provider sent Done without Start — emit a combined chunk.
				state = &toolState{index: nextToolIndex, id: ev.ToolCall.ID, name: ev.ToolCall.Name}
				nextToolIndex++
				toolCalls = append(toolCalls, state)
				input := ev.ToolCall.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				delta := map[string]any{
					"tool_calls": []any{map[string]any{
						"index": state.index,
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
			} else if !state.deltaed {
				input := ev.ToolCall.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				if err := emit(map[string]any{
					"tool_calls": []any{map[string]any{
						"index":    state.index,
						"function": map[string]any{"arguments": string(input)},
					}},
				}, nil); err != nil {
					return err
				}
			}
			state.done = true
			toolSeen = true

		case llm.EventError:
			msg := "stream error"
			if ev.Error != nil {
				msg = ev.Error.Error()
			}
			return emitError(msg)

		case llm.EventDone:
			fr := mapStopReason(ev.StopReason)
			if toolSeen {
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

	return emitError("provider stream closed without terminal event")
}
