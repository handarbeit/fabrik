package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	// int64(...): every real call site passes an already-int64-typed
	// variable (e.g. reconciler.go's appID), which boxes into BuildAppJWT's
	// `any` parameter unchanged — but an untyped integer literal like 12345
	// defaults to plain `int` when boxed into an interface{} parameter, not
	// int64, so it must be cast explicitly here to match what production
	// call sites actually pass.
	tok, err := BuildAppJWT(int64(12345), key)
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

// TestBuildAppJWT_StringIssuer is the AC3 regression test for #1712: GitHub
// now recommends the App's Client ID (a string, e.g. "Iv23li…") over the
// numeric App ID as the JWT issuer, and BuildAppJWT must produce a
// well-formed JWT with a string "iss" claim when given one.
func TestBuildAppJWT_StringIssuer(t *testing.T) {
	key, _ := testAppPrivateKey(t)
	tok, err := BuildAppJWT("Iv23liExampleClientID", key)
	if err != nil {
		t.Fatalf("BuildAppJWT: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT segments, got %d: %q", len(parts), tok)
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding claims: %v", err)
	}
	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshaling claims: %v", err)
	}
	if claims.Iss != "Iv23liExampleClientID" {
		t.Errorf("iss = %q, want %q", claims.Iss, "Iv23liExampleClientID")
	}
	if claims.Exp <= claims.Iat {
		t.Errorf("exp (%d) must be after iat (%d)", claims.Exp, claims.Iat)
	}
}

// TestBuildAppJWT_EmptyStringIssuerRejected guards against an empty Client
// ID silently producing a JWT GitHub would reject anyway — fail fast here
// instead.
func TestBuildAppJWT_EmptyStringIssuerRejected(t *testing.T) {
	key, _ := testAppPrivateKey(t)
	if _, err := BuildAppJWT("", key); err == nil {
		t.Fatal("expected an error for an empty string issuer, got nil")
	}
}

// TestBuildAppJWT_InvalidIssuerTypeRejected guards against a caller passing
// some other numeric type (e.g. int, int32) silently doing the wrong thing —
// only int64 (App ID) and string (Client ID) are accepted.
func TestBuildAppJWT_InvalidIssuerTypeRejected(t *testing.T) {
	key, _ := testAppPrivateKey(t)
	if _, err := BuildAppJWT(int64(12345), key); err != nil {
		t.Fatalf("sanity check: int64 issuer should still work: %v", err)
	}
	if _, err := BuildAppJWT(int32(12345), key); err == nil {
		t.Fatal("expected an error for an int32 issuer (only int64 and string are accepted), got nil")
	}
}

func TestFetchAppInstallations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
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

	installs, truncated, err := FetchAppInstallations(srv.URL, "test-jwt")
	if err != nil {
		t.Fatalf("FetchAppInstallations: %v", err)
	}
	if truncated {
		t.Error("truncated = true, want false for a short (2-item) page")
	}
	if len(installs) != 2 {
		t.Fatalf("expected 2 installations, got %d", len(installs))
	}
	if installs[0].ID != 111 || installs[0].Account != "handarbeit" {
		t.Errorf("installs[0] = %+v", installs[0])
	}
}

// TestFetchAppInstallations_Paginates is the regression test for real
// pagination (#1641): a first page of exactly 100 (the old cap) followed by
// a second, short page must all be accumulated, not just the first page
// returned to the caller.
func TestFetchAppInstallations_Paginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		var entries []map[string]interface{}
		switch page {
		case "1":
			entries = make([]map[string]interface{}, 100)
			for i := range entries {
				entries[i] = map[string]interface{}{"id": i + 1, "account": map[string]string{"login": "someorg"}}
			}
		case "2":
			entries = []map[string]interface{}{
				{"id": 101, "account": map[string]string{"login": "anotherorg"}},
			}
		default:
			t.Errorf("unexpected page %q", page)
		}
		json.NewEncoder(w).Encode(entries)
	}))
	defer srv.Close()

	installs, truncated, err := FetchAppInstallations(srv.URL, "test-jwt")
	if err != nil {
		t.Fatalf("FetchAppInstallations: %v", err)
	}
	if truncated {
		t.Error("truncated = true, want false: the second page was short, so pagination reached its natural end")
	}
	if len(installs) != 101 {
		t.Fatalf("expected 101 installations across both pages, got %d", len(installs))
	}
	if installs[100].ID != 101 || installs[100].Account != "anotherorg" {
		t.Errorf("installs[100] = %+v", installs[100])
	}
}

// TestFetchAppInstallations_WarnsAtPaginationCeiling is the regression test
// for a review finding: /app/installations was called with no
// per_page/pagination at all (GitHub's own default page size is only 30),
// so an App installed on more accounts than fit on one page would silently
// miss the rest — Reconcile would then treat an already-installed owner
// beyond page 1 as uninstalled and walk the operator through a spurious
// guided-install flow. Now that real pagination follows every full page,
// only a server that never returns a short page (the appFetchMaxPages
// ceiling) is genuinely ambiguous — that's what this test simulates.
// Mirrors TestListOpenPRs_WarnsAtCap's "explicit per_page cap + warn, don't
// silently truncate" convention (prs.go).
func TestFetchAppInstallations_WarnsAtPaginationCeiling(t *testing.T) {
	var warned bool
	Logf = func(issueNumber int, tag, format string, args ...any) {
		if tag == "app" {
			warned = true
		}
	}
	defer func() { Logf = nil }()

	entries := make([]map[string]interface{}, 100)
	for i := range entries {
		entries[i] = map[string]interface{}{
			"id": i + 1, "account": map[string]string{"login": "someorg"},
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every page comes back full (100 items) — never a short page — so
		// the loop must run out at appFetchMaxPages rather than looping
		// forever.
		json.NewEncoder(w).Encode(entries)
	}))
	defer srv.Close()

	installs, truncated, err := FetchAppInstallations(srv.URL, "test-jwt")
	if err != nil {
		t.Fatalf("FetchAppInstallations: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true: every page was full, so the ceiling should have been hit")
	}
	if want := appFetchMaxPages * appFetchPageSize; len(installs) != want {
		t.Fatalf("expected %d installations (ceiling), got %d", want, len(installs))
	}
	if !warned {
		t.Error("expected a warning when the pagination ceiling is hit (more installations may exist)")
	}
}

func TestFetchAppInstallations_DecodesRepositorySelection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": 111, "account": map[string]string{"login": "handarbeit"}, "repository_selection": "all"},
			{"id": 222, "account": map[string]string{"login": "someorg"}, "repository_selection": "selected"},
		})
	}))
	defer srv.Close()

	installs, _, err := FetchAppInstallations(srv.URL, "test-jwt")
	if err != nil {
		t.Fatalf("FetchAppInstallations: %v", err)
	}
	if len(installs) != 2 {
		t.Fatalf("expected 2 installations, got %d", len(installs))
	}
	if installs[0].RepositorySelection != "all" {
		t.Errorf("installs[0].RepositorySelection = %q, want %q", installs[0].RepositorySelection, "all")
	}
	if installs[1].RepositorySelection != "selected" {
		t.Errorf("installs[1].RepositorySelection = %q, want %q", installs[1].RepositorySelection, "selected")
	}
}

// TestFetchAppInstallations_DecodesPermissions guards the field
// FetchAppInstallations previously discarded entirely: GitHub's installation
// resource includes a "permissions" object reporting what was actually
// granted (distinct from GET /app's requested permissions) — see #1709.
func TestFetchAppInstallations_DecodesPermissions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"id": 111, "account": map[string]string{"login": "handarbeit"},
				"permissions": map[string]string{"issues": "read", "contents": "read"},
			},
		})
	}))
	defer srv.Close()

	installs, _, err := FetchAppInstallations(srv.URL, "test-jwt")
	if err != nil {
		t.Fatalf("FetchAppInstallations: %v", err)
	}
	if len(installs) != 1 {
		t.Fatalf("expected 1 installation, got %d", len(installs))
	}
	if got := installs[0].Permissions["issues"]; got != "read" {
		t.Errorf("Permissions[issues] = %q, want %q", got, "read")
	}
	if got := installs[0].Permissions["contents"]; got != "read" {
		t.Errorf("Permissions[contents] = %q, want %q", got, "read")
	}
}

func TestFetchAppInstallation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/111" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Method != "GET" {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-jwt" {
			t.Errorf("Authorization = %q, want Bearer test-jwt", got)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": 111, "account": map[string]string{"login": "handarbeit"},
			"repository_selection": "all",
			"permissions":          map[string]string{"issues": "write", "contents": "read"},
		})
	}))
	defer srv.Close()

	inst, err := FetchAppInstallation(srv.URL, "test-jwt", 111)
	if err != nil {
		t.Fatalf("FetchAppInstallation: %v", err)
	}
	if inst.ID != 111 || inst.Account != "handarbeit" || inst.RepositorySelection != "all" {
		t.Errorf("inst = %+v", inst)
	}
	if got := inst.Permissions["issues"]; got != "write" {
		t.Errorf("Permissions[issues] = %q, want %q", got, "write")
	}
}

func TestFetchAppInstallation_UnauthorizedWrapsErrAppUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer srv.Close()

	_, err := FetchAppInstallation(srv.URL, "test-jwt", 111)
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	if !errors.Is(err, ErrAppUnauthorized) {
		t.Errorf("err = %v, want errors.Is(err, ErrAppUnauthorized)", err)
	}
}

func TestFetchAppInstallation_NotFoundWrapsErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"not found"}`))
	}))
	defer srv.Close()

	_, err := FetchAppInstallation(srv.URL, "test-jwt", 111)
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want errors.Is(err, ErrNotFound)", err)
	}
}

func TestFetchInstallationRepositories(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/installation/repositories" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ghs_installation_token" {
			t.Errorf("Authorization = %q, want Bearer ghs_installation_token", got)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"repositories": []map[string]interface{}{
				{"full_name": "someorg/repo-one"},
				{"full_name": "someorg/repo-two"},
			},
		})
	}))
	defer srv.Close()

	repos, truncated, err := FetchInstallationRepositories(srv.URL, "ghs_installation_token")
	if err != nil {
		t.Fatalf("FetchInstallationRepositories: %v", err)
	}
	if truncated {
		t.Error("truncated = true, want false for a short (2-item) page")
	}
	want := []string{"someorg/repo-one", "someorg/repo-two"}
	if len(repos) != len(want) {
		t.Fatalf("repos = %v, want %v", repos, want)
	}
	for i := range want {
		if repos[i] != want[i] {
			t.Errorf("repos[%d] = %q, want %q", i, repos[i], want[i])
		}
	}
}

// TestFetchInstallationRepositories_Paginates mirrors
// TestFetchAppInstallations_Paginates for the sibling endpoint: a full first
// page followed by a short second page must be fully accumulated.
func TestFetchInstallationRepositories_Paginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		var repos []map[string]interface{}
		switch page {
		case "1":
			repos = make([]map[string]interface{}, 100)
			for i := range repos {
				repos[i] = map[string]interface{}{"full_name": fmt.Sprintf("someorg/repo-%d", i)}
			}
		case "2":
			repos = []map[string]interface{}{{"full_name": "someorg/repo-100"}}
		default:
			t.Errorf("unexpected page %q", page)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"repositories": repos})
	}))
	defer srv.Close()

	got, truncated, err := FetchInstallationRepositories(srv.URL, "ghs_installation_token")
	if err != nil {
		t.Fatalf("FetchInstallationRepositories: %v", err)
	}
	if truncated {
		t.Error("truncated = true, want false: the second page was short")
	}
	if len(got) != 101 {
		t.Fatalf("expected 101 repositories across both pages, got %d", len(got))
	}
	if got[100] != "someorg/repo-100" {
		t.Errorf("got[100] = %q, want someorg/repo-100", got[100])
	}
}

// TestFetchInstallationRepositories_WarnsAtPaginationCeiling is the
// regression test for a review finding: /installation/repositories was
// called with no per_page/pagination at all (GitHub's own default page size
// is only 30), so an installation granting access to more repos than fit on
// one page would silently miss the rest. Now that real pagination follows
// every full page, only a server that never returns a short page (the
// appFetchMaxPages ceiling) is genuinely ambiguous — that's what this test
// simulates. Mirrors TestListOpenPRs_WarnsAtCap's "explicit per_page cap +
// warn, don't silently truncate" convention (prs.go).
func TestFetchInstallationRepositories_WarnsAtPaginationCeiling(t *testing.T) {
	var warned bool
	Logf = func(issueNumber int, tag, format string, args ...any) {
		if tag == "app" {
			warned = true
		}
	}
	defer func() { Logf = nil }()

	repos := make([]map[string]interface{}, 100)
	for i := range repos {
		repos[i] = map[string]interface{}{"full_name": fmt.Sprintf("someorg/repo-%d", i)}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every page comes back full — never a short page.
		json.NewEncoder(w).Encode(map[string]interface{}{"repositories": repos})
	}))
	defer srv.Close()

	got, truncated, err := FetchInstallationRepositories(srv.URL, "ghs_installation_token")
	if err != nil {
		t.Fatalf("FetchInstallationRepositories: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true: every page was full, so the ceiling should have been hit")
	}
	if want := appFetchMaxPages * appFetchPageSize; len(got) != want {
		t.Fatalf("expected %d repositories (ceiling), got %d", want, len(got))
	}
	if !warned {
		t.Error("expected a warning when the pagination ceiling is hit (more repositories may exist)")
	}
}

func TestFetchInstallationRepositories_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"Forbidden"}`))
	}))
	defer srv.Close()

	if _, _, err := FetchInstallationRepositories(srv.URL, "ghs_bad"); err == nil {
		t.Fatal("expected error on 403, got nil")
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

func TestDeleteAppInstallation(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := DeleteAppInstallation(srv.URL, "test-jwt", 999); err != nil {
		t.Fatalf("DeleteAppInstallation: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if gotPath != "/app/installations/999" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer test-jwt" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-jwt")
	}
}

// TestDeleteAppInstallation_NotFoundWrapsErrNotFound guards the
// idempotent-success contract callers (internal/githubauth's R4 deletion
// path) rely on: an installation already gone (e.g. the installer removed it
// independently, or a previous call already deleted it) must be
// distinguishable via errors.Is, not just any generic 4xx.
func TestDeleteAppInstallation_NotFoundWrapsErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"not found"}`))
	}))
	defer srv.Close()

	err := DeleteAppInstallation(srv.URL, "test-jwt", 999)
	if err == nil {
		t.Fatal("expected error for a 404 response, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want errors.Is(err, ErrNotFound)", err)
	}
}

func TestDeleteAppInstallation_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"boom"}`))
	}))
	defer srv.Close()

	err := DeleteAppInstallation(srv.URL, "test-jwt", 999)
	if err == nil {
		t.Fatal("expected error for a 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "deleting installation 999") {
		t.Errorf("error = %v, want it to name the installation being deleted", err)
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

// TestFetchAppSlug_UnauthorizedWrapsErrAppUnauthorized and
// TestFetchAppSlug_NotFoundWrapsErrNotFound guard the distinction
// internal/githubauth's Reconcile relies on: only a JWT GitHub actively
// rejected (401/403) or an App ID it reports as gone (404) should read as
// "the App may have been deleted" — never a generic transport/5xx failure.
func TestFetchAppSlug_UnauthorizedWrapsErrAppUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"bad credentials"}`))
	}))
	defer srv.Close()

	_, err := FetchAppSlug(srv.URL, "test-jwt")
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
	if !errors.Is(err, ErrAppUnauthorized) {
		t.Errorf("err = %v, want errors.Is(err, ErrAppUnauthorized)", err)
	}
}

func TestFetchAppSlug_NotFoundWrapsErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"not found"}`))
	}))
	defer srv.Close()

	_, err := FetchAppSlug(srv.URL, "test-jwt")
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want errors.Is(err, ErrNotFound)", err)
	}
}

// TestFetchAppSlug_ServerErrorIsNotWrappedAsAppRejection guards the actual
// bug: a 500 must not satisfy errors.Is against either sentinel, or a caller
// distinguishing "rejected" from "transient" would misclassify it.
func TestFetchAppSlug_ServerErrorIsNotWrappedAsAppRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"internal server error"}`))
	}))
	defer srv.Close()

	_, err := FetchAppSlug(srv.URL, "test-jwt")
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if errors.Is(err, ErrAppUnauthorized) || errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want a 500 to NOT be wrapped as ErrAppUnauthorized or ErrNotFound", err)
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

	if _, _, err := FetchAppInstallations("", "test-jwt"); err != nil {
		t.Fatalf("FetchAppInstallations: %v", err)
	}
	want := defaultBaseURL + "/app/installations?per_page=100&page=1"
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

	if _, _, err := FetchAppInstallations(srv.URL, "test-jwt"); err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}

func TestFetchAppHookDeliveries_PaginatesByLinkCursor(t *testing.T) {
	var srvURL string
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/hook/deliveries" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/app/hook/deliveries?per_page=100&cursor=abc123>; rel="next", <%s/app/hook/deliveries?per_page=100>; rel="first"`, srvURL, srvURL))
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 3, "guid": "g3", "delivered_at": "2026-10-22T12:00:00Z", "status": "Service Unavailable", "status_code": 503, "event": "issues", "action": "opened"},
			})
		case "abc123":
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 2, "guid": "g2", "delivered_at": "2026-10-22T11:00:00Z", "redelivery": true, "status": "OK", "status_code": 200, "event": "ping"},
			})
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	page1, next, err := FetchAppHookDeliveries(srv.URL, "test-jwt", "", 100)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if next != "abc123" || len(page1) != 1 {
		t.Fatalf("page 1: next=%q items=%d", next, len(page1))
	}
	d := page1[0]
	if d.ID != 3 || d.GUID != "g3" || d.StatusCode != 503 || d.Succeeded() || d.Event != "issues" || d.DeliveredAt.IsZero() {
		t.Errorf("decoded = %+v", d)
	}
	page2, next, err := FetchAppHookDeliveries(srv.URL, "test-jwt", next, 100)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if next != "" || len(page2) != 1 || !page2[0].Succeeded() || !page2[0].Redelivery {
		t.Errorf("page 2: next=%q items=%+v", next, page2)
	}
	for _, a := range gotAuth {
		if a != "Bearer test-jwt" {
			t.Errorf("Authorization = %q", a)
		}
	}
}

// Cancelling the context aborts a blocked read promptly (#2480).
func TestFetchAppHookConfigContext_CancelAbortsBlockedRead(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := FetchAppHookConfigContext(ctx, srv.URL, "jwt")
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected an error from the cancelled read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled read did not return")
	}
}

// FetchAppHookConfig decodes url and content_type and never exposes the secret
// GitHub returns in the same response (#2143).
func TestFetchAppHookConfig_DropsSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/hook/config" || r.Header.Get("Authorization") != "Bearer jwt-1" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"url":"https://n.ts.net/webhook","content_type":"json","secret":"********","insecure_ssl":"0"}`))
	}))
	defer srv.Close()
	got, err := FetchAppHookConfig(srv.URL, "jwt-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://n.ts.net/webhook" || got.ContentType != "json" || got.InsecureSSL != "0" {
		t.Fatalf("got %#v", got)
	}
	if strings.Contains(fmt.Sprintf("%+v", got), "***") {
		t.Fatal("secret leaked into the decoded value")
	}
	for _, f := range reflect.VisibleFields(reflect.TypeOf(got)) {
		if strings.EqualFold(f.Name, "secret") {
			t.Fatal("AppHookConfig must have no secret field")
		}
	}
}

func TestNextCursorFromLink(t *testing.T) {
	cases := map[string]string{
		"":                                   "",
		"garbage":                            "",
		`<%zz>; rel="next"`:                  "",
		`<https://x/y?cursor=q>`:             "",
		`<https://x/y?cursor=q>; rel="prev"`: "",
		`<https://x/y?cursor=q>; rel="next"`: "q",
		`<https://x/a>; rel="first", <https://x/y?a=1&cursor=z>; rel="next"`: "z",
		`<https://x/y>; rel="next"`: "",
	}
	for in, want := range cases {
		if got := nextCursorFromLink(in); got != want {
			t.Errorf("nextCursorFromLink(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchAppHookDeliveries_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		status  int
		wantIs  error
		wantRL  bool
		headers map[string]string
	}{
		{status: 401, wantIs: ErrAppUnauthorized},
		{status: 403, wantIs: ErrAppUnauthorized},
		{status: 404, wantIs: ErrNotFound},
		{status: 403, wantRL: true, headers: map[string]string{"X-RateLimit-Remaining": "0"}},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k, v := range tc.headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(tc.status)
			io.WriteString(w, `{"message":"nope"}`)
		}))
		_, _, err := FetchAppHookDeliveries(srv.URL, "j", "", 100)
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
			t.Errorf("status %d: err = %v, want Is %v", tc.status, err, tc.wantIs)
		}
		if tc.wantRL {
			var rl *RateLimitError
			if !errors.As(err, &rl) {
				t.Errorf("status %d: err = %v, want RateLimitError", tc.status, err)
			}
		}
	}
}

func TestRedeliverAppHookDelivery(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	status := http.StatusAccepted
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(status)
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	if err := RedeliverAppHookDelivery(srv.URL, "test-jwt", 4242); err != nil {
		t.Fatalf("RedeliverAppHookDelivery: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/app/hook/deliveries/4242/attempts" || gotAuth != "Bearer test-jwt" {
		t.Errorf("got %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}

	status = http.StatusUnprocessableEntity
	err := RedeliverAppHookDelivery(srv.URL, "test-jwt", 1)
	if err == nil {
		t.Fatal("422: expected error")
	}
	if errors.Is(err, ErrAppUnauthorized) || errors.Is(err, ErrNotFound) {
		t.Errorf("422 must be a plain error, got %v", err)
	}
}

func TestFetchAppHookConfig_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"nope"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := FetchAppHookConfig(srv.URL, "x"); !errors.Is(err, ErrAppUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}

// UpdateAppHookConfig sends only the url, with the JWT, and never decodes the
// secret GitHub echoes back (#2427).
func TestUpdateAppHookConfig_SendsOnlyURL(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" || r.URL.Path != "/app/hook/config" || r.Header.Get("Authorization") != "Bearer jwt-1" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"url":"https://elsewhere.invalid/hook","content_type":"json","secret":"********","insecure_ssl":"0"}`))
	}))
	defer srv.Close()
	got, err := UpdateAppHookConfig(srv.URL, "jwt-1", "https://elsewhere.invalid/hook")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotBody) != 1 || gotBody["url"] != "https://elsewhere.invalid/hook" {
		t.Fatalf("request body = %v, want only url", gotBody)
	}
	if got.URL != "https://elsewhere.invalid/hook" || strings.Contains(fmt.Sprintf("%+v", got), "***") {
		t.Fatalf("got %#v", got)
	}
}

func TestUpdateAppHookConfig_ErrorWraps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := UpdateAppHookConfig(srv.URL, "jwt", "https://x"); !errors.Is(err, ErrAppUnauthorized) {
		t.Fatalf("err = %v, want ErrAppUnauthorized", err)
	}
}

// FetchAppHookDelivery returns the delivered payload and not the headers.
func TestFetchAppHookDelivery_ReturnsPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/hook/deliveries/42" || r.Header.Get("Authorization") != "Bearer jwt-1" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":42,"guid":"g-1","event":"issues","action":"labeled","status_code":200,"request":{"headers":{"X-Hub-Signature-256":"sha256=secret"},"payload":{"action":"labeled","issue":{"number":7}}}}`))
	}))
	defer srv.Close()
	got, err := FetchAppHookDelivery(srv.URL, "jwt-1", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.GUID != "g-1" || got.Event != "issues" || !got.Succeeded() {
		t.Fatalf("got %#v", got)
	}
	if !strings.Contains(string(got.Payload), `"number":7`) {
		t.Errorf("payload = %s", got.Payload)
	}
	if strings.Contains(fmt.Sprintf("%+v", got), "secret") {
		t.Error("request headers leaked into the decoded delivery")
	}
}
