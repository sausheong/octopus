// Command router runs the Anthropic-compatible LLM routing server.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/sausheong/llmrouter/config"
	"github.com/sausheong/llmrouter/registry"
	"github.com/sausheong/llmrouter/router"
	"github.com/sausheong/llmrouter/server"
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

	reg, err := registry.New(context.Background(), cfg)
	if err != nil {
		slog.Error("registry init failed", "err", err)
		os.Exit(1)
	}

	rt := router.NewRouter(cfg, reg)
	srv := server.New(rt, reg)

	slog.Info("llmrouter listening", "addr", cfg.ServerAddr)
	if err := http.ListenAndServe(cfg.ServerAddr, srv.Handler()); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
