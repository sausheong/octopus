// Package insights records aggregate request economics without retaining
// prompts, responses, session identifiers, or provider credentials.
package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/octopus/config"
	"github.com/sausheong/octopus/router"
)

const ledgerVersion = 3
const maxRecentDecisions = 200

// Observation is the completed, provider-reported usage needed to calculate
// request economics. Chat content is used only to identify the cache TTL and
// is never retained by Tracker.
type Observation struct {
	Chat     llm.ChatRequest
	Model    string
	Decision router.Decision
	Usage    *llm.Usage
	Catalog  []config.CatalogEntry
}

// Tracker owns the persistent aggregate ledger.
type Tracker struct {
	mu        sync.RWMutex
	path      string
	now       func() time.Time
	ledger    ledger
	lastErr   string
	persistCh chan chan error
	stopCh    chan struct{}
	doneCh    chan struct{}
	closeOnce sync.Once
	closed    bool
}

type ledger struct {
	Version         int                      `json:"version"`
	CreatedAt       time.Time                `json:"created_at"`
	Days            map[string]*DayAggregate `json:"days"`
	RecentDecisions []RoutingDecision        `json:"recent_decisions,omitempty"`
}

// DayAggregate is persisted by local calendar date.
type DayAggregate struct {
	Requests               int64                      `json:"requests"`
	PricedRequests         int64                      `json:"priced_requests"`
	InputTokens            int64                      `json:"input_tokens"`
	OutputTokens           int64                      `json:"output_tokens"`
	CacheCreationTokens    int64                      `json:"cache_creation_tokens"`
	CacheReadTokens        int64                      `json:"cache_read_tokens"`
	ActualCostUSD          float64                    `json:"actual_cost_usd"`
	BaselineCostUSD        float64                    `json:"baseline_cost_usd"`
	RoutingSavingsUSD      float64                    `json:"routing_savings_usd"`
	CacheSavingsUSD        float64                    `json:"cache_savings_usd"`
	ClassifierOverheadUSD  float64                    `json:"classifier_overhead_usd"`
	NetSavingsUSD          float64                    `json:"net_savings_usd"`
	Models                 map[string]*ModelAggregate `json:"models"`
	AmortizedDecisions     int64                      `json:"amortized_decisions"`
	AmortizedSwitches      int64                      `json:"amortized_switches"`
	ForecastSavingsUSD     float64                    `json:"forecast_savings_usd"`
	Difficulties           map[string]int64           `json:"difficulties,omitempty"`
	Domains                map[string]int64           `json:"domains,omitempty"`
	Risks                  map[string]int64           `json:"risks,omitempty"`
	ClassificationSources  map[string]int64           `json:"classification_sources,omitempty"`
	ClassificationStatuses map[string]int64           `json:"classification_statuses,omitempty"`
	FallbacksObserved      int64                      `json:"fallbacks_observed,omitempty"`
}

// ModelAggregate supports the model breakdown without storing request data.
type ModelAggregate struct {
	Requests      int64   `json:"requests"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	ActualCostUSD float64 `json:"actual_cost_usd"`
}

// Report is the range-filtered API view used by Settings.
type Report struct {
	RangeDays        int                         `json:"range_days"`
	Summary          Summary                     `json:"summary"`
	Days             []DayPoint                  `json:"days"`
	Models           []ModelSummary              `json:"models"`
	RoutingDecisions []RoutingDecision           `json:"routing_decisions"`
	Methodology      string                      `json:"methodology"`
	LastError        string                      `json:"last_error,omitempty"`
	ClassifierCache  router.ClassifierCacheStats `json:"classifier_cache"`
}

type Summary struct {
	Requests               int64            `json:"requests"`
	PricedRequests         int64            `json:"priced_requests"`
	InputTokens            int64            `json:"input_tokens"`
	OutputTokens           int64            `json:"output_tokens"`
	CacheCreationTokens    int64            `json:"cache_creation_tokens"`
	CacheReadTokens        int64            `json:"cache_read_tokens"`
	ActualCostUSD          float64          `json:"actual_cost_usd"`
	BaselineCostUSD        float64          `json:"baseline_cost_usd"`
	RoutingSavingsUSD      float64          `json:"routing_savings_usd"`
	CacheSavingsUSD        float64          `json:"cache_savings_usd"`
	ClassifierOverheadUSD  float64          `json:"classifier_overhead_usd"`
	NetSavingsUSD          float64          `json:"net_savings_usd"`
	SavingsPercent         float64          `json:"savings_percent"`
	CacheHitPercent        float64          `json:"cache_hit_percent"`
	AmortizedDecisions     int64            `json:"amortized_decisions"`
	AmortizedSwitches      int64            `json:"amortized_switches"`
	ForecastSavingsUSD     float64          `json:"forecast_savings_usd"`
	Difficulties           map[string]int64 `json:"difficulties,omitempty"`
	Domains                map[string]int64 `json:"domains,omitempty"`
	Risks                  map[string]int64 `json:"risks,omitempty"`
	ClassificationSources  map[string]int64 `json:"classification_sources,omitempty"`
	ClassificationStatuses map[string]int64 `json:"classification_statuses,omitempty"`
	FallbacksObserved      int64            `json:"fallbacks_observed,omitempty"`
}

// RoutingDecision is the prompt-free audit trail behind the Switch economics
// table. It records forecasts and actual cache-token outcomes only.
type RoutingDecision struct {
	Timestamp              time.Time                            `json:"timestamp"`
	Strategy               string                               `json:"strategy"`
	ActualModel            string                               `json:"actual_model"`
	Incumbent              string                               `json:"incumbent"`
	Candidate              string                               `json:"candidate"`
	Decision               string                               `json:"decision"`
	ExpectedTurnsIncumbent int                                  `json:"expected_turns_incumbent"`
	ExpectedTurnsCandidate int                                  `json:"expected_turns_candidate"`
	Confidence             float64                              `json:"confidence"`
	StayCostUSD            float64                              `json:"stay_cost_usd"`
	SwitchCostUSD          float64                              `json:"switch_cost_usd"`
	EstimatedSavingsUSD    float64                              `json:"estimated_savings_usd"`
	BreakEvenTurns         float64                              `json:"break_even_turns,omitempty"`
	CandidateCacheWarm     bool                                 `json:"candidate_cache_warm"`
	CacheCreationTokens    int                                  `json:"cache_creation_tokens"`
	CacheReadTokens        int                                  `json:"cache_read_tokens"`
	Reason                 string                               `json:"reason"`
	CostMode               router.CostMode                      `json:"cost_mode,omitempty"`
	Breakdowns             map[string]router.CandidateBreakdown `json:"breakdowns,omitempty"`
	Background             bool                                 `json:"background,omitempty"`
	BackgroundName         string                               `json:"background_name,omitempty"`
	WorkflowAffinity       bool                                 `json:"workflow_affinity,omitempty"`
	LegacyChosen           string                               `json:"legacy_chosen,omitempty"`
	LegacyChanged          bool                                 `json:"legacy_changed,omitempty"`
	// The task profile is a bounded set of classifier outputs and numeric
	// estimates. It deliberately excludes request text and classifier reasoning.
	Difficulty               string  `json:"difficulty,omitempty"`
	Domain                   string  `json:"domain,omitempty"`
	Risk                     string  `json:"risk,omitempty"`
	NeedsReasoning           bool    `json:"needs_reasoning,omitempty"`
	NeedsVision              bool    `json:"needs_vision,omitempty"`
	NeedsTools               bool    `json:"needs_tools,omitempty"`
	EstimatedInputTokens     int     `json:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens    int     `json:"estimated_output_tokens,omitempty"`
	EstimateConfidence       float64 `json:"estimate_confidence,omitempty"`
	ClassificationConfidence float64 `json:"classification_confidence,omitempty"`
	ExpectedRemainingTurns   int     `json:"expected_remaining_turns,omitempty"`
	ClassificationSource     string  `json:"classification_source,omitempty"`
	ClassificationStatus     string  `json:"classification_status,omitempty"`
	ClassifierLatencyMS      int64   `json:"classifier_latency_ms,omitempty"`
	AppliedQualityFloor      float64 `json:"applied_quality_floor,omitempty"`
	QualityPolicy            string  `json:"quality_policy,omitempty"`
	InitialModel             string  `json:"initial_model,omitempty"`
	SelectedModel            string  `json:"selected_model,omitempty"`
	FallbackObserved         bool    `json:"fallback_observed,omitempty"`
	ProviderAttemptsMinimum  int     `json:"provider_attempts_minimum,omitempty"`
	ProviderOutcome          string  `json:"provider_outcome,omitempty"`
}

type DayPoint struct {
	Date            string  `json:"date"`
	Requests        int64   `json:"requests"`
	ActualCostUSD   float64 `json:"actual_cost_usd"`
	BaselineCostUSD float64 `json:"baseline_cost_usd"`
	NetSavingsUSD   float64 `json:"net_savings_usd"`
}

type ModelSummary struct {
	Model         string  `json:"model"`
	Requests      int64   `json:"requests"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	ActualCostUSD float64 `json:"actual_cost_usd"`
}

// NewTracker loads an existing ledger or starts an empty one. A corrupt ledger
// is reported through Report.LastError and never prevents the router starting.
func NewTracker(path string) *Tracker {
	return newTracker(path, time.Now)
}

func newTracker(path string, now func() time.Time) *Tracker {
	t := &Tracker{
		path: path, now: now,
		persistCh: make(chan chan error, 1), stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
	t.ledger = ledger{Version: ledgerVersion, CreatedAt: now(), Days: make(map[string]*DayAggregate)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		go t.persistLoop()
		return t
	}
	if err != nil {
		t.lastErr = fmt.Sprintf("read insights: %v", err)
		go t.persistLoop()
		return t
	}
	var loaded ledger
	if err := json.Unmarshal(data, &loaded); err != nil || (loaded.Version < 1 || loaded.Version > ledgerVersion) || loaded.Days == nil {
		if err == nil {
			err = fmt.Errorf("unsupported ledger version %d", loaded.Version)
		}
		t.lastErr = fmt.Sprintf("load insights: %v", err)
		go t.persistLoop()
		return t
	}
	loaded.Version = ledgerVersion
	t.ledger = loaded
	go t.persistLoop()
	return t
}

// Record adds one successful request and atomically persists the aggregate.
func (t *Tracker) Record(observation Observation) {
	calculation, ok := calculate(observation)
	if !ok {
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	dayKey := t.now().Format("2006-01-02")
	day := t.ledger.Days[dayKey]
	if day == nil {
		day = newDayAggregate()
		t.ledger.Days[dayKey] = day
	}
	if day.Models == nil {
		day.Models = make(map[string]*ModelAggregate)
	}
	ensureAuditMaps(day)
	day.Requests++
	if calculation.priced {
		day.PricedRequests++
	}
	day.InputTokens += int64(calculation.inputTokens)
	day.OutputTokens += int64(calculation.outputTokens)
	day.CacheCreationTokens += int64(calculation.cacheCreationTokens)
	day.CacheReadTokens += int64(calculation.cacheReadTokens)
	day.ActualCostUSD += calculation.actualCost
	day.BaselineCostUSD += calculation.baselineCost
	day.RoutingSavingsUSD += calculation.routingSavings
	day.CacheSavingsUSD += calculation.cacheSavings
	day.ClassifierOverheadUSD += calculation.classifierOverhead
	day.NetSavingsUSD += calculation.netSavings
	profile := observation.Decision.Profile
	classificationSource, classificationStatus := classificationAudit(observation.Decision)
	selectedModel := observation.Decision.Chosen
	initialModel := observation.Decision.InitialChosen
	if initialModel == "" {
		initialModel = selectedModel
	}
	fallbackObserved := selectedModel != "" && observation.Model != selectedModel
	providerAttempts := 1
	if fallbackObserved {
		// The server currently reports only the completing provider to Tracker.
		// A model mismatch proves at least one prior attempt, but not its exact
		// count or error, so this is explicitly a lower bound.
		providerAttempts = 2
	}
	recent := RoutingDecision{
		Timestamp: t.now(), Strategy: observation.Decision.Strategy, ActualModel: observation.Model,
		Decision: observation.Decision.Reason, Reason: observation.Decision.Reason,
		CostMode: observation.Decision.CostMode, Breakdowns: observation.Decision.Breakdowns,
		Background: observation.Decision.Background, BackgroundName: observation.Decision.BackgroundName,
		WorkflowAffinity: observation.Decision.WorkflowAffinity,
		LegacyChosen:     observation.Decision.LegacyChosen, LegacyChanged: observation.Decision.LegacyChanged,
		CacheCreationTokens: calculation.cacheCreationTokens, CacheReadTokens: calculation.cacheReadTokens,
		Difficulty: boundedDifficulty(profile.Difficulty), Domain: boundedDomain(profile.Domain),
		Risk:           boundedRisk(profile.Risk),
		NeedsReasoning: profile.NeedsReasoning, NeedsVision: profile.NeedsVision, NeedsTools: profile.NeedsTools,
		EstimatedInputTokens: nonNegative(profile.EstTokensIn), EstimatedOutputTokens: nonNegative(profile.EstTokensOut),
		EstimateConfidence: boundedUnit(profile.EstimateConfidence), ExpectedRemainingTurns: boundedTurns(profile.ExpectedRemainingTurns),
		ClassificationConfidence: boundedUnit(profile.ClassificationConfidence),
		ClassificationSource:     classificationSource, ClassificationStatus: classificationStatus,
		ClassifierLatencyMS: nonNegative64(observation.Decision.ClassifierLatencyMS),
		AppliedQualityFloor: boundedUnit(observation.Decision.AppliedQualityFloor),
		QualityPolicy:       boundedQualityPolicy(observation.Decision.QualityPolicy),
		InitialModel:        initialModel, SelectedModel: selectedModel, FallbackObserved: fallbackObserved,
		ProviderAttemptsMinimum: providerAttempts, ProviderOutcome: "completed",
	}
	day.Difficulties[recent.Difficulty]++
	day.Domains[recent.Domain]++
	day.Risks[recent.Risk]++
	day.ClassificationSources[recent.ClassificationSource]++
	day.ClassificationStatuses[recent.ClassificationStatus]++
	if fallbackObserved {
		day.FallbacksObserved++
	}
	if economics := observation.Decision.Economics; economics != nil {
		day.AmortizedDecisions++
		if economics.Decision == "switch" {
			day.AmortizedSwitches++
		}
		day.ForecastSavingsUSD += economics.EstimatedSavingsUSD
		recent.Incumbent, recent.Candidate, recent.Decision = economics.Incumbent, economics.Candidate, economics.Decision
		recent.ExpectedTurnsIncumbent, recent.ExpectedTurnsCandidate = economics.ExpectedTurnsIncumbent, economics.ExpectedTurnsCandidate
		recent.Confidence, recent.StayCostUSD, recent.SwitchCostUSD = economics.Confidence, economics.StayCostUSD, economics.SwitchCostUSD
		recent.EstimatedSavingsUSD, recent.BreakEvenTurns = economics.EstimatedSavingsUSD, economics.BreakEvenTurns
		recent.CandidateCacheWarm = economics.CandidateCacheWarm
	}
	t.ledger.RecentDecisions = append(t.ledger.RecentDecisions, recent)
	if len(t.ledger.RecentDecisions) > maxRecentDecisions {
		t.ledger.RecentDecisions = append([]RoutingDecision(nil), t.ledger.RecentDecisions[len(t.ledger.RecentDecisions)-maxRecentDecisions:]...)
	}
	model := day.Models[observation.Model]
	if model == nil {
		model = &ModelAggregate{}
		day.Models[observation.Model] = model
	}
	model.Requests++
	model.InputTokens += int64(calculation.inputTokens + calculation.cacheCreationTokens + calculation.cacheReadTokens)
	model.OutputTokens += int64(calculation.outputTokens)
	model.ActualCostUSD += calculation.chosenActualCost
	t.mu.Unlock()
	select {
	case t.persistCh <- nil:
	default:
	}
}

func (t *Tracker) persistLoop() {
	defer close(t.doneCh)
	for {
		select {
		case done := <-t.persistCh:
			err := t.persistSnapshot()
			if done != nil {
				done <- err
				close(done)
			}
		case <-t.stopCh:
			_ = t.persistSnapshot()
			return
		}
	}
}

func (t *Tracker) persistSnapshot() error {
	t.mu.RLock()
	data, err := json.MarshalIndent(t.ledger, "", "  ")
	t.mu.RUnlock()
	if err == nil {
		err = atomicWrite(t.path, data)
	}
	t.mu.Lock()
	if err != nil {
		t.lastErr = fmt.Sprintf("save insights: %v", err)
	} else {
		t.lastErr = ""
	}
	t.mu.Unlock()
	return err
}

// Flush waits until all observations recorded before the call are represented
// in the durable snapshot.
func (t *Tracker) Flush(ctx context.Context) error {
	done := make(chan error, 1)
	select {
	case t.persistCh <- done:
	case <-ctx.Done():
		return ctx.Err()
	case <-t.doneCh:
		return nil
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-t.doneCh:
		return nil
	}
}

// Close performs a final durable flush and stops the writer. It is idempotent.
func (t *Tracker) Close(ctx context.Context) error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.closeOnce.Do(func() { close(t.stopCh) })
	select {
	case <-t.doneCh:
		t.mu.RLock()
		defer t.mu.RUnlock()
		if t.lastErr != "" {
			return errors.New(t.lastErr)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newDayAggregate() *DayAggregate {
	day := &DayAggregate{Models: make(map[string]*ModelAggregate)}
	ensureAuditMaps(day)
	return day
}

func ensureAuditMaps(day *DayAggregate) {
	if day.Difficulties == nil {
		day.Difficulties = make(map[string]int64)
	}
	if day.Domains == nil {
		day.Domains = make(map[string]int64)
	}
	if day.Risks == nil {
		day.Risks = make(map[string]int64)
	}
	if day.ClassificationSources == nil {
		day.ClassificationSources = make(map[string]int64)
	}
	if day.ClassificationStatuses == nil {
		day.ClassificationStatuses = make(map[string]int64)
	}
}

// classificationAudit maps existing prompt-free decision facts into a small,
// stable vocabulary. It does not guess provider errors or retain free-form
// classifier output. Router-supplied explicit audit fields can replace this
// compatibility mapping when available.
func classificationAudit(decision router.Decision) (source, status string) {
	if source, status := boundedClassificationSource(decision.ClassificationSource), boundedClassificationStatus(decision.ClassificationStatus); source != "" || status != "" {
		if source == "" {
			source = "unknown"
		}
		if status == "" {
			status = "unknown"
		}
		return source, status
	}
	if source := boundedClassificationSource(decision.Profile.ClassificationSource); source != "" {
		switch source {
		case "classifier", "classifier_cache":
			return source, "completed"
		case "conservative_fallback":
			return source, "fallback"
		default:
			return source, "skipped"
		}
	}
	switch {
	case decision.Background:
		return "deterministic_background", "skipped"
	case decision.ClassifierModel != "" && decision.ClassifierUsage != nil:
		return "classifier", "completed"
	case decision.ClassifierModel != "":
		return "classifier", "outcome_unavailable"
	case decision.Reason == "sticky session affinity":
		return "deterministic_sticky", "skipped"
	case decision.Profile.Difficulty == "trivial":
		return "deterministic_trivial", "skipped"
	default:
		return "conservative_fallback", "outcome_unavailable"
	}
}

func boundedClassificationSource(value string) string {
	switch value {
	case "classifier", "classifier_cache", "classifier_coalesced", "conservative_fallback", "allowlisted_background", "deterministic_background", "deterministic_sticky", "explicit_policy":
		return value
	default:
		return ""
	}
}

func boundedClassificationStatus(value string) string {
	switch value {
	case "completed", "no_user_turn", "skipped_allowlisted_background", "blocked_by_data_policy", "classifier_unavailable", "classifier_failed", "skipped", "fallback", "outcome_unavailable":
		return value
	default:
		return ""
	}
}

func boundedDifficulty(value string) string {
	switch value {
	case "trivial", "low", "medium", "high":
		return value
	default:
		return "unknown"
	}
}

func boundedDomain(value string) string {
	switch value {
	case "code", "writing", "qa", "math", "other":
		return value
	default:
		return "unknown"
	}
}

func boundedRisk(value string) string {
	switch value {
	case "ordinary", "important", "critical":
		return value
	default:
		return "unknown"
	}
}

func boundedQualityPolicy(value string) string {
	switch value {
	case "none", "strict", "minimum_quality":
		return value
	default:
		return ""
	}
}

func boundedUnit(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return 0
	}
	return value
}

func boundedTurns(value int) int {
	if value < 0 || value > 50 {
		return 0
	}
	return value
}

func nonNegative64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

type calculation struct {
	inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int
	actualCost, chosenActualCost, baselineCost                      float64
	routingSavings, cacheSavings, classifierOverhead, netSavings    float64
	priced                                                          bool
}

func calculate(observation Observation) (calculation, bool) {
	if observation.Usage == nil || observation.Model == "" {
		return calculation{}, false
	}
	entries := make(map[string]config.CatalogEntry, len(observation.Catalog))
	for _, entry := range observation.Catalog {
		entries[entry.ID] = entry
	}
	chosen, ok := entries[observation.Model]
	if !ok {
		return calculation{}, false
	}
	baseline := chosen
	for _, id := range observation.Decision.Eligible {
		entry, exists := entries[id]
		if exists && entry.Quality > baseline.Quality {
			baseline = entry
		}
	}
	usage := observation.Usage
	input := nonNegative(usage.InputTokens)
	output := nonNegative(usage.OutputTokens)
	created := nonNegative(usage.CacheCreationInputTokens)
	read := nonNegative(usage.CacheReadInputTokens)
	allInput := input + created + read
	chosenUncached := tokenCost(allInput, output, chosen)
	writeMultiplier := router.CacheWrite5mInputMultiplier
	if router.CacheTTL(observation.Chat) == time.Hour {
		writeMultiplier = router.CacheWrite1HourInputMultiplier
	}
	chosenActual := (float64(input)+float64(created)*writeMultiplier+float64(read)*router.CacheReadInputMultiplier)/1e6*chosen.CostPerMTokIn +
		float64(output)/1e6*chosen.CostPerMTokOut
	baselineCost := tokenCost(allInput, output, baseline)
	classifierOverhead := usageCost(observation.Decision.ClassifierUsage, entries[observation.Decision.ClassifierModel])
	routingSavings := baselineCost - chosenUncached
	cacheSavings := chosenUncached - chosenActual
	netSavings := routingSavings + cacheSavings - classifierOverhead
	return calculation{
		inputTokens: input, outputTokens: output, cacheCreationTokens: created, cacheReadTokens: read,
		actualCost: chosenActual + classifierOverhead, chosenActualCost: chosenActual, baselineCost: baselineCost,
		routingSavings: routingSavings, cacheSavings: cacheSavings, classifierOverhead: classifierOverhead,
		netSavings: netSavings,
		priced:     chosen.CostPerMTokIn > 0 || chosen.CostPerMTokOut > 0 || baseline.CostPerMTokIn > 0 || baseline.CostPerMTokOut > 0,
	}, true
}

func tokenCost(input, output int, entry config.CatalogEntry) float64 {
	return float64(input)/1e6*entry.CostPerMTokIn + float64(output)/1e6*entry.CostPerMTokOut
}

func usageCost(usage *llm.Usage, entry config.CatalogEntry) float64 {
	if usage == nil {
		return 0
	}
	input := nonNegative(usage.InputTokens) + nonNegative(usage.CacheCreationInputTokens) + nonNegative(usage.CacheReadInputTokens)
	return tokenCost(input, nonNegative(usage.OutputTokens), entry)
}

func nonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

// Report returns a dense daily series ending today. Ranges are bounded to keep
// the local settings response compact.
func (t *Tracker) Report(days int) Report {
	if days < 1 {
		days = 30
	}
	if days > 365 {
		days = 365
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	report := Report{
		RangeDays: days,
		Days:      make([]DayPoint, 0, days),
		Methodology: "Quality-baseline savings compare each completed request with the highest-quality eligible catalog model at uncached list price. " +
			"Actual cost uses provider-reported tokens, configured model prices, prompt-cache read/write multipliers, and classifier overhead. " +
			"Routing audit data contains bounded profile labels and numeric policy facts only; fallback attempt counts are lower bounds because only the completing provider is observed.",
		LastError:       t.lastErr,
		ClassifierCache: router.CurrentClassifierCacheStats(),
	}
	today := dateOnly(t.now())
	start := today.AddDate(0, 0, -(days - 1))
	for index := len(t.ledger.RecentDecisions) - 1; index >= 0; index-- {
		decision := t.ledger.RecentDecisions[index]
		if !dateOnly(decision.Timestamp).Before(start) {
			report.RoutingDecisions = append(report.RoutingDecisions, decision)
		}
	}
	modelTotals := make(map[string]*ModelAggregate)
	for offset := days - 1; offset >= 0; offset-- {
		date := today.AddDate(0, 0, -offset)
		key := date.Format("2006-01-02")
		point := DayPoint{Date: key}
		if day := t.ledger.Days[key]; day != nil {
			addDay(&report.Summary, day)
			point.Requests = day.Requests
			point.ActualCostUSD = day.ActualCostUSD
			point.BaselineCostUSD = day.BaselineCostUSD
			point.NetSavingsUSD = day.NetSavingsUSD
			for model, aggregate := range day.Models {
				total := modelTotals[model]
				if total == nil {
					total = &ModelAggregate{}
					modelTotals[model] = total
				}
				total.Requests += aggregate.Requests
				total.InputTokens += aggregate.InputTokens
				total.OutputTokens += aggregate.OutputTokens
				total.ActualCostUSD += aggregate.ActualCostUSD
			}
		}
		report.Days = append(report.Days, point)
	}
	if report.Summary.BaselineCostUSD != 0 {
		report.Summary.SavingsPercent = report.Summary.NetSavingsUSD / report.Summary.BaselineCostUSD * 100
	}
	totalInput := report.Summary.InputTokens + report.Summary.CacheCreationTokens + report.Summary.CacheReadTokens
	if totalInput > 0 {
		report.Summary.CacheHitPercent = float64(report.Summary.CacheReadTokens) / float64(totalInput) * 100
	}
	for model, aggregate := range modelTotals {
		report.Models = append(report.Models, ModelSummary{
			Model: model, Requests: aggregate.Requests, InputTokens: aggregate.InputTokens,
			OutputTokens: aggregate.OutputTokens, ActualCostUSD: aggregate.ActualCostUSD,
		})
	}
	sort.Slice(report.Models, func(i, j int) bool {
		if report.Models[i].Requests == report.Models[j].Requests {
			return report.Models[i].Model < report.Models[j].Model
		}
		return report.Models[i].Requests > report.Models[j].Requests
	})
	return report
}

func addDay(summary *Summary, day *DayAggregate) {
	summary.Requests += day.Requests
	summary.PricedRequests += day.PricedRequests
	summary.InputTokens += day.InputTokens
	summary.OutputTokens += day.OutputTokens
	summary.CacheCreationTokens += day.CacheCreationTokens
	summary.CacheReadTokens += day.CacheReadTokens
	summary.ActualCostUSD += day.ActualCostUSD
	summary.BaselineCostUSD += day.BaselineCostUSD
	summary.RoutingSavingsUSD += day.RoutingSavingsUSD
	summary.CacheSavingsUSD += day.CacheSavingsUSD
	summary.ClassifierOverheadUSD += day.ClassifierOverheadUSD
	summary.NetSavingsUSD += day.NetSavingsUSD
	summary.AmortizedDecisions += day.AmortizedDecisions
	summary.AmortizedSwitches += day.AmortizedSwitches
	summary.ForecastSavingsUSD += day.ForecastSavingsUSD
	summary.FallbacksObserved += day.FallbacksObserved
	mergeCounts(&summary.Difficulties, day.Difficulties)
	mergeCounts(&summary.Domains, day.Domains)
	mergeCounts(&summary.Risks, day.Risks)
	mergeCounts(&summary.ClassificationSources, day.ClassificationSources)
	mergeCounts(&summary.ClassificationStatuses, day.ClassificationStatuses)
}

func mergeCounts(destination *map[string]int64, source map[string]int64) {
	if len(source) == 0 {
		return
	}
	if *destination == nil {
		*destination = make(map[string]int64, len(source))
	}
	for key, count := range source {
		(*destination)[key] += count
	}
}

func dateOnly(value time.Time) time.Time {
	year, month, day := value.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, value.Location())
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".insights-*.json")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
