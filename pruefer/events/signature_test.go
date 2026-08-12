package events

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func sign(t *testing.T, body []byte, secret string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature_Valid(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	secret := "topsecret"
	sig := sign(t, body, secret)

	if !VerifySignature(body, sig, secret) {
		t.Error("expected a correctly-signed payload to verify")
	}
}

func TestVerifySignature_InvalidWrongSecret(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	sig := sign(t, body, "topsecret")

	if VerifySignature(body, sig, "wrongsecret") {
		t.Error("expected verification to fail with the wrong secret")
	}
}

func TestVerifySignature_InvalidTamperedBody(t *testing.T) {
	secret := "topsecret"
	sig := sign(t, []byte(`{"action":"opened"}`), secret)

	if VerifySignature([]byte(`{"action":"closed"}`), sig, secret) {
		t.Error("expected verification to fail for a body that doesn't match the signature")
	}
}

func TestVerifySignature_Missing(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	if VerifySignature(body, "", "topsecret") {
		t.Error("expected verification to fail for a missing signature")
	}
}

func TestVerifySignature_MalformedPrefix(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	sig := sign(t, body, "topsecret")
	// Strip the "sha256=" prefix — a malformed header, not a valid alternate scheme.
	raw := sig[len("sha256="):]
	if VerifySignature(body, raw, "topsecret") {
		t.Error("expected verification to fail without the sha256= prefix")
	}
}

func TestVerifySignature_NonHexSuffix(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	if VerifySignature(body, "sha256=not-hex!!", "topsecret") {
		t.Error("expected verification to fail for a non-hex signature suffix")
	}
}
