package settings

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sausheong/octopus/desktop"
	"github.com/sausheong/octopus/insights"
	"github.com/sausheong/octopus/internal/buildinfo"
)

//go:embed static/*
var staticFiles embed.FS

type ReloadFunc func(context.Context) error
type StatusFunc func() desktop.RouterStatus
type InsightsFunc func(days int) insights.Report

type Server struct {
	store      *Store
	reload     ReloadFunc
	status     StatusFunc
	httpServer *http.Server
	listener   net.Listener
	url        string
	logo       []byte
	insights   InsightsFunc
}

func NewServer(store *Store, reload ReloadFunc, status StatusFunc, insightFuncs ...InsightsFunc) *Server {
	server := &Server{store: store, reload: reload, status: status, logo: loadLogo()}
	if len(insightFuncs) > 0 {
		server.insights = insightFuncs[0]
	}
	return server
}

func (s *Server) Start() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen for settings: %w", err)
	}
	s.listener = listener
	s.url = "http://" + listener.Addr().String()
	s.httpServer = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() {
		if err := s.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// The menu application remains alive; clicking Settings again makes
			// the failed local endpoint visible to the user.
			return
		}
	}()
	return s.url, nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/insights", s.handleInsights)
	mux.HandleFunc("POST /api/structured", s.handleStructured)
	mux.HandleFunc("POST /api/yaml", s.handleYAML)
	mux.HandleFunc("GET /assets/octopus.png", s.handleLogo)
	assets, _ := fs.Sub(staticFiles, "static")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := staticFiles.ReadFile("static/index.html")
		if err != nil {
			http.Error(w, "settings unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})
	return securityHeaders(mux)
}

func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 365 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "days must be between 1 and 365"})
			return
		}
		days = parsed
	}
	if s.insights == nil {
		writeJSON(w, http.StatusOK, insights.Report{RangeDays: days, Days: []insights.DayPoint{}, Models: []insights.ModelSummary{}})
		return
	}
	writeJSON(w, http.StatusOK, s.insights(days))
}

func (s *Server) handleLogo(w http.ResponseWriter, r *http.Request) {
	if len(s.logo) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(s.logo)
}

func loadLogo() []byte {
	candidates := []string{"octopus.png", filepath.Join("..", "octopus.png")}
	if executable, err := os.Executable(); err == nil {
		executableDir := filepath.Dir(executable)
		candidates = append([]string{
			filepath.Join(executableDir, "..", "Resources", "Octopus.png"),
			filepath.Join(executableDir, "octopus.png"),
		}, candidates...)
	}
	for _, candidate := range candidates {
		if data, err := os.ReadFile(candidate); err == nil {
			return data
		}
	}
	return nil
}

type stateResponse struct {
	Document  Document             `json:"document"`
	YAML      string               `json:"yaml"`
	Exists    bool                 `json:"exists"`
	Version   string               `json:"version"`
	LoadError string               `json:"load_error,omitempty"`
	Router    desktop.RouterStatus `json:"router"`
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	doc, raw, exists, err := s.store.Load()
	response := stateResponse{Document: doc, YAML: string(raw), Exists: exists, Version: buildinfo.Version, Router: s.routerStatus()}
	if err != nil {
		response.LoadError = err.Error()
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleStructured(w http.ResponseWriter, r *http.Request) {
	if !validWriteRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "settings write rejected"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var doc Document
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid settings document: " + err.Error()})
		return
	}
	raw, err := s.store.SaveDocument(doc)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	s.finishSave(w, r, doc, raw)
}

func (s *Server) handleYAML(w http.ResponseWriter, r *http.Request) {
	if !validWriteRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "settings write rejected"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		YAML string `json:"yaml"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	doc, err := s.store.SaveYAML([]byte(body.YAML))
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	s.finishSave(w, r, doc, []byte(body.YAML))
}

func (s *Server) finishSave(w http.ResponseWriter, r *http.Request, doc Document, raw []byte) {
	reloadErr := error(nil)
	if s.reload != nil {
		reloadCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		reloadErr = s.reload(reloadCtx)
		cancel()
	}
	response := stateResponse{Document: doc, YAML: string(raw), Exists: true, Version: buildinfo.Version, Router: s.routerStatus()}
	if reloadErr != nil {
		response.LoadError = "Saved, but the router could not reload: " + reloadErr.Error()
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) routerStatus() desktop.RouterStatus {
	if s.status == nil {
		return desktop.RouterStatus{ConfigPath: s.store.Path()}
	}
	return s.status()
}

func validWriteRequest(r *http.Request) bool {
	if r.Header.Get("X-Octopus-Settings") != "1" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return false
	}
	origin := r.Header.Get("Origin")
	return origin == "" || origin == "http://"+r.Host || origin == "https://"+r.Host
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
