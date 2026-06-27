package router

import (
	"context"
	"testing"

	"github.com/sausheong/harness/llm"
)

// fakeProvider returns a scripted stream for ChatStream.
type fakeProvider struct {
	text    string
	failNew bool
}

func (f *fakeProvider) Models() []llm.ModelInfo { return nil }
func (f *fakeProvider) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (f *fakeProvider) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	if f.failNew {
		return nil, errString2("boom")
	}
	ch := make(chan llm.ChatEvent, 2)
	ch <- llm.ChatEvent{Type: llm.EventTextDelta, Text: f.text}
	ch <- llm.ChatEvent{Type: llm.EventDone}
	close(ch)
	return ch, nil
}

type errString2 string

func (e errString2) Error() string { return string(e) }

func TestClassifyParsesJSON(t *testing.T) {
	p := &fakeProvider{text: `{"difficulty":"high","needs_reasoning":true,"needs_vision":false,"needs_tools":false,"est_tokens_in":1200,"est_tokens_out":800,"domain":"code"}`}
	prof := Classify(context.Background(), p, "anthropic/haiku", 256, llm.Message{Role: "user", Content: "refactor this"})
	if prof.Difficulty != "high" || !prof.NeedsReasoning || prof.Domain != "code" {
		t.Fatalf("profile = %+v", prof)
	}
	if prof.EstTokensIn != 1200 || prof.EstTokensOut != 800 {
		t.Errorf("tokens = %d/%d", prof.EstTokensIn, prof.EstTokensOut)
	}
}

func TestClassifyJSONWithPreamble(t *testing.T) {
	// Model wraps JSON in prose; extractor should still find the object.
	p := &fakeProvider{text: "Here is the classification:\n{\"difficulty\":\"low\",\"domain\":\"qa\"}\nDone."}
	prof := Classify(context.Background(), p, "m", 256, llm.Message{Role: "user", Content: "hi"})
	if prof.Difficulty != "low" || prof.Domain != "qa" {
		t.Fatalf("profile = %+v", prof)
	}
}

func TestClassifyFallbackOnProviderError(t *testing.T) {
	p := &fakeProvider{failNew: true}
	prof := Classify(context.Background(), p, "m", 256, llm.Message{Role: "user", Content: "hi"})
	def := DefaultProfile()
	if prof != def {
		t.Fatalf("profile = %+v, want default %+v", prof, def)
	}
}

func TestClassifyFallbackOnGarbage(t *testing.T) {
	p := &fakeProvider{text: "no json here at all"}
	prof := Classify(context.Background(), p, "m", 256, llm.Message{Role: "user", Content: "hi"})
	if prof != DefaultProfile() {
		t.Fatalf("profile = %+v, want default", prof)
	}
}
