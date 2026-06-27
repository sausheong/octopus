package anthropicio

import (
	"crypto/rand"
	"encoding/hex"
)

// newMessageID returns a per-response unique message id of the form
// "msg_" + 24 hex chars. It falls back to a fixed id only if the system RNG
// fails, which is not expected.
func newMessageID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "msg_router" // fallback, never expected
	}
	return "msg_" + hex.EncodeToString(b[:])
}
