package anthropicio

import (
	"encoding/json"
	"fmt"

	"github.com/sausheong/harness/llm"
)

// SSEWriter is the minimal sink EncodeSSE needs: write bytes and flush after
// each event so the client receives a true stream.
type SSEWriter interface {
	Write([]byte) (int, error)
	Flush()
}

// EncodeSSE drains events and writes the Anthropic SSE sequence to w. It
// returns an error only when writing to w fails. A mid-stream EventError is
// surfaced as an SSE "error" event followed by message_stop (the HTTP status
// is already committed by the time streaming starts, so we cannot change it).
func EncodeSSE(w SSEWriter, model string, events <-chan llm.ChatEvent) error {
	enc := &sseState{w: w, model: model}
	if err := enc.start(); err != nil {
		return err
	}
	for ev := range events {
		switch ev.Type {
		case llm.EventTextDelta:
			if err := enc.textDelta(ev.Text); err != nil {
				return err
			}
		case llm.EventToolCallStart:
			if ev.ToolCall != nil {
				if err := enc.toolStart(ev.ToolCall.ID, ev.ToolCall.Name); err != nil {
					return err
				}
			}
		case llm.EventToolCallDelta:
			if ev.ToolCall != nil && len(ev.ToolCall.Input) > 0 {
				if err := enc.toolInputDelta(string(ev.ToolCall.Input)); err != nil {
					return err
				}
			}
		case llm.EventToolCallDone:
			if ev.ToolCall != nil {
				if err := enc.toolDone(string(ev.ToolCall.Input)); err != nil {
					return err
				}
			}
		case llm.EventError:
			return enc.errorEvent(ev.Error)
		case llm.EventDone:
			return enc.done(ev.StopReason, ev.Usage)
		}
	}
	// Channel closed without an explicit EventDone (e.g. provider closed
	// early): finish cleanly with end_turn and zero usage.
	return enc.done("end_turn", nil)
}

// sseState tracks block indices and the currently-open block so the emitted
// sequence is always well-formed.
type sseState struct {
	w           SSEWriter
	model       string
	index       int    // index of the next/open block
	openType    string // "", "text", "tool_use"
	toolDeltaed bool   // whether an input_json_delta was already sent for the open tool block
	finished    bool
}

func (s *sseState) emit(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	s.w.Flush()
	return nil
}

func (s *sseState) start() error {
	return s.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            newMessageID(),
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// closeOpen closes the currently open content block, if any.
func (s *sseState) closeOpen() error {
	if s.openType == "" {
		return nil
	}
	if err := s.emit("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": s.index,
	}); err != nil {
		return err
	}
	s.index++
	s.openType = ""
	s.toolDeltaed = false
	return nil
}

func (s *sseState) textDelta(text string) error {
	if s.openType != "text" {
		if err := s.closeOpen(); err != nil {
			return err
		}
		if err := s.emit("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         s.index,
			"content_block": map[string]any{"type": "text", "text": ""},
		}); err != nil {
			return err
		}
		s.openType = "text"
	}
	return s.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": s.index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (s *sseState) toolStart(id, name string) error {
	if err := s.closeOpen(); err != nil {
		return err
	}
	if err := s.emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": s.index,
		"content_block": map[string]any{
			"type": "tool_use", "id": id, "name": name, "input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	s.openType = "tool_use"
	s.toolDeltaed = false
	return nil
}

// toolInputDelta forwards an incremental input_json_delta (used only if a
// provider streams tool args; harness providers currently don't).
func (s *sseState) toolInputDelta(partial string) error {
	if s.openType != "tool_use" {
		return nil
	}
	s.toolDeltaed = true
	return s.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": s.index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
	})
}

// toolDone emits the full tool input as a single input_json_delta (unless
// deltas were already streamed) and closes the tool block.
func (s *sseState) toolDone(fullInput string) error {
	if s.openType != "tool_use" {
		// Defensive: a Done without a Start. Open a block so output is valid.
		if err := s.toolStart("toolu_unknown", "unknown"); err != nil {
			return err
		}
	}
	if !s.toolDeltaed && fullInput != "" {
		if err := s.toolInputDelta(fullInput); err != nil {
			return err
		}
	}
	return s.closeOpen()
}

func (s *sseState) done(stopReason string, usage *llm.Usage) error {
	if s.finished {
		return nil
	}
	if err := s.closeOpen(); err != nil {
		return err
	}
	if stopReason == "" {
		stopReason = "end_turn"
	}
	out := 0
	usageObj := map[string]any{"output_tokens": out}
	if usage != nil {
		out = usage.OutputTokens
		usageObj["output_tokens"] = out
		usageObj["input_tokens"] = usage.InputTokens
		if usage.CacheCreationInputTokens > 0 {
			usageObj["cache_creation_input_tokens"] = usage.CacheCreationInputTokens
		}
		if usage.CacheReadInputTokens > 0 {
			usageObj["cache_read_input_tokens"] = usage.CacheReadInputTokens
		}
	}
	if err := s.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": usageObj,
	}); err != nil {
		return err
	}
	s.finished = true
	return s.emit("message_stop", map[string]any{"type": "message_stop"})
}

func (s *sseState) errorEvent(err error) error {
	msg := "stream error"
	if err != nil {
		msg = err.Error()
	}
	if e := s.closeOpen(); e != nil {
		return e
	}
	if e := s.emit("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": msg},
	}); e != nil {
		return e
	}
	s.finished = true
	return s.emit("message_stop", map[string]any{"type": "message_stop"})
}
