package anthropicio

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/sausheong/harness/llm"
)

// Decode parses an Anthropic Messages API request body into a DecodedRequest.
func Decode(body []byte) (DecodedRequest, error) {
	var wr wireRequest
	if err := json.Unmarshal(body, &wr); err != nil {
		return DecodedRequest{}, fmt.Errorf("invalid request JSON: %w", err)
	}
	if len(wr.Messages) == 0 {
		return DecodedRequest{}, fmt.Errorf("messages must not be empty")
	}
	if wr.MaxTokens <= 0 {
		return DecodedRequest{}, fmt.Errorf("max_tokens must be > 0")
	}
	if wr.Temperature != nil && (math.IsNaN(*wr.Temperature) || math.IsInf(*wr.Temperature, 0) || *wr.Temperature < 0 || *wr.Temperature > 1) {
		return DecodedRequest{}, fmt.Errorf("temperature must be a finite number in [0,1]")
	}
	system, err := decodeSystem(wr.System)
	if err != nil {
		return DecodedRequest{}, err
	}

	chat := llm.ChatRequest{
		Model:        wr.Model,
		MaxTokens:    wr.MaxTokens,
		SystemPrompt: system,
	}
	if wr.Temperature != nil {
		chat.Temperature = *wr.Temperature
	}
	for _, t := range wr.Tools {
		if strings.TrimSpace(t.Name) == "" {
			return DecodedRequest{}, fmt.Errorf("tool name must not be empty")
		}
		if len(t.InputSchema) > 0 {
			var schema map[string]json.RawMessage
			if err := json.Unmarshal(t.InputSchema, &schema); err != nil || schema == nil {
				return DecodedRequest{}, fmt.Errorf("tool %q input_schema must be a JSON object", t.Name)
			}
		}
		chat.Tools = append(chat.Tools, llm.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  normalizeToolSchema(t.InputSchema),
		})
	}
	for _, wm := range wr.Messages {
		if wm.Role != "user" && wm.Role != "assistant" {
			return DecodedRequest{}, fmt.Errorf("invalid message role %q", wm.Role)
		}
		msgs, err := decodeMessage(wm)
		if err != nil {
			return DecodedRequest{}, err
		}
		chat.Messages = append(chat.Messages, msgs...)
	}

	return DecodedRequest{
		Chat:           chat,
		Stream:         wr.Stream,
		RequestedModel: wr.Model,
	}, nil
}

// decodeSystem accepts either a JSON string or an array of {type:text,text}
// blocks, joining block texts with newlines.
func decodeSystem(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []wireBlock
	if err := json.Unmarshal(raw, &blocks); err == nil && blocks != nil {
		var parts []string
		for _, b := range blocks {
			if b.Type != "text" {
				return "", fmt.Errorf("system content block type %q is not supported", b.Type)
			}
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n"), nil
	}
	return "", fmt.Errorf("system must be a string or an array of text blocks")
}

// decodeMessage converts one wire message into one or more llm.Messages.
// A user message containing tool_result blocks expands into one llm.Message
// per tool_result (each a user message with ToolCallID set), matching how
// harness's Anthropic provider reassembles them.
func decodeMessage(wm wireMessage) ([]llm.Message, error) {
	// Simple string content.
	var s string
	if err := json.Unmarshal(wm.Content, &s); err == nil {
		return []llm.Message{{Role: wm.Role, Content: s}}, nil
	}

	var blocks []wireBlock
	if err := json.Unmarshal(wm.Content, &blocks); err != nil {
		return nil, fmt.Errorf("invalid message content: %w", err)
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("message content blocks must not be empty")
	}

	var out []llm.Message
	base := llm.Message{Role: wm.Role}
	var textParts []string
	hasBase := false

	flushBase := func() {
		if hasBase || base.Content != "" || len(base.Images) > 0 || len(base.ToolCalls) > 0 {
			base.Content = strings.Join(textParts, "")
			out = append(out, base)
			base = llm.Message{Role: wm.Role}
			textParts = nil
			hasBase = false
		}
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
			hasBase = true
		case "thinking":
			if wm.Role != "assistant" {
				return nil, fmt.Errorf("thinking blocks are only valid in assistant messages")
			}
			if !appendThinkingBlock(&base, b.Thinking, b.Signature) {
				return nil, fmt.Errorf("thinking blocks require a harness version with thinking-block support")
			}
			hasBase = true
		case "image":
			if wm.Role != "user" {
				return nil, fmt.Errorf("image blocks are only valid in user messages")
			}
			if b.Source == nil || b.Source.Type != "base64" || b.Source.MediaType == "" || b.Source.Data == "" {
				return nil, fmt.Errorf("image source must be non-empty base64 data with a media_type")
			}
			data, err := base64.StdEncoding.DecodeString(b.Source.Data)
			if err != nil {
				return nil, fmt.Errorf("invalid base64 image: %w", err)
			}
			base.Images = append(base.Images, llm.ImageContent{MimeType: b.Source.MediaType, Data: data})
			hasBase = true
		case "tool_use":
			if wm.Role != "assistant" {
				return nil, fmt.Errorf("tool_use blocks are only valid in assistant messages")
			}
			if b.ID == "" || b.Name == "" || !json.Valid(b.Input) {
				return nil, fmt.Errorf("tool_use requires id, name, and valid JSON input")
			}
			var input map[string]json.RawMessage
			if err := json.Unmarshal(b.Input, &input); err != nil || input == nil {
				return nil, fmt.Errorf("tool_use input must be a JSON object")
			}
			base.ToolCalls = append(base.ToolCalls, llm.ToolCall{
				ID:    b.ID,
				Name:  b.Name,
				Input: b.Input,
			})
			hasBase = true
		case "tool_result":
			if wm.Role != "user" {
				return nil, fmt.Errorf("tool_result blocks are only valid in user messages")
			}
			if b.ToolUseID == "" {
				return nil, fmt.Errorf("tool_result requires tool_use_id")
			}
			// Tool results are their own user messages. Flush any pending
			// base first to preserve order.
			flushBase()
			content, err := toolResultText(b.Content)
			if err != nil {
				return nil, err
			}
			out = append(out, llm.Message{
				Role:       "user",
				ToolCallID: b.ToolUseID,
				Content:    content,
				IsError:    b.IsError,
			})
		default:
			return nil, fmt.Errorf("unsupported message content block type %q", b.Type)
		}
	}
	flushBase()
	return out, nil
}

// toolResultText extracts text from a tool_result content field, which may be
// a plain string or an array of text blocks.
func toolResultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []wireBlock
	if err := json.Unmarshal(raw, &blocks); err == nil && blocks != nil {
		var parts []string
		for _, b := range blocks {
			if b.Type != "text" {
				return "", fmt.Errorf("tool_result content block type %q is not supported", b.Type)
			}
			parts = append(parts, b.Text)
		}
		return strings.Join(parts, ""), nil
	}
	return "", fmt.Errorf("tool_result content must be a string or text-block array")
}

// emptyObjectSchema is the minimal valid JSON Schema for a no-argument tool.
var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// normalizeToolSchema ensures a tool's input_schema is a JSON object that
// always carries "type":"object" and a "properties" object. Clients such as
// Claude Code legitimately send no-argument tools as {"type":"object"} with no
// "properties" key (or omit the schema entirely); the Anthropic Messages API
// accepts that, but some Anthropic-compatible backends (e.g. Vertex via a
// proxy) reject a custom tool whose input_schema lacks "properties"
// ("tools.N.custom.input_schema: Field required"). Injecting an empty
// properties object keeps the request valid everywhere without changing the
// tool's meaning. A schema that already has properties is returned unchanged.
func normalizeToolSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return emptyObjectSchema
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		// Not a JSON object (unexpected); leave it to the provider/backend
		// to reject rather than silently rewriting something we don't model.
		return raw
	}
	changed := false
	if _, ok := m["type"]; !ok {
		m["type"] = json.RawMessage(`"object"`)
		changed = true
	}
	if _, ok := m["properties"]; !ok {
		m["properties"] = json.RawMessage(`{}`)
		changed = true
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}
