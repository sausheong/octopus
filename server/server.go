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

// tryProviders attempts ChatStream on the chosen model first, then falls back
// through the eligible list in score order on any ChatStream error. Returns the
// open event channel, the model id that succeeded, and an error only if every
// candidate failed. Fallback is intentionally pre-stream only — once headers
// are written the caller owns the channel and there is no going back.
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
		chat.Model = model
		ch, err := prov.ChatStream(ctx, chat)
		if err != nil {
			lastErr = err
			if id == dec.Chosen {
				slog.Warn("chosen provider failed, trying fallback", "model", id, "err", err)
			} else {
				slog.Warn("fallback provider failed", "model", id, "err", err)
			}
			continue
		}
		if id != dec.Chosen {
			slog.Info("using fallback model", "model", id, "original", dec.Chosen)
		}
		return ch, id, nil
	}
	return nil, "", lastErr
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

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	start := time.Now()

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
		ae := anthropicio.MapBackendError(err)
		writeOAIError(w, http.StatusBadGateway, "api_error", ae.Message)
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
			ae := anthropicio.MapBackendError(err)
			writeOAIError(w, http.StatusBadGateway, "api_error", ae.Message)
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
