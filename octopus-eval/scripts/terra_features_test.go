package router

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/octopus/config"
	"github.com/sausheong/octopus/registry"
)

// These tests are copied into the production router package by run.sh. They
// deliberately exercise the same package-private state and entry points used
// by the router rather than maintaining a second model of feature behaviour in
// the evaluation harness.

func evalFeatureConfig(strategy string) *config.Config {
	return &config.Config{
		ServerAddr: "127.0.0.1:8787",
		Classifier: config.ClassifierCfg{Model: "anthropic/classifier", MaxTokens: 64, Timeout: time.Second},
		Weights:    config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2},
		Routing: config.RoutingCfg{
			Strategy:              strategy,
			DataPolicy:            config.DataPolicyAllowRemote,
			SessionTTL:            time.Hour,
			CacheAware:            true,
			DefaultRemainingTurns: 4,
			CostMode:              config.CostModeAbsolute,
			CostReferenceUSD:      0.10,
			HighQualityFloor:      HighQualityFloor,
			WorkflowAffinity:      true,
			MaxAttempts:           3,
		},
		Providers: map[string]config.ProviderCreds{
			"anthropic": {Kind: "anthropic", APIKey: "synthetic-test-key"},
		},
		Catalog: []config.CatalogEntry{
			{
				ID: "anthropic/capable", Quality: 0.95, CostPerMTokIn: 8, CostPerMTokOut: 24, Speed: 0.5,
				Caps: config.Caps{Tools: true, Vision: true, Reasoning: true, MaxContext: 1_000_000},
			},
			{
				ID: "anthropic/cheap", Quality: 0.70, CostPerMTokIn: 1, CostPerMTokOut: 3, Speed: 0.95,
				Caps: config.Caps{MaxContext: 200_000},
			},
		},
	}
}

func evalFeatureRouter(t *testing.T, cfg *config.Config) *Router {
	t.Helper()
	reg, err := registry.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	return NewRouter(cfg, reg)
}

func TestEvalFeatureClassifierMemoCacheTTLAndModelIsolation(t *testing.T) {
	memo := newClassifierMemo(8, time.Minute)
	now := time.Unix(10_000, 0)
	memo.now = func() time.Time { return now }
	turn := llm.Message{Role: "user", Content: "synthetic classifier request"}
	keyA := classifierMemoKey("anthropic/classifier-a", 64, turn)
	keyB := classifierMemoKey("anthropic/classifier-b", 64, turn)
	if keyA == keyB {
		t.Fatal("classifier model identity was not included in the memo key")
	}

	var calls atomic.Int32
	load := func(context.Context) (TaskProfile, *llm.Usage, bool) {
		call := calls.Add(1)
		return TaskProfile{Difficulty: "low", EstTokensIn: int(call)}, &llm.Usage{InputTokens: 7}, true
	}

	first, usage, cached, _ := memo.do(context.Background(), keyA, load)
	if cached || usage == nil || first.EstTokensIn != 1 {
		t.Fatalf("first load = (%+v, %+v, cached=%v)", first, usage, cached)
	}
	repeated, usage, cached, _ := memo.do(context.Background(), keyA, load)
	if !cached || usage != nil || repeated != first || calls.Load() != 1 {
		t.Fatalf("cache hit = (%+v, %+v, cached=%v), provider calls=%d", repeated, usage, cached, calls.Load())
	}
	otherModel, _, cached, _ := memo.do(context.Background(), keyB, load)
	if cached || otherModel.EstTokensIn != 2 || calls.Load() != 2 {
		t.Fatalf("model-isolated load = (%+v, cached=%v), provider calls=%d", otherModel, cached, calls.Load())
	}

	now = now.Add(time.Minute + time.Nanosecond)
	expired, _, cached, _ := memo.do(context.Background(), keyA, load)
	if cached || expired.EstTokensIn != 3 || calls.Load() != 3 {
		t.Fatalf("expired load = (%+v, cached=%v), provider calls=%d", expired, cached, calls.Load())
	}
	stats := memo.snapshot()
	if stats.Hits != 1 || stats.Misses != 3 || stats.Stores != 3 {
		t.Fatalf("classifier memo stats = %+v", stats)
	}
}

func TestEvalFeatureClassifierMemoCoalescesConcurrentRequests(t *testing.T) {
	memo := newClassifierMemo(8, time.Minute)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	load := func(context.Context) (TaskProfile, *llm.Usage, bool) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return TaskProfile{Difficulty: "high", NeedsReasoning: true}, nil, true
	}

	const workers = 12
	var wg sync.WaitGroup
	wg.Add(workers)
	results := make(chan TaskProfile, workers)
	for range workers {
		go func() {
			defer wg.Done()
			profile, _, _, _ := memo.do(context.Background(), "one-synthetic-digest", load)
			results <- profile
		}()
	}
	<-started
	deadline := time.Now().Add(2 * time.Second)
	for memo.snapshot().Coalesced != workers-1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	close(results)

	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", calls.Load())
	}
	for profile := range results {
		if profile.Difficulty != "high" || !profile.NeedsReasoning {
			t.Fatalf("coalesced profile = %+v", profile)
		}
	}
	if stats := memo.snapshot(); stats.Coalesced != workers-1 {
		t.Fatalf("coalesced callers = %d, want %d (stats=%+v)", stats.Coalesced, workers-1, stats)
	}
}

func TestEvalFeatureStickyClassifiesOrdinaryFollowupsBeforeApplyingAffinity(t *testing.T) {
	cfg := evalFeatureConfig(config.RoutingStrategySticky)
	r := evalFeatureRouter(t, cfg)
	base := llm.ChatRequest{
		SessionID: "synthetic-sticky-session", MaxTokens: 128,
		Messages: []llm.Message{{Role: "user", Content: strings.Repeat("x", 700)}},
	}
	r.Observe(base, "anthropic/cheap", &llm.Usage{})
	var calls atomic.Int32
	r.SetClassifier(func(context.Context, llm.LLMProvider, string, int, llm.Message) TaskProfile {
		calls.Add(1)
		return TaskProfile{
			Difficulty: "low", EstTokensIn: 1_000, EstTokensOut: 128,
			ExpectedRemainingTurns: 3, EstimateConfidence: 1,
		}
	})

	eligible := r.Route(context.Background(), base)
	if calls.Load() != 1 || eligible.Chosen != "anthropic/cheap" || eligible.Reason != "sticky session affinity" {
		t.Fatalf("eligible sticky follow-up: calls=%d decision=%+v", calls.Load(), eligible)
	}

	withTool := base
	withTool.Tools = []llm.ToolDef{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}}
	ineligible := r.Route(context.Background(), withTool)
	if calls.Load() != 2 {
		t.Fatalf("classifier calls after incumbent became ineligible = %d, want 2", calls.Load())
	}
	if ineligible.Chosen != "anthropic/capable" || len(ineligible.Eligible) != 1 {
		t.Fatalf("ineligible incumbent fallback = %+v", ineligible)
	}
}

func TestEvalFeatureBackgroundExactMatchUsesIsolatedSession(t *testing.T) {
	cfg := evalFeatureConfig(config.RoutingStrategySticky)
	sig := ExactBackgroundSignature("synthetic-ping", "/v1/messages", "synthetic status ping")
	cfg.Routing.Background = config.BackgroundCfg{
		Enabled: true, Model: "anthropic/cheap",
		Signatures: []config.BackgroundSignatureCfg{{
			Name: sig.Name, Endpoint: sig.Endpoint, LastUserSHA256: sig.LastUserSHA256,
			RequireNonStreaming: true, ConversationIndependent: true,
		}},
	}
	r := evalFeatureRouter(t, cfg)
	var backgroundClassifierCalls atomic.Int32
	r.SetClassifier(func(context.Context, llm.LLMProvider, string, int, llm.Message) TaskProfile {
		backgroundClassifierCalls.Add(1)
		return TaskProfile{
			Difficulty: "low", EstTokensIn: 100, EstTokensOut: 32,
			ExpectedRemainingTurns: 1, EstimateConfidence: 1,
		}
	})
	chat := llm.ChatRequest{
		SessionID: "synthetic-main-session", MaxTokens: 32,
		CacheControl: &llm.CacheControl{Type: "ephemeral"},
		Messages: []llm.Message{
			{Role: "assistant", Content: "previous synthetic response"},
			{Role: "user", Content: "synthetic status ping"},
		},
	}
	r.Observe(chat, "anthropic/capable", &llm.Usage{InputTokens: 20, CacheCreationInputTokens: 80})
	mainID := SessionID(chat)
	mainBefore, ok := r.sessionSnapshot(mainID, time.Now())
	if !ok {
		t.Fatal("main session was not recorded")
	}

	d := r.RouteWithMetadata(context.Background(), chat, RequestMetadata{Endpoint: "/v1/messages", Stream: false})
	if backgroundClassifierCalls.Load() != 0 {
		t.Fatalf("conversation-independent background request called classifier %d time(s)", backgroundClassifierCalls.Load())
	}
	if !d.Background || d.BackgroundName != "synthetic-ping" || d.Chosen != "anthropic/cheap" {
		t.Fatalf("exact background decision = %+v", d)
	}
	if !strings.HasPrefix(d.RoutingSessionID, "background:") {
		t.Fatalf("background routing session = %q", d.RoutingSessionID)
	}
	upstream := RequestForDecision(chat, d)
	if len(upstream.Messages) != 1 || upstream.Messages[0].Content != "synthetic status ping" || upstream.CacheControl != nil {
		t.Fatalf("provider request retained main history/cache: %+v", upstream)
	}
	r.ObserveDecision(chat, d.Chosen, &llm.Usage{InputTokens: 60, CacheCreationInputTokens: 40}, d)

	mainAfter, ok := r.sessionSnapshot(mainID, time.Now())
	if !ok || mainAfter.Incumbent != mainBefore.Incumbent || mainAfter.TurnCount != mainBefore.TurnCount || len(mainAfter.Models) != len(mainBefore.Models) {
		t.Fatalf("background request mutated main state: before=%+v after=%+v", mainBefore, mainAfter)
	}
	backgroundID := SessionID(llm.ChatRequest{SessionID: d.RoutingSessionID})
	backgroundState, ok := r.sessionSnapshot(backgroundID, time.Now())
	if !ok || backgroundState.Incumbent != "anthropic/cheap" || backgroundID == mainID {
		t.Fatalf("isolated background state: id=%q main=%q state=%+v found=%v", backgroundID, mainID, backgroundState, ok)
	}

	ordinary := r.Route(context.Background(), chat)
	if ordinary.Background || ordinary.Chosen != "anthropic/capable" || ordinary.Reason != "sticky session affinity" {
		t.Fatalf("ordinary request after ping = %+v", ordinary)
	}

	variants := []struct {
		name string
		chat llm.ChatRequest
		meta RequestMetadata
	}{
		{name: "streaming", chat: chat, meta: RequestMetadata{Endpoint: "/v1/messages", Stream: true}},
		{name: "wrong endpoint", chat: chat, meta: RequestMetadata{Endpoint: "/v1/other"}},
		{name: "wrong content", chat: func() llm.ChatRequest {
			copyChat := chat
			copyChat.Messages = append([]llm.Message(nil), chat.Messages...)
			copyChat.Messages[len(copyChat.Messages)-1].Content = "different status ping"
			return copyChat
		}(), meta: RequestMetadata{Endpoint: "/v1/messages"}},
		{name: "tools present", chat: func() llm.ChatRequest {
			copyChat := chat
			copyChat.Tools = []llm.ToolDef{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}}
			return copyChat
		}(), meta: RequestMetadata{Endpoint: "/v1/messages"}},
	}
	for _, tc := range variants {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.RouteWithMetadata(context.Background(), tc.chat, tc.meta); got.Background {
				t.Fatalf("non-exact request matched background signature: %+v", got)
			}
		})
	}
}

func TestEvalFeatureWorkflowAffinityKeepsCachesSeparateAndFallsBackWhenIneligible(t *testing.T) {
	cfg := evalFeatureConfig(config.RoutingStrategySticky)
	r := evalFeatureRouter(t, cfg)
	r.SetClassifier(func(context.Context, llm.LLMProvider, string, int, llm.Message) TaskProfile {
		return TaskProfile{
			Difficulty: "low", EstTokensIn: 100, EstTokensOut: 100,
			ExpectedRemainingTurns: 3, EstimateConfidence: 1,
		}
	})
	workflow := "synthetic-workflow"
	cacheControl := &llm.CacheControl{Type: "ephemeral"}
	first := llm.ChatRequest{
		SessionID: "synthetic-subagent-1", CacheControl: cacheControl,
		Messages: []llm.Message{{Role: "user", Content: "first independent conversation"}},
	}
	firstDecision := r.RouteWithMetadata(context.Background(), first, RequestMetadata{WorkflowID: workflow})
	r.ObserveDecision(first, "anthropic/cheap", &llm.Usage{InputTokens: 20, CacheCreationInputTokens: 80}, firstDecision)

	second := llm.ChatRequest{
		SessionID: "synthetic-subagent-2", CacheControl: cacheControl,
		Messages: []llm.Message{{Role: "user", Content: "second independent conversation"}},
	}
	secondDecision := r.RouteWithMetadata(context.Background(), second, RequestMetadata{WorkflowID: workflow})
	if secondDecision.Chosen != "anthropic/cheap" || !secondDecision.WorkflowAffinity {
		t.Fatalf("second subagent affinity decision = %+v", secondDecision)
	}
	r.ObserveDecision(second, secondDecision.Chosen, &llm.Usage{InputTokens: 60, CacheCreationInputTokens: 40}, secondDecision)

	firstID, secondID := SessionID(first), SessionID(second)
	if firstID == secondID {
		t.Fatal("workflow affinity merged independent session identities")
	}
	firstState, firstOK := r.sessionSnapshot(firstID, time.Now())
	secondState, secondOK := r.sessionSnapshot(secondID, time.Now())
	if !firstOK || !secondOK {
		t.Fatalf("session states missing: first=%v second=%v", firstOK, secondOK)
	}
	firstFraction := firstState.Models["anthropic/cheap"].CacheFraction
	secondFraction := secondState.Models["anthropic/cheap"].CacheFraction
	if firstFraction != 0.8 || secondFraction != 0.4 {
		t.Fatalf("cache state was not independent: first=%v second=%v", firstFraction, secondFraction)
	}

	third := llm.ChatRequest{
		SessionID: "synthetic-subagent-3",
		Messages:  []llm.Message{{Role: "user", Content: strings.Repeat("tool request ", 60)}},
		Tools:     []llm.ToolDef{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
	}
	var classifierCalls atomic.Int32
	r.SetClassifier(func(context.Context, llm.LLMProvider, string, int, llm.Message) TaskProfile {
		classifierCalls.Add(1)
		return TaskProfile{
			Difficulty: "medium", NeedsTools: true, EstTokensIn: 400, EstTokensOut: 100,
			ExpectedRemainingTurns: 2, EstimateConfidence: 1,
		}
	})
	thirdDecision := r.RouteWithMetadata(context.Background(), third, RequestMetadata{WorkflowID: workflow})
	if classifierCalls.Load() != 1 {
		t.Fatalf("third subagent classifier calls = %d, want 1", classifierCalls.Load())
	}
	if thirdDecision.WorkflowAffinity || thirdDecision.Chosen != "anthropic/capable" || len(thirdDecision.Eligible) != 1 {
		t.Fatalf("ineligible workflow preference did not fall back: %+v", thirdDecision)
	}
}
