// Package server exposes the Anthropic-compatible HTTP endpoint, wiring the
// decoder, router, provider registry, and response encoders together.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
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

// maxRequestBytes caps the body size for inbound requests (32 MiB).
const maxRequestBytes = 32 << 20

// Server handles POST /v1/messages, POST /v1/chat/completions, and GET /v1/models.
type Server struct {
	rt            *router.Router
	reg           *registry.Registry
	catalog       []config.CatalogEntry
	usageObserver func(insights.Observation)
}

// New builds a Server.
func New(rt *router.Router, reg *registry.Registry, catalog []config.CatalogEntry, observers ...func(insights.Observation)) *Server {
	server := &Server{rt: rt, reg: reg, catalog: catalog}
	if len(observers) > 0 {
		server.usageObserver = observers[0]
	}
	return server
}

// Handler returns the HTTP handler (mux) for the server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", s.handleMessages)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/models", s.handleModels)
	return mux
}

// candidates returns the ordered list of provider IDs to try: chosen first,
// then the rest of Eligible in score order (duplicates removed).
func candidates(dec router.Decision) []string {
	out := []string{dec.Chosen}
	for _, id := range dec.Eligible {
		if id != dec.Chosen {
			out = append(out, id)
		}
	}
	return out
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

		ch, err := prov.ChatStream(ctx, attempt)
		if err != nil {
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
		first, ok := <-ch
		if !ok {
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
			go func() {
				for range ch {
				}
			}()
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
			defer close(out)
			for ev := range ch {
				out <- ev
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

		ch, err := prov.ChatStream(ctx, attempt)
		if err != nil {
			lastErr = err
			logAttemptFailure("provider stream open failed, trying fallback", id, err)
			attempts++
			if !anthropicio.Retryable(lastErr) || attempts >= maxTries {
				break
			}
			continue
		}

		// Require at least one meaningful event before handing the channel to
		// the collector. An empty stream (closed without events) is treated as
		// an upstream failure so the next candidate can be tried.
		peeked, peekErr := peekForContent(ch)
		if peekErr != nil {
			lastErr = peekErr
			logAttemptFailure("provider returned empty or failed stream, trying fallback", id, peekErr)
			attempts++
			if !anthropicio.Retryable(lastErr) || attempts >= maxTries {
				break
			}
			continue
		}

		out, err := collect(id, s.observeEvents(chat, id, dec, peeked))
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
					s.rt.Observe(chat, model, ev.Usage)
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
func peekForContent(ch <-chan llm.ChatEvent) (<-chan llm.ChatEvent, error) {
	first, ok := <-ch
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
	dec := s.rt.Route(ctx, dr.Chat)

	if dec.NoEligible {
		writeError(w, anthropicio.NewAPIError("invalid_request", "no catalog model can satisfy this request (context too large or missing capability)"))
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
	dec := s.rt.Route(ctx, chat)

	if dec.NoEligible {
		writeOAIError(w, http.StatusUnprocessableEntity, "invalid_request_error", "no catalog model can satisfy this request (context too large or missing capability)")
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

// flushWriter adapts http.ResponseWriter to SSEWriter, flushing after each
// event so the client gets a real stream.
type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(p []byte) (int, error) { return f.w.Write(p) }
func (f *flushWriter) Flush() {
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
}
