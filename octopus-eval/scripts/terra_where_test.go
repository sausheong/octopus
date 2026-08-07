package router

import (
	"fmt"
	"testing"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/octopus/config"
)

// Where can the cache multiplier change the production absolute per-turn
// choice? Legacy relative output is shown only as a comparison.
func TestWhereCacheThresholdMatters(t *testing.T) {
	cat := []config.CatalogEntry{
		{ID: "opus", Quality: 0.98, CostPerMTokIn: 15, CostPerMTokOut: 75, Speed: 0.40,
			Caps: config.Caps{Tools: true, Vision: true, MaxContext: 1000000}},
		{ID: "sonnet", Quality: 0.90, CostPerMTokIn: 3, CostPerMTokOut: 15, Speed: 0.70,
			Caps: config.Caps{Tools: true, Vision: true, MaxContext: 1000000}},
	}
	w := config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}
	prof := TaskProfile{Difficulty: "medium", EstTokensIn: 380_000, EstTokensOut: 2000}

	// A live session cached on opus at full coverage.
	mkRouter := func(sticky, cacheAware bool, chat llm.ChatRequest, frac float64) *Router {
		cfg, err := config.Parse([]byte(fmt.Sprintf(`
server: { addr: "127.0.0.1:8787" }
weights: { quality: 1 }
routing: { session_sticky: %t, session_ttl: "1h", cache_aware: %t }
providers:
  p: { kind: openai, base_url: "http://127.0.0.1:1" }
catalog:
  - id: "p/model"
    quality: 1
    speed: 1
    caps: { max_context: 1000 }
`, sticky, cacheAware)))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Catalog = cat
		cfg.Weights = w
		r := &Router{
			cfg:           cfg,
			sessions:      map[string]sessionState{},
			cachingModels: map[string]bool{"opus": true, "sonnet": true},
		}
		cached := int(frac * 1000)
		r.Observe(chat, "opus", &llm.Usage{CacheCreationInputTokens: cached, InputTokens: 1000 - cached})
		return r
	}
	// A request carrying a cache_control marker (required for CacheTTL > 0).
	chat := llm.ChatRequest{
		SessionID: "S",
		SystemPromptParts: []llm.SystemPromptPart{
			{Text: "big prefix", CacheControl: &llm.CacheControl{Type: "ephemeral"}},
		},
	}

	for _, tc := range []struct {
		sticky, cacheAware bool
		frac               float64
	}{
		{true, true, 1.00}, {false, true, 1.00}, {false, false, 1.00},
		{true, true, 0.90}, {false, true, 0.90},
	} {
		sid := SessionID(chat)
		r := mkRouter(tc.sticky, tc.cacheAware, chat, tc.frac)
		mult := r.cacheInputMultipliersForSession(chat, sid)
		d := productionScore(prof, cat, w, mult)
		legacy := legacyRelativeScore(prof, cat, w, mult)
		scored := d.Chosen
		final := scored
		note := "score stands"
		if s := r.stickyModelForSession(sid); s != "" {
			for _, id := range d.Eligible {
				if id == s {
					final = s
					note = "STICKY OVERRIDE"
					break
				}
			}
		}
		fmt.Printf("  sticky=%-5v cache_aware=%-5v coverage=%.2f  multipliers=%v\n", tc.sticky, tc.cacheAware, tc.frac, mult)
		fmt.Printf("      absolute=%-7s final=%-7s (%s); legacy(relative)=%s\n", scored, final, note, legacy.Chosen)
	}
}
