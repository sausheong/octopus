package router

import (
	"testing"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/octopus/config"
)

func economicRouter(turns int, confidence float64) (*Router, llm.ChatRequest, Decision) {
	cfg := &config.Config{
		Routing: config.RoutingCfg{
			Strategy: config.RoutingStrategyAmortized, CacheAware: true,
			DefaultRemainingTurns: 4, SwitchConfidence: 0.6,
		},
		Catalog: []config.CatalogEntry{
			{ID: "p/incumbent", CostPerMTokIn: 15, CostPerMTokOut: 75},
			{ID: "p/candidate", CostPerMTokIn: 3, CostPerMTokOut: 15},
		},
	}
	chat := llm.ChatRequest{SessionID: "economic-session", CacheControl: &llm.CacheControl{Type: "ephemeral"}}
	sid := SessionID(chat)
	now := time.Now()
	r := &Router{
		cfg: cfg, cachingModels: map[string]bool{"p/incumbent": true, "p/candidate": true},
		sessions: map[string]sessionState{sid: {
			Incumbent: "p/incumbent", ExpiresAt: now.Add(time.Hour), TurnCount: 1,
			Models: map[string]modelSessionState{"p/incumbent": {CacheUntil: now.Add(time.Minute), CacheFraction: 1}},
		}},
	}
	decision := Decision{
		Chosen: "p/candidate", Eligible: []string{"p/candidate", "p/incumbent"},
		Profile: TaskProfile{Difficulty: "medium", EstTokensIn: 380_000, EstTokensOut: 1_000,
			ExpectedRemainingTurns: turns, EstimateConfidence: confidence},
		Strategy: config.RoutingStrategyAmortized,
	}
	return r, chat, decision
}

func TestAmortizedRoutingWaitsForColdWritePayback(t *testing.T) {
	r, chat, decision := economicRouter(2, 1)
	got := r.applyAmortized(chat, SessionID(chat), decision)
	if got.Chosen != "p/incumbent" || got.Economics == nil || got.Economics.Decision != "retain" {
		t.Fatalf("two-turn decision = %+v", got)
	}

	decision.Profile.ExpectedRemainingTurns = 3
	got = r.applyAmortized(chat, SessionID(chat), decision)
	if got.Chosen != "p/candidate" || got.Economics == nil || got.Economics.Decision != "switch" {
		t.Fatalf("three-turn decision = %+v", got)
	}
	if got.Economics.BreakEvenTurns <= 2 || got.Economics.BreakEvenTurns >= 3 {
		t.Errorf("break-even = %v, want between two and three turns", got.Economics.BreakEvenTurns)
	}
}

func TestInitialAmortizedChoiceUsesConfidentCostToCompletion(t *testing.T) {
	r, chat, decision := economicRouter(4, 1)
	r.sessions = make(map[string]sessionState)
	decision.Chosen = "p/incumbent"
	got := r.applyAmortized(chat, SessionID(chat), decision)
	if got.Chosen != "p/candidate" || got.Reason != "initial lowest expected cost to completion" {
		t.Fatalf("initial decision = %+v", got)
	}
	decision.Profile.EstimateConfidence = 0.2
	got = r.applyAmortized(chat, SessionID(chat), decision)
	if got.Chosen != "p/incumbent" {
		t.Fatalf("low-confidence initial decision = %+v", got)
	}
}

func TestAmortizedRoutingRetainsOnLowConfidence(t *testing.T) {
	r, chat, decision := economicRouter(10, 0.4)
	got := r.applyAmortized(chat, SessionID(chat), decision)
	if got.Chosen != "p/incumbent" || got.Economics.Decision != "retain_low_confidence" {
		t.Fatalf("decision = %+v", got)
	}
}

func TestAmortizedRoutingUsesTurnEfficiency(t *testing.T) {
	r, chat, decision := economicRouter(4, 1)
	candidate := r.cfg.Catalog[1]
	candidate.TurnEfficiency.Medium = 0.25
	r.cfg.Catalog[1] = candidate
	got := r.applyAmortized(chat, SessionID(chat), decision)
	if got.Chosen != "p/incumbent" {
		t.Fatalf("inefficient candidate should be retained: %+v", got)
	}
	if got.Economics.ExpectedTurnsCandidate != 16 {
		t.Errorf("candidate turns = %d, want 16", got.Economics.ExpectedTurnsCandidate)
	}
}

func TestAmortizedRoutingRecognisesWarmCacheAfterSwitchBack(t *testing.T) {
	r, chat, decision := economicRouter(2, 1)
	sid := SessionID(chat)
	state := r.sessions[sid]
	state.Models["p/candidate"] = modelSessionState{CacheUntil: time.Now().Add(time.Minute), CacheFraction: 1}
	r.sessions[sid] = state
	got := r.applyAmortized(chat, sid, decision)
	if got.Chosen != "p/candidate" || !got.Economics.CandidateCacheWarm {
		t.Fatalf("warm switch-back decision = %+v", got)
	}
}

func TestTurnCostSplitsCacheablePrefixAndUncachedTail(t *testing.T) {
	r, chat, _ := economicRouter(1, 1)
	entry := config.CatalogEntry{ID: "p/candidate", CostPerMTokIn: 10, CostPerMTokOut: 20}
	// K=600 warm tokens at 0.1x, U=400 ordinary tokens, O=100 output.
	got := r.turnCost(chat, entry, 1000, 100, 0.6, true)
	want := ((600*0.1+400)*10 + 100*20) / 1e6
	if diff := got - want; diff < -1e-12 || diff > 1e-12 {
		t.Fatalf("turn cost = %.12f, want %.12f", got, want)
	}
}
