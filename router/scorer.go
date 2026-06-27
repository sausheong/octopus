package router

import "github.com/sausheong/llmrouter/config"

// HighQualityFloor is the quality a "high"-difficulty task wants. For such
// tasks, models below the floor are filtered out — but only when at least one
// at/above-floor model is otherwise eligible, so we never empty the set on
// quality alone. This keeps hard tasks on capable models without a fragile
// score penalty that cheap+fast models can overwhelm.
const HighQualityFloor = 0.85

// Decision is the structured record of one routing choice. Logged per request.
type Decision struct {
	Chosen   string
	Profile  TaskProfile
	Eligible []string
	Scores   map[string]float64
	Weights  config.Weights
	Reason   string
}

// eligible applies the hard capability filter: a model survives only if it
// satisfies every capability the profile requires and can hold the estimated
// token footprint.
func eligible(p TaskProfile, e config.CatalogEntry) bool {
	if p.NeedsVision && !e.Caps.Vision {
		return false
	}
	if p.NeedsTools && !e.Caps.Tools {
		return false
	}
	if p.NeedsReasoning && !e.Caps.Reasoning {
		return false
	}
	if e.Caps.MaxContext > 0 && p.EstTokensIn+p.EstTokensOut > e.Caps.MaxContext {
		return false
	}
	return true
}

// reqCost estimates the dollar cost of the request on a given model.
func reqCost(p TaskProfile, e config.CatalogEntry) float64 {
	return float64(p.EstTokensIn)/1e6*e.CostPerMTokIn +
		float64(p.EstTokensOut)/1e6*e.CostPerMTokOut
}

// normalize maps values to 0..1 across the set (max -> 1). If all values are
// equal (including all zero), every entry scores 1 so the term is neutral.
func normalize(vals []float64) []float64 {
	out := make([]float64, len(vals))
	var max float64
	for _, v := range vals {
		if v > max {
			max = v
		}
	}
	if max == 0 {
		for i := range out {
			out[i] = 1
		}
		return out
	}
	for i, v := range vals {
		out[i] = v / max
	}
	return out
}

// Score runs the full selection: filter, normalize sub-scores, weighted sum,
// deterministic tie-break by catalog order. Never errors; falls back to
// defaultModel when nothing is eligible.
func Score(p TaskProfile, catalog []config.CatalogEntry, w config.Weights, defaultModel string) Decision {
	var elig []config.CatalogEntry
	for _, e := range catalog {
		if eligible(p, e) {
			elig = append(elig, e)
		}
	}
	// High-difficulty quality floor, applied as a filter only when it leaves
	// at least one model standing — never empties the set on quality alone.
	if p.Difficulty == "high" {
		var aboveFloor []config.CatalogEntry
		for _, e := range elig {
			if e.Quality >= HighQualityFloor {
				aboveFloor = append(aboveFloor, e)
			}
		}
		if len(aboveFloor) > 0 {
			elig = aboveFloor
		}
	}
	if len(elig) == 0 {
		return Decision{
			Chosen:   defaultModel,
			Profile:  p,
			Eligible: nil,
			Scores:   map[string]float64{},
			Weights:  w,
			Reason:   "default_model fallback (no eligible)",
		}
	}

	qualities := make([]float64, len(elig))
	costs := make([]float64, len(elig)) // cost score = inverse cost
	speeds := make([]float64, len(elig))
	for i, e := range elig {
		qualities[i] = e.Quality
		c := reqCost(p, e)
		if c <= 0 {
			costs[i] = 0 // becomes 1 after normalize if all zero; else lowest
		} else {
			costs[i] = 1 / c
		}
		speeds[i] = e.Speed
	}
	qn := normalize(qualities)
	cn := normalize(costs)
	sn := normalize(speeds)

	wsum := w.Quality + w.Cost + w.Speed
	if wsum == 0 {
		wsum = 1
	}
	wq, wc, ws := w.Quality/wsum, w.Cost/wsum, w.Speed/wsum

	scores := make(map[string]float64, len(elig))
	eligIDs := make([]string, len(elig))
	bestIdx := 0
	bestScore := -1.0
	for i, e := range elig {
		s := wq*qn[i] + wc*cn[i] + ws*sn[i]
		scores[e.ID] = s
		eligIDs[i] = e.ID
		// Strictly greater keeps the earliest (catalog-order) entry on ties.
		if s > bestScore {
			bestScore = s
			bestIdx = i
		}
	}

	return Decision{
		Chosen:   elig[bestIdx].ID,
		Profile:  p,
		Eligible: eligIDs,
		Scores:   scores,
		Weights:  w,
		Reason:   "highest balanced score",
	}
}
