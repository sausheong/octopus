package openaiio

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sausheong/harness/llm"
)

// Decode parses an OpenAI Chat Completions request body into harness types.
func Decode(body []byte) (llm.ChatRequest, bool, string, error) {
	var wr wireRequest
	if err := json.Unmarshal(body, &wr); err != nil {
		return llm.ChatRequest{}, false, "", fmt.Errorf("invalid request JSON: %w", err)
	}
	if len(wr.Messages) == 0 {
		return llm.ChatRequest{}, false, "", fmt.Errorf("messages must not be empty")
	}

	chat := llm.ChatRequest{Model: wr.Model}
	if wr.MaxTokens != nil {
		chat.MaxTokens = *wr.MaxTokens
	}
	if wr.Temperature != nil {
		chat.Temperature = *wr.Temperature
	}
	for _, t := range wr.Tools {
		if t.Type != "function" {
			continue
		}
		chat.Tools = append(chat.Tools, llm.ToolDef{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  normalizeSchema(t.Function.Parameters),
		})
	}

	for _, wm := range wr.Messages {
		msgs, err := decodeMessage(wm)
		if err != nil {
			return llm.ChatRequest{}, false, "", err
		}
		chat.Messages = append(chat.Messages, msgs...)
	}

	return chat, wr.Stream, wr.Model, nil
}

// decodeMessage converts one OpenAI wire message into one or more llm.Messages.
func decodeMessage(wm wireMessage) ([]llm.Message, error) {
	// tool response
	if wm.Role == "tool" {
		var text string
		var s string
		if err := json.Unmarshal(wm.Content, &s); err == nil {
			text = s
		}
		return []llm.Message{{
			Role:       "user",
			ToolCallID: wm.ToolCallID,
			Content:    text,
		}}, nil
	}

	// system → SystemPrompt is handled by the caller scanning for system messages
	// below; we return it as a "system" role so the caller can extract it.

	msg := llm.Message{Role: wm.Role}

	// assistant messages may carry tool_calls
	if len(wm.ToolCalls) > 0 {
		for _, tc := range wm.ToolCalls {
			input := json.RawMessage(tc.Function.Arguments)
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
	}

	// content: string or array of parts
	if len(wm.Content) == 0 || string(wm.Content) == "null" {
		return []llm.Message{msg}, nil
	}

	var s string
	if err := json.Unmarshal(wm.Content, &s); err == nil {
		msg.Content = s
		return []llm.Message{msg}, nil
	}

	var parts []contentPart
	if err := json.Unmarshal(wm.Content, &parts); err != nil {
		return nil, fmt.Errorf("invalid message content: %w", err)
	}
	var textParts []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			textParts = append(textParts, p.Text)
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			img, err := decodeImageURL(p.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			msg.Images = append(msg.Images, img)
		}
	}
	msg.Content = strings.Join(textParts, "")
	return []llm.Message{msg}, nil
}

// decodeImageURL handles both data URIs and plain URLs.
// harness ImageContent only supports base64 inline data, so plain URLs are
// fetched; for now we just store the URL as a data:text/uri-list placeholder
// and rely on the provider to handle it.
func decodeImageURL(url string) (llm.ImageContent, error) {
	if strings.HasPrefix(url, "data:") {
		// data:<mediatype>;base64,<data>
		rest := url[5:]
		semi := strings.IndexByte(rest, ';')
		if semi < 0 {
			return llm.ImageContent{}, fmt.Errorf("malformed data URI")
		}
		mediaType := rest[:semi]
		rest = rest[semi+1:]
		if !strings.HasPrefix(rest, "base64,") {
			return llm.ImageContent{}, fmt.Errorf("only base64 data URIs supported")
		}
		data, err := base64.StdEncoding.DecodeString(rest[7:])
		if err != nil {
			return llm.ImageContent{}, fmt.Errorf("invalid base64 in data URI: %w", err)
		}
		return llm.ImageContent{MimeType: mediaType, Data: data}, nil
	}
	// Plain URL — not supported inline; return as empty image with URL hint.
	// Most providers handle image URLs natively, but harness ImageContent is
	// bytes-only. Callers that need URL images should use the Anthropic endpoint.
	return llm.ImageContent{}, fmt.Errorf("image URLs (non-data-URI) not supported; use base64 data URIs")
}

// emptyObjectSchema is the minimal valid JSON Schema.
var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

func normalizeSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return emptyObjectSchema
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
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

// ExtractSystem pulls system-role messages out of the slice and returns the
// system prompt string and the remaining messages. OpenAI puts the system
// prompt as the first message with role "system"; harness uses a dedicated
// SystemPrompt field.
func ExtractSystem(msgs []llm.Message) (string, []llm.Message) {
	var systemParts []string
	var rest []llm.Message
	for _, m := range msgs {
		if m.Role == "system" {
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
		} else {
			rest = append(rest, m)
		}
	}
	return strings.Join(systemParts, "\n"), rest
}
