// Package server exposes the Anthropic-compatible HTTP endpoint, wiring the
// decoder, router, provider registry, and response encoders together.
package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/llmrouter/anthropicio"
	"github.com/sausheong/llmrouter/config"
	"github.com/sausheong/llmrouter/openaiio"
	"github.com/sausheong/llmrouter/registry"
	"github.com/sausheong/llmrouter/router"
)

// maxRequestBytes caps the body size for inbound requests (32 MiB).
const maxRequestBytes = 32 << 20

// Server handles POST /v1/messages, POST /v1/chat/completions, and GET /v1/models.
type Server struct {
	rt      *router.Router
	reg     *registry.Registry
	catalog []config.CatalogEntry
}

// New builds a Server.
func New(rt *router.Router, reg *registry.Registry, catalog []config.CatalogEntry) *Server {
	return &Server{rt: rt, reg: reg, catalog: catalog}
}

// Handler returns the HTTP handler (mux) for the server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", s.handleMessages)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/models", s.handleModels)
	return mux
}

// tryProviders attempts ChatStream on the chosen model first (Eligible is
// already sorted by descending score), then falls back through the rest on
// any ChatStream error. For each candidate it applies provider-specific tool
// schema normalization before the call. Returns the open event channel, the
// model id that succeeded, and an error only if every candidate failed.
// Fallback is intentionally pre-stream only — once headers are written the
// caller owns the channel and there is no going back.
func (s *Server) tryProviders(ctx context.Context, dec router.Decision, chat llm.ChatRequest) (<-chan llm.ChatEvent, string, error) {
	candidates := []string{dec.Chosen}
	for _, id := range dec.Eligible {
		if id != dec.Chosen {
			candidates = append(candidates, id)
		}
	}

	var lastErr error
	for _, id := range candidates {
		prov, model, err := s.reg.Resolve(id)
		if err != nil {
			lastErr = err
			slog.Warn("provider resolve failed during fallback", "model", id, "err", err)
			continue
		}

		// Apply provider-specific tool schema normalization for this candidate.
		attempt := chat
		attempt.Model = model
		if len(chat.Tools) > 0 {
			normalized, diags := prov.NormalizeToolSchema(chat.Tools)
			attempt.Tools = normalized
			for _, d := range diags {
				slog.Warn("tool schema normalized", "model", id, "field", d.Field, "reason", d.Reason)
			}
		}

		ch, err := prov.ChatStream(ctx, attempt)
		if err != nil {
			lastErr = err
			if id == dec.Chosen {
				slog.Warn("chosen provider failed, trying fallback", "model", id, "err", err)
			} else {
				slog.Warn("fallback provider failed", "model", id, "err", err)
			}
			continue
		}

		// Peek at the first event. If it is an immediate EventError the provider
		// signalled failure via the channel rather than via the error return. We
		// can still fall back because no HTTP headers have been written yet.
		first, ok := <-ch
		if !ok {
			// Channel closed with no events — treat as empty success.
			empty := make(chan llm.ChatEvent)
			close(empty)
			return empty, id, nil
		}
		if first.Type == llm.EventError {
			chErr := first.Error
			if chErr == nil {
				chErr = errString("provider stream error")
			}
			lastErr = chErr
			if id == dec.Chosen {
				slog.Warn("chosen provider stream error, trying fallback", "model", id, "err", chErr)
			} else {
				slog.Warn("fallback provider stream error", "model", id, "err", chErr)
			}
			// Drain the now-broken channel before moving on.
			go func() {
				for range ch {
				}
			}()
			continue
		}

		// First event is valid — prepend it back onto a new channel.
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
		return out, id, nil
	}
	return nil, "", lastErr
}

type errString string

func (e errString) Error() string { return string(e) }

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

	ctx := r.Context()
	dec := s.rt.Route(ctx, dr.Chat)

	ch, chosen, err := s.tryProviders(ctx, dec, dr.Chat)
	if err != nil {
		writeError(w, anthropicio.MapBackendError(err))
		return
	}

	if dr.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		fw := &flushWriter{w: w}
		if err := anthropicio.EncodeSSE(fw, chosen, ch); err != nil {
			slog.Error("sse encode failed", "err", err, "model", chosen)
		}
	} else {
		out, err := anthropicio.CollectMessage(chosen, ch)
		if err != nil {
			writeError(w, anthropicio.MapBackendError(err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}

	slog.Info("request handled",
		"model", chosen,
		"requested_model", dr.RequestedModel,
		"stream", dr.Stream,
		"reason", dec.Reason,
		"elapsed_ms", time.Since(start).Milliseconds(),
	)
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

	systemPrompt, msgs := openaiio.ExtractSystem(chat.Messages)
	chat.SystemPrompt = systemPrompt
	chat.Messages = msgs

	ctx := r.Context()
	dec := s.rt.Route(ctx, chat)

	ch, chosen, err := s.tryProviders(ctx, dec, chat)
	if err != nil {
		oaiBackendError(w, err)
		return
	}

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		fw := &flushWriter{w: w}
		if err := openaiio.EncodeSSE(fw, chosen, ch); err != nil {
			slog.Error("oai sse encode failed", "err", err, "model", chosen)
		}
	} else {
		out, err := openaiio.CollectCompletion(chosen, ch)
		if err != nil {
			oaiBackendError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}

	slog.Info("request handled",
		"endpoint", "oai",
		"model", chosen,
		"requested_model", requestedModel,
		"stream", stream,
		"reason", dec.Reason,
		"elapsed_ms", time.Since(start).Milliseconds(),
	)
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
