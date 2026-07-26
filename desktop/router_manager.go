// Package desktop contains the long-lived services used by the macOS app.
package desktop

import (
	"context"
	"errors"
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
	mu         sync.RWMutex
	configPath string
	environ    map[string]string
	httpServer *http.Server
	listener   net.Listener
	status     RouterStatus
	insights   *insights.Tracker
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
	cfg, err := config.Load(m.configPath)
	if err != nil {
		m.setFailure(err)
		return err
	}
	resolved := cloneAndResolve(cfg, m.environ)
	reg, err := registry.New(ctx, resolved)
	if err != nil {
		m.setFailure(err)
		return err
	}
	rt := router.NewRouter(resolved, reg)
	srv := server.New(rt, reg, resolved.Catalog, m.insights.Record)
	// Opt-in: an unconfigured or unset variable yields "", which leaves the
	// endpoints open exactly as they were before this option existed.
	srv.SetAuthToken(resolved.AuthToken())
	handler := srv.Handler()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.httpServer != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = m.httpServer.Shutdown(shutdownCtx)
		cancel()
		m.httpServer = nil
		m.listener = nil
	}

	listener, err := net.Listen("tcp", resolved.ServerAddr)
	if err != nil {
		m.status = RouterStatus{ConfigPath: m.configPath, ConfigValid: true, LastError: err.Error(), UpdatedAt: time.Now()}
		return err
	}
	httpServer := &http.Server{
		Addr:              resolved.ServerAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	m.httpServer = httpServer
	m.listener = listener
	m.status = RouterStatus{
		Running:     true,
		ConfigValid: true,
		Address:     resolved.ServerAddr,
		ConfigPath:  m.configPath,
		UpdatedAt:   time.Now(),
	}
	go m.serve(httpServer, listener)
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.httpServer == nil {
		return nil
	}
	err := m.httpServer.Shutdown(ctx)
	m.httpServer = nil
	m.listener = nil
	m.status.Running = false
	m.status.UpdatedAt = time.Now()
	return err
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
