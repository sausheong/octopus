package router

import (
	"fmt"
	"testing"

	"github.com/sausheong/octopus/config"
)

// T3: compare production absolute per-turn scoring with the explicitly
// labelled legacy relative scorer. Amortized routing is tested separately by
// TestTerraProductionAmortizedScenarios.
//
// Drives the real scorer with a Terra-shaped session (measured average context
// of ~380k tokens, per docs/cost-efficiency-review-2026-07-25.md) and reports
// which model wins with no cache versus with a live cache pinned to each
// candidate. If the cached model always wins, cache-awareness is equivalent to
// "never switch" and the router cannot arbitrage price.
func TestTerraSwitchConvergence(t *testing.T) {
	catalog := []config.CatalogEntry{
		{ID: "anthropic/opus", Quality: 0.98, CostPerMTokIn: 15, CostPerMTokOut: 75, Speed: 0.40,
			Caps: config.Caps{Tools: true, Vision: true, Reasoning: true, MaxContext: 1000000}},
		{ID: "anthropic/sonnet", Quality: 0.90, CostPerMTokIn: 3, CostPerMTokOut: 15, Speed: 0.70,
			Caps: config.Caps{Tools: true, Vision: true, Reasoning: true, MaxContext: 1000000}},
		{ID: "anthropic/haiku", Quality: 0.70, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.95,
			Caps: config.Caps{Tools: true, Vision: true, Reasoning: false, MaxContext: 200000}},
	}
	w := config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}

	for _, tc := range []struct {
		name     string
		tokensIn int
	}{
		{"terra-average-380k", 380000},
		{"terra-small-turn-50k", 50000},
	} {
		prof := TaskProfile{Difficulty: "medium", NeedsTools: true,
			EstTokensIn: tc.tokensIn, EstTokensOut: 2000}

		base := productionScore(prof, catalog, w, nil)
		legacyBase := legacyRelativeScore(prof, catalog, w, nil)
		fmt.Printf("\n[%s] eligible=%v\n", tc.name, base.Eligible)
		fmt.Printf("  production absolute, no cache -> chosen=%s\n", base.Chosen)
		fmt.Printf("  legacy relative, no cache      -> chosen=%s\n", legacyBase.Chosen)

		// Now pin a live cache to each candidate in turn (fraction=1.0, the
		// steady state for a long session) and see whether the choice moves.
		for _, cached := range base.Eligible {
			m := map[string]float64{}
			for _, e := range catalog {
				m[e.ID] = CacheWrite5mInputMultiplier
			}
			m[cached] = 1 - 1.0 + 1.0*CacheReadInputMultiplier // = 0.10
			d := productionScore(prof, catalog, w, m)
			legacy := legacyRelativeScore(prof, catalog, w, m)
			verdict := "SWITCHED away from cache"
			if d.Chosen == cached {
				verdict = "stayed on cached model"
			}
			fmt.Printf("  cache on %-18s -> absolute=%-18s %-24s legacy(relative)=%s\n", cached, d.Chosen, verdict, legacy.Chosen)
		}
	}
}
