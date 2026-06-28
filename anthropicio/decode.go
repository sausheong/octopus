package anthropicio

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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

	chat := llm.ChatRequest{
		Model:        wr.Model,
		MaxTokens:    wr.MaxTokens,
		SystemPrompt: decodeSystem(wr.System),
	}
	if wr.Temperature != nil {
		chat.Temperature = *wr.Temperature
	}
	for _, t := range wr.Tools {
		chat.Tools = append(chat.Tools, llm.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  normalizeToolSchema(t.InputSchema),
		})
	}
	for _, wm := range wr.Messages {
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
func decodeSystem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []wireBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
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
		case "image":
			if b.Source != nil && b.Source.Type == "base64" {
				data, err := base64.StdEncoding.DecodeString(b.Source.Data)
				if err != nil {
					return nil, fmt.Errorf("invalid base64 image: %w", err)
				}
				base.Images = append(base.Images, llm.ImageContent{
					MimeType: b.Source.MediaType,
					Data:     data,
				})
				hasBase = true
			}
		case "tool_use":
			base.ToolCalls = append(base.ToolCalls, llm.ToolCall{
				ID:    b.ID,
				Name:  b.Name,
				Input: b.Input,
			})
			hasBase = true
		case "tool_result":
			// Tool results are their own user messages. Flush any pending
			// base first to preserve order.
			flushBase()
			out = append(out, llm.Message{
				Role:       "user",
				ToolCallID: b.ToolUseID,
				Content:    toolResultText(b.Content),
				IsError:    b.IsError,
			})
		}
	}
	flushBase()
	return out, nil
}

// toolResultText extracts text from a tool_result content field, which may be
// a plain string or an array of text blocks.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []wireBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "")
	}
	return ""
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
