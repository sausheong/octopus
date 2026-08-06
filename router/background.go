package router

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/sausheong/harness/llm"
)

// BackgroundSignature describes one explicitly recognised, conversation-
// independent client request. Matching is deliberately exact: request shape
// heuristics such as "small, non-streaming, and tool-free" are not sufficient
// evidence that a request can safely bypass ordinary routing.
type BackgroundSignature struct {
	Name                    string
	Endpoint                string
	LastUserSHA256          string
	RequireNonStreaming     bool
	ConversationIndependent bool
	Model                   string
}

// ExactBackgroundSignature builds a signature without retaining the matched
// client text. Endpoint is matched exactly after trimming a trailing slash.
func ExactBackgroundSignature(name, endpoint, lastUserText string) BackgroundSignature {
	sum := sha256.Sum256([]byte(lastUserText))
	return BackgroundSignature{
		Name:                    name,
		Endpoint:                normaliseEndpoint(endpoint),
		LastUserSHA256:          hex.EncodeToString(sum[:]),
		RequireNonStreaming:     true,
		ConversationIndependent: true,
	}
}

// BackgroundMatch is the safe routing policy attached to a matched signature.
// ConversationIndependent is explicit so a future caller cannot infer that
// history may be discarded merely from the request's size or transport mode.
type BackgroundMatch struct {
	Name                    string
	Model                   string
	ConversationIndependent bool
}

// Isolate returns a request copy with a distinct routing session. Both Route
// and Observe must receive this copy; using the original request for Observe
// would incorrectly update the main conversation's incumbent/cache state.
// Only an explicitly conversation-independent signature may discard history
// and cache markers, preventing a maintenance ping from pulling the main
// session's full cache into a cheap background model.
func (m BackgroundMatch) Isolate(chat llm.ChatRequest, meta RequestMetadata) llm.ChatRequest {
	isolated := chat
	isolated.SessionID = BackgroundSessionID(chat, meta, m)
	if m.ConversationIndependent {
		if turn, ok := LastUserTurn(chat.Messages); ok {
			isolated.Messages = []llm.Message{turn}
		}
		isolated.SystemPrompt = ""
		isolated.SystemPromptParts = nil
		isolated.CacheControl = nil
		isolated.CacheLastMessage = false
	}
	return isolated
}

// BackgroundDetector recognises a bounded allowlist of exact request
// signatures. It is immutable after construction and safe for concurrent use.
type BackgroundDetector struct {
	signatures []BackgroundSignature
}

func NewBackgroundDetector(signatures []BackgroundSignature) (*BackgroundDetector, error) {
	seen := make(map[string]struct{}, len(signatures))
	copyOf := make([]BackgroundSignature, 0, len(signatures))
	for _, sig := range signatures {
		sig.Name = strings.TrimSpace(sig.Name)
		sig.Endpoint = normaliseEndpoint(sig.Endpoint)
		sig.LastUserSHA256 = strings.ToLower(strings.TrimSpace(sig.LastUserSHA256))
		if sig.Name == "" || sig.Endpoint == "" || len(sig.LastUserSHA256) != sha256.Size*2 {
			return nil, errors.New("background signature requires a name, endpoint, and SHA-256 digest")
		}
		if _, err := hex.DecodeString(sig.LastUserSHA256); err != nil {
			return nil, errors.New("background signature has an invalid SHA-256 digest")
		}
		key := sig.Endpoint + "\x00" + sig.LastUserSHA256
		if _, ok := seen[key]; ok {
			return nil, errors.New("duplicate background signature")
		}
		seen[key] = struct{}{}
		copyOf = append(copyOf, sig)
	}
	return &BackgroundDetector{signatures: copyOf}, nil
}

// Detect returns a match only for an exact allowlisted request. Tool use or
// images always disqualify a request, even if its last user text is known.
func (d *BackgroundDetector) Detect(chat llm.ChatRequest, meta RequestMetadata) (BackgroundMatch, bool) {
	if d == nil || len(chat.Tools) != 0 || requestHasImages(chat) || requestHasToolTraffic(chat) {
		return BackgroundMatch{}, false
	}
	turn, ok := LastUserTurn(chat.Messages)
	if !ok {
		return BackgroundMatch{}, false
	}
	sum := sha256.Sum256([]byte(turn.Content))
	digest := hex.EncodeToString(sum[:])
	endpoint := normaliseEndpoint(meta.Endpoint)
	for _, sig := range d.signatures {
		if endpoint != sig.Endpoint || digest != sig.LastUserSHA256 {
			continue
		}
		if sig.RequireNonStreaming && meta.Stream {
			continue
		}
		return BackgroundMatch{
			Name:                    sig.Name,
			Model:                   sig.Model,
			ConversationIndependent: sig.ConversationIndependent,
		}, true
	}
	return BackgroundMatch{}, false
}

// BackgroundSessionID produces routing state that is cryptographically and
// textually separate from the main conversation's session/cache identity.
// Callers should route and observe a matched background request under this ID,
// never under SessionID(chat).
func BackgroundSessionID(chat llm.ChatRequest, meta RequestMetadata, match BackgroundMatch) string {
	h := sha256.New()
	h.Write([]byte(SessionID(chat)))
	h.Write([]byte{0})
	h.Write([]byte(meta.WorkflowID))
	h.Write([]byte{0})
	h.Write([]byte(match.Name))
	return "background:" + hex.EncodeToString(h.Sum(nil))
}

func requestHasToolTraffic(chat llm.ChatRequest) bool {
	for _, msg := range chat.Messages {
		if msg.ToolCallID != "" || len(msg.ToolCalls) != 0 {
			return true
		}
	}
	return false
}

func normaliseEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint != "/" {
		endpoint = strings.TrimSuffix(endpoint, "/")
	}
	return endpoint
}
