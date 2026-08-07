package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

func TestRouterManagerSameAddressReloadKeepsListener(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	port := freePort(t)
	writeManagerConfig(t, path, port, "local/first")
	m := NewRouterManager(path)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("first Reload: %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	firstListener := m.listener
	firstServer := m.httpServer

	writeManagerConfig(t, path, port, "local/second")
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("second Reload: %v", err)
	}
	if m.listener != firstListener || m.httpServer != firstServer {
		t.Fatal("same-address reload replaced listener or HTTP server")
	}
	body := getBody(t, fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
	if !strings.Contains(body, "local/second") || strings.Contains(body, "local/first") {
		t.Fatalf("models after atomic reload: %s", body)
	}
}

func TestRouterManagerAddressChangeBindsBeforeStoppingOld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	oldPort, newPort := freePort(t), freePort(t)
	writeManagerConfig(t, path, oldPort, "local/first")
	m := NewRouterManager(path)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("first Reload: %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	oldListener := m.listener

	writeManagerConfig(t, path, newPort, "local/second")
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("address-change Reload: %v", err)
	}
	if m.listener == oldListener || m.Status().Address != fmt.Sprintf("127.0.0.1:%d", newPort) {
		t.Fatalf("new listener/status not published: %+v", m.Status())
	}
	body := getBody(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", newPort))
	if !strings.Contains(body, "ready") {
		t.Fatalf("new readiness body: %s", body)
	}
}

func TestRouterManagerBindFailurePreservesLastKnownGood(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	oldPort := freePort(t)
	writeManagerConfig(t, path, oldPort, "local/first")
	m := NewRouterManager(path)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("first Reload: %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	oldListener := m.listener

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	blockedPort := occupied.Addr().(*net.TCPAddr).Port
	writeManagerConfig(t, path, blockedPort, "local/second")
	if err := m.Reload(context.Background()); err == nil {
		t.Fatal("reload to occupied address succeeded")
	}
	status := m.Status()
	if !status.Running || !status.ConfigValid || status.Address != fmt.Sprintf("127.0.0.1:%d", oldPort) || status.LastError == "" {
		t.Fatalf("status after bind failure: %+v", status)
	}
	if m.listener != oldListener {
		t.Fatal("bind failure replaced last-known-good listener")
	}
	if body := getBody(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", oldPort)); !strings.Contains(body, "ok") {
		t.Fatalf("old server health body: %s", body)
	}
}

func TestRouterManagerMissingConfiguredAuthPreservesLastKnownGood(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	port := freePort(t)
	writeManagerConfig(t, path, port, "local/first")
	m := NewRouterManager(path)
	delete(m.environ, "OCTOPUS_TEST_MISSING_TOKEN")
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("first Reload: %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data),
		fmt.Sprintf(`server: {addr: "127.0.0.1:%d"}`, port),
		fmt.Sprintf("server:\n  addr: \"127.0.0.1:%d\"\n  auth_token_env: OCTOPUS_TEST_MISSING_TOKEN", port), 1)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background()); err == nil {
		t.Fatal("reload with missing configured auth token succeeded")
	}
	status := m.Status()
	if !status.Running || status.ConfigValid || status.LastError == "" {
		t.Fatalf("status after auth failure: %+v", status)
	}
	if body := getBody(t, fmt.Sprintf("http://127.0.0.1:%d/v1/models", port)); !strings.Contains(body, "local/first") {
		t.Fatalf("last-known-good handler was not retained: %s", body)
	}
}

type failingListener struct{ err error }

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (failingListener) Close() error                { return nil }
func (failingListener) Addr() net.Addr              { return &net.TCPAddr{} }

func TestRouterManagerServeFailureClearsDeadServerState(t *testing.T) {
	m := NewRouterManager(filepath.Join(t.TempDir(), "config.yaml"))
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	httpServer := &http.Server{Handler: http.NotFoundHandler()}
	listener := failingListener{err: errors.New("accept failed")}
	m.httpServer = httpServer
	m.listener = listener
	m.handler = newReloadableHandler(http.NotFoundHandler())
	m.status = RouterStatus{Running: true, Address: "127.0.0.1:8787"}

	m.serve(httpServer, listener)
	if m.httpServer != nil || m.listener != nil || m.handler != nil || m.Status().Running {
		t.Fatalf("dead server state retained: status=%+v", m.Status())
	}
}

func writeManagerConfig(t *testing.T, path string, port int, model string) {
	t.Helper()
	value := fmt.Sprintf(`
server: {addr: "127.0.0.1:%d"}
weights: {quality: 1, cost: 1, speed: 1}
providers:
  local: {kind: openai, base_url: "http://127.0.0.1:9999/v1"}
catalog:
  - id: %q
    quality: 0.5
    speed: 0.5
    caps: {max_context: 1000}
`, port, model)
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", url, resp.StatusCode, body)
	}
	return string(body)
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
