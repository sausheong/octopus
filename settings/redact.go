package settings

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// redactedAPIKey is the prefix of the placeholder that stands in for a stored
// inline provider key in every response that leaves this process. A structured
// save echoing a placeholder back means "keep the key already on disk": the
// browser form is never sent the real value, so without a placeholder a
// redacted round trip would blank the credential — the silent-data-loss failure
// this codebase has already hit five times.
//
// The value is deliberately not key-shaped. Should it ever reach a provider it
// fails authentication loudly rather than passing for a live secret.
const redactedAPIKey = "__octopus_key_unchanged__"

// redactedKeyFor returns the placeholder issued for the provider stored under
// name. The name travels inside the placeholder because the placeholder must
// mean "the key belonging to the row this field was rendered for", and the
// row's own name field is editable by the user in the very same form. Matching
// on that editable field instead — as the first version of this code did —
// resolves a rename against a name nothing is stored under, which destroys the
// key, and resolves a two-way rename against the *other* provider, which sends
// a live credential to an endpoint the user never chose.
//
// The handle is not secret. Provider names already travel in the same document,
// and a forged handle only reaches resolution through POST /api/structured,
// which is gated by the loopback Host check and the CSRF token.
func redactedKeyFor(name string) string {
	return redactedAPIKey + ":" + name
}

// isRedactedKey reports whether a value is a placeholder rather than a
// credential. The bare constant counts: a configuration file may contain it
// literally, and treating that text as a secret would withhold the YAML tab
// from a file that holds none.
func isRedactedKey(value string) bool {
	return value == redactedAPIKey || strings.HasPrefix(value, redactedAPIKey+":")
}

// redactedKeyName returns the stored provider name a placeholder refers to.
// The bare constant carries no name and so refers to nothing resolvable.
func redactedKeyName(value string) (string, bool) {
	return strings.CutPrefix(value, redactedAPIKey+":")
}

// inlineKeyLine matches a YAML mapping entry named api_key that carries a
// value. It is textual rather than parsed because it must also judge a file
// that fails to parse — precisely when the Advanced YAML editor matters most
// and when config.Parse can say nothing about the contents.
//
// It errs towards matching: a false positive costs the YAML tab, a false
// negative discloses a credential.
var inlineKeyLine = regexp.MustCompile(`(?mi)^[^\S\n]*#*[^\S\n]*['"]?api_key['"]?[^\S\n]*:(.*)$`)

// rawHasInlineKey reports whether configuration text appears to assign a
// non-empty inline provider key.
func rawHasInlineKey(raw []byte) bool {
	for _, match := range inlineKeyLine.FindAllStringSubmatch(string(raw), -1) {
		value := strings.TrimSpace(match[1])
		// A value left unset carries no secret; everything else might.
		switch value {
		case "", `""`, "''", "~", "null", "Null", "NULL":
			continue
		}
		// A trailing comment is not part of the value, but a value that starts
		// one means the field itself was commented out.
		if strings.HasPrefix(value, "#") {
			continue
		}
		return true
	}
	return false
}

// documentHasInlineKey reports whether a parsed configuration holds an inline
// key. It complements rawHasInlineKey: the line-oriented scan only sees an
// api_key that begins a line, so a flow mapping (`p: {kind: x, api_key: s}`) or
// a whole file written in JSON syntax hides the assignment from it entirely,
// and only the parsed view finds those. Either check alone can be fooled, so
// the caller withholds when either says yes.
//
// Placeholders are excluded deliberately. They are not credentials, and
// counting them would withhold the YAML tab from a config that holds no secret.
func documentHasInlineKey(doc Document) bool {
	return slices.ContainsFunc(doc.Providers, func(p ProviderDocument) bool {
		return p.APIKey != "" && !isRedactedKey(p.APIKey)
	})
}

// redactDocument returns a copy of doc with every inline key replaced by a
// placeholder naming the provider it was taken from. The copy matters: the
// caller's Document is the value written to disk, and substituting into it
// would persist the placeholder as the key.
func redactDocument(doc Document) Document {
	doc.Providers = slices.Clone(doc.Providers)
	for i := range doc.Providers {
		if doc.Providers[i].APIKey != "" {
			doc.Providers[i].APIKey = redactedKeyFor(doc.Providers[i].Name)
		}
	}
	return doc
}

// usesRedactedKey reports whether an incoming document echoes a placeholder.
func usesRedactedKey(doc Document) bool {
	return slices.ContainsFunc(doc.Providers, func(p ProviderDocument) bool {
		return isRedactedKey(p.APIKey)
	})
}

// resolveRedactedKeys replaces each placeholder in an incoming document with
// the key stored for the provider that placeholder was issued for, so a save
// echoing a redacted response preserves the credential — including across a
// rename, because the placeholder names the row's original provider rather than
// whatever the user has since typed into the name field. A key the user typed
// over, or deliberately blanked, is passed through untouched: a placeholder is
// the only value that means "unchanged", so changing and clearing both work.
//
// A placeholder that matches no stored provider is an error rather than an
// empty key. It means the file changed underneath this page, or the field was
// hand-edited, and in both cases the server cannot tell which credential was
// meant. Writing empty would silently destroy a key; refusing tells the user
// exactly which field to retype, and they can still rename freely by typing the
// key in or clearing the field outright.
func resolveRedactedKeys(incoming Document, stored Document) (Document, error) {
	if !usesRedactedKey(incoming) {
		return incoming, nil
	}
	keys := make(map[string]string, len(stored.Providers))
	for _, provider := range stored.Providers {
		keys[provider.Name] = provider.APIKey
	}
	// Cloned so no Document sharing this backing array gains a resolved secret.
	incoming.Providers = slices.Clone(incoming.Providers)
	for i := range incoming.Providers {
		provider := incoming.Providers[i]
		if !isRedactedKey(provider.APIKey) {
			continue
		}
		name, named := redactedKeyName(provider.APIKey)
		key, known := keys[name]
		if !named || !known {
			return Document{}, fmt.Errorf(
				"the saved API key for provider %q could not be matched to the stored configuration, "+
					"which has changed since this page was loaded; reopen Settings, then re-enter that key",
				provider.Name)
		}
		incoming.Providers[i].APIKey = key
	}
	return incoming, nil
}
