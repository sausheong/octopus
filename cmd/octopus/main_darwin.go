//go:build darwin

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sausheong/octopus/desktop"
	"github.com/sausheong/octopus/menubar"
	"github.com/sausheong/octopus/settings"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	home, err := os.UserHomeDir()
	if err != nil {
		slog.Error("find home directory", "err", err)
		return
	}
	configPath := filepath.Join(home, ".octopus", "config.yaml")
	routerManager := desktop.NewRouterManager(configPath)
	if err := routerManager.Reload(context.Background()); err != nil {
		slog.Info("router awaiting valid settings", "config", configPath, "err", err)
	}
	routerManager.ScrubAnthropicEnvironment()

	settingsServer := settings.NewServer(settings.NewStore(configPath), routerManager.Reload, routerManager.Status, routerManager.Insights)
	settingsURL, err := settingsServer.Start()
	if err != nil {
		slog.Error("start settings", "err", err)
		_ = routerManager.Shutdown(context.Background())
		return
	}
	slog.Info("settings ready", "url", settingsURL, "config", configPath)

	quit := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = settingsServer.Shutdown(ctx)
		_ = routerManager.Shutdown(ctx)
	}
	if err := menubar.Run(settingsURL, quit); err != nil {
		slog.Error("run menu bar", "err", err)
		quit()
	}
}
