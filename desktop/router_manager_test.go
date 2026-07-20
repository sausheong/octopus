package desktop

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sausheong/octopus/config"
)

func TestRouterManagerReloadKeepsHealthyServerOnInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	port := freePort(t)
	valid := fmt.Sprintf(`
server: {addr: "127.0.0.1:%d"}
weights: {quality: 1, cost: 1, speed: 1}
providers:
  local: {kind: openai, base_url: "http://127.0.0.1:9999/v1"}
catalog:
  - id: "local/test"
    quality: 0.5
    speed: 0.5
    caps: {max_context: 1000}
`, port)
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewRouterManager(path)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("Reload valid: %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	if !m.Status().Running {
		t.Fatal("router should be running")
	}
	if err := os.WriteFile(path, []byte("invalid: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background()); err == nil {
		t.Fatal("invalid reload should fail")
	}
	status := m.Status()
	if !status.Running || status.ConfigValid || status.LastError == "" {
		t.Fatalf("status after invalid reload = %+v", status)
	}
}

func TestCloneAndResolveUsesLaunchEnvironment(t *testing.T) {
	cfg := testConfig()
	resolved := cloneAndResolve(cfg, map[string]string{"PROVIDER_KEY": "secret"})
	if got := resolved.Providers["anthropic"].APIKey; got != "secret" {
		t.Fatalf("resolved key = %q", got)
	}
	if cfg.Providers["anthropic"].APIKey != "" {
		t.Fatal("input config was mutated")
	}
}

func testConfig() *config.Config {
	return &config.Config{
		ServerAddr: "127.0.0.1:8787",
		Weights:    config.Weights{Quality: 1},
		Routing:    config.RoutingCfg{SessionTTL: time.Hour},
		Providers:  map[string]config.ProviderCreds{"anthropic": {APIKeyEnv: "PROVIDER_KEY"}},
		Catalog: []config.CatalogEntry{{
			ID: "anthropic/test", Quality: 0.5, Speed: 0.5, Caps: config.Caps{MaxContext: 1000},
		}},
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listeners unavailable: %v", err)
		return 0
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
