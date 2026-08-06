package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/octopus/config"
	"github.com/sausheong/octopus/registry"
)

// Router turns an inbound chat request into a routing Decision.
type Router struct {
	cfg *config.Config
	reg *registry.Registry
	// classifyFn is the classification seam. Nil uses classifyWithUsage;
	// tests override it to avoid real provider calls.
	classifyFn func(ctx context.Context, prov llm.LLMProvider, model string, maxTokens int, turn llm.Message) TaskProfile
	sessionsMu sync.Mutex
	sessions   map[string]sessionState
	// cachingModels is the set of catalog IDs that support prompt caching,
	// computed once at build time to avoid per-request Resolve + type assertion.
	cachingModels map[string]bool
}

type sessionState struct {
	Incumbent       string
	ExpiresAt       time.Time
	TurnCount       int
	Models          map[string]modelSessionState
	LastInputTokens int
	InputGrowthEMA  float64
	OutputEMA       float64
}

type modelSessionState struct {
	CacheUntil    time.Time
	CacheFraction float64
}

// NewRouter builds a Router. The classifier provider is resolved per request
// from the registry using cfg.Classifier.Model.
func NewRouter(cfg *config.Config, reg *registry.Registry) *Router {
	// Config.Parse always supplies an explicit strategy. Keep direct Config
	// construction (used by embedders and older tests) compatible with the
	// former SessionSticky boolean.
	if cfg.Routing.Strategy == "" {
		if cfg.Routing.SessionSticky {
			cfg.Routing.Strategy = config.RoutingStrategySticky
		} else {
			cfg.Routing.Strategy = config.RoutingStrategyPerTurn
		}
	}
	if cfg.Routing.DataPolicy == "" {
		cfg.Routing.DataPolicy = config.DataPolicyAllowRemote
	}
	caching := make(map[string]bool, len(cfg.Catalog))
	for _, entry := range cfg.Catalog {
		prov, _, err := reg.Resolve(entry.ID)
		if err != nil {
			continue
		}
		if p, ok := prov.(llm.PromptCachingProvider); ok && p.SupportsPromptCaching() {
			caching[entry.ID] = true
		}
	}
	return &Router{
		cfg:           cfg,
		reg:           reg,
		sessions:      make(map[string]sessionState),
		cachingModels: caching,
	}
}

// shortCircuitTokens is the content-length threshold (in bytes, used as a
// proxy for tokens) below which the classifier is skipped. Requests this
// small are assumed trivial/low-difficulty with no special capability needs.
const shortCircuitBytes = 500

// Route classifies the last genuine user turn, applies request-derived
// capability cross-checks, scores the catalog, and returns the Decision.
func (r *Router) Route(ctx context.Context, chat llm.ChatRequest) Decision {
	turn, ok := LastUserTurn(chat.Messages)
	var prof TaskProfile
	var classifierUsage *llm.Usage
	classifierModel := ""
	if !ok {
		prof = DefaultProfile()
	} else if isTrivial(chat, turn) {
		prof = TrivialProfile()
		slog.Debug("classifier skipped (trivial request)")
	} else {
		// A classifier receives request content before model scoring. Under a
		// local-only policy it therefore has to be local too; otherwise skip it
		// and use the conservative deterministic profile.
		classifierAllowed := r.cfg.Routing.DataPolicy != config.DataPolicyLocalOnly || r.cfg.IsLocalModel(r.cfg.Classifier.Model)
		if !classifierAllowed {
			slog.Info("remote classifier blocked by local-only data policy", "model", r.cfg.Classifier.Model)
			prof = DefaultProfile()
		} else {
			prov, model, err := r.reg.Resolve(r.cfg.Classifier.Model)
			if err != nil {
				slog.Warn("classifier provider unresolved; using default profile", "err", err)
				prof = DefaultProfile()
			} else {
				classifierModel = r.cfg.Classifier.Model
				cctx := ctx
				if r.cfg.Classifier.Timeout > 0 {
					var cancel context.CancelFunc
					cctx, cancel = context.WithTimeout(ctx, r.cfg.Classifier.Timeout)
					defer cancel()
				}
				classifierTurn := classificationTurn(chat, turn)
				if r.classifyFn != nil {
					prof = r.classifyFn(cctx, prov, model, r.cfg.Classifier.MaxTokens, classifierTurn)
				} else {
					prof, classifierUsage = classifyWithUsage(cctx, prov, model, r.cfg.Classifier.MaxTokens, classifierTurn)
				}
			}
		}
	}

	// Request-derived cross-checks override the classifier's guesses: if the
	// actual request carries images or tools, those capabilities are required.
	if requestHasImages(chat) {
		prof.NeedsVision = true
	}
	if len(chat.Tools) > 0 {
		prof.NeedsTools = true
	}

	// Apply deterministic token floor: classifier context is deliberately
	// bounded, so it can underestimate large system prompts, histories, and
	// tool schemas. EstimateRequestTokens counts the full request; we take the max
	// so the classifier's semantic estimate is never overridden downward.
	detIn := EstimateRequestTokens(chat)
	if detIn > prof.EstTokensIn {
		prof.EstTokensIn = detIn
	}
	// Reserve capacity for the requested output (MaxTokens = 0 means "model
	// default"; treat it as a modest 1024 for context-filter purposes).
	detOut := chat.MaxTokens
	if detOut <= 0 {
		detOut = 1024
	}
	if detOut > prof.EstTokensOut {
		prof.EstTokensOut = detOut
	}
	if prof.ExpectedRemainingTurns < 1 || prof.ExpectedRemainingTurns > 50 {
		prof.ExpectedRemainingTurns = r.cfg.Routing.DefaultRemainingTurns
		if prof.ExpectedRemainingTurns < 1 {
			prof.ExpectedRemainingTurns = 4
		}
	}
	if prof.EstimateConfidence < 0 || prof.EstimateConfidence > 1 {
		prof.EstimateConfidence = 0.25
	}

	// Compute session ID once; reuse for both sticky and cache multipliers.
	sid := ""
	if r.cfg.Routing.Strategy != config.RoutingStrategyPerTurn || r.cfg.Routing.CacheAware {
		sid = SessionID(chat)
	}
	var multipliers map[string]float64
	if r.cfg.Routing.Strategy != config.RoutingStrategyAmortized {
		multipliers = r.cacheInputMultipliersForSession(chat, sid)
	}
	// Placement policy is applied to the catalog before scoring. Keeping the
	// resulting Eligible set policy-safe also constrains sticky affinity,
	// amortized selection, and server fallback without a second escape path.
	catalog := r.cfg.Catalog
	if r.cfg.Routing.DataPolicy == config.DataPolicyLocalOnly {
		catalog = r.localCatalog()
	} else if r.cfg.Routing.DataPolicy == config.DataPolicyPreferLocal {
		locals := r.localCatalog()
		if localDecision := ScoreWithInputMultipliers(prof, locals, r.cfg.Weights, multipliers); !localDecision.NoEligible {
			catalog = locals
		}
	}
	d := ScoreWithInputMultipliers(prof, catalog, r.cfg.Weights, multipliers)
	d.Strategy = r.cfg.Routing.Strategy
	d.DataPolicy = r.cfg.Routing.DataPolicy
	d.RemoteFallbackBlocked = r.cfg.Routing.DataPolicy == config.DataPolicyLocalOnly
	d.ClassifierModel = classifierModel
	d.ClassifierUsage = classifierUsage
	d.MaxAttempts = r.cfg.Routing.MaxAttempts
	switch r.cfg.Routing.Strategy {
	case config.RoutingStrategySticky:
		if sticky := r.stickyModelForSession(sid); sticky != "" {
			for _, id := range d.Eligible {
				if id == sticky {
					d.Chosen = sticky
					d.Reason = "sticky session affinity"
					break
				}
			}
		}
	case config.RoutingStrategyAmortized:
		d = r.applyAmortized(chat, sid, d)
	}
	// If the profile benefits from reasoning, recommend medium effort. The
	// server applies it only to candidates that advertise reasoning support.
	if prof.NeedsReasoning {
		d.Reasoning = llm.ReasoningMedium
	}
	slog.Info("routing decision",
		"chosen", d.Chosen,
		"reason", d.Reason,
		"strategy", d.Strategy,
		"data_policy", d.DataPolicy,
		"remote_fallback_blocked", d.RemoteFallbackBlocked,
		"difficulty", d.Profile.Difficulty,
		"expected_remaining_turns", d.Profile.ExpectedRemainingTurns,
		"estimate_confidence", d.Profile.EstimateConfidence,
		"needs_reasoning", d.Profile.NeedsReasoning,
		"needs_vision", d.Profile.NeedsVision,
		"needs_tools", d.Profile.NeedsTools,
		"eligible", d.Eligible,
		"switch_economics", d.Economics,
	)
	return d
}

func (r *Router) localCatalog() []config.CatalogEntry {
	local := make([]config.CatalogEntry, 0, len(r.cfg.Catalog))
	for _, entry := range r.cfg.Catalog {
		if r.cfg.IsLocalModel(entry.ID) {
			local = append(local, entry)
		}
	}
	return local
}

const (
	CacheReadInputMultiplier       = 0.10
	CacheWrite5mInputMultiplier    = 1.25
	CacheWrite1HourInputMultiplier = 2.00
)

// SessionID returns one of three shapes: an "explicit:" hash of a client
// session ID, or a "derived:" hash of the stable conversation prefix used by
// prompt caching — with a "user:"-tagged identifier, when present, folded into
// that derived hash rather than treated as an explicit ID.
func SessionID(chat llm.ChatRequest) string {
	// A "user:" prefix marks a client-supplied user identifier rather than a
	// conversation. It disambiguates two users who send the same opening
	// prompt, but must not pin all of one user's conversations together — so
	// it feeds the derived hash instead of short-circuiting as an explicit ID.
	// The prefix is a convention set by the decoders, not a guarantee: a client
	// that sends a literal "user:..." X-Octopus-Session-ID header gets derived
	// behaviour. That is an accepted trade-off over escaping every value.
	userTag := ""
	if strings.HasPrefix(chat.SessionID, "user:") {
		userTag = chat.SessionID
	} else if chat.SessionID != "" {
		sum := sha256.Sum256([]byte(chat.SessionID))
		return "explicit:" + hex.EncodeToString(sum[:])
	}
	h := sha256.New()
	h.Write([]byte(userTag))
	h.Write([]byte{0})
	h.Write([]byte(chat.SystemPrompt))
	for _, part := range chat.SystemPromptParts {
		h.Write([]byte(part.Text))
		h.Write([]byte{0})
	}
	for _, tool := range chat.Tools {
		h.Write([]byte(tool.Name))
		h.Write(tool.Parameters)
		h.Write([]byte{0})
	}
	for _, msg := range chat.Messages {
		if msg.Role == "user" && msg.ToolCallID == "" {
			h.Write([]byte(msg.Content))
			break
		}
	}
	return "derived:" + hex.EncodeToString(h.Sum(nil))
}

func (r *Router) stickyModelForSession(sid string) string {
	if r.cfg.Routing.Strategy != config.RoutingStrategySticky || sid == "" {
		return ""
	}
	now := time.Now()
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	state, ok := r.sessions[sid]
	if !ok || !state.ExpiresAt.After(now) {
		delete(r.sessions, sid)
		return ""
	}
	return state.Incumbent
}

func (r *Router) cacheInputMultipliersForSession(chat llm.ChatRequest, sid string) map[string]float64 {
	ttl := CacheTTL(chat)
	if !r.cfg.Routing.CacheAware || ttl == 0 || sid == "" {
		return nil
	}
	now := time.Now()
	r.sessionsMu.Lock()
	state := cloneSessionState(r.sessions[sid])
	r.sessionsMu.Unlock()

	multipliers := make(map[string]float64)
	for _, entry := range r.cfg.Catalog {
		if !r.cachingModels[entry.ID] {
			continue
		}
		multipliers[entry.ID] = CacheWrite5mInputMultiplier
		if ttl == time.Hour {
			multipliers[entry.ID] = CacheWrite1HourInputMultiplier
		}
		if modelState, ok := state.Models[entry.ID]; ok && modelState.CacheUntil.After(now) {
			fraction := modelState.CacheFraction
			if fraction <= 0 || fraction > 1 {
				fraction = 1
			}
			multipliers[entry.ID] = 1 - fraction + fraction*CacheReadInputMultiplier
		}
	}
	return multipliers
}

// Observe records the provider that completed a turn and any prompt cache it
// created or read. It is safe to call concurrently from streaming requests.
func (r *Router) Observe(chat llm.ChatRequest, model string, usage *llm.Usage) {
	if r.cfg.Routing.Strategy == config.RoutingStrategyPerTurn && !r.cfg.Routing.CacheAware {
		return
	}
	now := time.Now()
	ttl := r.cfg.Routing.SessionTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	id := SessionID(chat)
	r.sessionsMu.Lock()
	state := cloneSessionState(r.sessions[id])
	state.Incumbent = model
	state.ExpiresAt = now.Add(ttl)
	state.TurnCount++
	if state.Models == nil {
		state.Models = make(map[string]modelSessionState)
	}
	modelState := state.Models[model]
	if usage != nil && (usage.CacheCreationInputTokens > 0 || usage.CacheReadInputTokens > 0) {
		if cacheDuration := CacheTTL(chat); cacheDuration > 0 {
			modelState.CacheUntil = now.Add(cacheDuration)
			cached := usage.CacheCreationInputTokens + usage.CacheReadInputTokens
			total := cached + usage.InputTokens
			if total > 0 {
				modelState.CacheFraction = float64(cached) / float64(total)
			}
		}
	}
	state.Models[model] = modelState
	if usage != nil {
		input := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
		const alpha = 0.5
		if state.LastInputTokens > 0 && input >= state.LastInputTokens {
			growth := float64(input - state.LastInputTokens)
			if state.InputGrowthEMA == 0 {
				state.InputGrowthEMA = growth
			} else {
				state.InputGrowthEMA = alpha*growth + (1-alpha)*state.InputGrowthEMA
			}
		}
		if usage.OutputTokens >= 0 {
			if state.OutputEMA == 0 {
				state.OutputEMA = float64(usage.OutputTokens)
			} else {
				state.OutputEMA = alpha*float64(usage.OutputTokens) + (1-alpha)*state.OutputEMA
			}
		}
		state.LastInputTokens = input
	}
	const maxSessionEntries = 4096
	if _, exists := r.sessions[id]; !exists && len(r.sessions) >= maxSessionEntries {
		for key, candidate := range r.sessions {
			if !candidate.ExpiresAt.After(now) {
				delete(r.sessions, key)
			}
		}
		for len(r.sessions) >= maxSessionEntries {
			for key := range r.sessions {
				delete(r.sessions, key)
				break
			}
		}
	}
	r.sessions[id] = state
	r.sessionsMu.Unlock()
	if usage != nil {
		slog.Info("prompt cache usage", "model", model,
			"cache_creation_input_tokens", usage.CacheCreationInputTokens,
			"cache_read_input_tokens", usage.CacheReadInputTokens)
	}
}

func cloneSessionState(state sessionState) sessionState {
	if state.Models == nil {
		return state
	}
	original := state.Models
	state.Models = make(map[string]modelSessionState, len(original))
	for id, modelState := range original {
		state.Models[id] = modelState
	}
	return state
}

func (r *Router) sessionSnapshot(sid string, now time.Time) (sessionState, bool) {
	if sid == "" {
		return sessionState{}, false
	}
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	state, ok := r.sessions[sid]
	if !ok || !state.ExpiresAt.After(now) {
		delete(r.sessions, sid)
		return sessionState{}, false
	}
	return cloneSessionState(state), true
}

func CacheTTL(chat llm.ChatRequest) time.Duration {
	var controls []*llm.CacheControl
	controls = append(controls, chat.CacheControl)
	for _, part := range chat.SystemPromptParts {
		controls = append(controls, part.CacheControl)
		if part.Cache {
			controls = append(controls, &llm.CacheControl{Type: "ephemeral"})
		}
	}
	for i := range chat.Tools {
		controls = append(controls, chat.Tools[i].CacheControl)
	}
	for i := range chat.Messages {
		controls = append(controls, chat.Messages[i].CacheControl)
	}
	if chat.CacheLastMessage {
		controls = append(controls, &llm.CacheControl{Type: "ephemeral"})
	}
	var ttl time.Duration
	for _, control := range controls {
		if control == nil {
			continue
		}
		if control.TTL == "1h" {
			return time.Hour
		}
		ttl = 5 * time.Minute
	}
	return ttl
}

const maxClassifierContextBytes = 12 << 10

// classificationTurn gives the classifier enough recent conversation to
// understand replies such as "continue" while bounding classifier cost. A
// genuinely single-turn request is passed through unchanged.
func classificationTurn(chat llm.ChatRequest, turn llm.Message) llm.Message {
	if chat.SystemPrompt == "" && len(chat.Messages) == 1 && len(turn.Content) <= maxClassifierContextBytes {
		return turn
	}

	chunks := make([]string, 0, len(chat.Messages)+1)
	if chat.SystemPrompt != "" {
		chunks = append(chunks, "system: "+chat.SystemPrompt)
	}
	for _, m := range chat.Messages {
		var b strings.Builder
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		if len(m.Images) > 0 {
			b.WriteString(" [image attached]")
		}
		for _, tc := range m.ToolCalls {
			b.WriteString(" [tool call ")
			b.WriteString(tc.Name)
			b.WriteString(": ")
			b.Write(tc.Input)
			b.WriteByte(']')
		}
		if m.ToolCallID != "" {
			b.WriteString(" [tool result for ")
			b.WriteString(m.ToolCallID)
			b.WriteByte(']')
		}
		chunks = append(chunks, b.String())
	}

	content := strings.Join(chunks, "\n")
	if len(content) > maxClassifierContextBytes {
		const marker = "\n...[middle context omitted]...\n"
		available := maxClassifierContextBytes - len(marker)
		headBytes := available / 2
		tailBytes := available - headBytes
		head := content[:headBytes]
		for !utf8.ValidString(head) {
			head = head[:len(head)-1]
		}
		tail := content[len(content)-tailBytes:]
		for !utf8.ValidString(tail) {
			tail = tail[1:]
		}
		content = head + marker + tail
	}
	return llm.Message{Role: "user", Content: content, Images: turn.Images}
}

// requestHasImages reports whether any message carries image content.
func requestHasImages(chat llm.ChatRequest) bool {
	for _, m := range chat.Messages {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}

// NeedsObservation reports whether the router requires post-request observation
// (incumbent/cache forecasts, sticky affinity, or cache-aware per-turn scoring).
// When false the server can skip the wrapper goroutine entirely.
func (r *Router) NeedsObservation() bool {
	return r.cfg.Routing.Strategy != config.RoutingStrategyPerTurn || r.cfg.Routing.CacheAware
}

// SetClassifier overrides the classification function (test seam).
func (r *Router) SetClassifier(fn func(ctx context.Context, prov llm.LLMProvider, model string, maxTokens int, turn llm.Message) TaskProfile) {
	r.classifyFn = fn
}
