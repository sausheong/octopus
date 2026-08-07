// Command octotest-insights runs Octopus headlessly and records a fresh
// Insights ledger for the live routing comparison in run2.sh.
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
	"github.com/sausheong/octopus/insights"
	"github.com/sausheong/octopus/registry"
	"github.com/sausheong/octopus/router"
	"github.com/sausheong/octopus/server"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config YAML")
	insightsPath := flag.String("insights", "run2-insights.json", "path to a fresh Insights ledger")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	for name, creds := range cfg.Providers {
		creds.APIKey = creds.Key()
		cfg.Providers[name] = creds
	}
	// Do not leave provider credentials available to child processes.
	os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	os.Unsetenv("ANTHROPIC_API_KEY")

	reg, err := registry.New(context.Background(), cfg)
	if err != nil {
		slog.Error("registry init failed", "err", err)
		os.Exit(1)
	}
	rt := router.NewRouter(cfg, reg)
	tracker := insights.NewTracker(*insightsPath)
	srv := server.New(rt, reg, cfg.Catalog, tracker.Record)
	srv.SetAuthToken(cfg.AuthToken())

	httpSrv := &http.Server{
		Addr:              cfg.ServerAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       180 * time.Second,
		IdleTimeout:       180 * time.Second,
	}
	go func() {
		slog.Info("octotest-insights listening", "addr", cfg.ServerAddr, "insights", *insightsPath)
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	if err := tracker.Close(ctx); err != nil {
		slog.Error("insights close failed", "err", err)
		os.Exit(1)
	}
}
