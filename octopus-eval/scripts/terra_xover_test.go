package router

import (
	"fmt"
	"testing"

	"github.com/sausheong/octopus/config"
)

// With haiku removed, at what context does production absolute per-turn
// scoring prefer cached opus to fresh sonnet?
func TestOpusSonnetCrossover(t *testing.T) {
	w := config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}
	cat := []config.CatalogEntry{}
	for _, e := range terraCatalog() {
		if e.ID != "haiku" {
			cat = append(cat, e)
		}
	}
	lo, hi := 1_000, 200_000
	for lo < hi-100 {
		mid := (lo + hi) / 2
		p := TaskProfile{Difficulty: "medium", NeedsTools: true,
			EstTokensIn: mid, EstTokensOut: 2000}
		if productionScore(p, cat, w, multipliers(cat, "opus")).Chosen == "opus" {
			hi = mid
		} else {
			lo = mid
		}
	}
	fmt.Printf("\n  production absolute: cached-opus overtakes fresh-sonnet at ctx ~= %d tokens\n", hi)
	fmt.Printf("  pure-cost crossover (ignoring quality/speed weights):\n")
	fmt.Printf("    opus_cached(In)  = In*1.5e-6  + 0.150   (output 2000 @ $75/M)\n")
	fmt.Printf("    sonnet_fresh(In) = In*3.75e-6 + 0.030   (output 2000 @ $15/M)\n")
	fmt.Printf("    equal at In = %.0f tokens\n", 0.12/2.25e-6)
}
