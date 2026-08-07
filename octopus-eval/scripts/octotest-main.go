// Command octotest runs Octopus headlessly for measurement.
//
// It is a byte-for-byte copy of cmd/octopus/main.go's wiring (the !darwin
// build), minus the menubar/settings UI that the darwin entrypoint forces.
// Same config -> registry -> router -> server chain, so measurements taken
// through this binary exercise the real routing and proxying code paths.
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
	srv.SetAuthToken(cfg.AuthToken())

	httpSrv := &http.Server{
		Addr:              cfg.ServerAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		slog.Info("octotest listening", "addr", cfg.ServerAddr)
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
}
