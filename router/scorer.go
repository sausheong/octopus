package router

import (
	"math"
	"sort"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/octopus/config"
)

// HighQualityFloor is the quality a "high"-difficulty task wants. For such
// tasks, models below the floor are filtered out — but only when at least one
// at/above-floor model is otherwise eligible, so we never empty the set on
// quality alone. This keeps hard tasks on capable models without a fragile
// score penalty that cheap+fast models can overwhelm.
const HighQualityFloor = 0.85

// CostMode controls how request cost is converted to a score. Relative keeps
// the historical catalogue-relative behaviour. Absolute uses a stable dollar
// reference, so unrelated catalogue changes cannot alter a model's utility.
type CostMode string

const (
	CostModeRelative CostMode = "relative"
	CostModeAbsolute CostMode = "absolute"
)

// ScoringOptions provides an opt-in path to stable, dollar-aware scoring while
// preserving the existing Score APIs. Its zero value selects legacy scoring.
type ScoringOptions struct {
	CostMode         CostMode
	ReferenceCostUSD float64
	QualityFloors    map[string]float64
	ReasoningBonus   *float64
}

// AbsoluteScoringOptions returns conservative defaults for stable scoring.
func AbsoluteScoringOptions() ScoringOptions {
	return ScoringOptions{
		CostMode:         CostModeAbsolute,
		ReferenceCostUSD: 0.10,
		QualityFloors:    map[string]float64{"high": HighQualityFloor},
	}
}

// CandidateBreakdown is safe to log: it contains catalogue values and numeric
// scoring inputs, but no prompt, response, API key, or session identifier.
type CandidateBreakdown struct {
	ModelID              string   `json:"model_id"`
	Eligible             bool     `json:"eligible"`
	RejectionReasons     []string `json:"rejection_reasons,omitempty"`
	RequestCostUSD       float64  `json:"request_cost_usd"`
	InputPriceMultiplier float64  `json:"input_price_multiplier"`
	QualityRaw           float64  `json:"quality_raw"`
	QualityUtility       float64  `json:"quality_utility"`
	CostUtility          float64  `json:"cost_utility"`
	SpeedRaw             float64  `json:"speed_raw"`
	SpeedUtility         float64  `json:"speed_utility"`
	QualityContribution  float64  `json:"quality_contribution"`
	CostContribution     float64  `json:"cost_contribution"`
	SpeedContribution    float64  `json:"speed_contribution"`
	ReasoningBonus       float64  `json:"reasoning_bonus"`
	TotalScore           float64  `json:"total_score"`
}

// Decision is the structured record of one routing choice. Logged per request.
type Decision struct {
	Chosen     string
	Profile    TaskProfile
	Eligible   []string
	Scores     map[string]float64
	Weights    config.Weights
	Reason     string
	Reasoning  llm.ReasoningMode // recommended mode when an attempted candidate supports reasoning
	NoEligible bool              // true when no catalog model passed the capability filter
	// DataPolicy records the placement boundary applied to this decision.
	// RemoteFallbackBlocked is true when every attempted candidate is local.
	DataPolicy            string
	RemoteFallbackBlocked bool
	// ClassifierModel and ClassifierUsage let Insights include routing overhead
	// in request economics. They are empty when classification was skipped.
	ClassifierModel string
	ClassifierUsage *llm.Usage
	// MaxAttempts bounds provider fallback for this request.
	MaxAttempts int
	// Strategy and Economics explain whether a conversation was retained or
	// switched after comparing expected cost to completion. Economics is nil
	// for initial, sticky, and ordinary per-turn choices.
	Strategy  string
	Economics *SwitchEconomics
	// Breakdowns explains both rejected and scored catalogue candidates.
	Breakdowns                        map[string]CandidateBreakdown `json:"breakdowns,omitempty"`
	CostMode                          CostMode                      `json:"cost_mode,omitempty"`
	Background                        bool                          `json:"background,omitempty"`
	BackgroundName                    string                        `json:"background_name,omitempty"`
	BackgroundConversationIndependent bool                          `json:"-"`
	WorkflowAffinity                  bool                          `json:"workflow_affinity,omitempty"`
	// RoutingSessionID is internal metadata used to isolate an allowlisted
	// background request from the main conversation's incumbent/cache state.
	RoutingSessionID string `json:"-"`
	WorkflowID       string `json:"-"`
	LegacyChosen     string `json:"legacy_chosen,omitempty"`
	LegacyChanged    bool   `json:"legacy_changed,omitempty"`
}

// SwitchEconomics is safe to log and persist: it contains model IDs and
// numeric forecasts, never prompts, responses, or session identifiers.
type SwitchEconomics struct {
	Incumbent              string  `json:"incumbent"`
	Candidate              string  `json:"candidate"`
	Decision               string  `json:"decision"`
	ExpectedTurnsIncumbent int     `json:"expected_turns_incumbent"`
	ExpectedTurnsCandidate int     `json:"expected_turns_candidate"`
	Confidence             float64 `json:"confidence"`
	StayCostUSD            float64 `json:"stay_cost_usd"`
	SwitchCostUSD          float64 `json:"switch_cost_usd"`
	FirstCandidateCostUSD  float64 `json:"first_candidate_cost_usd"`
	WarmIncumbentCostUSD   float64 `json:"warm_incumbent_cost_usd"`
	WarmCandidateCostUSD   float64 `json:"warm_candidate_cost_usd"`
	EstimatedSavingsUSD    float64 `json:"estimated_savings_usd"`
	RequiredSavingsUSD     float64 `json:"required_savings_usd"`
	BreakEvenTurns         float64 `json:"break_even_turns,omitempty"`
	CandidateCacheWarm     bool    `json:"candidate_cache_warm"`
}

// eligible applies the hard capability filter: a model survives only if it
// satisfies every capability the profile requires and can hold the estimated
// token footprint.
func eligible(p TaskProfile, e config.CatalogEntry) bool {
	return len(eligibilityReasons(p, e)) == 0
}

func eligibilityReasons(p TaskProfile, e config.CatalogEntry) []string {
	var reasons []string
	if p.NeedsVision && !e.Caps.Vision {
		reasons = append(reasons, "vision capability required")
	}
	if p.NeedsTools && !e.Caps.Tools {
		reasons = append(reasons, "tool capability required")
	}
	if p.EstTokensIn+p.EstTokensOut > e.Caps.MaxContext {
		reasons = append(reasons, "estimated tokens exceed context limit")
	}
	// A model whose output limit is below the expected response would reject
	// the request outright, so filter it out here rather than discovering it
	// at the backend. Zero means the catalog entry declares no output limit.
	if e.Caps.MaxOutputTokens > 0 && p.EstTokensOut > e.Caps.MaxOutputTokens {
		reasons = append(reasons, "estimated output exceeds output limit")
	}
	return reasons
}

func reqCostWithInputMultiplier(p TaskProfile, e config.CatalogEntry, inputMultiplier float64) float64 {
	return float64(p.EstTokensIn)/1e6*e.CostPerMTokIn*inputMultiplier +
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
// deterministic tie-break by catalog order. Reasoning support is a preference,
// not a hard requirement: ordinary models can still serve a request when no
// reasoning-capable model is available.
func Score(p TaskProfile, catalog []config.CatalogEntry, w config.Weights) Decision {
	return ScoreWithInputMultipliers(p, catalog, w, nil)
}

// ScoreWithInputMultipliers applies per-model input-token price multipliers.
// Missing entries use ordinary uncached pricing (1x).
func ScoreWithInputMultipliers(p TaskProfile, catalog []config.CatalogEntry, w config.Weights, multipliers map[string]float64) Decision {
	return ScoreWithOptions(p, catalog, w, multipliers, ScoringOptions{})
}

// ScoreWithOptions scores candidates with explicit policy options. This is the
// integration point for configuration-backed absolute scoring.
func ScoreWithOptions(p TaskProfile, catalog []config.CatalogEntry, w config.Weights, multipliers map[string]float64, opts ScoringOptions) Decision {
	mode := opts.CostMode
	if mode == "" {
		mode = CostModeRelative
	}
	if mode != CostModeRelative && mode != CostModeAbsolute {
		mode = CostModeRelative
	}
	breakdowns := make(map[string]CandidateBreakdown, len(catalog))
	var elig []config.CatalogEntry
	for _, e := range catalog {
		multiplier := inputMultiplier(e.ID, multipliers)
		b := CandidateBreakdown{
			ModelID: e.ID, QualityRaw: e.Quality, SpeedRaw: e.Speed,
			InputPriceMultiplier: multiplier,
			RequestCostUSD:       reqCostWithInputMultiplier(p, e, multiplier),
		}
		b.RejectionReasons = eligibilityReasons(p, e)
		b.Eligible = len(b.RejectionReasons) == 0
		breakdowns[e.ID] = b
		if b.Eligible {
			elig = append(elig, e)
		}
	}
	// High-difficulty quality floor, applied as a filter only when it leaves
	// at least one model standing — never empties the set on quality alone.
	floor, applyFloor := qualityFloor(p.Difficulty, opts)
	if applyFloor {
		var aboveFloor []config.CatalogEntry
		for _, e := range elig {
			if e.Quality >= floor {
				aboveFloor = append(aboveFloor, e)
			}
		}
		if len(aboveFloor) > 0 {
			for _, e := range elig {
				if e.Quality < floor {
					b := breakdowns[e.ID]
					b.Eligible = false
					b.RejectionReasons = append(b.RejectionReasons, "below quality floor")
					breakdowns[e.ID] = b
				}
			}
			elig = aboveFloor
		}
	}
	if len(elig) == 0 {
		return Decision{
			Chosen:     "",
			Profile:    p,
			Eligible:   nil,
			Scores:     map[string]float64{},
			Weights:    w,
			Reason:     "no eligible model",
			NoEligible: true,
			Breakdowns: breakdowns,
			CostMode:   mode,
		}
	}

	qualities := make([]float64, len(elig))
	// costScores are computed in two passes so that free (zero-cost) models
	// always receive the maximum cost score of 1.0 regardless of the scale of
	// paid models' inverse-cost values. Paid models are normalised within [0,1)
	// relative to each other; free models sit above all of them at exactly 1.0.
	costScores := make([]float64, len(elig))
	speeds := make([]float64, len(elig))
	var paidInvCosts []float64
	paidIdx := make([]int, 0, len(elig))
	requestCosts := make([]float64, len(elig))
	for i, e := range elig {
		qualities[i] = e.Quality
		speeds[i] = e.Speed
		multiplier := inputMultiplier(e.ID, multipliers)
		c := reqCostWithInputMultiplier(p, e, multiplier)
		requestCosts[i] = c
		if c > 0 {
			paidInvCosts = append(paidInvCosts, 1/c)
			paidIdx = append(paidIdx, i)
		}
		// Free models left at 0; filled in below.
	}
	// Normalise paid models within [0, maxPaidNorm] so that free models at 1.0
	// always beat any paid model regardless of the cost weight. The gap of
	// 1.0 - maxPaidNorm is the minimum advantage a free model has over the
	// cheapest paid model in the cost dimension alone. 0.5 means a free model
	// needs at least half the cost weight less in other dimensions to win,
	// which is a meaningful preference without completely overriding quality.
	if mode == CostModeAbsolute {
		reference := opts.ReferenceCostUSD
		if reference <= 0 || math.IsNaN(reference) || math.IsInf(reference, 0) {
			reference = 0.10
		}
		for i, cost := range requestCosts {
			costScores[i] = 1 / (1 + cost/reference)
		}
	} else {
		const maxPaidNorm = 0.5
		if len(paidInvCosts) > 0 {
			normed := normalize(paidInvCosts)
			for k, i := range paidIdx {
				costScores[i] = normed[k] * maxPaidNorm
			}
		}
		for i := range elig {
			if costScores[i] == 0 && requestCosts[i] <= 0 {
				costScores[i] = 1.0
			}
		}
	}
	qn := normalize(qualities)
	cn := costScores // already in [0,1] — free=1.0, paid in [0,0.99]
	sn := normalize(speeds)
	if mode == CostModeAbsolute {
		// Quality and speed are catalogue values on a stable 0..1 scale.
		for i := range elig {
			qn[i] = clamp01(qualities[i])
			sn[i] = clamp01(speeds[i])
		}
	}

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
		qc, cc, sc := wq*qn[i], wc*cn[i], ws*sn[i]
		s := qc + cc + sc
		// Keep this bonus deliberately modest. It breaks otherwise close choices
		// in favour of native reasoning without making that optional feature
		// outweigh the configured quality/cost/speed policy.
		bonus := reasoningBonus(opts)
		if p.NeedsReasoning && e.Caps.Reasoning {
			s += bonus
		}
		b := breakdowns[e.ID]
		b.QualityUtility, b.CostUtility, b.SpeedUtility = qn[i], cn[i], sn[i]
		b.QualityContribution, b.CostContribution, b.SpeedContribution = qc, cc, sc
		if p.NeedsReasoning && e.Caps.Reasoning {
			b.ReasoningBonus = bonus
		}
		b.TotalScore = s
		breakdowns[e.ID] = b
		scores[e.ID] = s
		eligIDs[i] = e.ID
		// Strictly greater keeps the earliest (catalog-order) entry on ties.
		if s > bestScore {
			bestScore = s
			bestIdx = i
		}
	}

	// Sort eligible IDs by descending score so tryProviders walks them in the
	// same order as the primary selection. Catalog order is the tie-breaker:
	// stable sort preserves the original catalog positions for equal scores.
	sort.SliceStable(eligIDs, func(i, j int) bool {
		return scores[eligIDs[i]] > scores[eligIDs[j]]
	})

	return Decision{
		Chosen:     elig[bestIdx].ID,
		Profile:    p,
		Eligible:   eligIDs,
		Scores:     scores,
		Weights:    w,
		Reason:     "highest balanced score",
		Breakdowns: breakdowns,
		CostMode:   mode,
	}
}

func inputMultiplier(modelID string, multipliers map[string]float64) float64 {
	m := multipliers[modelID]
	if m <= 0 || math.IsNaN(m) || math.IsInf(m, 0) {
		return 1
	}
	return m
}

func qualityFloor(difficulty string, opts ScoringOptions) (float64, bool) {
	if opts.QualityFloors != nil {
		floor, ok := opts.QualityFloors[difficulty]
		return floor, ok && floor > 0
	}
	if difficulty == "high" {
		return HighQualityFloor, true
	}
	return 0, false
}

func reasoningBonus(opts ScoringOptions) float64 {
	if opts.ReasoningBonus != nil {
		return *opts.ReasoningBonus
	}
	return 0.1
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
