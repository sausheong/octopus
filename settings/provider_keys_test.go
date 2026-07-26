package settings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sausheong/octopus/config"
)

// Settings serves the stored configuration verbatim, inline provider keys
// included, because a local settings screen that hides part of the file cannot
// show the user what they are about to change. These tests pin the editing
// behaviour that has to keep working now that no value is substituted on the
// way out: a key the user does not touch survives every kind of edit to the
// rows around it.

const inlineKeySecret = "sk-ant-SUPERSECRET"

// seedInlineKeyConfig writes a config whose only provider carries an inline
// API key, and returns its path.
func seedInlineKeyConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	seed := "server:\n  addr: \"127.0.0.1:8787\"\nweights:\n  quality: 1\n" +
		"providers:\n  p:\n    kind: anthropic\n    api_key: \"" + inlineKeySecret + "\"\n" +
		"catalog:\n  - id: \"p/m\"\n    quality: 0.5\n    speed: 0.5\n    caps: { max_context: 1000 }\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// seedTwoProviderConfig writes a config with two providers, each carrying a
// distinct inline key and its own base_url, and returns its path. The base_urls
// matter: they are what makes a mis-paired key observable, and without one a
// provider that loses its key fails config.Validate instead of saving quietly.
func seedTwoProviderConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	seed := "server:\n  addr: \"127.0.0.1:8787\"\nweights:\n  quality: 1\nproviders:\n" +
		"  alpha:\n    kind: anthropic\n    api_key: \"KEY-ALPHA\"\n    base_url: \"https://alpha.invalid\"\n" +
		"  beta:\n    kind: openai\n    api_key: \"KEY-BETA\"\n    base_url: \"https://beta.invalid\"\n" +
		"catalog:\n  - id: \"alpha/m\"\n    quality: 0.5\n    speed: 0.5\n    caps: { max_context: 1000 }\n" +
		"  - id: \"beta/m\"\n    quality: 0.5\n    speed: 0.5\n    caps: { max_context: 1000 }\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// getState fetches /api/state through the full handler and returns the decoded
// response alongside the raw body, so a test can assert on both the structured
// fields and on the bytes actually sent.
func getState(t *testing.T, server *Server) (stateResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/state status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var state stateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	return state, rec.Body.String()
}

// postDocument sends a structured save and returns the decoded response.
func postDocument(t *testing.T, server *Server, doc Document) (*httptest.ResponseRecorder, stateResponse) {
	t.Helper()
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	req := writeReq(t, server, "/api/structured", string(body))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	var state stateResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
	}
	return rec, state
}

// storedKey reads the provider key straight off disk, bypassing every response
// path, so an assertion about what was persisted cannot be satisfied by the
// same handler logic under test.
func storedKey(t *testing.T, path, provider string) string {
	t.Helper()
	return storedProvider(t, path, provider).APIKey
}

// storedProvider reads one provider straight off disk, for the same reason.
func storedProvider(t *testing.T, path, name string) ProviderDocument {
	t.Helper()
	doc, _, _, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("load stored config: %v", err)
	}
	for _, p := range doc.Providers {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("provider %q is not in the stored config", name)
	return ProviderDocument{}
}

// The Advanced YAML tab exists to show the complete file. An earlier version
// blanked it whenever any provider set an inline api_key, which left the user
// looking at an empty read-only box on exactly the configurations they most
// needed to inspect. Both channels must carry the real file.
func TestStateServesTheCompleteFileIncludingInlineKeys(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), nil, nil)

	// Guard the guard: if the fixture ever stops carrying a key, the
	// assertions below pass while proving nothing.
	if got := storedKey(t, path, "p"); got != inlineKeySecret {
		t.Fatalf("fixture stored key = %q, want the inline secret", got)
	}

	state, _ := getState(t, server)

	if !strings.Contains(state.YAML, inlineKeySecret) {
		t.Errorf("the YAML channel does not carry the stored key; the Advanced editor cannot show the file:\n%s", state.YAML)
	}
	if got := state.Document.Providers[0].APIKey; got != inlineKeySecret {
		t.Errorf("the document channel carries %q, want the stored key so the Providers field shows it", got)
	}
	// The raw file must arrive byte for byte, not re-serialised from the
	// parsed document, or the editor shows something the user never wrote.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.YAML != string(onDisk) {
		t.Errorf("the served YAML differs from the file on disk:\ngot:\n%s\nwant:\n%s", state.YAML, onDisk)
	}
}

// A file Octopus cannot parse is precisely when the Advanced editor matters
// most: it is the only way to see and repair the syntax error.
func TestStateServesYAMLThatFailsToParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	broken := "server:\n  addr: \"127.0.0.1:8787\"\n  this is not: [valid: yaml\n"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewStore(path), nil, nil)

	state, _ := getState(t, server)
	if state.YAML != broken {
		t.Errorf("an unparseable file was not served for repair:\ngot:\n%s\nwant:\n%s", state.YAML, broken)
	}
	if state.LoadError == "" {
		t.Error("the load error is not reported, so the user cannot see why the file was rejected")
	}
}

// Typing a new key must replace the stored one.
func TestStructuredSaveSetsNewKey(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)
	state, _ := getState(t, server)
	doc := state.Document
	doc.Providers[0].APIKey = "sk-ant-REPLACEMENT"
	rec, _ := postDocument(t, server, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := storedKey(t, path, "p"); got != "sk-ant-REPLACEMENT" {
		t.Errorf("stored key = %q, want the newly typed value", got)
	}
}

// Clearing the field must clear the credential, not silently keep the old one.
func TestStructuredSaveClearsKeyWhenEmptied(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)
	state, _ := getState(t, server)
	doc := state.Document
	doc.Providers[0].APIKey = ""
	// A provider needs some credential source to validate; the env var stands
	// in, which is what a user clearing an inline key would realistically do.
	doc.Providers[0].APIKeyEnv = "P_API_KEY"
	rec, _ := postDocument(t, server, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := storedKey(t, path, "p"); got != "" {
		t.Errorf("stored key = %q, want it cleared", got)
	}
}

// Posting back what the form was handed, having changed something else, must
// leave the key alone. This is the ordinary case — an unrelated edit — and the
// one that silently destroyed credentials in earlier versions of this code.
func TestUnrelatedEditPreservesTheKey(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)

	state, _ := getState(t, server)
	doc := state.Document
	doc.ServerAddr = "127.0.0.1:9999"
	rec, _ := postDocument(t, server, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := storedKey(t, path, "p"); got != inlineKeySecret {
		t.Errorf("stored key = %q after an unrelated edit, want it preserved", got)
	}
}

// Repeated round trips must not degrade the key: each save re-renders the form
// from the response, and each subsequent save posts that back.
func TestRoundTripThroughUIPreservesKeyAcrossSaves(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)
	for i := range 3 {
		state, _ := getState(t, server)
		doc := state.Document
		doc.Routing.SessionTTL = []string{"1h", "2h", "3h"}[i]
		if rec, _ := postDocument(t, server, doc); rec.Code != http.StatusOK {
			t.Fatalf("save %d: status = %d, body=%s", i, rec.Code, rec.Body.String())
		}
		if got := storedKey(t, path, "p"); got != inlineKeySecret {
			t.Fatalf("after save %d the stored key is %q, want it preserved", i, got)
		}
	}
}

// Renaming a provider must carry its key across rather than dropping it.
func TestRenamingProviderKeepsItsKey(t *testing.T) {
	path := seedTwoProviderConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)

	state, _ := getState(t, server)
	doc := state.Document
	renamed := false
	for i := range doc.Providers {
		if doc.Providers[i].Name == "alpha" {
			doc.Providers[i].Name = "renamed"
			renamed = true
		}
	}
	if !renamed {
		t.Fatal("fixture has no provider named alpha")
	}
	for i := range doc.Catalog {
		if doc.Catalog[i].ID == "alpha/m" {
			doc.Catalog[i].ID = "renamed/m"
		}
	}

	rec, _ := postDocument(t, server, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := storedProvider(t, path, "renamed"); got.APIKey != "KEY-ALPHA" {
		t.Errorf("renamed provider holds key %q, want KEY-ALPHA carried across the rename", got.APIKey)
	}
	if got := storedProvider(t, path, "beta"); got.APIKey != "KEY-BETA" {
		t.Errorf("the untouched provider's key became %q", got.APIKey)
	}
}

// Swapping two provider names must move each key with its own row. Getting
// this wrong does not merely lose a key: it sends a live credential to a
// base_url the user never chose for it.
func TestSwappingProviderNamesNeverMisdirectsAKey(t *testing.T) {
	path := seedTwoProviderConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)

	state, _ := getState(t, server)
	doc := state.Document
	// Swap only the names. Every other field, base_url included, stays on the
	// row it came from, which is what makes a mis-pairing observable.
	for i := range doc.Providers {
		switch doc.Providers[i].Name {
		case "alpha":
			doc.Providers[i].Name = "beta"
		case "beta":
			doc.Providers[i].Name = "alpha"
		}
	}

	rec, _ := postDocument(t, server, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("swap status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []ProviderDocument{
		{Name: "beta", APIKey: "KEY-ALPHA", BaseURL: "https://alpha.invalid"},
		{Name: "alpha", APIKey: "KEY-BETA", BaseURL: "https://beta.invalid"},
	} {
		got := storedProvider(t, path, want.Name)
		if got.APIKey != want.APIKey {
			t.Errorf("provider %q holds key %q, want %q; the swap misdirected a credential",
				want.Name, got.APIKey, want.APIKey)
		}
		if got.BaseURL != want.BaseURL {
			t.Errorf("provider %q has base_url %q, want %q", want.Name, got.BaseURL, want.BaseURL)
		}
	}
}

// Deleting one provider and adding another in the same save must leave the
// survivor's key alone and store the new row's key as typed.
func TestDeletingAndAddingProvidersPreservesTheSurvivorsKey(t *testing.T) {
	path := seedTwoProviderConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)

	state, _ := getState(t, server)
	doc := state.Document
	kept := make([]ProviderDocument, 0, len(doc.Providers))
	for _, provider := range doc.Providers {
		if provider.Name != "alpha" {
			kept = append(kept, provider)
		}
	}
	if len(kept) != len(doc.Providers)-1 {
		t.Fatalf("fixture: expected to drop exactly one provider, kept %d of %d", len(kept), len(doc.Providers))
	}
	doc.Providers = append(kept, ProviderDocument{
		Name: "gamma", Kind: "openai", APIKey: "KEY-GAMMA", BaseURL: "https://gamma.invalid",
	})
	catalog := make([]CatalogDocument, 0, len(doc.Catalog))
	for _, entry := range doc.Catalog {
		if entry.ID != "alpha/m" {
			catalog = append(catalog, entry)
		}
	}
	doc.Catalog = append(catalog, CatalogDocument{
		ID: "gamma/m", Quality: 0.5, Speed: 0.5, Caps: config.Caps{MaxContext: 1000},
	})

	rec, _ := postDocument(t, server, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := storedProvider(t, path, "beta"); got.APIKey != "KEY-BETA" {
		t.Errorf("the surviving provider's key is %q, want KEY-BETA", got.APIKey)
	}
	if got := storedProvider(t, path, "gamma"); got.APIKey != "KEY-GAMMA" {
		t.Errorf("the newly added provider's key is %q, want the value typed in", got.APIKey)
	}
	stored, _, _, err := NewStore(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range stored.Providers {
		if provider.Name == "alpha" {
			t.Error("the deleted provider is still in the file")
		}
	}
}

// A key typed into the Advanced editor must be stored exactly as written.
func TestYAMLSaveStoresInlineKeyVerbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)
	yaml := "server:\n  addr: \"127.0.0.1:8787\"\nweights:\n  quality: 1\n" +
		"providers:\n  p:\n    kind: anthropic\n    api_key: \"" + inlineKeySecret + "\"\n" +
		"catalog:\n  - id: \"p/m\"\n    quality: 0.5\n    speed: 0.5\n    caps: { max_context: 1000 }\n"
	body, err := json.Marshal(map[string]string{"yaml": yaml})
	if err != nil {
		t.Fatal(err)
	}
	req := writeReq(t, server, "/api/yaml", string(body))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := storedKey(t, path, "p"); got != inlineKeySecret {
		t.Errorf("stored key = %q, want the key exactly as typed", got)
	}
}

// The browser must submit the key field as the user left it. Trimming or
// otherwise normalising it here would alter a credential on the way past.
func TestBrowserFormRoundTripsProviderAPIKey(t *testing.T) {
	data, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `api_key: $('[data-field="api_key"]', item).value`) {
		t.Error("app.js collectDocument() does not submit the api_key field; a save would clear inlined keys")
	}
}

// The provider form must offer an editable key field, since it is the
// structured way to manage an inlined credential.
func TestProviderFormHasEditableKeyField(t *testing.T) {
	data, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `data-field="api_key"`) {
		t.Error("index.html has no inline API key input")
	}
}
