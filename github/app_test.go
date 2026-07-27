package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// roundTripFunc adapts a function to http.RoundTripper, letting tests
// intercept appHTTPClient's requests without going over the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// testAppPrivateKey generates a small (1024-bit — fast, test-only) RSA key
// and returns it both as *rsa.PrivateKey and PKCS1-PEM-encoded bytes.
func testAppPrivateKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generating test RSA key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return key, pemBytes
}

func TestParseAppPrivateKey_PKCS1(t *testing.T) {
	key, pemBytes := testAppPrivateKey(t)
	parsed, err := ParseAppPrivateKey(pemBytes)
	if err != nil {
		t.Fatalf("ParseAppPrivateKey: %v", err)
	}
	if parsed.N.Cmp(key.N) != 0 {
		t.Error("parsed key modulus does not match original")
	}
}

func TestParseAppPrivateKey_PKCS8(t *testing.T) {
	key, _ := testAppPrivateKey(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling PKCS8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})

	parsed, err := ParseAppPrivateKey(pemBytes)
	if err != nil {
		t.Fatalf("ParseAppPrivateKey (PKCS8): %v", err)
	}
	if parsed.N.Cmp(key.N) != 0 {
		t.Error("parsed key modulus does not match original")
	}
}

func TestParseAppPrivateKey_InvalidPEM(t *testing.T) {
	if _, err := ParseAppPrivateKey([]byte("not a pem block")); err == nil {
		t.Fatal("expected error for invalid PEM, got nil")
	}
}

func TestBuildAppJWT_WellFormed(t *testing.T) {
	key, _ := testAppPrivateKey(t)
	tok, err := BuildAppJWT(12345, key)
	if err != nil {
		t.Fatalf("BuildAppJWT: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT segments, got %d: %q", len(parts), tok)
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decoding header: %v", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("unmarshaling header: %v", err)
	}
	if header.Alg != "RS256" || header.Typ != "JWT" {
		t.Errorf("header = %+v, want alg=RS256 typ=JWT", header)
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding claims: %v", err)
	}
	var claims struct {
		Iat int64 `json:"iat"`
		Exp int64 `json:"exp"`
		Iss int64 `json:"iss"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshaling claims: %v", err)
	}
	if claims.Iss != 12345 {
		t.Errorf("iss = %d, want 12345", claims.Iss)
	}
	if claims.Exp <= claims.Iat {
		t.Errorf("exp (%d) must be after iat (%d)", claims.Exp, claims.Iat)
	}
	now := time.Now().Unix()
	if claims.Iat > now || claims.Iat < now-120 {
		t.Errorf("iat = %d, want within [now-120, now] (now=%d)", claims.Iat, now)
	}
}

func TestFetchAppInstallations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-jwt" {
			t.Errorf("Authorization = %q, want Bearer test-jwt", got)
		}
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": 111, "account": map[string]string{"login": "handarbeit"}},
			{"id": 222, "account": map[string]string{"login": "someorg"}},
		})
	}))
	defer srv.Close()

	installs, err := FetchAppInstallations(srv.URL, "test-jwt")
	if err != nil {
		t.Fatalf("FetchAppInstallations: %v", err)
	}
	if len(installs) != 2 {
		t.Fatalf("expected 2 installations, got %d", len(installs))
	}
	if installs[0].ID != 111 || installs[0].Account != "handarbeit" {
		t.Errorf("installs[0] = %+v", installs[0])
	}
}

func TestMintInstallationToken(t *testing.T) {
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/app/installations/999/access_tokens" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"token":      "ghs_minted",
			"expires_at": expiry.Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	token, expiresAt, err := MintInstallationToken(srv.URL, "test-jwt", 999)
	if err != nil {
		t.Fatalf("MintInstallationToken: %v", err)
	}
	if token != "ghs_minted" {
		t.Errorf("token = %q", token)
	}
	if !expiresAt.Equal(expiry) {
		t.Errorf("expiresAt = %v, want %v", expiresAt, expiry)
	}
}

func TestFetchAppSlug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"slug": "pruefer-bot", "id": 42})
	}))
	defer srv.Close()

	slug, err := FetchAppSlug(srv.URL, "test-jwt")
	if err != nil {
		t.Fatalf("FetchAppSlug: %v", err)
	}
	if slug != "pruefer-bot" {
		t.Errorf("slug = %q, want pruefer-bot", slug)
	}
}

func TestFetchAppSlug_MissingSlug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"id": 42})
	}))
	defer srv.Close()

	if _, err := FetchAppSlug(srv.URL, "test-jwt"); err == nil {
		t.Fatal("expected error when slug is missing, got nil")
	}
}

// TestAppRequest_EmptyBaseURLFallsBackToDefaultBaseURL guards the production
// code path specifically: every other Bootstrap/appRequest test in this file
// supplies an explicit httptest URL, so none of them would have caught
// appRequest building a request against a bare relative path (and failing
// with "unsupported protocol scheme \"\"") when baseURL == "" — which is
// exactly what every real (non-test) caller passes. This intercepts
// appHTTPClient's transport instead of using httptest, so the assertion is
// "the request targets defaultBaseURL" without actually hitting the network.
func TestAppRequest_EmptyBaseURLFallsBackToDefaultBaseURL(t *testing.T) {
	var capturedURL string
	origTransport := appHTTPClient.Transport
	appHTTPClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		capturedURL = req.URL.String()
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`[]`)),
			Header:     make(http.Header),
		}, nil
	})
	defer func() { appHTTPClient.Transport = origTransport }()

	if _, err := FetchAppInstallations("", "test-jwt"); err != nil {
		t.Fatalf("FetchAppInstallations: %v", err)
	}
	want := defaultBaseURL + "/app/installations"
	if capturedURL != want {
		t.Errorf("request URL = %q, want %q (empty baseURL must fall back to defaultBaseURL)", capturedURL, want)
	}
}

func TestAppRequest_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"Forbidden"}`))
	}))
	defer srv.Close()

	if _, err := FetchAppInstallations(srv.URL, "test-jwt"); err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}
