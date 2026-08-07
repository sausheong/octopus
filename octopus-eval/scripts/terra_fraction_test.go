package router

import (
	"fmt"
	"testing"

	"github.com/sausheong/octopus/config"
)

// Historical comparison: where did the legacy relative scorer's pin engage?
func TestCacheFractionLegacyFineSweep(t *testing.T) {
	catalog := terraCatalog()
	w := config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}
	p := TaskProfile{Difficulty: "medium", NeedsTools: true,
		EstTokensIn: 380_000, EstTokensOut: 2000}
	fmt.Printf("\n  %-10s %-12s %s\n", "fraction", "multiplier", "chosen")
	for _, f := range []float64{0.90, 0.95, 0.97, 0.98, 0.985, 0.99, 0.995, 1.00} {
		m := multipliers(catalog, "")
		m["opus"] = 1 - f + f*CacheReadInputMultiplier
		d := legacyRelativeScore(p, catalog, w, m)
		fmt.Printf("  %-10.3f %-12.4f %s\n", f, m["opus"], d.Chosen)
	}
}
