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
// Historical note: before #1142's extraction, this logic was independently
// duplicated in engine/webhook.go's own verifySignature, kept deliberately
// unshared per adrs/1113-pruefer-v1-architecture.md's "no shared Go imports
// with engine" constraint. That constraint was never absolute — it always
// meant "no direct pruefer<->engine imports," and internal/selfupgrade and
// internal/githubauth had already established the "share via internal/,
// import neither package from the other" pattern. engine/webhook.go now
// calls this function directly rather than carrying its own copy; see
// adrs/1142-hookdeck-ingestion-for-app-auth.md.
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
