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
	profile, _ := classifyWithUsage(ctx, prov, model, maxTokens, turn)
	return profile
}

func classifyWithUsage(ctx context.Context, prov llm.LLMProvider, model string, maxTokens int, turn llm.Message) (TaskProfile, *llm.Usage) {
	req := llm.ChatRequest{
		Model:        model,
		MaxTokens:    maxTokens,
		SystemPrompt: classifierSystemPrompt,
		Messages:     []llm.Message{{Role: "user", Content: turn.Content}},
	}
	ch, err := prov.ChatStream(ctx, req)
	if err != nil {
		return DefaultProfile(), nil
	}
	var sb strings.Builder
	var usage *llm.Usage
	for ev := range ch {
		switch ev.Type {
		case llm.EventTextDelta:
			sb.WriteString(ev.Text)
		case llm.EventDone:
			usage = ev.Usage
		case llm.EventError:
			return DefaultProfile(), usage
		}
	}
	prof, ok := parseProfile(sb.String())
	if !ok {
		return DefaultProfile(), usage
	}
	return prof, usage
}

// rawProfile is the wire type for classifier JSON — pointer fields let us
// distinguish missing keys from zero/false values.
type rawProfile struct {
	Difficulty     *string `json:"difficulty"`
	NeedsReasoning *bool   `json:"needs_reasoning"`
	NeedsVision    *bool   `json:"needs_vision"`
	NeedsTools     *bool   `json:"needs_tools"`
	EstTokensIn    *int    `json:"est_tokens_in"`
	EstTokensOut   *int    `json:"est_tokens_out"`
	Domain         *string `json:"domain"`
}

// parseProfile extracts the first JSON object from s, validates required
// fields are present, and clamps all values. Returns ok=false — causing
// the caller to use DefaultProfile — if the JSON is missing, malformed,
// or omits any required field.
func parseProfile(s string) (TaskProfile, bool) {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return TaskProfile{}, false
	}
	var raw rawProfile
	if err := json.Unmarshal([]byte(s[start:end+1]), &raw); err != nil {
		return TaskProfile{}, false
	}

	// All fields are required; a missing field means the classifier produced
	// an incomplete response — treat it as a failure and use DefaultProfile.
	if raw.Difficulty == nil || raw.NeedsReasoning == nil || raw.NeedsVision == nil ||
		raw.NeedsTools == nil || raw.EstTokensIn == nil || raw.EstTokensOut == nil ||
		raw.Domain == nil {
		return TaskProfile{}, false
	}

	p := TaskProfile{
		NeedsReasoning: *raw.NeedsReasoning,
		NeedsVision:    *raw.NeedsVision,
		NeedsTools:     *raw.NeedsTools,
		EstTokensIn:    *raw.EstTokensIn,
		EstTokensOut:   *raw.EstTokensOut,
	}

	// Clamp difficulty to known values; unknown → conservative "high".
	switch *raw.Difficulty {
	case "trivial", "low", "medium", "high":
		p.Difficulty = *raw.Difficulty
	default:
		p.Difficulty = "high"
	}

	// Clamp domain to known values.
	switch *raw.Domain {
	case "code", "writing", "qa", "math", "other":
		p.Domain = *raw.Domain
	default:
		p.Domain = "other"
	}

	// Reject negative token estimates — they would bypass context filtering.
	// Cap at 1M to guard against absurd values.
	if p.EstTokensIn < 0 || p.EstTokensOut < 0 {
		return TaskProfile{}, false
	}
	if p.EstTokensIn > 1_000_000 {
		p.EstTokensIn = 1_000_000
	}
	if p.EstTokensOut > 1_000_000 {
		p.EstTokensOut = 1_000_000
	}

	return p, true
}
