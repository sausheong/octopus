package router

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/sausheong/harness/llm"
)

const classifierSystemPrompt = `You are a request classifier for an LLM router.
Read the user's request and respond with ONLY a JSON object (no prose, no code fences) with these fields:
- "difficulty": one of "trivial","low","medium","high"
- "needs_reasoning": boolean (multi-step logic, math, planning, hard debugging)
- "needs_vision": boolean (the request depends on understanding images)
- "needs_tools": boolean (the request expects tool/function calling)
- "est_tokens_in": integer estimate of input size in tokens
- "est_tokens_out": integer estimate of expected output size in tokens
- "domain": one of "code","writing","qa","math","other"
Respond with the JSON object only.`

// Classify runs the fixed classifier model on the given user turn and returns
// the parsed TaskProfile. Any failure (provider error, empty output,
// unparseable JSON) yields DefaultProfile() — a classifier hiccup must never
// break routing, only make it conservative.
func Classify(ctx context.Context, prov llm.LLMProvider, model string, maxTokens int, turn llm.Message) TaskProfile {
	req := llm.ChatRequest{
		Model:        model,
		MaxTokens:    maxTokens,
		SystemPrompt: classifierSystemPrompt,
		Messages:     []llm.Message{{Role: "user", Content: turn.Content}},
	}
	ch, err := prov.ChatStream(ctx, req)
	if err != nil {
		return DefaultProfile()
	}
	var sb strings.Builder
	for ev := range ch {
		switch ev.Type {
		case llm.EventTextDelta:
			sb.WriteString(ev.Text)
		case llm.EventError:
			return DefaultProfile()
		}
	}
	prof, ok := parseProfile(sb.String())
	if !ok {
		return DefaultProfile()
	}
	return prof
}

// parseProfile extracts the first JSON object from s and unmarshals it into a
// TaskProfile. Returns ok=false if no object is found or it doesn't parse.
func parseProfile(s string) (TaskProfile, bool) {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return TaskProfile{}, false
	}
	var p TaskProfile
	if err := json.Unmarshal([]byte(s[start:end+1]), &p); err != nil {
		return TaskProfile{}, false
	}
	return p, true
}
