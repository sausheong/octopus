package router

import (
	"math"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/octopus/config"
)

// applyAmortized compares the incumbent's expected cost to completion with
// every eligible alternative. The candidate pays a cold first-turn cache write
// unless that model still has a warm per-session cache of its own.
func (r *Router) applyAmortized(chat llm.ChatRequest, sid string, decision Decision) Decision {
	if decision.NoEligible || decision.Chosen == "" {
		return decision
	}
	now := time.Now()
	state, ok := r.sessionSnapshot(sid, now)
	if !ok || state.Incumbent == "" {
		return r.applyInitialAmortized(chat, decision)
	}

	incumbentEligible := false
	for _, id := range decision.Eligible {
		if id == state.Incumbent {
			incumbentEligible = true
			break
		}
	}
	if !incumbentEligible {
		decision.Reason = "incumbent ineligible; highest balanced score"
		return decision
	}

	entries := make(map[string]config.CatalogEntry, len(r.cfg.Catalog))
	for _, entry := range r.cfg.Catalog {
		entries[entry.ID] = entry
	}
	incumbent := entries[state.Incumbent]
	cacheFraction := inferredCacheFraction(state)
	incumbentTurns := turnsForModel(decision.Profile.ExpectedRemainingTurns, incumbent, decision.Profile.Difficulty)
	incumbentWarm := r.usableCacheWarm(chat, state, incumbent.ID, now)
	incumbentFraction := cacheFractionForModel(state, incumbent.ID, cacheFraction)
	stay := r.forecastCost(chat, decision.Profile, state, incumbent, incumbentTurns, incumbentWarm, incumbentFraction)
	warmIncumbent := r.turnCost(chat, incumbent, forecastInput(decision.Profile.EstTokensIn, state.InputGrowthEMA, 0), forecastOutput(decision.Profile.EstTokensOut, state.OutputEMA), incumbentFraction, true)

	bestID := ""
	bestCost := math.Inf(1)
	bestTurns := 0
	bestFirst := 0.0
	bestWarm := 0.0
	bestCacheWarm := false
	for _, id := range decision.Eligible {
		if id == incumbent.ID {
			continue
		}
		candidate := entries[id]
		turns := turnsForModel(decision.Profile.ExpectedRemainingTurns, candidate, decision.Profile.Difficulty)
		cacheWarm := r.usableCacheWarm(chat, state, candidate.ID, now)
		candidateFraction := cacheFraction
		if cacheWarm {
			candidateFraction = cacheFractionForModel(state, candidate.ID, cacheFraction)
		}
		cost := r.forecastCost(chat, decision.Profile, state, candidate, turns, cacheWarm, candidateFraction)
		if cost < bestCost {
			bestID = candidate.ID
			bestCost = cost
			bestTurns = turns
			bestCacheWarm = cacheWarm
			input := forecastInput(decision.Profile.EstTokensIn, state.InputGrowthEMA, 0)
			output := forecastOutput(decision.Profile.EstTokensOut, state.OutputEMA)
			bestFirst = r.turnCost(chat, candidate, input, output, candidateFraction, cacheWarm)
			bestWarm = r.turnCost(chat, candidate, input, output, candidateFraction, true)
		}
	}
	if bestID == "" {
		decision.Chosen = incumbent.ID
		decision.Reason = "amortized retain; no alternative eligible"
		return decision
	}

	savings := stay - bestCost
	required := math.Max(r.cfg.Routing.MinSwitchSavingsUSD, stay*r.cfg.Routing.MinSwitchSavingsPct)
	breakEven := 0.0
	if denominator := warmIncumbent - bestWarm; denominator > 0 {
		breakEven = (bestFirst - bestWarm) / denominator
		if breakEven < 0 {
			breakEven = 0
		}
	}
	economics := &SwitchEconomics{
		Incumbent: incumbent.ID, Candidate: bestID,
		ExpectedTurnsIncumbent: incumbentTurns, ExpectedTurnsCandidate: bestTurns,
		Confidence:  decision.Profile.EstimateConfidence,
		StayCostUSD: stay, SwitchCostUSD: bestCost,
		FirstCandidateCostUSD: bestFirst, WarmIncumbentCostUSD: warmIncumbent,
		WarmCandidateCostUSD: bestWarm, EstimatedSavingsUSD: savings,
		RequiredSavingsUSD: required, BreakEvenTurns: breakEven,
		CandidateCacheWarm: bestCacheWarm,
	}
	decision.Economics = economics

	if decision.Profile.EstimateConfidence < r.cfg.Routing.SwitchConfidence {
		decision.Chosen = incumbent.ID
		decision.Reason = "amortized retain; forecast confidence below threshold"
		economics.Decision = "retain_low_confidence"
		return decision
	}
	if savings > required {
		decision.Chosen = bestID
		decision.Reason = "amortized switch; expected cost-to-completion savings exceed threshold"
		economics.Decision = "switch"
		return decision
	}
	decision.Chosen = incumbent.ID
	decision.Reason = "amortized retain; expected savings do not cover switch threshold"
	economics.Decision = "retain"
	return decision
}

func (r *Router) applyInitialAmortized(chat llm.ChatRequest, decision Decision) Decision {
	if decision.Profile.EstimateConfidence < r.cfg.Routing.SwitchConfidence {
		decision.Reason = "initial highest balanced score; forecast confidence below threshold"
		return decision
	}
	entries := make(map[string]config.CatalogEntry, len(r.cfg.Catalog))
	for _, entry := range r.cfg.Catalog {
		entries[entry.ID] = entry
	}
	state := sessionState{}
	chosenEntry, ok := entries[decision.Chosen]
	if !ok {
		return decision
	}
	chosenTurns := turnsForModel(decision.Profile.ExpectedRemainingTurns, chosenEntry, decision.Profile.Difficulty)
	chosenCost := r.forecastCost(chat, decision.Profile, state, chosenEntry, chosenTurns, false, 1)
	bestID, bestCost := decision.Chosen, chosenCost
	for _, id := range decision.Eligible {
		entry, exists := entries[id]
		if !exists {
			continue
		}
		turns := turnsForModel(decision.Profile.ExpectedRemainingTurns, entry, decision.Profile.Difficulty)
		cost := r.forecastCost(chat, decision.Profile, state, entry, turns, false, 1)
		if cost < bestCost {
			bestID, bestCost = id, cost
		}
	}
	savings := chosenCost - bestCost
	required := math.Max(r.cfg.Routing.MinSwitchSavingsUSD, chosenCost*r.cfg.Routing.MinSwitchSavingsPct)
	if bestID != decision.Chosen && savings > required {
		decision.Chosen = bestID
		decision.Reason = "initial lowest expected cost to completion"
		return decision
	}
	decision.Reason = "initial highest balanced score; forecast saving below threshold"
	return decision
}

func turnsForModel(base int, entry config.CatalogEntry, difficulty string) int {
	if base < 1 {
		base = 1
	}
	turns := int(math.Ceil(float64(base) / entry.TurnEfficiency.ForDifficulty(difficulty)))
	if turns < 1 {
		return 1
	}
	if turns > 50 {
		return 50
	}
	return turns
}

func inferredCacheFraction(state sessionState) float64 {
	fraction := 0.0
	for _, model := range state.Models {
		if model.CacheFraction > fraction {
			fraction = model.CacheFraction
		}
	}
	if fraction <= 0 || fraction > 1 {
		return 1
	}
	return fraction
}

func cacheFractionForModel(state sessionState, model string, fallback float64) float64 {
	if value := state.Models[model].CacheFraction; value > 0 && value <= 1 {
		return value
	}
	return fallback
}

func modelCacheWarm(state sessionState, model string, now time.Time) bool {
	modelState, ok := state.Models[model]
	return ok && modelState.CacheUntil.After(now)
}

func (r *Router) usableCacheWarm(chat llm.ChatRequest, state sessionState, model string, now time.Time) bool {
	return r.cfg.Routing.CacheAware && r.cachingModels[model] && CacheTTL(chat) > 0 && modelCacheWarm(state, model, now)
}

func forecastInput(base int, growth float64, turn int) int {
	value := float64(base) + growth*float64(turn)
	if value < 0 {
		return 0
	}
	return int(math.Ceil(value))
}

func forecastOutput(base int, observedEMA float64) int {
	if observedEMA > float64(base) {
		return int(math.Ceil(observedEMA))
	}
	if base < 0 {
		return 0
	}
	return base
}

func (r *Router) forecastCost(chat llm.ChatRequest, profile TaskProfile, state sessionState, entry config.CatalogEntry, turns int, firstWarm bool, cacheFraction float64) float64 {
	total := 0.0
	output := forecastOutput(profile.EstTokensOut, state.OutputEMA)
	for turn := 0; turn < turns; turn++ {
		warm := turn > 0 || firstWarm
		input := forecastInput(profile.EstTokensIn, state.InputGrowthEMA, turn)
		total += r.turnCost(chat, entry, input, output, cacheFraction, warm)
	}
	return total
}

// turnCost splits input into a cacheable prefix K and uncached tail U.
// Cold requests pay the provider's cache-write multiplier for K; warm requests
// pay the cache-read multiplier. Providers without prompt caching pay 1x.
func (r *Router) turnCost(chat llm.ChatRequest, entry config.CatalogEntry, input, output int, cacheFraction float64, warm bool) float64 {
	if input < 0 {
		input = 0
	}
	if output < 0 {
		output = 0
	}
	multiplier := 1.0
	if r.cfg.Routing.CacheAware && r.cachingModels[entry.ID] && CacheTTL(chat) > 0 {
		if warm {
			multiplier = CacheReadInputMultiplier
		} else if CacheTTL(chat) == time.Hour {
			multiplier = CacheWrite1HourInputMultiplier
		} else {
			multiplier = CacheWrite5mInputMultiplier
		}
	} else {
		cacheFraction = 0
	}
	if cacheFraction < 0 {
		cacheFraction = 0
	}
	if cacheFraction > 1 {
		cacheFraction = 1
	}
	cacheable := float64(input) * cacheFraction
	uncached := float64(input) - cacheable
	return ((cacheable*multiplier+uncached)*entry.CostPerMTokIn + float64(output)*entry.CostPerMTokOut) / 1e6
}
