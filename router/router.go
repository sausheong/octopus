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
	Model         string
	ExpiresAt     time.Time
	CacheUntil    time.Time
	CacheFraction float64
}

// NewRouter builds a Router. The classifier provider is resolved per request
// from the registry using cfg.Classifier.Model.
func NewRouter(cfg *config.Config, reg *registry.Registry) *Router {
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
	if !ok {
		prof = DefaultProfile()
	} else if isTrivial(chat, turn) {
		prof = TrivialProfile()
		slog.Debug("classifier skipped (trivial request)")
	} else {
		prov, model, err := r.reg.Resolve(r.cfg.Classifier.Model)
		if err != nil {
			slog.Warn("classifier provider unresolved; using default profile", "err", err)
			prof = DefaultProfile()
		} else {
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

	// Compute session ID once; reuse for both sticky and cache multipliers.
	sid := ""
	if r.cfg.Routing.SessionSticky || r.cfg.Routing.CacheAware {
		sid = SessionID(chat)
	}
	multipliers := r.cacheInputMultipliersForSession(chat, sid)
	d := ScoreWithInputMultipliers(prof, r.cfg.Catalog, r.cfg.Weights, multipliers)
	d.ClassifierModel = r.cfg.Classifier.Model
	d.ClassifierUsage = classifierUsage
	if sticky := r.stickyModelForSession(sid); sticky != "" {
		for _, id := range d.Eligible {
			if id == sticky {
				d.Chosen = sticky
				d.Reason = "sticky session affinity"
				break
			}
		}
	}
	// If the profile benefits from reasoning, recommend medium effort. The
	// server applies it only to candidates that advertise reasoning support.
	if prof.NeedsReasoning {
		d.Reasoning = llm.ReasoningMedium
	}
	slog.Info("routing decision",
		"chosen", d.Chosen,
		"reason", d.Reason,
		"difficulty", d.Profile.Difficulty,
		"needs_reasoning", d.Profile.NeedsReasoning,
		"needs_vision", d.Profile.NeedsVision,
		"needs_tools", d.Profile.NeedsTools,
		"eligible", d.Eligible,
	)
	return d
}

const (
	CacheReadInputMultiplier       = 0.10
	CacheWrite5mInputMultiplier    = 1.25
	CacheWrite1HourInputMultiplier = 2.00
)

// SessionID returns an explicit client session ID, or a deterministic fallback
// based on the stable conversation prefix used by prompt caching.
func SessionID(chat llm.ChatRequest) string {
	if chat.SessionID != "" {
		sum := sha256.Sum256([]byte(chat.SessionID))
		return "explicit:" + hex.EncodeToString(sum[:])
	}
	h := sha256.New()
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
	if !r.cfg.Routing.SessionSticky || sid == "" {
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
	return state.Model
}

func (r *Router) cacheInputMultipliersForSession(chat llm.ChatRequest, sid string) map[string]float64 {
	ttl := CacheTTL(chat)
	if !r.cfg.Routing.CacheAware || ttl == 0 || sid == "" {
		return nil
	}
	now := time.Now()
	r.sessionsMu.Lock()
	state := r.sessions[sid]
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
		if state.Model == entry.ID && state.CacheUntil.After(now) {
			fraction := state.CacheFraction
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
	if !r.cfg.Routing.SessionSticky && !r.cfg.Routing.CacheAware {
		return
	}
	now := time.Now()
	ttl := r.cfg.Routing.SessionTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	id := SessionID(chat)
	r.sessionsMu.Lock()
	previous := r.sessions[id]
	r.sessionsMu.Unlock()
	state := sessionState{Model: model, ExpiresAt: now.Add(ttl)}
	if previous.Model == model && previous.CacheUntil.After(now) {
		state.CacheUntil = previous.CacheUntil
		state.CacheFraction = previous.CacheFraction
	}
	if usage != nil && (usage.CacheCreationInputTokens > 0 || usage.CacheReadInputTokens > 0) {
		if cacheDuration := CacheTTL(chat); cacheDuration > 0 {
			state.CacheUntil = now.Add(cacheDuration)
			cached := usage.CacheCreationInputTokens + usage.CacheReadInputTokens
			total := cached + usage.InputTokens
			if total > 0 {
				state.CacheFraction = float64(cached) / float64(total)
			}
		}
	}
	r.sessionsMu.Lock()
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
// (session affinity or cache-aware scoring). When false the server can skip the
// observeEvents wrapper goroutine entirely, reducing per-request overhead.
func (r *Router) NeedsObservation() bool {
	return r.cfg.Routing.SessionSticky || r.cfg.Routing.CacheAware
}

// SetClassifier overrides the classification function (test seam).
func (r *Router) SetClassifier(fn func(ctx context.Context, prov llm.LLMProvider, model string, maxTokens int, turn llm.Message) TaskProfile) {
	r.classifyFn = fn
}
