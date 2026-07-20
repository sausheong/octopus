package settings

import (
	"os"
	"path/filepath"
	"testing"
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
