// Package openaiio is the only package that knows the OpenAI Chat Completions
// API wire format: it decodes inbound requests into harness types and encodes
// harness ChatEvents back into OpenAI completion JSON / SSE chunks.
package openaiio

import "encoding/json"

// wireRequest mirrors the OpenAI Chat Completions API request body.
type wireRequest struct {
	Model       string        `json:"model"`
	MaxTokens   *int          `json:"max_tokens"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature"`
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools"`
}

type wireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // string OR []contentPart
	ToolCalls  []wireToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

type contentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL *imageURL       `json:"image_url"`
}

type imageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail"`
}

type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// wireToolCallFunction is the function field inside a tool_call in a message
// (assistant turn): it carries name + arguments (a JSON string), not parameters.
type wireToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireToolCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function wireToolCallFunction `json:"function"`
}
