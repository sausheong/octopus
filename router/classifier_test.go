package router

import (
	"context"
	"strings"
	"testing"

	"github.com/sausheong/harness/llm"
)

func isolateClassifierCache(t *testing.T) {
	t.Helper()
	processClassifierCache.reset()
	t.Cleanup(processClassifierCache.reset)
}

// fakeProvider returns a scripted stream for ChatStream.
type fakeProvider struct {
	text    string
	failNew bool
	usage   *llm.Usage
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
	ch <- llm.ChatEvent{Type: llm.EventDone, Usage: f.usage}
	close(ch)
	return ch, nil
}

func TestClassifyWithUsageReturnsClassifierOverhead(t *testing.T) {
	isolateClassifierCache(t)
	want := &llm.Usage{InputTokens: 120, OutputTokens: 18}
	p := &fakeProvider{
		text:  `{"difficulty":"low","risk":"ordinary","needs_reasoning":false,"needs_vision":false,"needs_tools":false,"est_tokens_in":50,"est_tokens_out":100,"domain":"qa","expected_remaining_turns":3,"estimate_confidence":0.8,"classification_confidence":0.9}`,
		usage: want,
	}
	profile, got := classifyWithUsage(context.Background(), p, "m", 256, llm.Message{Role: "user", Content: "hi"})
	if profile.Difficulty != "low" || got != want {
		t.Fatalf("profile=%+v usage=%+v", profile, got)
	}
}

func TestClassifyDetailedReportsCacheHit(t *testing.T) {
	isolateClassifierCache(t)
	p := &fakeProvider{text: `{"difficulty":"low","risk":"ordinary","needs_reasoning":false,"needs_vision":false,"needs_tools":false,"est_tokens_in":50,"est_tokens_out":100,"domain":"qa","expected_remaining_turns":3,"estimate_confidence":0.8,"classification_confidence":0.9}`}
	turn := llm.Message{Role: "user", Content: "cache me"}
	_, _, firstSource := classifyWithUsageIdentityDetailed(context.Background(), p, "m", "provider/m", 256, turn)
	_, usage, secondSource := classifyWithUsageIdentityDetailed(context.Background(), p, "m", "provider/m", 256, turn)
	if firstSource != "classifier" || secondSource != "classifier_cache" || usage != nil {
		t.Fatalf("sources = %q/%q usage=%+v", firstSource, secondSource, usage)
	}
}

type errString2 string

func (e errString2) Error() string { return string(e) }

func TestClassifyParsesJSON(t *testing.T) {
	isolateClassifierCache(t)
	p := &fakeProvider{text: `{"difficulty":"high","risk":"critical","needs_reasoning":true,"needs_vision":false,"needs_tools":false,"est_tokens_in":1200,"est_tokens_out":800,"domain":"code","expected_remaining_turns":6,"estimate_confidence":0.9,"classification_confidence":0.8}`}
	prof := Classify(context.Background(), p, "anthropic/haiku", 256, llm.Message{Role: "user", Content: "refactor this"})
	if prof.Difficulty != "high" || !prof.NeedsReasoning || prof.Domain != "code" {
		t.Fatalf("profile = %+v", prof)
	}
	if prof.EstTokensIn != 1200 || prof.EstTokensOut != 800 {
		t.Errorf("tokens = %d/%d", prof.EstTokensIn, prof.EstTokensOut)
	}
	if prof.ExpectedRemainingTurns != 6 || prof.EstimateConfidence != 0.9 {
		t.Errorf("forecast = %d/%v", prof.ExpectedRemainingTurns, prof.EstimateConfidence)
	}
	if prof.Risk != "critical" || prof.ClassificationConfidence != 0.8 || prof.ClassificationSource != "classifier" {
		t.Errorf("semantic safety fields = %+v", prof)
	}
}

func TestClassifierRubricTreatsShortHardRequestsConservatively(t *testing.T) {
	for _, phrase := range []string{"A short prompt is not necessarily easy", "fix this production race", "choose the higher one"} {
		if !strings.Contains(classifierSystemPrompt, phrase) {
			t.Fatalf("classifier rubric missing %q", phrase)
		}
	}
}

func TestClassifyJSONWithPreamble(t *testing.T) {
	isolateClassifierCache(t)
	// Model wraps complete JSON in prose; extractor should find the object.
	full := `{"difficulty":"low","risk":"ordinary","needs_reasoning":false,"needs_vision":false,"needs_tools":false,"est_tokens_in":50,"est_tokens_out":100,"domain":"qa","expected_remaining_turns":2,"estimate_confidence":0.7,"classification_confidence":0.85}`
	p := &fakeProvider{text: "Here is the classification:\n" + full + "\nDone."}
	prof := Classify(context.Background(), p, "m", 256, llm.Message{Role: "user", Content: "hi"})
	if prof.Difficulty != "low" || prof.Domain != "qa" {
		t.Fatalf("profile = %+v", prof)
	}
}

func TestClassifyIncompleteJSONFallsBack(t *testing.T) {
	isolateClassifierCache(t)
	// Partial JSON (missing required fields) must fall back to DefaultProfile.
	p := &fakeProvider{text: `{"difficulty":"low","domain":"qa"}`}
	prof := Classify(context.Background(), p, "m", 256, llm.Message{Role: "user", Content: "hi"})
	if prof != DefaultProfile() {
		t.Fatalf("profile = %+v, want default", prof)
	}
}

func TestClassifyFallbackOnProviderError(t *testing.T) {
	isolateClassifierCache(t)
	p := &fakeProvider{failNew: true}
	prof := Classify(context.Background(), p, "m", 256, llm.Message{Role: "user", Content: "hi"})
	def := DefaultProfile()
	if prof != def {
		t.Fatalf("profile = %+v, want default %+v", prof, def)
	}
}

func TestClassifyFallbackOnGarbage(t *testing.T) {
	isolateClassifierCache(t)
	p := &fakeProvider{text: "no json here at all"}
	prof := Classify(context.Background(), p, "m", 256, llm.Message{Role: "user", Content: "hi"})
	if prof != DefaultProfile() {
		t.Fatalf("profile = %+v, want default", prof)
	}
}
