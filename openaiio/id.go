package openaiio

import (
	"crypto/rand"
	"encoding/hex"
)

func newChatID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "chatcmpl_router"
	}
	return "chatcmpl_" + hex.EncodeToString(b[:])
}
