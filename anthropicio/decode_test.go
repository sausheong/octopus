package anthropicio

import (
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
