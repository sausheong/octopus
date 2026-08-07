package router

import (
	"fmt"
	"testing"

	"github.com/sausheong/octopus/config"
)

func TestCacheFractionLegacyThreshold(t *testing.T) {
	catalog := terraCatalog()
	w := config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}
	p := TaskProfile{Difficulty: "medium", NeedsTools: true,
		EstTokensIn: 380_000, EstTokensOut: 2000}
	lo, hi := 0.90, 0.95
	for i := 0; i < 40; i++ {
		mid := (lo + hi) / 2
		m := multipliers(catalog, "")
		m["opus"] = 1 - mid + mid*CacheReadInputMultiplier
		if legacyRelativeScore(p, catalog, w, m).Chosen == "opus" {
			hi = mid
		} else {
			lo = mid
		}
	}
	fmt.Printf("\n  legacy relative pin engages at CacheFraction >= %.4f (multiplier %.4f)\n",
		hi, 1-hi+hi*CacheReadInputMultiplier)
}
