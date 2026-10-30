package domain

import (
	"crypto/sha256"
	"encoding/base64"
)

// BrowserCapabilityVerifier is the stable v1 digest of a launch capability.
// It is not a bearer credential; callers must still authenticate its producer.
func BrowserCapabilityVerifier(sessionID SessionID, token string) string {
	if sessionID == "" || token == "" {
		return ""
	}
	h := sha256.New()
	_, _ = h.Write([]byte("ao-browser-capability-verifier-v1\x00"))
	_, _ = h.Write([]byte(sessionID))
	_, _ = h.Write([]byte{'\x00'})
	_, _ = h.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
