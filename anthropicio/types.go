// Package anthropicio is the only package that knows the Anthropic Messages
// API wire format: it decodes inbound requests into harness types and encodes
// harness ChatEvents back into Anthropic SSE / Message JSON.
package anthropicio

import (
	"encoding/json"

	"github.com/sausheong/harness/llm"
)

// DecodedRequest is the result of decoding an inbound /v1/messages body.
// Chat is ready to hand to a provider (Model is left as the inbound model
// and is overwritten by the router after it picks one). Stream selects SSE
// vs buffered response. RequestedModel is kept for logging only.
type DecodedRequest struct {
	Chat           llm.ChatRequest
	Stream         bool
	RequestedModel string
}

// wireRequest mirrors the Anthropic Messages API request body.
type wireRequest struct {
	Model        string            `json:"model"`
	MaxTokens    int               `json:"max_tokens"`
	Stream       bool              `json:"stream"`
	Temperature  *float64          `json:"temperature"`
	System       json.RawMessage   `json:"system"` // string OR []textBlock
	Messages     []wireMessage     `json:"messages"`
	Tools        []wireTool        `json:"tools"`
	CacheControl *wireCacheControl `json:"cache_control"`
	Metadata     wireMetadata      `json:"metadata"`
}

type wireMetadata struct {
	UserID string `json:"user_id"`
}

type wireCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string OR []block
}

type wireTool struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	InputSchema  json.RawMessage   `json:"input_schema"`
	CacheControl *wireCacheControl `json:"cache_control"`
}

type wireBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	Source       *wireImageSrc     `json:"source"`
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Input        json.RawMessage   `json:"input"`
	ToolUseID    string            `json:"tool_use_id"`
	Content      json.RawMessage   `json:"content"` // tool_result: string OR []block
	IsError      bool              `json:"is_error"`
	Thinking     string            `json:"thinking"`
	Signature    string            `json:"signature"`
	CacheControl *wireCacheControl `json:"cache_control"`
}

type wireImageSrc struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}
