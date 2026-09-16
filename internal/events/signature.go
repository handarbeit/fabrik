package events

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// VerifySignature checks the HMAC-SHA256 signature on a GitHub webhook
// payload. sig is the value of the X-Hub-Signature-256 header (format:
// "sha256=<hex>"). Required on every received event regardless of
// transport — a forwarding gateway's own transport auth (e.g. Hookdeck's
// API-key-scoped session) is not a substitute for verifying the payload
// actually came from GitHub.
//
// Implemented independently from engine/webhook.go's verifySignature, not
// imported from it — see adrs/1113-pruefer-v1-architecture.md's "no shared
// Go imports with engine" constraint.
func VerifySignature(body []byte, sig, secret string) bool {
	if !strings.HasPrefix(sig, "sha256=") {
		return false
	}
	sigHex := sig[len("sha256="):]
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), sigBytes)
}
