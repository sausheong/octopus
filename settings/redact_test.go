package settings

import (
	"bytes"
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
// same redaction logic under test.
func storedKey(t *testing.T, path, provider string) string {
	t.Helper()
	doc, _, _, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("load stored config: %v", err)
	}
	for _, p := range doc.Providers {
		if p.Name == provider {
			return p.APIKey
		}
	}
	t.Fatalf("provider %q is not in the stored config", provider)
	return ""
}

// The rebinding attacker this milestone exists to stop cannot read config.yaml
// but can fetch /api/state same-origin, so an inline key in that response is a
// genuine disclosure. Both channels carry it — the structured document feeds
// the provider form and the raw YAML feeds the Advanced editor — so redacting
// either one alone leaves the key readable through the other.
func TestStateWithholdsInlineAPIKey(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), nil, nil)

	// Guard the guard: if the fixture ever stops carrying a key, every
	// absence assertion below passes while proving nothing.
	if got := storedKey(t, path, "p"); got != inlineKeySecret {
		t.Fatalf("fixture stored key = %q, want the inline secret", got)
	}

	state, body := getState(t, server)

	if strings.Contains(body, inlineKeySecret) {
		t.Error("GET /api/state response body contains the inline API key")
	}
	for _, provider := range state.Document.Providers {
		if provider.APIKey == inlineKeySecret {
			t.Errorf("document channel leaks the inline API key for provider %q", provider.Name)
		}
	}
	if strings.Contains(state.YAML, inlineKeySecret) {
		t.Error("yaml channel leaks the inline API key")
	}
}

// The same response must be safe when the attacker's Host reaches it, since
// that is the case the redaction exists for and reads are not Host-gated.
func TestRebindingReadCannotSeeInlineAPIKey(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.Host = "evil.example.com:54321"
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (reads stay ungated)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), inlineKeySecret) {
		t.Error("a rebound read obtained the inline API key")
	}
}

// Redaction without a way to say "unchanged" would destroy the credential on
// the next save: the form posts back whatever it was rendered, and a blank
// would be written straight over the stored key.
func TestStructuredSaveEchoingRedactedKeyPreservesIt(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)

	// Post back exactly what the browser was handed, changing something else,
	// as the form does after an unrelated edit.
	state, _ := getState(t, server)
	doc := state.Document
	doc.ServerAddr = "127.0.0.1:9999"
	rec, _ := postDocument(t, server, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	if got := storedKey(t, path, "p"); got != inlineKeySecret {
		t.Errorf("stored key = %q after an echoed save, want it preserved; the save wiped a credential", got)
	}
	// The sentinel must never be mistaken for the key itself.
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), redactedAPIKey) {
		t.Errorf("the placeholder was persisted as the API key:\n%s", written)
	}
}

// Redaction must not make the key un-editable.
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

// Clearing the field must clear the credential. A scheme where only the
// sentinel round-trips and an empty value is also treated as "unchanged" would
// leave the user unable to remove an inlined key from the UI at all.
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

// The save response is the next render's state, so it must be redacted on the
// same terms as GET /api/state. Otherwise saving is a way to read back the key
// that the read endpoint withholds.
func TestSaveResponseWithholdsInlineAPIKey(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)
	state, _ := getState(t, server)
	doc := state.Document
	doc.Providers[0].APIKey = "sk-ant-REPLACEMENT"
	rec, saved := postDocument(t, server, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-ant-REPLACEMENT") {
		t.Error("the save response echoes back the stored API key")
	}
	if !saved.YAMLWithheld {
		t.Error("the save response served raw YAML for a config holding an inline key")
	}
}

// A config with no inline key has nothing to hide, so the Advanced YAML tab
// must keep working. Without this, withholding everything would pass every
// leak assertion while removing a shipped feature.
func TestStateServesYAMLWhenNoInlineKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	seed := "server:\n  addr: \"127.0.0.1:8787\"\nweights:\n  quality: 1\n" +
		"providers:\n  p:\n    kind: anthropic\n    api_key_env: \"P_API_KEY\"\n" +
		"catalog:\n  - id: \"p/m\"\n    quality: 0.5\n    speed: 0.5\n    caps: { max_context: 1000 }\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewStore(path), nil, nil)
	state, _ := getState(t, server)
	if state.YAMLWithheld {
		t.Fatal("YAML was withheld for a config with no inline key")
	}
	if !strings.Contains(state.YAML, "api_key_env") {
		t.Errorf("Advanced YAML did not receive the file contents: %q", state.YAML)
	}
}

// The raw file cannot be redacted in place — a placeholder in the text would
// be saved back as the literal key — so the whole tab is withheld while an
// inline key is present, and the client is told why. Serving the text with the
// key removed, or serving it silently empty, would both be worse: the first
// discloses nothing but invites a save that rewrites the file without the key,
// the second is indistinguishable from an empty config.
func TestYAMLIsWithheldRatherThanPartiallyRedacted(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), nil, nil)
	state, _ := getState(t, server)
	if !state.YAMLWithheld {
		t.Fatal("yaml_withheld is false for a config holding an inline key")
	}
	if state.YAML != "" {
		t.Errorf("yaml = %q, want empty; a partially redacted file invites saving it back", state.YAML)
	}
}

// A file that fails to parse still gets scanned for a key, because the raw
// text is served verbatim on that path and the parsed document is empty.
func TestUnparseableConfigWithInlineKeyStillWithholdsYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	// Valid enough to read, invalid to Parse: the catalog is missing.
	seed := "server:\n  addr: \"127.0.0.1:8787\"\nproviders:\n  p:\n    api_key: \"" + inlineKeySecret + "\"\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewStore(path), nil, nil)
	state, body := getState(t, server)
	if state.LoadError == "" {
		t.Fatal("fixture parsed cleanly; it must fail to parse for this test to mean anything")
	}
	if strings.Contains(body, inlineKeySecret) {
		t.Error("an unparseable config disclosed its inline API key")
	}
	if !state.YAMLWithheld {
		t.Error("yaml_withheld is false for an unparseable config holding an inline key")
	}
}

// load_error is a third channel out of this endpoint. A YAML library that
// quoted the offending line back would reintroduce the disclosure through an
// error message rather than through data; yaml.v3 does not today, so pin it.
func TestLoadErrorDoesNotQuoteInlineKey(t *testing.T) {
	for name, seed := range map[string]string{
		"unknown field beside the key": "server:\n  addr: \"127.0.0.1:8787\"\nweights:\n  quality: 1\n" +
			"providers:\n  p:\n    kind: anthropic\n    api_key: \"" + inlineKeySecret + "\"\n    bogus_field: 1\n" +
			"catalog:\n  - id: \"p/m\"\n    quality: 0.5\n    speed: 0.5\n    caps: { max_context: 1000 }\n",
		"wrong type for the key": "server:\n  addr: \"127.0.0.1:8787\"\nweights:\n  quality: 1\n" +
			"providers:\n  p:\n    kind: anthropic\n    api_key: [\"" + inlineKeySecret + "\"]\n" +
			"catalog:\n  - id: \"p/m\"\n    quality: 0.5\n    speed: 0.5\n    caps: { max_context: 1000 }\n",
		"tab indentation": "server:\n  addr: \"127.0.0.1:8787\"\nproviders:\n  p:\n\tapi_key: \"" + inlineKeySecret + "\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
				t.Fatal(err)
			}
			server := NewServer(NewStore(path), nil, nil)
			state, body := getState(t, server)
			if state.LoadError == "" {
				t.Fatal("fixture parsed cleanly; it must fail for this test to mean anything")
			}
			if strings.Contains(body, inlineKeySecret) {
				t.Errorf("the key reached the client through an error message: %s", state.LoadError)
			}
		})
	}
}

func TestRawHasInlineKey(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  string
		want bool
	}{
		{"plain", "providers:\n  p:\n    api_key: sk-live\n", true},
		{"quoted", "providers:\n  p:\n    api_key: \"sk-live\"\n", true},
		{"quoted field name", "providers:\n  p:\n    \"api_key\": sk-live\n", true},
		{"trailing comment", "providers:\n  p:\n    api_key: sk-live # mine\n", true},
		{"uppercase", "providers:\n  p:\n    API_KEY: sk-live\n", true},
		// A commented-out assignment still puts the secret's text in the file,
		// and serving that text discloses it just as surely as an active line
		// would. Withholding is the right answer even though YAML ignores it.
		{"commented out but still present", "providers:\n  p:\n    # api_key: sk-live\n", true},
		{"env var only", "providers:\n  p:\n    api_key_env: K\n", false},
		{"empty value", "providers:\n  p:\n    api_key:\n", false},
		{"empty string", "providers:\n  p:\n    api_key: \"\"\n", false},
		{"null", "providers:\n  p:\n    api_key: null\n", false},
		{"value is only a comment", "providers:\n  p:\n    api_key: # unset\n", false},
		{"marshalled empty key", "providers:\n    p:\n        api_key: \"\"\n        api_key_env: K\n", false},
		{"no providers", "server:\n  addr: 127.0.0.1:8787\n", false},
		{"empty file", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := rawHasInlineKey([]byte(c.raw)); got != c.want {
				t.Errorf("rawHasInlineKey(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// api_key_env must not be mistaken for api_key: treating it as a secret would
// withhold the YAML tab from the recommended, secret-free configuration.
func TestAPIKeyEnvIsNotTreatedAsAnInlineKey(t *testing.T) {
	if rawHasInlineKey([]byte("providers:\n  p:\n    api_key_env: \"ANTHROPIC_API_KEY\"\n")) {
		t.Error("api_key_env was treated as an inline key; the YAML tab would be withheld from every safe config")
	}
}

// inlineKeyOnlyParsingFinds holds configurations whose inline key the textual
// scan cannot see, because rawHasInlineKey is line-oriented and neither of
// these puts api_key at the start of a line. They are the whole reason
// documentHasInlineKey exists as a second, independent check — for these
// inputs, it is the only thing standing between the key and the attacker.
var inlineKeyOnlyParsingFinds = map[string]string{
	// A flow mapping keeps the whole provider on one line.
	"flow mapping": "server:\n  addr: \"127.0.0.1:8787\"\nweights:\n  quality: 1\n" +
		"providers:\n  p: { kind: anthropic, api_key: " + inlineKeySecret + " }\n" +
		"catalog:\n  - id: \"p/m\"\n    quality: 0.5\n    speed: 0.5\n    caps: { max_context: 1000 }\n",
	// JSON is valid YAML, so a whole file may arrive in this shape.
	"json-style whole file": `{"server": {"addr": "127.0.0.1:8787"}, "weights": {"quality": 1}, ` +
		`"providers": {"p": {"kind": "anthropic", "api_key": "` + inlineKeySecret + `"}}, ` +
		`"catalog": [{"id": "p/m", "quality": 0.5, "speed": 0.5, "caps": {"max_context": 1000}}]}`,
}

// The two detection checks are complementary, and the parsed one carries these
// inputs alone. Without this test documentHasInlineKey is entirely unpinned:
// replacing its body with `return false` left the whole settings suite green,
// so a refactor could delete it and silently reopen the disclosure.
func TestParsedCheckCatchesKeysTheRawScanMisses(t *testing.T) {
	for name, seed := range inlineKeyOnlyParsingFinds {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
				t.Fatal(err)
			}
			doc, raw, _, err := NewStore(path).Load()
			if err != nil {
				t.Fatalf("fixture must parse for the parsed check to see it: %v", err)
			}

			// Guard the guard twice over. If the fixture ever stops carrying a
			// key, or the raw scan grows to catch these shapes, this test would
			// keep passing while proving nothing about documentHasInlineKey.
			if got := storedKey(t, path, "p"); got != inlineKeySecret {
				t.Fatalf("fixture stored key = %q, want the inline secret", got)
			}
			if rawHasInlineKey(raw) {
				t.Fatal("rawHasInlineKey now catches this shape, so the fixture no longer isolates " +
					"the parsed check and the end-to-end assertions below would pass with " +
					"documentHasInlineKey deleted. Replace it with a config only the parser sees.")
			}

			if !documentHasInlineKey(doc) {
				t.Fatal("documentHasInlineKey missed a key the raw scan cannot see; /api/state would serve the file with the key in it")
			}

			// End to end, because the checks only matter through the response.
			server := NewServer(NewStore(path), nil, nil)
			state, body := getState(t, server)
			if strings.Contains(body, inlineKeySecret) {
				t.Error("GET /api/state disclosed the inline API key")
			}
			if !state.YAMLWithheld {
				t.Error("yaml_withheld is false for a config holding an inline key")
			}
		})
	}
}

// A placeholder that matches no stored provider must fail the save rather than
// resolve to empty. Resolving to empty is how the first version of this code
// destroyed a key on rename: it reported success while writing nothing over a
// live credential. It must equally never fall through to some other provider's
// secret.
func TestRedactedKeyForUnknownProviderIsRejected(t *testing.T) {
	stored := Document{Providers: []ProviderDocument{{Name: "p", APIKey: inlineKeySecret}}}
	for name, incoming := range map[string]Document{
		"placeholder for a provider that is not stored": {
			Providers: []ProviderDocument{{Name: "renamed", APIKey: redactedKeyFor("gone")}},
		},
		// The bare constant names nothing, so it cannot identify a row either.
		// A config file could contain this literal text as its api_key.
		"placeholder carrying no provider name": {
			Providers: []ProviderDocument{{Name: "p", APIKey: redactedAPIKey}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := resolveRedactedKeys(incoming, stored)
			if err == nil {
				t.Fatalf("resolve succeeded with key %q, want an error", got.Providers[0].APIKey)
			}
			if strings.Contains(err.Error(), inlineKeySecret) {
				t.Errorf("the error message discloses the stored key: %v", err)
			}
		})
	}
}

// redactDocument must not overwrite the caller's slice. Today the save path
// happens to write to disk before building its response, so an in-place
// substitution would go unnoticed; the day those two steps are reordered it
// would persist the placeholder as the user's API key. Pin the contract rather
// than rely on the ordering.
func TestRedactDocumentDoesNotMutateCaller(t *testing.T) {
	doc := Document{Providers: []ProviderDocument{{Name: "p", APIKey: inlineKeySecret}}}
	redacted := redactDocument(doc)
	if doc.Providers[0].APIKey != inlineKeySecret {
		t.Errorf("caller's document was mutated to %q; a save would persist the placeholder", doc.Providers[0].APIKey)
	}
	if redacted.Providers[0].APIKey != redactedKeyFor("p") {
		t.Errorf("returned document was not redacted: %q", redacted.Providers[0].APIKey)
	}
}

// resolveRedactedKeys must not write a resolved secret into the caller's
// slice, or a Document already queued for a response would gain the key.
func TestResolveRedactedKeysDoesNotMutateCaller(t *testing.T) {
	stored := Document{Providers: []ProviderDocument{{Name: "p", APIKey: inlineKeySecret}}}
	incoming := Document{Providers: []ProviderDocument{{Name: "p", APIKey: redactedKeyFor("p")}}}
	if _, err := resolveRedactedKeys(incoming, stored); err != nil {
		t.Fatal(err)
	}
	if incoming.Providers[0].APIKey != redactedKeyFor("p") {
		t.Errorf("caller's document was mutated to %q", incoming.Providers[0].APIKey)
	}
}

// The browser must round-trip the sentinel rather than dropping the field.
// app.js builds the POST body key by key; an omitted api_key decodes as "" and
// clears the credential — the same root cause as the auth_token_env incident.
func TestBrowserFormRoundTripsProviderAPIKey(t *testing.T) {
	data, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `api_key: $('[data-field="api_key"]', item).value`) {
		t.Error("app.js collectDocument() does not submit the api_key field; a save would clear inlined keys")
	}
}

// The YAML editor must not post an empty body back when the server withheld
// the file: that would replace the real configuration with nothing. Two
// independent behaviours guard that, and a grep for the flag name alone would
// pass while either was disabled, so pin each one separately.
func TestBrowserFormGuardsWithheldYAML(t *testing.T) {
	data, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	// The editor is made read-only from the flag, so the user cannot type into
	// a textarea whose contents would overwrite a file they were never shown.
	if !strings.Contains(source, "Boolean(state.yaml_withheld)") {
		t.Error("renderYAMLTab() does not derive its state from yaml_withheld; the editor would appear to hold the real file")
	}
	// And the save path refuses outright, so a stale flag or a scripted click
	// still cannot post the blank.
	if !strings.Contains(source, "isYAML && state?.yaml_withheld") {
		t.Error("save() does not refuse a withheld YAML save; posting the empty editor would blank the configuration")
	}
}

// A YAML save is unaffected by redaction — the user supplies the whole file, so
// a key typed there must be stored verbatim and not confused with the sentinel.
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
	if strings.Contains(rec.Body.String(), inlineKeySecret) {
		t.Error("the YAML save response echoed the key straight back")
	}
}

// A save must not be able to smuggle a placeholder into the file as a real key,
// or the next load would treat that literal string as a credential. Both forms
// are checked: the bare constant and one carrying a provider name.
func TestYAMLSaveOfSentinelIsNotResolved(t *testing.T) {
	for _, literal := range []string{redactedAPIKey, redactedKeyFor("p")} {
		t.Run(literal, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			store := NewStore(path)
			// The document channel is the one that interprets a placeholder; a
			// YAML save bypasses it entirely, so the text is written as-is.
			seed := "server:\n  addr: \"127.0.0.1:8787\"\nweights:\n  quality: 1\n" +
				"providers:\n  p:\n    kind: anthropic\n    api_key: \"" + literal + "\"\n" +
				"catalog:\n  - id: \"p/m\"\n    quality: 0.5\n    speed: 0.5\n    caps: { max_context: 1000 }\n"
			if _, err := store.SaveYAML([]byte(seed)); err != nil {
				t.Fatal(err)
			}
			// documentHasInlineKey must not count a placeholder as a real key,
			// but rawHasInlineKey scans text and will — so the tab is withheld,
			// which is the fail-safe direction.
			doc, raw, _, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if documentHasInlineKey(doc) {
				t.Error("the placeholder was counted as a real inline key")
			}
			if !rawHasInlineKey(raw) {
				t.Error("the raw scan should withhold conservatively when it sees any api_key value")
			}
		})
	}
}

// Structured saves that never mention the sentinel must not pay for a second
// disk read, and more importantly must not be affected by whatever is on disk.
func TestStructuredSaveWithoutSentinelIgnoresStoredKey(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)
	doc := defaultDocument()
	doc.Providers[0].Name = "p"
	doc.Providers[0].APIKeyEnv = "P_API_KEY"
	doc.Catalog[0].ID = "p/m"
	rec, _ := postDocument(t, server, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := storedKey(t, path, "p"); got != "" {
		t.Errorf("stored key = %q; a save that never sent the sentinel resurrected the old key", got)
	}
}

// A structured save must reach the store with the resolved key, not the
// sentinel, even when the response afterwards shows the sentinel again.
func TestRoundTripThroughUIPreservesKeyAcrossTwoSaves(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)
	for i := range 3 {
		state, _ := getState(t, server)
		doc := state.Document
		doc.Routing.MaxAttempts = 3 + i
		if rec, _ := postDocument(t, server, doc); rec.Code != http.StatusOK {
			t.Fatalf("save %d: status = %d, body=%s", i, rec.Code, rec.Body.String())
		}
		if got := storedKey(t, path, "p"); got != inlineKeySecret {
			t.Fatalf("after save %d the stored key is %q, want it preserved", i, got)
		}
	}
}

// The provider form is the only remaining way to manage an inlined key once the
// YAML tab is withheld, so the input must exist and be editable.
func TestProviderFormHasEditableKeyField(t *testing.T) {
	data, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `data-field="api_key"`) {
		t.Error("index.html has no inline API key input; a withheld YAML tab would leave the key uneditable")
	}
}

// Guard against the sentinel drifting into something key-shaped: if it ever
// looked like a real credential, a leaked placeholder would be indistinguishable
// from a leaked secret in a response body.
func TestSentinelIsNotKeyShaped(t *testing.T) {
	for _, prefix := range []string{"sk-", "sk_", "AIza", "Bearer "} {
		if strings.HasPrefix(redactedAPIKey, prefix) {
			t.Errorf("the redaction sentinel %q looks like a real API key", redactedAPIKey)
		}
	}
}

// Confirms the leak this finding is about is actually closed end to end, using
// the exact probe from the review: fetch state as the rebinding page, then feed
// the response into a save and check the credential survived.
func TestInlineKeyIsNeitherDisclosedNorDestroyed(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.Host = "evil.example.com:54321"
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), inlineKeySecret) {
		t.Fatal("disclosure: the rebound read returned the inline key")
	}

	var state stateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(state.Document)
	if err != nil {
		t.Fatal(err)
	}
	save := writeReq(t, server, "/api/structured", string(body))
	saveRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(saveRec, save)
	if saveRec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body=%s", saveRec.Code, saveRec.Body.String())
	}
	if got := storedKey(t, path, "p"); got != inlineKeySecret {
		t.Fatalf("destruction: stored key = %q after saving the redacted document", got)
	}
}

// The sentinel must survive a JSON round trip unchanged, since the browser
// carries it through as an opaque string.
func TestSentinelSurvivesJSONRoundTrip(t *testing.T) {
	doc := redactDocument(Document{Providers: []ProviderDocument{{Name: "p", APIKey: inlineKeySecret}}})
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(inlineKeySecret)) {
		t.Fatal("redactDocument left the key in the encoded document")
	}
	var back Document
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if back.Providers[0].APIKey != redactedKeyFor("p") {
		t.Errorf("placeholder = %q after a JSON round trip", back.Providers[0].APIKey)
	}
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

// storedProvider reads one provider straight off disk, bypassing every response
// path, so an assertion about what was persisted cannot be satisfied by the
// redaction logic under test.
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

// Renaming a provider must not destroy its key. The placeholder means "the key
// for the row this field was rendered next to", and the name on that row is
// user-editable in the same form — so resolving by the submitted name, as the
// first version of this code did, looks up a name nothing is stored under and
// writes an empty key over a live credential while returning 200.
func TestRenamingProviderDoesNotSilentlyDestroyItsKey(t *testing.T) {
	path := seedTwoProviderConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)

	state, _ := getState(t, server)
	doc := state.Document
	// Exactly what the browser posts after a rename: the name field edited, the
	// key field left holding whatever the server put in it.
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
	// The placeholder identifies the row, so the rename carries the key across
	// rather than needing the user to retype it. Pinned strictly: accepting a
	// refusal here as well would let a change that breaks renaming outright
	// pass this test, and renaming has to keep working.
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := storedProvider(t, path, "renamed"); got.APIKey != "KEY-ALPHA" {
		t.Errorf("renamed provider holds key %q, want KEY-ALPHA carried across the rename", got.APIKey)
	}
	if got := storedProvider(t, path, "beta"); got.APIKey != "KEY-BETA" {
		t.Errorf("the untouched provider's key became %q", got.APIKey)
	}
	if strings.Contains(rec.Body.String(), "KEY-ALPHA") {
		t.Error("the save response echoed the preserved key back to the page")
	}
}

// Swapping two provider names pairs each placeholder with the other provider's
// entry when resolution goes by submitted name. That does not merely lose a
// key: it sends a live credential to a base_url the user never chose for it.
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

	// Each key must still sit beside the base_url it was configured with. Under
	// name-based resolution both rows resolve to the other provider's key, so
	// the base_url pairing is what makes the misdirection visible.
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
// survivor's key alone. Resolution keys off the placeholder, so a row that
// disappeared must not shift which stored key any other row resolves to.
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
	// A brand-new row carries a key the user just typed, never a placeholder.
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

// A provider with no base_url has the inline key as its only credential
// source, so losing the key on rename used to fail config.Validate and surface
// as a confusing 422 — safe, but only by accident. Now the key survives and the
// rename simply succeeds, which is what the user asked for.
func TestRenamingProviderWithoutBaseURLKeepsTheKey(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)

	state, _ := getState(t, server)
	doc := state.Document
	// Guard the guard: the point of this fixture is the absent base_url.
	if doc.Providers[0].BaseURL != "" {
		t.Fatalf("fixture provider has base_url %q; this test needs one without", doc.Providers[0].BaseURL)
	}
	doc.Providers[0].Name = "renamed"
	doc.Catalog[0].ID = "renamed/m"

	rec, _ := postDocument(t, server, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := storedKey(t, path, "renamed"); got != inlineKeySecret {
		t.Errorf("stored key = %q after the rename, want it carried across", got)
	}
	if strings.Contains(rec.Body.String(), inlineKeySecret) {
		t.Error("the response disclosed the inline API key")
	}
}

// Renaming must remain possible. If the only way to rename were to lose the
// credential, the fix for the rename bug would have broken the feature instead.
func TestRenamingProviderSucceedsWhenTheKeyIsRetyped(t *testing.T) {
	path := seedTwoProviderConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)

	state, _ := getState(t, server)
	doc := state.Document
	for i := range doc.Providers {
		if doc.Providers[i].Name == "alpha" {
			doc.Providers[i].Name = "renamed"
			doc.Providers[i].APIKey = "KEY-RETYPED"
		}
	}
	for i := range doc.Catalog {
		if doc.Catalog[i].ID == "alpha/m" {
			doc.Catalog[i].ID = "renamed/m"
		}
	}

	rec, _ := postDocument(t, server, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; renaming with a retyped key must work", rec.Code, rec.Body.String())
	}
	if got := storedProvider(t, path, "renamed"); got.APIKey != "KEY-RETYPED" {
		t.Errorf("renamed provider holds key %q, want the retyped value", got.APIKey)
	}
	if got := storedProvider(t, path, "beta"); got.APIKey != "KEY-BETA" {
		t.Errorf("the untouched provider's key became %q", got.APIKey)
	}
}

// A refused save must say which field to fix and must not put the credential in
// the message: the error is rendered into the page, which is the same surface
// the disclosure fix exists to protect.
func TestRejectedResolveExplainsWhatToDoWithoutLeakingTheKey(t *testing.T) {
	path := seedInlineKeyConfig(t)
	server := NewServer(NewStore(path), func(_ context.Context) error { return nil }, nil)

	doc, _, _, err := NewStore(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	// A placeholder for a provider that is not on disk: what a stale page sends
	// after the file has been edited underneath it.
	doc.Providers[0].APIKey = redactedKeyFor("vanished")

	rec, _ := postDocument(t, server, doc)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for an unresolvable key", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, inlineKeySecret) {
		t.Error("the rejection message disclosed the stored key")
	}
	for _, want := range []string{"p", "re-enter"} {
		if !strings.Contains(body, want) {
			t.Errorf("the rejection message does not mention %q, so the user cannot act on it: %s", want, body)
		}
	}
	if got := storedKey(t, path, "p"); got != inlineKeySecret {
		t.Errorf("a rejected save still modified the stored key: %q", got)
	}
}

// The browser must round-trip the placeholder untouched. app.js reads the key
// field back verbatim, so any normalisation there — trimming, or treating the
// placeholder as a value to clear — would turn a preserved key into a lost one.
func TestBrowserFormDoesNotAlterTheRedactedKeyField(t *testing.T) {
	data, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	// Every other text field is trimmed; this one must not be, because the
	// placeholder has to arrive back byte for byte to be recognised.
	if !strings.Contains(source, `api_key: $('[data-field="api_key"]', item).value,`) {
		t.Error("collectDocument() no longer submits the api_key field verbatim; a save would not preserve inlined keys")
	}
	if strings.Contains(source, `[data-field="api_key"]', item).value.trim()`) {
		t.Error("collectDocument() trims the api_key field; a placeholder must round-trip byte for byte")
	}
}
