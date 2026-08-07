package insights

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/octopus/config"
	"github.com/sausheong/octopus/router"
)

func TestTrackerCalculatesAndPersistsSavings(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.Local)
	path := filepath.Join(t.TempDir(), ".octopus", "insights.json")
	tracker := newTestTracker(t, path, func() time.Time { return now })
	catalog := []config.CatalogEntry{
		{ID: "premium/model", Quality: 0.95, CostPerMTokIn: 10, CostPerMTokOut: 30},
		{ID: "economy/model", Quality: 0.70, CostPerMTokIn: 2, CostPerMTokOut: 6},
	}
	tracker.Record(Observation{
		Chat:  llm.ChatRequest{CacheControl: &llm.CacheControl{Type: "ephemeral"}},
		Model: "economy/model",
		Decision: router.Decision{
			Eligible:        []string{"premium/model", "economy/model"},
			ClassifierModel: "economy/model",
			ClassifierUsage: &llm.Usage{InputTokens: 1000, OutputTokens: 100},
		},
		Usage:   &llm.Usage{InputTokens: 100000, OutputTokens: 10000, CacheCreationInputTokens: 20000, CacheReadInputTokens: 80000},
		Catalog: catalog,
	})

	report := tracker.Report(7)
	if report.Summary.Requests != 1 || report.Summary.PricedRequests != 1 {
		t.Fatalf("summary counts = %+v", report.Summary)
	}
	closeEnough(t, report.Summary.BaselineCostUSD, 2.3)
	closeEnough(t, report.Summary.RoutingSavingsUSD, 1.84)
	closeEnough(t, report.Summary.CacheSavingsUSD, 0.134)
	closeEnough(t, report.Summary.ClassifierOverheadUSD, 0.0026)
	closeEnough(t, report.Summary.ActualCostUSD, 0.3286)
	closeEnough(t, report.Summary.NetSavingsUSD, 1.9714)
	if len(report.Models) != 1 || report.Models[0].Model != "economy/model" {
		t.Fatalf("models = %+v", report.Models)
	}

	flushTracker(t, tracker)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode = %o", info.Mode().Perm())
	}
	reloaded := newTestTracker(t, path, func() time.Time { return now })
	closeEnough(t, reloaded.Report(7).Summary.NetSavingsUSD, 1.9714)
}

func TestTrackerDoesNotRetainRequestContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "insights.json")
	tracker := newTestTracker(t, path, time.Now)
	tracker.Record(Observation{
		Chat:     llm.ChatRequest{SystemPrompt: "private-system", Messages: []llm.Message{{Role: "user", Content: "private-user"}}},
		Model:    "local/model",
		Decision: router.Decision{Eligible: []string{"local/model"}},
		Usage:    &llm.Usage{InputTokens: 10, OutputTokens: 5},
		Catalog:  []config.CatalogEntry{{ID: "local/model", Quality: 1}},
	})
	flushTracker(t, tracker)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-system", "private-user"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("ledger retained request content %q", secret)
		}
	}
}

func TestTrackerRecordsConcurrentRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "insights.json")
	tracker := newTestTracker(t, path, time.Now)
	observation := Observation{
		Model:    "local/model",
		Decision: router.Decision{Eligible: []string{"local/model"}},
		Usage:    &llm.Usage{InputTokens: 10, OutputTokens: 5},
		Catalog:  []config.CatalogEntry{{ID: "local/model", Quality: 1}},
	}
	var group sync.WaitGroup
	for index := 0; index < 12; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			tracker.Record(observation)
		}()
	}
	group.Wait()
	if got := tracker.Report(1).Summary.Requests; got != 12 {
		t.Fatalf("requests = %d", got)
	}
}

func TestTrackerCloseFlushesFinalSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "insights.json")
	tracker := newTestTracker(t, path, time.Now)
	tracker.Record(Observation{
		Model: "local/model", Decision: router.Decision{Eligible: []string{"local/model"}},
		Usage: &llm.Usage{InputTokens: 10, OutputTokens: 5}, Catalog: []config.CatalogEntry{{ID: "local/model", Quality: 1}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := tracker.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reloaded := newTestTracker(t, path, time.Now)
	if got := reloaded.Report(1).Summary.Requests; got != 1 {
		t.Fatalf("persisted requests = %d", got)
	}
}

func TestTrackerReportsPromptFreeSwitchEconomics(t *testing.T) {
	now := time.Date(2026, 8, 6, 1, 0, 0, 0, time.Local)
	path := filepath.Join(t.TempDir(), "insights.json")
	tracker := newTestTracker(t, path, func() time.Time { return now })
	tracker.Record(Observation{
		Chat:  llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "do not persist me"}}},
		Model: "p/candidate",
		Decision: router.Decision{
			Strategy: config.RoutingStrategyAmortized, Eligible: []string{"p/incumbent", "p/candidate"},
			Economics: &router.SwitchEconomics{
				Incumbent: "p/incumbent", Candidate: "p/candidate", Decision: "switch",
				ExpectedTurnsIncumbent: 4, ExpectedTurnsCandidate: 3, Confidence: 0.8,
				StayCostUSD: 1.2, SwitchCostUSD: 0.7, EstimatedSavingsUSD: 0.5, BreakEvenTurns: 2.28,
			},
		},
		Usage:   &llm.Usage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 80},
		Catalog: []config.CatalogEntry{{ID: "p/incumbent", Quality: 1}, {ID: "p/candidate", Quality: 0.8}},
	})
	report := tracker.Report(7)
	if report.Summary.AmortizedDecisions != 1 || report.Summary.AmortizedSwitches != 1 {
		t.Fatalf("summary = %+v", report.Summary)
	}
	if len(report.RoutingDecisions) != 1 || report.RoutingDecisions[0].BreakEvenTurns != 2.28 || report.RoutingDecisions[0].CacheReadTokens != 80 {
		t.Fatalf("routing decisions = %+v", report.RoutingDecisions)
	}
	flushTracker(t, tracker)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "do not persist me") {
		t.Fatal("switch ledger retained request content")
	}
}

func TestTrackerMigratesVersionOneLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "insights.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"created_at":"2026-07-20T00:00:00Z","days":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tracker := newTestTracker(t, path, time.Now)
	if tracker.lastErr != "" || tracker.ledger.Version != ledgerVersion {
		t.Fatalf("migration failed: version=%d error=%q", tracker.ledger.Version, tracker.lastErr)
	}
}

func TestTrackerMigratesVersionTwoLedgerAndInitialisesAuditAggregates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "insights.json")
	if err := os.WriteFile(path, []byte(`{"version":2,"created_at":"2026-07-20T00:00:00Z","days":{"2026-07-20":{"requests":1,"models":{}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.Local)
	tracker := newTestTracker(t, path, func() time.Time { return now })
	tracker.Record(Observation{
		Model:    "local/model",
		Decision: router.Decision{Chosen: "local/model", Eligible: []string{"local/model"}, Profile: router.TaskProfile{Difficulty: "low", Domain: "code"}},
		Usage:    &llm.Usage{InputTokens: 10, OutputTokens: 5},
		Catalog:  []config.CatalogEntry{{ID: "local/model", Quality: 1}},
	})
	if tracker.lastErr != "" || tracker.ledger.Version != ledgerVersion {
		t.Fatalf("migration failed: version=%d error=%q", tracker.ledger.Version, tracker.lastErr)
	}
	if got := tracker.Report(1).Summary.Difficulties["low"]; got != 1 {
		t.Fatalf("low difficulty count = %d", got)
	}
}

func TestTrackerPersistsBoundedPromptFreeRoutingAudit(t *testing.T) {
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.Local)
	path := filepath.Join(t.TempDir(), "insights.json")
	tracker := newTestTracker(t, path, func() time.Time { return now })
	tracker.Record(Observation{
		Chat: llm.ChatRequest{
			SystemPrompt: "audit-secret-system",
			Messages:     []llm.Message{{Role: "user", Content: "audit-secret-user"}},
		},
		Model: "provider/fallback",
		Decision: router.Decision{
			Chosen: "provider/initial", InitialChosen: "provider/scored", Eligible: []string{"provider/initial", "provider/fallback"},
			Profile: router.TaskProfile{
				Difficulty: "high", Domain: "code", NeedsReasoning: true, NeedsTools: true,
				EstTokensIn: 1234, EstTokensOut: 567, ExpectedRemainingTurns: 4, EstimateConfidence: .8,
			},
			ClassifierModel:      "provider/classifier",
			ClassifierUsage:      &llm.Usage{InputTokens: 20, OutputTokens: 3},
			ClassificationSource: "classifier", ClassificationStatus: "completed", ClassifierLatencyMS: 17,
			AppliedQualityFloor: .85, QualityPolicy: "strict",
		},
		Usage: &llm.Usage{InputTokens: 100, OutputTokens: 20},
		Catalog: []config.CatalogEntry{
			{ID: "provider/initial", Quality: .8}, {ID: "provider/fallback", Quality: .9},
			{ID: "provider/classifier", Quality: .5},
		},
	})

	report := tracker.Report(1)
	if len(report.RoutingDecisions) != 1 {
		t.Fatalf("routing decisions = %d", len(report.RoutingDecisions))
	}
	decision := report.RoutingDecisions[0]
	if decision.InitialModel != "provider/scored" || decision.SelectedModel != "provider/initial" || decision.ActualModel != "provider/fallback" || !decision.FallbackObserved {
		t.Fatalf("model audit = %+v", decision)
	}
	if decision.ProviderAttemptsMinimum != 2 || decision.ProviderOutcome != "completed" {
		t.Fatalf("provider audit = %+v", decision)
	}
	if decision.Difficulty != "high" || decision.Domain != "code" || decision.EstimatedInputTokens != 1234 || decision.ExpectedRemainingTurns != 4 {
		t.Fatalf("profile audit = %+v", decision)
	}
	if decision.ClassificationSource != "classifier" || decision.ClassificationStatus != "completed" {
		t.Fatalf("classification audit = %+v", decision)
	}
	if decision.ClassifierLatencyMS != 17 || decision.AppliedQualityFloor != .85 || decision.QualityPolicy != "strict" {
		t.Fatalf("routing policy audit = %+v", decision)
	}
	if report.Summary.Difficulties["high"] != 1 || report.Summary.Domains["code"] != 1 || report.Summary.FallbacksObserved != 1 {
		t.Fatalf("audit aggregates = %+v", report.Summary)
	}

	flushTracker(t, tracker)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"audit-secret-system", "audit-secret-user"} {
		if strings.Contains(string(data), private) {
			t.Fatalf("ledger retained private request content %q", private)
		}
	}
}

func TestTrackerBoundsClassifierControlledProfileLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "insights.json")
	tracker := newTestTracker(t, path, time.Now)
	tracker.Record(Observation{
		Model: "local/model",
		Decision: router.Decision{
			Chosen: "local/model", Eligible: []string{"local/model"},
			Profile: router.TaskProfile{
				Difficulty: "secret-difficulty", Domain: "secret-domain", Risk: "secret-risk",
				ClassificationSource: "secret-source", EstimateConfidence: 9,
				ClassificationConfidence: 9, MinimumQuality: 9, ExpectedRemainingTurns: 900,
			},
		},
		Usage:   &llm.Usage{InputTokens: 10, OutputTokens: 5},
		Catalog: []config.CatalogEntry{{ID: "local/model", Quality: 1}},
	})
	decision := tracker.Report(1).RoutingDecisions[0]
	if decision.Difficulty != "unknown" || decision.Domain != "unknown" || decision.Risk != "unknown" ||
		decision.EstimateConfidence != 0 || decision.ClassificationConfidence != 0 ||
		decision.AppliedQualityFloor != 0 || decision.ExpectedRemainingTurns != 0 {
		t.Fatalf("unbounded profile audit = %+v", decision)
	}
	flushTracker(t, tracker)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"secret-difficulty", "secret-domain", "secret-risk", "secret-source"} {
		if strings.Contains(string(data), private) {
			t.Fatalf("ledger persisted unbounded classifier-controlled label %q", private)
		}
	}
}

func closeEnough(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %.12f, want %.12f", got, want)
	}
}

func flushTracker(t *testing.T, tracker *Tracker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := tracker.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

func newTestTracker(t *testing.T, path string, now func() time.Time) *Tracker {
	t.Helper()
	tracker := newTracker(path, now)
	t.Cleanup(func() { _ = tracker.Close(context.Background()) })
	return tracker
}
