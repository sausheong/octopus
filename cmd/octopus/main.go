//go:build !darwin

// Command octopus runs the Anthropic- and OpenAI-compatible LLM routing server.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sausheong/octopus/config"
	"github.com/sausheong/octopus/registry"
	"github.com/sausheong/octopus/router"
	"github.com/sausheong/octopus/server"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config YAML")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}

	// Resolve every provider's key into its config (reading api_key_env now,
	// while the environment is still intact), then scrub the ambient
	// ANTHROPIC_* credentials. The Anthropic SDK otherwise picks up
	// ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY from the process environment
	// and sends it as an Authorization: Bearer header alongside the explicit
	// per-provider x-api-key — Anthropic-compatible backends (DeepSeek,
	// MiniMax, Qwen) authenticate off that Bearer header, so the ambient
	// token would leak into and break every non-litellm backend. Pinning each
	// key inline and clearing the env guarantees each client uses only its own.
	for name, creds := range cfg.Providers {
		creds.APIKey = creds.Key()
		cfg.Providers[name] = creds
	}
	os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	os.Unsetenv("ANTHROPIC_API_KEY")

	reg, err := registry.New(context.Background(), cfg)
	if err != nil {
		slog.Error("registry init failed", "err", err)
		os.Exit(1)
	}

	rt := router.NewRouter(cfg, reg)
	srv := server.New(rt, reg, cfg.Catalog)
	// Opt-in: an unconfigured or unset variable yields "", which leaves the
	// endpoints open exactly as they were before this option existed.
	if cfg.AuthTokenMisconfigured() {
		slog.Warn("auth token variable is empty; routing endpoints are UNAUTHENTICATED",
			"auth_token_env", cfg.AuthTokenEnv)
	}
	srv.SetAuthToken(cfg.AuthToken())

	httpSrv := &http.Server{
		Addr:    cfg.ServerAddr,
		Handler: srv.Handler(),
		// ReadHeaderTimeout prevents Slowloris-style header attacks.
		// ReadTimeout covers header + body so slow body senders cannot hold
		// connections indefinitely. SSE responses are not affected because
		// ReadTimeout only applies until the request body is fully read.
		// No WriteTimeout: SSE streams are long-lived by design.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Start serving in a goroutine so the main goroutine can wait on signals.
	go func() {
		slog.Info("octopus listening", "addr", cfg.ServerAddr)
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down, draining in-flight requests...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "err", err)
		os.Exit(1)
	}
	slog.Info("stopped")
}
