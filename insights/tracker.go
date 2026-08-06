// Package insights records aggregate request economics without retaining
// prompts, responses, session identifiers, or provider credentials.
package insights

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/octopus/config"
	"github.com/sausheong/octopus/router"
)

const ledgerVersion = 2
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
	mu      sync.RWMutex
	path    string
	now     func() time.Time
	ledger  ledger
	lastErr string
}

type ledger struct {
	Version         int                      `json:"version"`
	CreatedAt       time.Time                `json:"created_at"`
	Days            map[string]*DayAggregate `json:"days"`
	RecentDecisions []RoutingDecision        `json:"recent_decisions,omitempty"`
}

// DayAggregate is persisted by local calendar date.
type DayAggregate struct {
	Requests              int64                      `json:"requests"`
	PricedRequests        int64                      `json:"priced_requests"`
	InputTokens           int64                      `json:"input_tokens"`
	OutputTokens          int64                      `json:"output_tokens"`
	CacheCreationTokens   int64                      `json:"cache_creation_tokens"`
	CacheReadTokens       int64                      `json:"cache_read_tokens"`
	ActualCostUSD         float64                    `json:"actual_cost_usd"`
	BaselineCostUSD       float64                    `json:"baseline_cost_usd"`
	RoutingSavingsUSD     float64                    `json:"routing_savings_usd"`
	CacheSavingsUSD       float64                    `json:"cache_savings_usd"`
	ClassifierOverheadUSD float64                    `json:"classifier_overhead_usd"`
	NetSavingsUSD         float64                    `json:"net_savings_usd"`
	Models                map[string]*ModelAggregate `json:"models"`
	AmortizedDecisions    int64                      `json:"amortized_decisions"`
	AmortizedSwitches     int64                      `json:"amortized_switches"`
	ForecastSavingsUSD    float64                    `json:"forecast_savings_usd"`
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
	Requests              int64   `json:"requests"`
	PricedRequests        int64   `json:"priced_requests"`
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	CacheCreationTokens   int64   `json:"cache_creation_tokens"`
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	ActualCostUSD         float64 `json:"actual_cost_usd"`
	BaselineCostUSD       float64 `json:"baseline_cost_usd"`
	RoutingSavingsUSD     float64 `json:"routing_savings_usd"`
	CacheSavingsUSD       float64 `json:"cache_savings_usd"`
	ClassifierOverheadUSD float64 `json:"classifier_overhead_usd"`
	NetSavingsUSD         float64 `json:"net_savings_usd"`
	SavingsPercent        float64 `json:"savings_percent"`
	CacheHitPercent       float64 `json:"cache_hit_percent"`
	AmortizedDecisions    int64   `json:"amortized_decisions"`
	AmortizedSwitches     int64   `json:"amortized_switches"`
	ForecastSavingsUSD    float64 `json:"forecast_savings_usd"`
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
	t := &Tracker{path: path, now: now}
	t.ledger = ledger{Version: ledgerVersion, CreatedAt: now(), Days: make(map[string]*DayAggregate)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return t
	}
	if err != nil {
		t.lastErr = fmt.Sprintf("read insights: %v", err)
		return t
	}
	var loaded ledger
	if err := json.Unmarshal(data, &loaded); err != nil || (loaded.Version != 1 && loaded.Version != ledgerVersion) || loaded.Days == nil {
		if err == nil {
			err = fmt.Errorf("unsupported ledger version %d", loaded.Version)
		}
		t.lastErr = fmt.Sprintf("load insights: %v", err)
		return t
	}
	loaded.Version = ledgerVersion
	t.ledger = loaded
	return t
}

// Record adds one successful request and atomically persists the aggregate.
func (t *Tracker) Record(observation Observation) {
	calculation, ok := calculate(observation)
	if !ok {
		return
	}
	t.mu.Lock()
	dayKey := t.now().Format("2006-01-02")
	day := t.ledger.Days[dayKey]
	if day == nil {
		day = &DayAggregate{Models: make(map[string]*ModelAggregate)}
		t.ledger.Days[dayKey] = day
	}
	if day.Models == nil {
		day.Models = make(map[string]*ModelAggregate)
	}
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
	recent := RoutingDecision{
		Timestamp: t.now(), Strategy: observation.Decision.Strategy, ActualModel: observation.Model,
		Decision: observation.Decision.Reason, Reason: observation.Decision.Reason,
		CostMode: observation.Decision.CostMode, Breakdowns: observation.Decision.Breakdowns,
		Background: observation.Decision.Background, BackgroundName: observation.Decision.BackgroundName,
		WorkflowAffinity: observation.Decision.WorkflowAffinity,
		LegacyChosen:     observation.Decision.LegacyChosen, LegacyChanged: observation.Decision.LegacyChanged,
		CacheCreationTokens: calculation.cacheCreationTokens, CacheReadTokens: calculation.cacheReadTokens,
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
	data, err := json.MarshalIndent(t.ledger, "", "  ")
	if err == nil {
		err = atomicWrite(t.path, data)
	}
	if err != nil {
		t.lastErr = fmt.Sprintf("save insights: %v", err)
	} else {
		t.lastErr = ""
	}
	t.mu.Unlock()
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
			"Actual cost uses provider-reported tokens, configured model prices, prompt-cache read/write multipliers, and classifier overhead.",
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
