package router

import (
	"fmt"
	"testing"

	"github.com/sausheong/octopus/config"
)

func terraCatalog() []config.CatalogEntry {
	return []config.CatalogEntry{
		{ID: "opus", Quality: 0.98, CostPerMTokIn: 15, CostPerMTokOut: 75, Speed: 0.40,
			Caps: config.Caps{Tools: true, Vision: true, Reasoning: true, MaxContext: 1000000}},
		{ID: "sonnet", Quality: 0.90, CostPerMTokIn: 3, CostPerMTokOut: 15, Speed: 0.70,
			Caps: config.Caps{Tools: true, Vision: true, Reasoning: true, MaxContext: 1000000}},
		{ID: "haiku", Quality: 0.70, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.95,
			Caps: config.Caps{Tools: true, Vision: true, Reasoning: false, MaxContext: 200000}},
	}
}

func multipliers(catalog []config.CatalogEntry, cached string) map[string]float64 {
	m := map[string]float64{}
	for _, e := range catalog {
		m[e.ID] = CacheWrite5mInputMultiplier
	}
	if cached != "" {
		m[cached] = CacheReadInputMultiplier
	}
	return m
}

// productionScore is the scorer used by the current router defaults.
func productionScore(p TaskProfile, catalog []config.CatalogEntry, w config.Weights, multipliers map[string]float64) Decision {
	return ScoreWithOptions(p, catalog, w, multipliers, AbsoluteScoringOptions())
}

// legacyRelativeScore is retained only for explicitly labelled comparisons.
func legacyRelativeScore(p TaskProfile, catalog []config.CatalogEntry, w config.Weights, multipliers map[string]float64) Decision {
	return ScoreWithInputMultipliers(p, catalog, w, multipliers)
}

// T3b: sweep context size. For each size, report the no-cache choice and
// whether a cache pinned to each model holds the router in place.
func TestSweepContextSizes(t *testing.T) {
	catalog := terraCatalog()
	w := config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}
	sizes := []int{5_000, 10_000, 25_000, 50_000, 100_000, 150_000,
		200_000, 300_000, 380_000, 500_000, 700_000, 900_000}

	fmt.Printf("\n  production absolute scoring\n")
	fmt.Printf("%-10s %-12s %-12s %s\n", "ctx", "absolute", "legacy(rel)", "cache pinned to -> absolute chosen")
	fmt.Println("  " + "-----------------------------------------------------------------")
	for _, n := range sizes {
		p := TaskProfile{Difficulty: "medium", NeedsTools: true,
			EstTokensIn: n, EstTokensOut: 2000}
		base := productionScore(p, catalog, w, nil)
		legacy := legacyRelativeScore(p, catalog, w, nil)
		line := fmt.Sprintf("%-10d %-12s %-12s ", n, base.Chosen, legacy.Chosen)
		for _, cand := range []string{"haiku", "sonnet", "opus"} {
			elig := false
			for _, e := range base.Eligible {
				if e == cand {
					elig = true
				}
			}
			if !elig {
				line += fmt.Sprintf(" %s:--", cand)
				continue
			}
			d := productionScore(p, catalog, w, multipliers(catalog, cand))
			mark := "STAY"
			if d.Chosen != cand {
				mark = "->" + d.Chosen
			}
			line += fmt.Sprintf("  %s:%s", cand, mark)
		}
		fmt.Println("  " + line)
	}
}

// T3c: sweep the weight vector at Terra's measured context. Does any sane
// weighting make the router leave a warm cache?
func TestSweepWeights(t *testing.T) {
	catalog := terraCatalog()
	p := TaskProfile{Difficulty: "medium", NeedsTools: true,
		EstTokensIn: 380_000, EstTokensOut: 2000}

	weightings := []struct {
		name string
		w    config.Weights
	}{
		{"default   (q.5 c.3 s.2)", config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}},
		{"cost-lean (q.3 c.6 s.1)", config.Weights{Quality: 0.3, Cost: 0.6, Speed: 0.1}},
		{"cost-max  (q.0 c1.0 s.0)", config.Weights{Quality: 0.0, Cost: 1.0, Speed: 0.0}},
		{"quality   (q.9 c.05 s.05)", config.Weights{Quality: 0.9, Cost: 0.05, Speed: 0.05}},
	}

	fmt.Printf("\n  production absolute scoring (legacy relative shown for comparison)\n")
	fmt.Printf("%-26s %-10s %-12s %s\n", "weights", "absolute", "legacy(rel)", "absolute cache on opus")
	fmt.Println("  " + "------------------------------------------------------------")
	for _, tc := range weightings {
		base := productionScore(p, catalog, tc.w, nil)
		legacy := legacyRelativeScore(p, catalog, tc.w, nil)
		d := productionScore(p, catalog, tc.w, multipliers(catalog, "opus"))
		mark := "STAY on opus"
		if d.Chosen != "opus" {
			mark = "SWITCHED to " + d.Chosen
		}
		fmt.Printf("  %-26s %-10s %-12s %s\n", tc.name, base.Chosen, legacy.Chosen, mark)
	}
}

// T3d: how big must the cached fraction be before the cache pins the router?
func TestSweepCacheFraction(t *testing.T) {
	catalog := terraCatalog()
	w := config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}
	p := TaskProfile{Difficulty: "medium", NeedsTools: true,
		EstTokensIn: 380_000, EstTokensOut: 2000}

	fmt.Printf("\n  cached fraction on opus -> chosen\n")
	for _, f := range []float64{0.0, 0.1, 0.25, 0.5, 0.75, 0.9, 1.0} {
		m := multipliers(catalog, "")
		m["opus"] = 1 - f + f*CacheReadInputMultiplier
		d := productionScore(p, catalog, w, m)
		fmt.Printf("    %4.0f%%  (multiplier %.3f) -> %s\n", f*100, m["opus"], d.Chosen)
	}
}
