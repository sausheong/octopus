package router

import (
	"fmt"
	"testing"

	"github.com/sausheong/octopus/config"
)

// Why does a FREE local model lose to a paid cloud model on a small request?
// Context is not the reason when the request fits — isolate the scoring cause.
func TestLocalVsCloudSmallRequest(t *testing.T) {
	catalog := []config.CatalogEntry{
		{ID: "ollama/qwen2.5:3b", Quality: 0.45, CostPerMTokIn: 0, CostPerMTokOut: 0, Speed: 0.85,
			Caps: config.Caps{Tools: true, MaxContext: 32768}},
		{ID: "litellm/haiku", Quality: 0.75, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.90,
			Caps: config.Caps{Tools: true, MaxContext: 200000}},
	}
	weightings := []struct {
		name string
		w    config.Weights
	}{
		{"default   (q.5 c.3 s.2)", config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}},
		{"cost-lean (q.3 c.6 s.1)", config.Weights{Quality: 0.3, Cost: 0.6, Speed: 0.1}},
		{"cost-max  (q0 c1.0 s0)", config.Weights{Quality: 0.0, Cost: 1.0, Speed: 0.0}},
	}
	for _, ctx := range []int{1_000, 5_000, 20_000} {
		p := TaskProfile{Difficulty: "medium", EstTokensIn: ctx, EstTokensOut: 500}
		fmt.Printf("\n  ctx=%d (fits BOTH models)\n", ctx)
		for _, tc := range weightings {
			d := productionScore(p, catalog, tc.w, nil)
			legacy := legacyRelativeScore(p, catalog, tc.w, nil)
			fmt.Printf("    %-26s eligible=%v absolute=%s legacy(relative)=%s\n", tc.name, d.Eligible, d.Chosen, legacy.Chosen)
		}
	}
	// And when it does not fit the local window:
	p := TaskProfile{Difficulty: "medium", EstTokensIn: 60_000, EstTokensOut: 500}
	d := productionScore(p, catalog, config.Weights{Quality: 0, Cost: 1, Speed: 0}, nil)
	fmt.Printf("\n  ctx=60000 (exceeds local 32768), cost-max: eligible=%v chosen=%s\n",
		d.Eligible, d.Chosen)
}
