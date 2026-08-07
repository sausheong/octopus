// Package server exposes the Anthropic-compatible HTTP endpoint, wiring the
// decoder, router, provider registry, and response encoders together.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/octopus/anthropicio"
	"github.com/sausheong/octopus/config"
	"github.com/sausheong/octopus/insights"
	"github.com/sausheong/octopus/openaiio"
	"github.com/sausheong/octopus/registry"
	"github.com/sausheong/octopus/router"
)

const (
	// maxRequestBytes caps the body size for inbound requests (32 MiB).
	maxRequestBytes = 32 << 20
	// defaultFirstEventTimeout bounds how long an upstream may keep a request
	// waiting without producing even its first event. Long-lived SSE responses
	// remain unlimited after that point and are governed by client cancellation.
	defaultFirstEventTimeout = 30 * time.Second
)

var errFirstEventTimeout = errors.New("provider timed out before first event")

const (
	minQualityHeader     = "X-Octopus-Min-Quality"
	fixedModelHeader     = "X-Octopus-Fixed-Model"
	highestQualityHeader = "X-Octopus-Highest-Quality"
)

// Server handles POST /v1/messages, POST /v1/chat/completions, and GET /v1/models.
type Server struct {
	rt                *router.Router
	reg               *registry.Registry
	catalog           []config.CatalogEntry
	usageObserver     func(insights.Observation)
	authToken         string
	firstEventTimeout time.Duration
}

// SetAuthToken enables shared-secret authentication on the routing endpoints.
// An empty token disables it, which is the default and preserves the
// no-credentials behaviour every existing client relies on.
func (s *Server) SetAuthToken(token string) { s.authToken = token }

// New builds a Server.
func New(rt *router.Router, reg *registry.Registry, catalog []config.CatalogEntry, observers ...func(insights.Observation)) *Server {
	server := &Server{rt: rt, reg: reg, catalog: catalog, firstEventTimeout: defaultFirstEventTimeout}
	if len(observers) > 0 {
		server.usageObserver = observers[0]
	}
	return server
}

// SetFirstEventTimeout changes the maximum wait for the first provider event.
// It primarily exists to make the safety bound testable without slow tests.
// Non-positive values restore the production default.
func (s *Server) SetFirstEventTimeout(timeout time.Duration) {
	if timeout <= 0 {
		timeout = defaultFirstEventTimeout
	}
	s.firstEventTimeout = timeout
}

// authorized reports whether the request carries the configured shared secret.
// Anthropic clients send x-api-key and OpenAI clients send an Authorization
// bearer, so both are accepted. Always true when no token is configured.
func (s *Server) authorized(r *http.Request) bool {
	if s.authToken == "" {
		return true
	}
	if key := r.Header.Get("x-api-key"); key != "" &&
		subtle.ConstantTimeCompare([]byte(key), []byte(s.authToken)) == 1 {
		return true
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return bearer != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(s.authToken)) == 1
}

// Handler returns the HTTP handler (mux) for the server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeAnthropicUnauthorized(w)
			return
		}
		s.handleMessages(w, r)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeOAIError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid credentials")
			return
		}
		s.handleChatCompletions(w, r)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeOAIError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid credentials")
			return
		}
		s.handleModels(w, r)
	})
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeProbe(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	writeProbe(w, http.StatusOK, "ok")
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeProbe(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	// Registry creation validates provider construction before a Server is
	// published. Keep this response deliberately minimal and unauthenticated so
	// local launch agents can probe it without exposing catalogue or credentials.
	if s.rt == nil || s.reg == nil || len(s.catalog) == 0 {
		writeProbe(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	writeProbe(w, http.StatusOK, "ready")
}

func writeProbe(w http.ResponseWriter, status int, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": value})
}

// firstEvent waits for an upstream's first event without allowing a provider
// that accepted the request but then stalled to pin the client indefinitely.
func (s *Server) firstEvent(ctx context.Context, ch <-chan llm.ChatEvent) (llm.ChatEvent, bool, error) {
	timer := time.NewTimer(s.firstEventTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return llm.ChatEvent{}, false, ctx.Err()
	case <-timer.C:
		return llm.ChatEvent{}, false, errFirstEventTimeout
	case first, ok := <-ch:
		return first, ok, nil
	}
}

// requestMetadata admits routing-policy overrides only on deployments that
// configured authentication and only from the authenticated caller. Header
// values on an open endpoint are ignored, preventing an arbitrary local client
// from forcing expensive models. Workflow affinity retains its legacy status
// as a non-policy placement hint.
func (s *Server) requestMetadata(r *http.Request, endpoint string, stream bool) (router.RequestMetadata, error) {
	meta := router.RequestMetadata{
		Endpoint: endpoint, Stream: stream, WorkflowID: r.Header.Get(router.WorkflowIDHeader),
	}
	if s.authToken == "" || !s.authorized(r) {
		return meta, nil
	}
	if value := strings.TrimSpace(r.Header.Get(minQualityHeader)); value != "" {
		minimum, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(minimum) || math.IsInf(minimum, 0) || minimum < 0 || minimum > 1 {
			return meta, errors.New("X-Octopus-Min-Quality must be a number between 0 and 1")
		}
		meta.MinQuality = minimum
	}
	meta.FixedModel = strings.TrimSpace(r.Header.Get(fixedModelHeader))
	if value := strings.TrimSpace(r.Header.Get(highestQualityHeader)); value != "" {
		highest, err := strconv.ParseBool(value)
		if err != nil {
			return meta, errors.New("X-Octopus-Highest-Quality must be true or false")
		}
		meta.HighestQuality = highest
	}
	if meta.FixedModel != "" && meta.HighestQuality {
		return meta, errors.New("fixed-model and highest-quality overrides cannot be combined")
	}
	return meta, nil
}

// candidates returns the ordered list of provider IDs to try: chosen first,
// then the rest of Eligible in score order (duplicates removed).
func candidates(dec router.Decision) []string {
	if dec.PolicyOverride == "fixed_model" {
		return []string{dec.Chosen}
	}
	out := []string{dec.Chosen}
	for _, id := range dec.Eligible {
		if id != dec.Chosen {
			out = append(out, id)
		}
	}
	return out
}

func noEligibleMessage(dec router.Decision) string {
	if dec.QualityPolicy == "strict" || dec.Reason == "no eligible model meets quality floor" {
		return fmt.Sprintf("no catalog model meets the required quality floor %.2f", dec.AppliedQualityFloor)
	}
	if dec.DataPolicy == config.DataPolicyLocalOnly {
		return "no local model can satisfy this request; remote routing is disabled by routing.data_policy=local_only"
	}
	return "no catalog model can satisfy this request (context too large or missing capability)"
}

// prepareAttempt resolves a provider candidate, normalizes tools, and applies
// the routing decision's reasoning mode to the request.
func (s *Server) prepareAttempt(id string, chat llm.ChatRequest, dec router.Decision) (llm.LLMProvider, llm.ChatRequest, error) {
	prov, model, err := s.reg.Resolve(id)
	if err != nil {
		return nil, llm.ChatRequest{}, err
	}
	attempt := chat
	attempt.Model = model
	attempt.Reasoning = llm.ReasoningOff
	for _, entry := range s.catalog {
		if entry.ID == id && entry.Caps.Reasoning {
			attempt.Reasoning = dec.Reasoning
			break
		}
	}
	if len(chat.Tools) > 0 {
		normalized, diags := prov.NormalizeToolSchema(chat.Tools)
		attempt.Tools = normalized
		for _, d := range diags {
			slog.Warn("tool schema normalized", "model", id, "field", d.Field, "reason", d.Reason)
		}
	}
	return prov, attempt, nil
}

// attemptCap returns the per-request fallback bound, defaulting to 3 when a
// Decision predates the config field (hand-built Decisions in tests).
func attemptCap(dec router.Decision) int {
	if dec.MaxAttempts > 0 {
		return dec.MaxAttempts
	}
	return 3
}

// logAttemptFailure records a failed candidate. A client hang-up is not a
// server fault, so it is logged at debug: warning per candidate would turn one
// disconnect into a burst of alarming noise.
func logAttemptFailure(msg, id string, err error) {
	if err != nil && anthropicio.MapBackendError(err).Kind == anthropicio.KindCanceled {
		slog.Debug("request cancelled during provider attempt", "model", id)
		return
	}
	slog.Warn(msg, "model", id, "err", err)
}

// tryProvidersStream opens a streaming channel for the request, trying each
// candidate in order. It peeks the first event: an EventError or a closed
// channel (empty stream) both trigger fallback. Returns the pre-populated
// channel, the winning model ID, and an error only if every candidate failed.
func (s *Server) tryProvidersStream(ctx context.Context, dec router.Decision, chat llm.ChatRequest) (<-chan llm.ChatEvent, string, error) {
	var lastErr error
	// attempts counts only real backend calls, so a bound cap is spent on
	// things a different backend could plausibly fix.
	attempts := 0
	maxTries := attemptCap(dec)
	for _, id := range candidates(dec) {
		prov, attempt, err := s.prepareAttempt(id, chat, dec)
		if err != nil {
			// A misconfigured catalog entry never reached a backend, so it costs
			// nothing and must not consume one of the bounded attempts.
			lastErr = err
			slog.Warn("provider resolve failed", "model", id, "err", err)
			continue
		}

		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		ch, err := prov.ChatStream(attemptCtx, attempt)
		if err != nil {
			cancelAttempt()
			lastErr = err
			logAttemptFailure("provider stream open failed, trying fallback", id, err)
			attempts++
			if !anthropicio.Retryable(lastErr) || attempts >= maxTries {
				break
			}
			continue
		}

		// Peek the first event: empty channel or immediate EventError both mean
		// the provider failed before producing any content — we can still fall back.
		first, ok, waitErr := s.firstEvent(attemptCtx, ch)
		if waitErr != nil {
			cancelAttempt()
			lastErr = waitErr
			logAttemptFailure("provider produced no event before timeout, trying fallback", id, waitErr)
			attempts++
			if !anthropicio.Retryable(lastErr) || attempts >= maxTries {
				break
			}
			continue
		}
		if !ok {
			cancelAttempt()
			lastErr = errors.New("provider returned empty stream")
			slog.Warn("provider returned empty stream, trying fallback", "model", id)
			attempts++
			if !anthropicio.Retryable(lastErr) || attempts >= maxTries {
				break
			}
			continue
		}
		if first.Type == llm.EventError {
			chErr := first.Error
			if chErr == nil {
				chErr = errors.New("provider stream error")
			}
			lastErr = chErr
			logAttemptFailure("provider stream error on first event, trying fallback", id, chErr)
			cancelAttempt()
			attempts++
			if !anthropicio.Retryable(lastErr) || attempts >= maxTries {
				break
			}
			continue
		}

		// Valid first event — prepend onto a new channel and return.
		out := make(chan llm.ChatEvent, 1)
		out <- first
		go func() {
			defer cancelAttempt()
			defer close(out)
			for ev := range ch {
				select {
				case out <- ev:
				case <-attemptCtx.Done():
					return
				}
			}
		}()
		if id != dec.Chosen {
			slog.Info("using fallback model", "model", id, "original", dec.Chosen)
		}
		return s.observeEvents(chat, id, dec, out), id, nil
	}
	return nil, "", lastErr
}

// collectWithFallback attempts buffered collection for the request, trying each
// candidate in order. Unlike streaming, the full response is collected before
// any bytes are written, so mid-stream errors in the channel can also trigger
// fallback. Returns the collected bytes, the winning model ID, and an error
// only if every candidate failed.
func (s *Server) collectWithFallback(
	ctx context.Context,
	dec router.Decision,
	chat llm.ChatRequest,
	collect func(model string, ch <-chan llm.ChatEvent) ([]byte, error),
) ([]byte, string, error) {
	var lastErr error
	// attempts counts only real backend calls, so a bound cap is spent on
	// things a different backend could plausibly fix.
	attempts := 0
	maxTries := attemptCap(dec)
	for _, id := range candidates(dec) {
		prov, attempt, err := s.prepareAttempt(id, chat, dec)
		if err != nil {
			// A misconfigured catalog entry never reached a backend, so it costs
			// nothing and must not consume one of the bounded attempts.
			lastErr = err
			slog.Warn("provider resolve failed", "model", id, "err", err)
			continue
		}

		// A buffered client request does not need to make the upstream use SSE.
		// Prefer the provider's native non-streaming transport when it exposes
		// that optional capability; providers without it retain the existing
		// stream-and-collect behaviour.
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		var ch <-chan llm.ChatEvent
		if nonStreaming, ok := prov.(llm.NonStreamingProvider); ok {
			ch, err = nonStreaming.ChatNonStreaming(attemptCtx, attempt)
		} else {
			ch, err = prov.ChatStream(attemptCtx, attempt)
		}
		if err != nil {
			cancelAttempt()
			lastErr = err
			logAttemptFailure("provider request failed, trying fallback", id, err)
			attempts++
			if !anthropicio.Retryable(lastErr) || attempts >= maxTries {
				break
			}
			continue
		}

		// Require at least one meaningful event before handing the channel to
		// the collector. An empty stream (closed without events) is treated as
		// an upstream failure so the next candidate can be tried.
		peeked, peekErr := s.peekForContent(attemptCtx, ch)
		if peekErr != nil {
			cancelAttempt()
			lastErr = peekErr
			logAttemptFailure("provider returned empty or failed stream, trying fallback", id, peekErr)
			attempts++
			if !anthropicio.Retryable(lastErr) || attempts >= maxTries {
				break
			}
			continue
		}

		out, err := collect(id, s.observeEvents(chat, id, dec, peeked))
		cancelAttempt()
		if err != nil {
			lastErr = err
			logAttemptFailure("provider collection failed, trying fallback", id, err)
			attempts++
			if !anthropicio.Retryable(lastErr) || attempts >= maxTries {
				break
			}
			continue
		}

		if id != dec.Chosen {
			slog.Info("using fallback model", "model", id, "original", dec.Chosen)
		}
		return out, id, nil
	}
	return nil, "", lastErr
}

func (s *Server) observeEvents(chat llm.ChatRequest, model string, decision router.Decision, in <-chan llm.ChatEvent) <-chan llm.ChatEvent {
	// Skip the wrapper goroutine entirely when observation is a no-op.
	if (s.rt == nil || !s.rt.NeedsObservation()) && s.usageObserver == nil {
		return in
	}
	out := make(chan llm.ChatEvent, 1)
	go func() {
		defer close(out)
		for ev := range in {
			if ev.Type == llm.EventDone {
				if s.rt != nil && s.rt.NeedsObservation() {
					s.rt.ObserveDecision(chat, model, ev.Usage, decision)
				}
				if s.usageObserver != nil && ev.Usage != nil {
					s.usageObserver(insights.Observation{
						Chat: chat, Model: model, Decision: decision, Usage: ev.Usage, Catalog: s.catalog,
					})
				}
			}
			out <- ev
		}
	}()
	return out
}

// peekForContent reads the first event from ch. If the channel is empty
// (closed without events) or the first event is EventError, it returns an
// error so collectWithFallback can try the next candidate. Otherwise it
// returns a new channel with the first event prepended.
func (s *Server) peekForContent(ctx context.Context, ch <-chan llm.ChatEvent) (<-chan llm.ChatEvent, error) {
	first, ok, err := s.firstEvent(ctx, ch)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("provider returned empty stream")
	}
	if first.Type == llm.EventError {
		go func() {
			for range ch {
			}
		}()
		if first.Error != nil {
			return nil, first.Error
		}
		return nil, errors.New("provider stream error")
	}
	out := make(chan llm.ChatEvent, 1)
	out <- first
	go func() {
		defer close(out)
		for ev := range ch {
			out <- ev
		}
	}()
	return out, nil
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	entries := make([]modelEntry, 0, len(s.catalog))
	for _, e := range s.catalog {
		owner, _, _ := strings.Cut(e.ID, "/")
		entries = append(entries, modelEntry{
			ID:      e.ID,
			Object:  "model",
			OwnedBy: owner,
		})
	}
	body, _ := json.Marshal(map[string]any{"object": "list", "data": entries})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, err error) {
	status, body := anthropicio.MapError(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeOAIError writes an OpenAI-shaped error response.
func writeOAIError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    errType,
		},
	})
	_, _ = w.Write(body)
}

// oaiBackendError maps a provider error to an OpenAI-shaped HTTP response,
// preserving 429/503 status semantics rather than collapsing everything to 502.
func oaiBackendError(w http.ResponseWriter, err error) {
	ae := anthropicio.MapBackendError(err)
	var status int
	var errType string
	switch ae.Kind {
	case "rate_limit":
		status, errType = http.StatusTooManyRequests, "rate_limit_error"
	case "overloaded":
		status, errType = http.StatusServiceUnavailable, "overloaded_error"
	case "invalid_request":
		status, errType = http.StatusBadRequest, "invalid_request_error"
	case anthropicio.KindCanceled:
		// Both handlers return without writing anything for a hang-up, so this
		// is unreachable from them. It exists so this mapping cannot drift from
		// anthropicio.MapError (499) and hand a future caller a bogus 502.
		status, errType = 499, "invalid_request_error"
	default:
		status, errType = http.StatusBadGateway, "api_error"
	}
	writeOAIError(w, status, errType, ae.Message)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	start := time.Now()

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, anthropicio.NewAPIError("invalid_request", "cannot read body"))
		return
	}
	dr, err := anthropicio.Decode(body)
	if err != nil {
		writeError(w, anthropicio.NewAPIError("invalid_request", err.Error()))
		return
	}
	if sessionID := sessionIDHeader(r); sessionID != "" {
		dr.Chat.SessionID = sessionID
	}

	ctx := r.Context()
	meta, err := s.requestMetadata(r, "/v1/messages", dr.Stream)
	if err != nil {
		writeError(w, anthropicio.NewAPIError("invalid_request", err.Error()))
		return
	}
	dec := s.rt.RouteWithMetadata(ctx, dr.Chat, meta)
	dr.Chat = router.RequestForDecision(dr.Chat, dec)

	if dec.NoEligible {
		writeError(w, anthropicio.NewAPIError("invalid_request", noEligibleMessage(dec)))
		return
	}

	if dr.Stream {
		ch, chosen, err := s.tryProvidersStream(ctx, dec, dr.Chat)
		if err != nil {
			if anthropicio.MapBackendError(err).Kind == anthropicio.KindCanceled {
				// The client hung up; there is no one to receive a status or body.
				slog.Debug("request cancelled by client", "requested_model", dr.RequestedModel)
				return
			}
			writeError(w, anthropicio.MapBackendError(err))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		fw := &flushWriter{w: w}
		if err := anthropicio.EncodeSSE(fw, chosen, ch); err != nil {
			slog.Error("sse encode failed", "err", err, "model", chosen)
		}
		slog.Info("request handled", "model", chosen, "requested_model", dr.RequestedModel,
			"stream", true, "reason", dec.Reason, "elapsed_ms", time.Since(start).Milliseconds())
	} else {
		out, chosen, err := s.collectWithFallback(ctx, dec, dr.Chat, func(model string, ch <-chan llm.ChatEvent) ([]byte, error) {
			return anthropicio.CollectMessage(model, ch)
		})
		if err != nil {
			if anthropicio.MapBackendError(err).Kind == anthropicio.KindCanceled {
				// The client hung up; there is no one to receive a status or body.
				slog.Debug("request cancelled by client", "requested_model", dr.RequestedModel)
				return
			}
			writeError(w, anthropicio.MapBackendError(err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		slog.Info("request handled", "model", chosen, "requested_model", dr.RequestedModel,
			"stream", false, "reason", dec.Reason, "elapsed_ms", time.Since(start).Milliseconds())
	}
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	start := time.Now()

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "cannot read body")
		return
	}

	chat, stream, requestedModel, err := openaiio.Decode(body)
	if err != nil {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if sessionID := sessionIDHeader(r); sessionID != "" {
		chat.SessionID = sessionID
	}

	systemPrompt, msgs := openaiio.ExtractSystem(chat.Messages)
	chat.SystemPrompt = systemPrompt
	chat.Messages = msgs

	ctx := r.Context()
	meta, err := s.requestMetadata(r, "/v1/chat/completions", stream)
	if err != nil {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	dec := s.rt.RouteWithMetadata(ctx, chat, meta)
	chat = router.RequestForDecision(chat, dec)

	if dec.NoEligible {
		writeOAIError(w, http.StatusUnprocessableEntity, "invalid_request_error", noEligibleMessage(dec))
		return
	}

	if stream {
		ch, chosen, err := s.tryProvidersStream(ctx, dec, chat)
		if err != nil {
			if anthropicio.MapBackendError(err).Kind == anthropicio.KindCanceled {
				// The client hung up; there is no one to receive a status or body.
				slog.Debug("request cancelled by client", "requested_model", requestedModel)
				return
			}
			oaiBackendError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		fw := &flushWriter{w: w}
		if err := openaiio.EncodeSSE(fw, chosen, ch); err != nil {
			slog.Error("oai sse encode failed", "err", err, "model", chosen)
		}
		slog.Info("request handled", "endpoint", "oai", "model", chosen,
			"requested_model", requestedModel, "stream", true,
			"reason", dec.Reason, "elapsed_ms", time.Since(start).Milliseconds())
	} else {
		out, chosen, err := s.collectWithFallback(ctx, dec, chat, func(model string, ch <-chan llm.ChatEvent) ([]byte, error) {
			return openaiio.CollectCompletion(model, ch)
		})
		if err != nil {
			if anthropicio.MapBackendError(err).Kind == anthropicio.KindCanceled {
				// The client hung up; there is no one to receive a status or body.
				slog.Debug("request cancelled by client", "requested_model", requestedModel)
				return
			}
			oaiBackendError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		slog.Info("request handled", "endpoint", "oai", "model", chosen,
			"requested_model", requestedModel, "stream", false,
			"reason", dec.Reason, "elapsed_ms", time.Since(start).Milliseconds())
	}
}

func sessionIDHeader(r *http.Request) string {
	if sessionID := strings.TrimSpace(r.Header.Get("X-Octopus-Session-ID")); sessionID != "" {
		return sessionID
	}
	return strings.TrimSpace(r.Header.Get("X-LLMRouter-Session-ID"))
}

// writeErrorStatus writes an Anthropic-shaped error with an explicit status
// (used for transport-level errors like 405 that don't come from MapError).
func writeErrorStatus(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"`+msg+`"}}`)
}

// writeAnthropicUnauthorized emits a 401 in the Anthropic error shape.
// writeError has no 401 mapping, and reusing its invalid_request kind would
// report a credentials failure as a malformed request.
func writeAnthropicUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"missing or invalid credentials"}}`)
}

// flushWriter adapts http.ResponseWriter to SSEWriter, flushing after each
// event so the client gets a real stream.
type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(p []byte) (int, error) { return f.w.Write(p) }
func (f *flushWriter) Flush() {
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
}
