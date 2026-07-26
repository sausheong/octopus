package settings

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sausheong/octopus/config"
)

func TestStoreCreatesSecureConfigAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".octopus", "config.yaml")
	store := NewStore(path)
	doc, _, exists, err := store.Load()
	if err != nil || exists {
		t.Fatalf("initial Load = exists %v, err %v", exists, err)
	}
	doc.ServerAddr = "127.0.0.1:9876"
	if _, err := store.SaveDocument(doc); err != nil {
		t.Fatalf("SaveDocument: %v", err)
	}
	loaded, _, exists, err := store.Load()
	if err != nil || !exists || loaded.ServerAddr != doc.ServerAddr {
		t.Fatalf("round trip = %#v, exists %v, err %v", loaded, exists, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %o", info.Mode().Perm())
	}
}

func TestStoreRejectsInvalidYAMLWithoutReplacingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".octopus", "config.yaml")
	store := NewStore(path)
	doc := defaultDocument()
	original, err := store.SaveDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveYAML([]byte("invalid: true\n")); err == nil {
		t.Fatal("invalid YAML should fail")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("invalid save replaced valid config")
	}
}

func TestDocumentRoundTripPreservesMaxAttempts(t *testing.T) {
	cfg := &config.Config{
		ServerAddr: "127.0.0.1:8787",
		Weights:    config.Weights{Quality: 1},
		Routing:    config.RoutingCfg{SessionSticky: true, SessionTTL: time.Hour, CacheAware: true, MaxAttempts: 5},
		Providers:  map[string]config.ProviderCreds{"p": {Kind: "anthropic", APIKeyEnv: "K"}},
		Catalog: []config.CatalogEntry{
			{ID: "p/m", Quality: 0.5, Speed: 0.5, Caps: config.Caps{MaxContext: 1000}},
		},
	}
	back, err := documentFromConfig(cfg).config()
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if back.Routing.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d after round trip, want 5", back.Routing.MaxAttempts)
	}
}

// A structured (form) save must not silently drop the auth token env name.
// Document.config() rebuilds config.Config field by field, so a field the
// Document does not know about is zeroed on every save — and zeroing this one
// turns authentication off without any error or indication to the user.
func TestDocumentRoundTripPreservesAuthTokenEnv(t *testing.T) {
	cfg := &config.Config{
		ServerAddr:   "127.0.0.1:8787",
		AuthTokenEnv: "OCTOPUS_AUTH_TOKEN",
		Weights:      config.Weights{Quality: 1},
		Routing:      config.RoutingCfg{SessionSticky: true, SessionTTL: time.Hour, CacheAware: true, MaxAttempts: 3},
		Providers:    map[string]config.ProviderCreds{"p": {Kind: "anthropic", APIKeyEnv: "K"}},
		Catalog: []config.CatalogEntry{
			{ID: "p/m", Quality: 0.5, Speed: 0.5, Caps: config.Caps{MaxContext: 1000}},
		},
	}
	back, err := documentFromConfig(cfg).config()
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if back.AuthTokenEnv != "OCTOPUS_AUTH_TOKEN" {
		t.Errorf("AuthTokenEnv = %q after round trip, want %q (a form save must not disable authentication)",
			back.AuthTokenEnv, "OCTOPUS_AUTH_TOKEN")
	}
}
