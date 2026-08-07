// Package desktop contains the long-lived services used by the macOS app.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sausheong/octopus/config"
	"github.com/sausheong/octopus/insights"
	"github.com/sausheong/octopus/registry"
	"github.com/sausheong/octopus/router"
	"github.com/sausheong/octopus/server"
)

// RouterStatus is safe to expose to the loopback settings application.
type RouterStatus struct {
	Running     bool      `json:"running"`
	ConfigValid bool      `json:"config_valid"`
	Address     string    `json:"address,omitempty"`
	ConfigPath  string    `json:"config_path"`
	LastError   string    `json:"last_error,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RouterManager owns the reloadable routing HTTP server.
type RouterManager struct {
	reloadMu   sync.Mutex
	mu         sync.RWMutex
	configPath string
	environ    map[string]string
	httpServer *http.Server
	listener   net.Listener
	handler    *reloadableHandler
	status     RouterStatus
	insights   *insights.Tracker
}

// reloadableHandler lets a same-address config reload publish a fully built
// router atomically while the listener and in-flight requests keep running.
// ServeHTTP copies the current handler under a read lock and releases it before
// executing the request, so slow streams never block a future swap.
type reloadableHandler struct {
	mu      sync.RWMutex
	current http.Handler
}

func newReloadableHandler(handler http.Handler) *reloadableHandler {
	return &reloadableHandler{current: handler}
}

func (h *reloadableHandler) Swap(handler http.Handler) {
	h.mu.Lock()
	h.current = handler
	h.mu.Unlock()
}

func (h *reloadableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	current := h.current
	h.mu.RUnlock()
	current.ServeHTTP(w, r)
}

func NewRouterManager(configPath string) *RouterManager {
	environ := make(map[string]string)
	for _, item := range os.Environ() {
		if key, value, ok := strings.Cut(item, "="); ok {
			environ[key] = value
		}
	}
	return &RouterManager{
		configPath: configPath,
		environ:    environ,
		status: RouterStatus{
			ConfigPath: configPath,
			UpdatedAt:  time.Now(),
		},
		insights: insights.NewTracker(filepath.Join(filepath.Dir(configPath), "insights.json")),
	}
}

// ScrubAnthropicEnvironment prevents ambient Claude credentials from being
// added by the Anthropic SDK to custom Anthropic-shaped provider requests.
// RouterManager retains a private launch-time copy for future reloads.
func (m *RouterManager) ScrubAnthropicEnvironment() {
	_ = os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
}

func (m *RouterManager) Status() RouterStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

// Insights returns range-filtered, aggregate request economics.
func (m *RouterManager) Insights(days int) insights.Report {
	return m.insights.Report(days)
}

// Reload validates the file and constructs every provider before disturbing
// the currently running server. A malformed save therefore cannot stop a
// healthy router.
func (m *RouterManager) Reload(ctx context.Context) error {
	// Serialise reloads while doing expensive provider construction as well as
	// publication; otherwise two simultaneous saves could publish out of order.
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	cfg, err := config.Load(m.configPath)
	if err != nil {
		m.setFailure(err)
		return err
	}
	resolved := cloneAndResolve(cfg, m.environ)
	authToken := ""
	if resolved.AuthTokenEnv != "" {
		authToken = m.environ[resolved.AuthTokenEnv]
	}
	if resolved.AuthTokenEnv != "" && authToken == "" {
		err := fmt.Errorf("server.auth_token_env %q is configured but unset", resolved.AuthTokenEnv)
		m.setFailure(err)
		return err
	}
	reg, err := registry.New(ctx, resolved)
	if err != nil {
		m.setFailure(err)
		return err
	}
	rt := router.NewRouter(resolved, reg)
	srv := server.New(rt, reg, resolved.Catalog, m.insights.Record)
	srv.SetAuthToken(authToken)
	nextHandler := srv.Handler()

	m.mu.Lock()
	if m.httpServer != nil && m.status.Address == resolved.ServerAddr {
		m.handler.Swap(nextHandler)
		m.status = RouterStatus{
			Running:     true,
			ConfigValid: true,
			Address:     resolved.ServerAddr,
			ConfigPath:  m.configPath,
			UpdatedAt:   time.Now(),
		}
		m.mu.Unlock()
		slog.Info("octopus router reloaded", "addr", resolved.ServerAddr, "config", m.configPath)
		return nil
	}
	oldServer := m.httpServer

	listener, err := net.Listen("tcp", resolved.ServerAddr)
	if err != nil {
		// Binding the replacement failed. The old listener and handler remain
		// untouched, and status continues to describe that last-known-good server.
		m.status.ConfigValid = true
		m.status.LastError = err.Error()
		m.status.UpdatedAt = time.Now()
		m.mu.Unlock()
		return err
	}
	handler := newReloadableHandler(nextHandler)
	httpServer := &http.Server{
		Addr:              resolved.ServerAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	m.httpServer = httpServer
	m.listener = listener
	m.handler = handler
	m.status = RouterStatus{
		Running:     true,
		ConfigValid: true,
		Address:     resolved.ServerAddr,
		ConfigPath:  m.configPath,
		UpdatedAt:   time.Now(),
	}
	m.mu.Unlock()
	go m.serve(httpServer, listener)

	// The new address is already bound and published. Only now drain the old
	// server; failure to drain cannot take the replacement back down.
	if oldServer != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := oldServer.Shutdown(shutdownCtx); err != nil {
			slog.Warn("old router did not drain cleanly after address change", "err", err)
		}
		cancel()
	}
	slog.Info("octopus router reloaded", "addr", resolved.ServerAddr, "config", m.configPath)
	return nil
}

func (m *RouterManager) serve(httpServer *http.Server, listener net.Listener) {
	err := httpServer.Serve(listener)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.httpServer == httpServer {
		m.status.Running = false
		m.status.LastError = err.Error()
		m.status.UpdatedAt = time.Now()
		m.httpServer = nil
		m.listener = nil
		m.handler = nil
	}
	slog.Error("router server stopped", "err", err)
}

func (m *RouterManager) setFailure(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.ConfigValid = false
	m.status.LastError = err.Error()
	m.status.UpdatedAt = time.Now()
}

func (m *RouterManager) Shutdown(ctx context.Context) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.mu.Lock()
	httpServer := m.httpServer
	m.httpServer = nil
	m.listener = nil
	m.handler = nil
	m.status.Running = false
	m.status.UpdatedAt = time.Now()
	m.mu.Unlock()
	var serverErr error
	if httpServer != nil {
		serverErr = httpServer.Shutdown(ctx)
	}
	insightsErr := m.insights.Close(ctx)
	return errors.Join(serverErr, insightsErr)
}

func cloneAndResolve(cfg *config.Config, environ map[string]string) *config.Config {
	copyCfg := *cfg
	copyCfg.Providers = make(map[string]config.ProviderCreds, len(cfg.Providers))
	for name, creds := range cfg.Providers {
		if creds.APIKey == "" && creds.APIKeyEnv != "" {
			creds.APIKey = environ[creds.APIKeyEnv]
		}
		copyCfg.Providers[name] = creds
	}
	copyCfg.Catalog = append([]config.CatalogEntry(nil), cfg.Catalog...)
	return &copyCfg
}
