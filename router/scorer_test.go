package router

import (
	"testing"

	"github.com/sausheong/llmrouter/config"
)

func cat() []config.CatalogEntry {
	return []config.CatalogEntry{
		{ID: "anthropic/opus", Quality: 0.98, CostPerMTokIn: 15, CostPerMTokOut: 75, Speed: 0.4,
			Caps: config.Caps{Tools: true, Vision: true, Reasoning: true, MaxContext: 1000000}},
		{ID: "anthropic/haiku", Quality: 0.70, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.95,
			Caps: config.Caps{Tools: true, Vision: true, Reasoning: false, MaxContext: 200000}},
	}
}

func TestScoreCostWeightPicksCheap(t *testing.T) {
	// Heavy cost weight, easy task → cheap model wins.
	p := TaskProfile{Difficulty: "low", EstTokensIn: 1000, EstTokensOut: 500, Domain: "qa"}
	w := config.Weights{Quality: 0.1, Cost: 0.8, Speed: 0.1}
	d := Score(p, cat(), w, "anthropic/haiku")
	if d.Chosen != "anthropic/haiku" {
		t.Fatalf("Chosen = %q, want anthropic/haiku", d.Chosen)
	}
	if d.Reason != "highest balanced score" {
		t.Errorf("Reason = %q", d.Reason)
	}
}

func TestScoreReasoningFilterDropsHaiku(t *testing.T) {
	// Needs reasoning → haiku (no reasoning) filtered out → opus only eligible.
	p := TaskProfile{Difficulty: "high", NeedsReasoning: true, EstTokensIn: 1000, EstTokensOut: 500}
	w := config.Weights{Quality: 0.1, Cost: 0.8, Speed: 0.1}
	d := Score(p, cat(), w, "anthropic/haiku")
	if d.Chosen != "anthropic/opus" {
		t.Fatalf("Chosen = %q, want anthropic/opus (haiku filtered)", d.Chosen)
	}
	if len(d.Eligible) != 1 || d.Eligible[0] != "anthropic/opus" {
		t.Errorf("Eligible = %v", d.Eligible)
	}
}

func TestScoreVisionFilter(t *testing.T) {
	c := cat()
	c[1].Caps.Vision = false // haiku loses vision
	p := TaskProfile{Difficulty: "low", NeedsVision: true, EstTokensIn: 100, EstTokensOut: 100}
	w := config.Weights{Quality: 0.1, Cost: 0.8, Speed: 0.1}
	d := Score(p, c, w, "anthropic/opus")
	if d.Chosen != "anthropic/opus" {
		t.Fatalf("Chosen = %q, want anthropic/opus", d.Chosen)
	}
}

func TestScoreContextFilter(t *testing.T) {
	p := TaskProfile{Difficulty: "low", EstTokensIn: 500000, EstTokensOut: 0} // exceeds haiku 200k
	w := config.Weights{Quality: 0.1, Cost: 0.8, Speed: 0.1}
	d := Score(p, cat(), w, "anthropic/opus")
	if d.Chosen != "anthropic/opus" {
		t.Fatalf("Chosen = %q, want anthropic/opus (haiku over context)", d.Chosen)
	}
}

func TestScoreNoEligibleFallsBack(t *testing.T) {
	p := TaskProfile{Difficulty: "high", NeedsReasoning: true, EstTokensIn: 2_000_000} // nothing fits
	w := config.Weights{Quality: 1}
	d := Score(p, cat(), w, "anthropic/haiku")
	if d.Chosen != "anthropic/haiku" {
		t.Fatalf("Chosen = %q, want default", d.Chosen)
	}
	if d.Reason != "default_model fallback (no eligible)" {
		t.Errorf("Reason = %q", d.Reason)
	}
}

func TestScoreTieBreakByCatalogOrder(t *testing.T) {
	// Two identical models: earlier catalog entry wins deterministically.
	c := []config.CatalogEntry{
		{ID: "a/one", Quality: 0.8, CostPerMTokIn: 1, CostPerMTokOut: 1, Speed: 0.8,
			Caps: config.Caps{MaxContext: 100000}},
		{ID: "a/two", Quality: 0.8, CostPerMTokIn: 1, CostPerMTokOut: 1, Speed: 0.8,
			Caps: config.Caps{MaxContext: 100000}},
	}
	p := TaskProfile{Difficulty: "low", EstTokensIn: 10, EstTokensOut: 10}
	w := config.Weights{Quality: 1, Cost: 1, Speed: 1}
	d := Score(p, c, w, "a/one")
	if d.Chosen != "a/one" {
		t.Fatalf("Chosen = %q, want a/one (tie-break by order)", d.Chosen)
	}
}

func TestScoreHighDifficultyPrefersQuality(t *testing.T) {
	// High difficulty + balanced weights: the quality floor penalty on the
	// low-quality model should let opus win even though haiku is cheaper.
	p := TaskProfile{Difficulty: "high", EstTokensIn: 1000, EstTokensOut: 1000}
	w := config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}
	d := Score(p, cat(), w, "anthropic/haiku")
	if d.Chosen != "anthropic/opus" {
		t.Fatalf("Chosen = %q, want anthropic/opus for hard task", d.Chosen)
	}
}
