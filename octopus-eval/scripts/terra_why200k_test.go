package router

import (
	"fmt"
	"testing"

	"github.com/sausheong/octopus/config"
)

// Is the 200k boundary about size, or about WHICH COMPETITORS survive the
// eligibility filter? Run the identical sweep with haiku removed.
func TestWhy200kBoundary(t *testing.T) {
	w := config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}
	full := terraCatalog()
	noHaiku := []config.CatalogEntry{}
	for _, e := range full {
		if e.ID != "haiku" {
			noHaiku = append(noHaiku, e)
		}
	}

	for _, tc := range []struct {
		name string
		cat  []config.CatalogEntry
	}{{"WITH haiku (200k cap)", full}, {"WITHOUT haiku", noHaiku}} {
		fmt.Printf("\n  === %s ===\n", tc.name)
		fmt.Printf("  %-9s %-9s %-12s %s\n", "ctx", "absolute", "legacy(rel)", "absolute cache on opus")
		for _, n := range []int{100, 1_000, 10_000, 50_000, 150_000, 250_000, 380_000} {
			p := TaskProfile{Difficulty: "medium", NeedsTools: true,
				EstTokensIn: n, EstTokensOut: 2000}
			base := productionScore(p, tc.cat, w, nil)
			legacy := legacyRelativeScore(p, tc.cat, w, nil)
			d := productionScore(p, tc.cat, w, multipliers(tc.cat, "opus"))
			mark := "STAY on opus"
			if d.Chosen != "opus" {
				mark = "-> " + d.Chosen
			}
			fmt.Printf("  %-9d %-9s %-12s %s\n", n, base.Chosen, legacy.Chosen, mark)
		}
	}

	// Effective per-Mtok input prices that drive it
	fmt.Printf("\n  effective input $/Mtok:\n")
	fmt.Printf("    opus  cached (x0.10) : %.2f\n", 15*0.10)
	fmt.Printf("    haiku fresh  (x1.25) : %.2f   <- beats cached opus\n", 1*1.25)
	fmt.Printf("    sonnet fresh (x1.25) : %.2f   <- loses to cached opus\n", 3*1.25)
}
