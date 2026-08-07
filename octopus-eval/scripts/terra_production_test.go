package router

import (
	"math"
	"testing"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/octopus/config"
)

func assertTerraUSD(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("%s = $%.12f, want $%.12f", name, got, want)
	}
}

func terraAmortizedFixture(turns int, confidence float64) (*Router, llm.ChatRequest, Decision) {
	catalog := []config.CatalogEntry{
		{ID: "opus", Quality: 0.98, CostPerMTokIn: 15, CostPerMTokOut: 75, Speed: 0.40,
			Caps: config.Caps{Tools: true, MaxContext: 1_000_000}},
		{ID: "sonnet", Quality: 0.90, CostPerMTokIn: 3, CostPerMTokOut: 15, Speed: 0.70,
			Caps: config.Caps{Tools: true, MaxContext: 1_000_000}},
	}
	cfg := &config.Config{
		Weights: config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2},
		Routing: config.RoutingCfg{
			Strategy: config.RoutingStrategyAmortized, CacheAware: true,
			DefaultRemainingTurns: 4, MinSwitchSavingsUSD: 0.01,
			MinSwitchSavingsPct: 0.10, SwitchConfidence: 0.60,
			CostMode: config.CostModeAbsolute, CostReferenceUSD: 0.10,
		},
		Catalog: catalog,
	}
	chat := llm.ChatRequest{
		SessionID:    "terra-production-economics",
		CacheControl: &llm.CacheControl{Type: "ephemeral"},
	}
	now := time.Now()
	sid := SessionID(chat)
	r := &Router{
		cfg:           cfg,
		cachingModels: map[string]bool{"opus": true, "sonnet": true},
		sessions: map[string]sessionState{sid: {
			Incumbent: "opus", ExpiresAt: now.Add(time.Hour), TurnCount: 1,
			Models: map[string]modelSessionState{
				"opus": {CacheUntil: now.Add(time.Minute), CacheFraction: 1},
			},
		}},
	}
	decision := Decision{
		Chosen: "sonnet", Eligible: []string{"sonnet", "opus"},
		Profile: TaskProfile{
			Difficulty: "medium", EstTokensIn: 380_000, EstTokensOut: 1_000,
			ExpectedRemainingTurns: turns, EstimateConfidence: confidence,
		},
		Strategy: config.RoutingStrategyAmortized,
	}
	return r, chat, decision
}

func TestTerraProductionAbsoluteScoring(t *testing.T) {
	profile := TaskProfile{Difficulty: "medium", NeedsTools: true, EstTokensIn: 380_000, EstTokensOut: 1_000}
	weights := config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2}
	absolute := productionScore(profile, terraCatalog(), weights, nil)
	legacy := legacyRelativeScore(profile, terraCatalog(), weights, nil)

	if absolute.CostMode != CostModeAbsolute || absolute.Chosen != "sonnet" {
		t.Fatalf("production absolute decision = %+v", absolute)
	}
	if legacy.CostMode != CostModeRelative {
		t.Fatalf("legacy comparison not labelled relative: %+v", legacy)
	}
	assertTerraUSD(t, "absolute opus request cost", absolute.Breakdowns["opus"].RequestCostUSD, 5.775)
	assertTerraUSD(t, "absolute sonnet request cost", absolute.Breakdowns["sonnet"].RequestCostUSD, 1.155)
	assertTerraUSD(t, "absolute sonnet cost utility", absolute.Breakdowns["sonnet"].CostUtility, 1/(1+1.155/0.10))
}

func TestTerraProductionAmortizedScenarios(t *testing.T) {
	// At 380k input and 1k output, the warm Opus turn costs $0.645.
	// A cold Sonnet cache write costs $1.440 and each warm turn costs $0.129.
	const warmOpus = 0.645
	const coldSonnet = 1.440
	const warmSonnet = 0.129

	t.Run("initial", func(t *testing.T) {
		r, chat, decision := terraAmortizedFixture(4, 1)
		r.sessions = map[string]sessionState{}
		// Exercise the real production absolute scorer before the amortized
		// decision. A quality-only policy selects Opus for suitability; expected
		// cost to completion then selects Sonnet for this four-turn task.
		decision = productionScore(decision.Profile, r.cfg.Catalog, config.Weights{Quality: 1}, nil)
		decision.Strategy = config.RoutingStrategyAmortized
		if decision.Chosen != "opus" || decision.CostMode != CostModeAbsolute {
			t.Fatalf("initial production score = %+v", decision)
		}
		got := r.applyAmortized(chat, SessionID(chat), decision)
		if got.Chosen != "sonnet" || got.Reason != "initial lowest expected cost to completion" {
			t.Fatalf("initial decision = %+v", got)
		}
		opus := r.cfg.Catalog[0]
		sonnet := r.cfg.Catalog[1]
		assertTerraUSD(t, "initial opus completion", r.forecastCost(chat, decision.Profile, sessionState{}, opus, 4, false, 1), 7.200+3*warmOpus)
		assertTerraUSD(t, "initial sonnet completion", r.forecastCost(chat, decision.Profile, sessionState{}, sonnet, 4, false, 1), coldSonnet+3*warmSonnet)
	})

	t.Run("stay before payback", func(t *testing.T) {
		r, chat, decision := terraAmortizedFixture(2, 1)
		got := r.applyAmortized(chat, SessionID(chat), decision)
		if got.Chosen != "opus" || got.Economics == nil || got.Economics.Decision != "retain" {
			t.Fatalf("two-turn decision = %+v", got)
		}
		assertTerraUSD(t, "stay cost", got.Economics.StayCostUSD, 2*warmOpus)
		assertTerraUSD(t, "cold switch cost", got.Economics.SwitchCostUSD, coldSonnet+warmSonnet)
		assertTerraUSD(t, "required saving", got.Economics.RequiredSavingsUSD, 0.10*(2*warmOpus))
	})

	t.Run("switch after payback", func(t *testing.T) {
		r, chat, decision := terraAmortizedFixture(3, 1)
		got := r.applyAmortized(chat, SessionID(chat), decision)
		if got.Chosen != "sonnet" || got.Economics == nil || got.Economics.Decision != "switch" {
			t.Fatalf("three-turn decision = %+v", got)
		}
		assertTerraUSD(t, "stay cost", got.Economics.StayCostUSD, 3*warmOpus)
		assertTerraUSD(t, "switch cost", got.Economics.SwitchCostUSD, coldSonnet+2*warmSonnet)
		assertTerraUSD(t, "estimated saving", got.Economics.EstimatedSavingsUSD, 3*warmOpus-(coldSonnet+2*warmSonnet))
		assertTerraUSD(t, "break even", got.Economics.BreakEvenTurns, (coldSonnet-warmSonnet)/(warmOpus-warmSonnet))
	})

	t.Run("warm candidate", func(t *testing.T) {
		r, chat, decision := terraAmortizedFixture(2, 1)
		sid := SessionID(chat)
		state := r.sessions[sid]
		state.Models["sonnet"] = modelSessionState{CacheUntil: time.Now().Add(time.Minute), CacheFraction: 1}
		r.sessions[sid] = state
		got := r.applyAmortized(chat, sid, decision)
		if got.Chosen != "sonnet" || got.Economics == nil || !got.Economics.CandidateCacheWarm {
			t.Fatalf("warm-candidate decision = %+v", got)
		}
		assertTerraUSD(t, "warm switch cost", got.Economics.SwitchCostUSD, 2*warmSonnet)
		assertTerraUSD(t, "warm first candidate turn", got.Economics.FirstCandidateCostUSD, warmSonnet)
	})

	t.Run("turn efficiency", func(t *testing.T) {
		r, chat, decision := terraAmortizedFixture(4, 1)
		candidate := r.cfg.Catalog[1]
		candidate.TurnEfficiency.Medium = 0.25
		r.cfg.Catalog[1] = candidate
		got := r.applyAmortized(chat, SessionID(chat), decision)
		if got.Chosen != "opus" || got.Economics.ExpectedTurnsCandidate != 16 {
			t.Fatalf("turn-efficiency decision = %+v", got)
		}
		assertTerraUSD(t, "efficient stay cost", got.Economics.StayCostUSD, 4*warmOpus)
		assertTerraUSD(t, "inefficient candidate cost", got.Economics.SwitchCostUSD, coldSonnet+15*warmSonnet)
	})

	t.Run("low confidence", func(t *testing.T) {
		r, chat, decision := terraAmortizedFixture(3, 0.40)
		got := r.applyAmortized(chat, SessionID(chat), decision)
		if got.Chosen != "opus" || got.Economics.Decision != "retain_low_confidence" {
			t.Fatalf("low-confidence decision = %+v", got)
		}
		assertTerraUSD(t, "low-confidence stay cost", got.Economics.StayCostUSD, 3*warmOpus)
		assertTerraUSD(t, "low-confidence switch cost", got.Economics.SwitchCostUSD, coldSonnet+2*warmSonnet)
	})

	t.Run("incumbent ineligible", func(t *testing.T) {
		r, chat, decision := terraAmortizedFixture(2, 1)
		decision.Eligible = []string{"sonnet"}
		decision.Chosen = "sonnet"
		got := r.applyAmortized(chat, SessionID(chat), decision)
		if got.Chosen != "sonnet" || got.Economics != nil || got.Reason != "incumbent ineligible; highest balanced score" {
			t.Fatalf("ineligible-incumbent decision = %+v", got)
		}
	})
}
