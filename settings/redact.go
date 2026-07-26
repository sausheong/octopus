package settings

import (
	"regexp"
	"slices"
	"strings"
)

// redactedAPIKey stands in for a stored inline provider key in every response
// that leaves this process. A structured save echoing it back means "keep the
// key already on disk": the browser form is never sent the real value, so
// without a sentinel a redacted round trip would blank the credential — the
// silent-data-loss failure this codebase has already hit five times.
//
// The value is deliberately not key-shaped. Should it ever reach a provider it
// fails authentication loudly rather than passing for a live secret.
const redactedAPIKey = "__octopus_key_unchanged__"

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
// key. It complements rawHasInlineKey: the parser resolves anchors, merge keys,
// and quoting styles that a line-oriented scan can miss. Either one alone can
// be fooled, so the caller withholds when either says yes.
//
// The sentinel is excluded deliberately. It is not a credential, and counting
// it would withhold the YAML tab from a config that holds no secret at all.
func documentHasInlineKey(doc Document) bool {
	return slices.ContainsFunc(doc.Providers, func(p ProviderDocument) bool {
		return p.APIKey != "" && p.APIKey != redactedAPIKey
	})
}

// redactDocument returns a copy of doc with every inline key replaced by the
// sentinel. The copy matters: the caller's Document is the value written to
// disk, and substituting into it would persist the placeholder as the key.
func redactDocument(doc Document) Document {
	doc.Providers = slices.Clone(doc.Providers)
	for i := range doc.Providers {
		if doc.Providers[i].APIKey != "" {
			doc.Providers[i].APIKey = redactedAPIKey
		}
	}
	return doc
}

// usesRedactedKey reports whether an incoming document echoes the sentinel.
func usesRedactedKey(doc Document) bool {
	return slices.ContainsFunc(doc.Providers, func(p ProviderDocument) bool {
		return p.APIKey == redactedAPIKey
	})
}

// resolveRedactedKeys replaces the sentinel in an incoming document with the
// key currently stored for the provider of the same name, so a save that
// echoes a redacted response preserves the credential. A key the user typed
// over, or deliberately blanked, is passed through untouched — the sentinel is
// the only value that means "unchanged", so changing and clearing both work.
//
// A sentinel naming a provider with no stored key resolves to empty: the
// server cannot invent a credential it never held, and rejecting the save
// would block renaming a provider.
func resolveRedactedKeys(incoming Document, stored Document) Document {
	if !usesRedactedKey(incoming) {
		return incoming
	}
	keys := make(map[string]string, len(stored.Providers))
	for _, provider := range stored.Providers {
		keys[provider.Name] = provider.APIKey
	}
	// Cloned so no Document sharing this backing array gains a resolved secret.
	incoming.Providers = slices.Clone(incoming.Providers)
	for i := range incoming.Providers {
		if incoming.Providers[i].APIKey == redactedAPIKey {
			incoming.Providers[i].APIKey = keys[incoming.Providers[i].Name]
		}
	}
	return incoming
}
