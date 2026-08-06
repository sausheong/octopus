package router

import (
	"strings"
	"testing"

	"github.com/sausheong/harness/llm"
)

func TestBackgroundDetectorRequiresExactAllowlistedSignature(t *testing.T) {
	detector, err := NewBackgroundDetector([]BackgroundSignature{
		ExactBackgroundSignature("claude-code-status", "/v1/messages", "known status probe"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// This has the tempting heuristic shape, but is not allowlisted.
	unknown := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "ping"}}}
	if _, ok := detector.Detect(unknown, RequestMetadata{Endpoint: "/v1/messages", Stream: false}); ok {
		t.Fatal("unknown tiny non-streaming request matched as background")
	}

	known := llm.ChatRequest{Messages: []llm.Message{
		{Role: "user", Content: "original task"},
		{Role: "assistant", Content: "working"},
		{Role: "user", Content: "known status probe"},
	}}
	match, ok := detector.Detect(known, RequestMetadata{Endpoint: "/v1/messages", Stream: false})
	if !ok || match.Name != "claude-code-status" || !match.ConversationIndependent {
		t.Fatalf("match = %+v, %v", match, ok)
	}
}

func TestBackgroundDetectorRejectsUnsafeShapeChanges(t *testing.T) {
	sig := ExactBackgroundSignature("probe", "/v1/messages", "status")
	detector, err := NewBackgroundDetector([]BackgroundSignature{sig})
	if err != nil {
		t.Fatal(err)
	}
	base := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "status"}}}

	tests := []struct {
		name string
		chat llm.ChatRequest
		meta RequestMetadata
	}{
		{"streaming", base, RequestMetadata{Endpoint: "/v1/messages", Stream: true}},
		{"wrong endpoint", base, RequestMetadata{Endpoint: "/v1/chat/completions"}},
		{"tool definition", llm.ChatRequest{Messages: base.Messages, Tools: []llm.ToolDef{{Name: "read"}}}, RequestMetadata{Endpoint: "/v1/messages"}},
		{"tool result", llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "status", ToolCallID: "tool-1"}}}, RequestMetadata{Endpoint: "/v1/messages"}},
		{"image", llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "status", Images: []llm.ImageContent{{MimeType: "image/png"}}}}}, RequestMetadata{Endpoint: "/v1/messages"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := detector.Detect(tc.chat, tc.meta); ok {
				t.Fatal("unsafe shape matched as background")
			}
		})
	}
}

func TestBackgroundSessionIDCannotAliasMainSession(t *testing.T) {
	chat := llm.ChatRequest{SessionID: "conversation-1", Messages: []llm.Message{{Role: "user", Content: "status"}}}
	match := BackgroundMatch{Name: "probe", ConversationIndependent: true}
	background := BackgroundSessionID(chat, RequestMetadata{WorkflowID: "workflow-1"}, match)
	if background == SessionID(chat) || !strings.HasPrefix(background, "background:") {
		t.Fatalf("background ID %q aliases main ID %q", background, SessionID(chat))
	}
	other := BackgroundSessionID(chat, RequestMetadata{WorkflowID: "workflow-2"}, match)
	if background == other {
		t.Fatal("different workflows shared background routing state")
	}
}

func TestBackgroundMatchIsolationDoesNotMutateOriginalRequest(t *testing.T) {
	chat := llm.ChatRequest{
		SessionID: "conversation-1", SystemPrompt: "large cached prompt",
		CacheControl: &llm.CacheControl{Type: "ephemeral"},
		Messages:     []llm.Message{{Role: "user", Content: "old"}, {Role: "assistant", Content: "answer"}, {Role: "user", Content: "status"}},
	}
	match := BackgroundMatch{Name: "probe", ConversationIndependent: true}
	isolated := match.Isolate(chat, RequestMetadata{WorkflowID: "workflow-1"})
	if chat.SessionID != "conversation-1" {
		t.Fatalf("original session mutated to %q", chat.SessionID)
	}
	if isolated.SessionID == chat.SessionID || !strings.HasPrefix(isolated.SessionID, "background:") {
		t.Fatalf("isolated session = %q, main = %q", isolated.SessionID, chat.SessionID)
	}
	if len(isolated.Messages) != 1 || isolated.Messages[0].Content != "status" || isolated.SystemPrompt != "" || isolated.CacheControl != nil {
		t.Fatalf("conversation-independent request retained main history/cache: %+v", isolated)
	}
	if len(chat.Messages) != 3 || chat.SystemPrompt == "" || chat.CacheControl == nil {
		t.Fatalf("original request content was mutated: %+v", chat)
	}
}

func TestNewBackgroundDetectorRejectsInvalidAndDuplicateSignatures(t *testing.T) {
	if _, err := NewBackgroundDetector([]BackgroundSignature{{Name: "bad", Endpoint: "/v1/messages", LastUserSHA256: "nope"}}); err == nil {
		t.Fatal("invalid digest accepted")
	}
	sig := ExactBackgroundSignature("one", "/v1/messages", "same")
	duplicate := sig
	duplicate.Name = "two"
	if _, err := NewBackgroundDetector([]BackgroundSignature{sig, duplicate}); err == nil {
		t.Fatal("duplicate signature accepted")
	}
}
