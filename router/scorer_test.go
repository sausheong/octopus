package router

import (
	"math"
	"testing"

	"github.com/sausheong/octopus/config"
)

func cat() []config.CatalogEntry {
	return []config.CatalogEntry{
		{ID: "anthropic/opus", Quality: 0.98, CostPerMTokIn: 15, CostPerMTokOut: 75, Speed: 0.4,
			Caps: config.Caps{Tools: true, Vision: true, Reasoning: true, MaxContext: 1000000}},
		{ID: "anthropic/haiku", Quality: 0.70, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.95,
			Caps: config.Caps{Tools: true, Vision: true, Reasoning: false, MaxContext: 200000}},
	}
}

func TestScoreBreakdownExplainsRejectedCandidate(t *testing.T) {
	catalog := []config.CatalogEntry{{
		ID: "p/small", Quality: 0.7, Speed: 0.8,
		Caps: config.Caps{MaxContext: 1000, MaxOutputTokens: 100, Tools: false, Vision: false},
	}}
	p := TaskProfile{NeedsTools: true, NeedsVision: true, EstTokensIn: 1000, EstTokensOut: 200}
	d := Score(p, catalog, config.Weights{Quality: 1})
	b, ok := d.Breakdowns["p/small"]
	if !ok {
		t.Fatal("missing candidate breakdown")
	}
	if b.Eligible {
		t.Fatal("rejected candidate marked eligible")
	}
	if len(b.RejectionReasons) != 4 {
		t.Fatalf("rejection reasons = %v, want all four capability failures", b.RejectionReasons)
	}
	if b.QualityRaw != 0.7 || b.SpeedRaw != 0.8 || b.InputPriceMultiplier != 1 {
		t.Fatalf("breakdown raw values = %+v", b)
	}
}

func TestAbsoluteScoringIsInvariantToUnrelatedCatalogEntry(t *testing.T) {
	catalog := []config.CatalogEntry{
		{ID: "p/a", Quality: 0.8, Speed: 0.7, CostPerMTokIn: 2, CostPerMTokOut: 8, Caps: config.Caps{MaxContext: 100000}},
		{ID: "p/b", Quality: 0.7, Speed: 0.9, CostPerMTokIn: 1, CostPerMTokOut: 4, Caps: config.Caps{MaxContext: 100000}},
	}
	p := TaskProfile{Difficulty: "low", EstTokensIn: 10000, EstTokensOut: 1000}
	w := config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}
	opts := AbsoluteScoringOptions()
	before := ScoreWithOptions(p, catalog, w, nil, opts)

	// This model changes every catalogue-relative maximum but cannot win.
	withUnrelated := append(append([]config.CatalogEntry{}, catalog...), config.CatalogEntry{
		ID: "p/unrelated", Quality: 1, Speed: 1, CostPerMTokIn: 1000, CostPerMTokOut: 1000,
		Caps: config.Caps{MaxContext: 100000},
	})
	after := ScoreWithOptions(p, withUnrelated, w, nil, opts)
	for _, id := range []string{"p/a", "p/b"} {
		if before.Scores[id] != after.Scores[id] {
			t.Errorf("%s score changed from %v to %v after unrelated entry", id, before.Scores[id], after.Scores[id])
		}
	}
}

func TestAbsoluteScoringUsesDollarReference(t *testing.T) {
	catalog := []config.CatalogEntry{{
		ID: "p/model", Quality: 0.8, Speed: 0.8,
		CostPerMTokIn: 10, CostPerMTokOut: 10, Caps: config.Caps{MaxContext: 100000},
	}}
	// 5k + 5k at $10/MTok costs exactly $0.10, giving utility 1/(1+1).
	p := TaskProfile{EstTokensIn: 5000, EstTokensOut: 5000}
	opts := AbsoluteScoringOptions()
	d := ScoreWithOptions(p, catalog, config.Weights{Cost: 1}, nil, opts)
	b := d.Breakdowns["p/model"]
	if math.Abs(b.RequestCostUSD-0.10) > 1e-12 || math.Abs(b.CostUtility-0.5) > 1e-12 {
		t.Fatalf("absolute cost breakdown = %+v", b)
	}
	if d.CostMode != CostModeAbsolute {
		t.Fatalf("cost mode = %q", d.CostMode)
	}
}

func TestScoreConfigurableQualityFloor(t *testing.T) {
	catalog := []config.CatalogEntry{
		{ID: "p/high", Quality: 0.9, Speed: 0.2, Caps: config.Caps{MaxContext: 100000}},
		{ID: "p/cheap", Quality: 0.7, Speed: 1, Caps: config.Caps{MaxContext: 100000}},
	}
	p := TaskProfile{Difficulty: "medium", EstTokensIn: 100, EstTokensOut: 100}
	opts := AbsoluteScoringOptions()
	opts.QualityFloors["medium"] = 0.85
	d := ScoreWithOptions(p, catalog, config.Weights{Speed: 1}, nil, opts)
	if d.Chosen != "p/high" {
		t.Fatalf("chosen = %q, want quality-floor survivor", d.Chosen)
	}
	b := d.Breakdowns["p/cheap"]
	if b.Eligible || len(b.RejectionReasons) != 1 || b.RejectionReasons[0] != "below quality floor" {
		t.Fatalf("quality floor breakdown = %+v", b)
	}
}

func TestScoreReasoningBonusCanBeDisabled(t *testing.T) {
	zero := 0.0
	catalog := []config.CatalogEntry{
		{ID: "p/ordinary", Quality: 0.8, Speed: 0.8, Caps: config.Caps{MaxContext: 10000}},
		{ID: "p/reasoning", Quality: 0.8, Speed: 0.8, Caps: config.Caps{Reasoning: true, MaxContext: 10000}},
	}
	p := TaskProfile{NeedsReasoning: true, EstTokensIn: 100, EstTokensOut: 100}
	d := ScoreWithOptions(p, catalog, config.Weights{Quality: 1}, nil, ScoringOptions{ReasoningBonus: &zero})
	if d.Chosen != "p/ordinary" {
		t.Fatalf("chosen = %q, want catalogue-order tie when bonus disabled", d.Chosen)
	}
}

func TestScoreCostWeightPicksCheap(t *testing.T) {
	// Heavy cost weight, easy task → cheap model wins.
	p := TaskProfile{Difficulty: "low", EstTokensIn: 1000, EstTokensOut: 500, Domain: "qa"}
	w := config.Weights{Quality: 0.1, Cost: 0.8, Speed: 0.1}
	d := Score(p, cat(), w)
	if d.Chosen != "anthropic/haiku" {
		t.Fatalf("Chosen = %q, want anthropic/haiku", d.Chosen)
	}
	if d.Reason != "highest balanced score" {
		t.Errorf("Reason = %q", d.Reason)
	}
}

func TestScoreReasoningPrefersCapableModel(t *testing.T) {
	// Reasoning support is preferred, but remains an optional capability.
	c := []config.CatalogEntry{
		{ID: "p/ordinary", Quality: 0.8, Speed: 0.8, Caps: config.Caps{MaxContext: 10000}},
		{ID: "p/reasoning", Quality: 0.8, Speed: 0.8, Caps: config.Caps{Reasoning: true, MaxContext: 10000}},
	}
	p := TaskProfile{Difficulty: "medium", NeedsReasoning: true, EstTokensIn: 1000, EstTokensOut: 500}
	d := Score(p, c, config.Weights{Quality: 1, Cost: 1, Speed: 1})
	if d.Chosen != "p/reasoning" {
		t.Fatalf("Chosen = %q, want p/reasoning", d.Chosen)
	}
	if len(d.Eligible) != 2 {
		t.Errorf("Eligible = %v, want both models", d.Eligible)
	}
}

func TestScoreReasoningFallsBackToOrdinaryModel(t *testing.T) {
	c := cat()[1:]
	p := TaskProfile{Difficulty: "high", NeedsReasoning: true, EstTokensIn: 1000, EstTokensOut: 500}
	d := Score(p, c, config.Weights{Quality: 1})
	if d.NoEligible || d.Chosen != "anthropic/haiku" {
		t.Fatalf("decision = %+v, want ordinary model fallback", d)
	}
}

func TestScoreVisionFilter(t *testing.T) {
	c := cat()
	c[1].Caps.Vision = false // haiku loses vision
	p := TaskProfile{Difficulty: "low", NeedsVision: true, EstTokensIn: 100, EstTokensOut: 100}
	w := config.Weights{Quality: 0.1, Cost: 0.8, Speed: 0.1}
	d := Score(p, c, w)
	if d.Chosen != "anthropic/opus" {
		t.Fatalf("Chosen = %q, want anthropic/opus", d.Chosen)
	}
}

func TestScoreContextFilter(t *testing.T) {
	p := TaskProfile{Difficulty: "low", EstTokensIn: 500000, EstTokensOut: 0} // exceeds haiku 200k
	w := config.Weights{Quality: 0.1, Cost: 0.8, Speed: 0.1}
	d := Score(p, cat(), w)
	if d.Chosen != "anthropic/opus" {
		t.Fatalf("Chosen = %q, want anthropic/opus (haiku over context)", d.Chosen)
	}
}

func TestScoreNoEligibleSetsFlag(t *testing.T) {
	p := TaskProfile{Difficulty: "high", NeedsReasoning: true, EstTokensIn: 2_000_000} // nothing fits
	w := config.Weights{Quality: 1}
	d := Score(p, cat(), w)
	if !d.NoEligible {
		t.Fatal("expected NoEligible=true when no model passes capability filter")
	}
	if d.Reason != "no eligible model" {
		t.Errorf("Reason = %q", d.Reason)
	}
	if d.Chosen != "" {
		t.Errorf("Chosen = %q, want empty when no model is eligible", d.Chosen)
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
	d := Score(p, c, w)
	if d.Chosen != "a/one" {
		t.Fatalf("Chosen = %q, want a/one (tie-break by order)", d.Chosen)
	}
}

func TestScoreHighDifficultyPrefersQuality(t *testing.T) {
	// High difficulty + balanced weights: the quality floor penalty on the
	// low-quality model should let opus win even though haiku is cheaper.
	p := TaskProfile{Difficulty: "high", EstTokensIn: 1000, EstTokensOut: 1000}
	w := config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}
	d := Score(p, cat(), w)
	if d.Chosen != "anthropic/opus" {
		t.Fatalf("Chosen = %q, want anthropic/opus for hard task", d.Chosen)
	}
}

func TestScoreFreeModeGetsMaxCostScore(t *testing.T) {
	// A zero-cost local model must not be penalized with the worst cost score.
	// With equal quality and speed, the free model must win on a cost-heavy weight.
	catalog := []config.CatalogEntry{
		{ID: "cloud/paid", Quality: 0.8, CostPerMTokIn: 10, CostPerMTokOut: 30, Speed: 0.8,
			Caps: config.Caps{MaxContext: 100000}},
		{ID: "local/free", Quality: 0.8, CostPerMTokIn: 0, CostPerMTokOut: 0, Speed: 0.8,
			Caps: config.Caps{MaxContext: 100000}},
	}
	p := TaskProfile{Difficulty: "low", EstTokensIn: 1000, EstTokensOut: 500}
	w := config.Weights{Quality: 0.1, Cost: 0.8, Speed: 0.1}
	d := Score(p, catalog, w)
	if d.Chosen != "local/free" {
		t.Fatalf("Chosen = %q, want local/free (free model must win cost-heavy scoring)", d.Chosen)
	}
	// Also verify the free model's cost score is strictly higher than the paid model's.
	if d.Scores["local/free"] <= d.Scores["cloud/paid"] {
		t.Errorf("free model score %v not > paid model score %v", d.Scores["local/free"], d.Scores["cloud/paid"])
	}
}

func TestScoreFreeModelWinsReadmeRecipe(t *testing.T) {
	// Regression: the exact mixed local/cloud catalog and weights from the README
	// must route a TrivialProfile to the local free model, not to Haiku.
	catalog := []config.CatalogEntry{
		{ID: "anthropic/claude-opus-4-0-20250514", Quality: 0.98,
			CostPerMTokIn: 15.0, CostPerMTokOut: 75.0, Speed: 0.4,
			Caps: config.Caps{Tools: true, Vision: true, Reasoning: true, MaxContext: 1000000}},
		{ID: "anthropic/claude-haiku-3-5-20241022", Quality: 0.70,
			CostPerMTokIn: 1.0, CostPerMTokOut: 5.0, Speed: 0.95,
			Caps: config.Caps{Tools: true, Vision: true, Reasoning: false, MaxContext: 200000}},
		{ID: "mlx/Qwen3-8B-4bit", Quality: 0.60,
			CostPerMTokIn: 0.0, CostPerMTokOut: 0.0, Speed: 0.85,
			Caps: config.Caps{Tools: false, Vision: false, Reasoning: false, MaxContext: 32768}},
	}
	w := config.Weights{Quality: 0.5, Cost: 0.4, Speed: 0.1}
	p := TrivialProfile() // EstTokensIn:100, EstTokensOut:200 — well within MLX context
	d := Score(p, catalog, w)
	if d.Chosen != "mlx/Qwen3-8B-4bit" {
		t.Fatalf("Chosen = %q, want mlx/Qwen3-8B-4bit (free local model should win for trivial tasks)", d.Chosen)
	}
}

func TestScoreEligibleSortedByScore(t *testing.T) {
	// Eligible list must be in descending score order so the first fallback
	// candidate is always the second-best model.
	p := TaskProfile{Difficulty: "low", EstTokensIn: 100, EstTokensOut: 100}
	w := config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}
	d := Score(p, cat(), w)
	if len(d.Eligible) < 2 {
		t.Fatalf("expected >= 2 eligible, got %v", d.Eligible)
	}
	for i := 1; i < len(d.Eligible); i++ {
		if d.Scores[d.Eligible[i-1]] < d.Scores[d.Eligible[i]] {
			t.Errorf("eligible[%d] score %v < eligible[%d] score %v — not sorted descending",
				i-1, d.Scores[d.Eligible[i-1]], i, d.Scores[d.Eligible[i]])
		}
	}
}

func TestEligibilityRespectsMaxOutputTokens(t *testing.T) {
	catalog := []config.CatalogEntry{
		{ID: "p/small", Quality: 0.9, Speed: 0.9,
			Caps: config.Caps{MaxContext: 200000, MaxOutputTokens: 4096}},
		{ID: "p/large", Quality: 0.8, Speed: 0.5,
			Caps: config.Caps{MaxContext: 200000, MaxOutputTokens: 64000}},
		{ID: "p/unset", Quality: 0.7, Speed: 0.5,
			Caps: config.Caps{MaxContext: 200000}},
	}
	w := config.Weights{Quality: 1}

	// Within every limit: all three eligible.
	small := Score(TaskProfile{EstTokensIn: 100, EstTokensOut: 1000}, catalog, w)
	if len(small.Eligible) != 3 {
		t.Errorf("small request eligible = %v, want all 3", small.Eligible)
	}

	// Exceeds p/small's output limit only.
	big := Score(TaskProfile{EstTokensIn: 100, EstTokensOut: 30000}, catalog, w)
	for _, id := range big.Eligible {
		if id == "p/small" {
			t.Errorf("p/small (4096 output cap) eligible for a 30000-token output request")
		}
	}
	if len(big.Eligible) != 2 {
		t.Errorf("big request eligible = %v, want p/large and p/unset", big.Eligible)
	}
}
