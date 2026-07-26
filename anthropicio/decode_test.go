package anthropicio

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeStringContent(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet","max_tokens":1024,"stream":true,
		"system":"be brief",
		"messages":[{"role":"user","content":"hello"}]
	}`)
	dr, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !dr.Stream {
		t.Error("Stream should be true")
	}
	if dr.RequestedModel != "claude-sonnet" {
		t.Errorf("RequestedModel = %q", dr.RequestedModel)
	}
	if dr.Chat.SystemPrompt != "be brief" {
		t.Errorf("SystemPrompt = %q", dr.Chat.SystemPrompt)
	}
	if dr.Chat.MaxTokens != 1024 {
		t.Errorf("MaxTokens = %d", dr.Chat.MaxTokens)
	}
	if len(dr.Chat.Messages) != 1 || dr.Chat.Messages[0].Content != "hello" {
		t.Fatalf("Messages = %+v", dr.Chat.Messages)
	}
}

func TestDecodeSystemArray(t *testing.T) {
	body := []byte(`{
		"model":"m","max_tokens":1,
		"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],
		"messages":[{"role":"user","content":"x"}]
	}`)
	dr, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if dr.Chat.SystemPrompt != "a\nb" {
		t.Errorf("SystemPrompt = %q, want \"a\\nb\"", dr.Chat.SystemPrompt)
	}
}

func TestDecodeSystemMessageCompatibility(t *testing.T) {
	body := []byte(`{
		"model":"octopus","max_tokens":1,
		"system":"top-level",
		"messages":[
			{"role":"system","content":"client compatibility prompt"},
			{"role":"user","content":"hello"}
		]
	}`)
	dr, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if dr.Chat.SystemPrompt != "top-level\nclient compatibility prompt" {
		t.Fatalf("SystemPrompt = %q", dr.Chat.SystemPrompt)
	}
	if len(dr.Chat.SystemPromptParts) != 0 {
		t.Fatalf("SystemPromptParts = %+v, want unstructured prompt", dr.Chat.SystemPromptParts)
	}
	if len(dr.Chat.Messages) != 1 || dr.Chat.Messages[0].Role != "user" {
		t.Fatalf("Messages = %+v", dr.Chat.Messages)
	}
}

func TestDecodeStructuredSystemMessagePreservesCacheControl(t *testing.T) {
	body := []byte(`{
		"model":"octopus","max_tokens":1,
		"system":"top-level",
		"messages":[
			{"role":"system","content":[
				{"type":"text","text":"cached compatibility prompt","cache_control":{"type":"ephemeral","ttl":"1h"}}
			]},
			{"role":"user","content":"hello"}
		]
	}`)
	dr, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if dr.Chat.SystemPrompt != "top-level\ncached compatibility prompt" {
		t.Fatalf("SystemPrompt = %q", dr.Chat.SystemPrompt)
	}
	if len(dr.Chat.SystemPromptParts) != 2 {
		t.Fatalf("SystemPromptParts = %+v", dr.Chat.SystemPromptParts)
	}
	if dr.Chat.SystemPromptParts[1].CacheControl == nil || dr.Chat.SystemPromptParts[1].CacheControl.TTL != "1h" {
		t.Fatalf("cached SystemPromptPart = %+v", dr.Chat.SystemPromptParts[1])
	}
}

func TestDecodeRejectsSystemOnlyMessages(t *testing.T) {
	body := []byte(`{"model":"octopus","max_tokens":1,"messages":[{"role":"system","content":"only"}]}`)
	if _, err := Decode(body); err == nil || !strings.Contains(err.Error(), "user or assistant") {
		t.Fatalf("Decode error = %v", err)
	}
}

func TestDecodePreservesPromptCacheControls(t *testing.T) {
	body := []byte(`{
		"model":"m","max_tokens":10,
		"metadata":{"user_id":"claude-session-1"},
		"cache_control":{"type":"ephemeral","ttl":"1h"},
		"system":[
			{"type":"text","text":"stable","cache_control":{"type":"ephemeral","ttl":"1h"}},
			{"type":"text","text":"dynamic"}
		],
		"tools":[{"name":"get","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[
			{"type":"text","text":"prefix","cache_control":{"type":"ephemeral","ttl":"5m"}},
			{"type":"text","text":"suffix"}
		]}]
	}`)
	dr, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if dr.Chat.SessionID != "user:claude-session-1" {
		t.Fatalf("SessionID = %q", dr.Chat.SessionID)
	}
	if dr.Chat.CacheControl == nil || dr.Chat.CacheControl.TTL != "1h" {
		t.Fatalf("top-level CacheControl = %+v", dr.Chat.CacheControl)
	}
	if len(dr.Chat.SystemPromptParts) != 2 || dr.Chat.SystemPromptParts[0].CacheControl == nil || dr.Chat.SystemPromptParts[0].CacheControl.TTL != "1h" {
		t.Fatalf("SystemPromptParts = %+v", dr.Chat.SystemPromptParts)
	}
	if dr.Chat.Tools[0].CacheControl == nil || dr.Chat.Tools[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("tool CacheControl = %+v", dr.Chat.Tools[0].CacheControl)
	}
	if len(dr.Chat.Messages) != 2 || dr.Chat.Messages[0].Content != "prefix" || dr.Chat.Messages[0].CacheControl == nil || dr.Chat.Messages[1].Content != "suffix" {
		t.Fatalf("Messages = %+v", dr.Chat.Messages)
	}
}

func TestDecodeRejectsInvalidCacheControl(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":1,"cache_control":{"type":"persistent"},"messages":[{"role":"user","content":"x"}]}`)
	if _, err := Decode(body); err == nil || !strings.Contains(err.Error(), "cache_control") {
		t.Fatalf("Decode error = %v, want invalid cache_control", err)
	}
}

func TestDecodeImageBlock(t *testing.T) {
	// "aGk=" is base64 for "hi".
	body := []byte(`{
		"model":"m","max_tokens":1,
		"messages":[{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}
		]}]
	}`)
	dr, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	m := dr.Chat.Messages[0]
	if m.Content != "look" {
		t.Errorf("Content = %q", m.Content)
	}
	if len(m.Images) != 1 || m.Images[0].MimeType != "image/png" {
		t.Fatalf("Images = %+v", m.Images)
	}
	if string(m.Images[0].Data) != "hi" {
		t.Errorf("image data = %q, want \"hi\"", string(m.Images[0].Data))
	}
}

func TestDecodeToolUseAndResult(t *testing.T) {
	body := []byte(`{
		"model":"m","max_tokens":1,
		"tools":[{"name":"get","description":"d","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":"do it"},
			{"role":"assistant","content":[
				{"type":"text","text":"calling"},
				{"type":"tool_use","id":"toolu_1","name":"get","input":{"q":"x"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"42","is_error":false}
			]}
		]
	}`)
	dr, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(dr.Chat.Tools) != 1 || dr.Chat.Tools[0].Name != "get" {
		t.Fatalf("Tools = %+v", dr.Chat.Tools)
	}
	// messages: user, assistant(with tool call), user(tool result)
	if len(dr.Chat.Messages) != 3 {
		t.Fatalf("len(Messages) = %d, want 3", len(dr.Chat.Messages))
	}
	asst := dr.Chat.Messages[1]
	if asst.Role != "assistant" || asst.Content != "calling" {
		t.Errorf("assistant = %+v", asst)
	}
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "toolu_1" || asst.ToolCalls[0].Name != "get" {
		t.Fatalf("ToolCalls = %+v", asst.ToolCalls)
	}
	tr := dr.Chat.Messages[2]
	if tr.Role != "user" || tr.ToolCallID != "toolu_1" || tr.Content != "42" {
		t.Errorf("tool_result message = %+v", tr)
	}
}

func TestDecodeToolResultWithImage(t *testing.T) {
	body := []byte(`{
		"model":"m","max_tokens":1,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"screenshot","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}},
				{"type":"text","text":"captured"}
			]}]}
		]
	}`)
	dr, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	result := dr.Chat.Messages[1]
	if result.Content != "captured" || len(result.Images) != 1 {
		t.Fatalf("tool result = %+v", result)
	}
	if result.Images[0].MimeType != "image/png" || string(result.Images[0].Data) != "hi" {
		t.Fatalf("tool result image = %+v", result.Images[0])
	}
}

func TestDecodeInvalidJSON(t *testing.T) {
	if _, err := Decode([]byte("{not json")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestDecodeNoMessages(t *testing.T) {
	if _, err := Decode([]byte(`{"model":"m","max_tokens":1,"messages":[]}`)); err == nil {
		t.Fatal("expected error for empty messages")
	}
}

func TestDecodeRejectsInvalidRequests(t *testing.T) {
	tests := []string{
		`{"max_tokens":0,"messages":[{"role":"user","content":"x"}]}`,
		`{"max_tokens":1,"temperature":1.1,"messages":[{"role":"user","content":"x"}]}`,
		`{"max_tokens":1,"messages":[{"role":"developer","content":"x"}]}`,
		`{"max_tokens":1,"system":{"bad":true},"messages":[{"role":"user","content":"x"}]}`,
		`{"max_tokens":1,"tools":[{"name":"","input_schema":{}}],"messages":[{"role":"user","content":"x"}]}`,
		`{"max_tokens":1,"messages":[{"role":"user","content":[{"type":"audio"}]}]}`,
	}
	for _, body := range tests {
		if _, err := Decode([]byte(body)); err == nil {
			t.Errorf("Decode accepted invalid request: %s", body)
		}
	}
}

// hasProperties reports whether a tool's decoded Parameters JSON contains a
// "properties" object (the field some backends require on every tool schema).
func hasProperties(t *testing.T, raw json.RawMessage) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("tool Parameters is not a JSON object: %s", raw)
	}
	_, ok := m["properties"]
	return ok
}

func TestDecodeToolSchemaInjectsProperties(t *testing.T) {
	// A no-arg tool sent as {"type":"object"} (no properties) — Claude Code
	// does this for several built-in tools. The decoder must inject an empty
	// properties object so backends that require it accept the request.
	body := []byte(`{
		"model":"m","max_tokens":1,
		"tools":[{"name":"noargs","description":"d","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	dr, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(dr.Chat.Tools) != 1 {
		t.Fatalf("len(Tools) = %d", len(dr.Chat.Tools))
	}
	if !hasProperties(t, dr.Chat.Tools[0].Parameters) {
		t.Errorf("expected injected properties, got: %s", dr.Chat.Tools[0].Parameters)
	}
}

func TestDecodeToolSchemaPreservesExistingProperties(t *testing.T) {
	// A tool that already declares properties must pass through unchanged.
	body := []byte(`{
		"model":"m","max_tokens":1,
		"tools":[{"name":"getw","description":"d","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	dr, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	p := string(dr.Chat.Tools[0].Parameters)
	if !strings.Contains(p, `"city"`) || !strings.Contains(p, `"required"`) {
		t.Errorf("existing schema not preserved: %s", p)
	}
}

func TestNormalizeToolSchemaEmpty(t *testing.T) {
	// Missing/empty input_schema yields a valid empty-object schema.
	got := normalizeToolSchema(nil)
	if !hasProperties(t, got) {
		t.Errorf("empty schema not normalized: %s", got)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(got, &m)
	if string(m["type"]) != `"object"` {
		t.Errorf("expected type object, got: %s", got)
	}
}

func TestDecodeTagsUserIDAsNonExplicitSession(t *testing.T) {
	dr, err := Decode([]byte(`{"model":"m","max_tokens":10,
		"metadata":{"user_id":"alice"},
		"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dr.Chat.SessionID != "user:alice" {
		t.Errorf("SessionID = %q, want %q", dr.Chat.SessionID, "user:alice")
	}
}

func TestDecodeLeavesSessionEmptyWithoutUserID(t *testing.T) {
	dr, err := Decode([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dr.Chat.SessionID != "" {
		t.Errorf("SessionID = %q, want empty", dr.Chat.SessionID)
	}
}
