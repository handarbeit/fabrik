package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"time"
)

// appHTTPTimeout bounds every GitHub App auth-bootstrap HTTP call (JWT-based
// installation discovery and token minting). These are small, infrequent
// calls — the same 30s budget used elsewhere in this package is generous.
const appHTTPTimeout = 30 * time.Second

// appHTTPClient is package-level so tests can point it at an httptest server
// without needing a constructed *Client (these are free functions — see
// ADR-1113's rationale for why App-auth bootstrap does not live on *Client).
var appHTTPClient = &http.Client{Timeout: appHTTPTimeout}

// jwtClockSkew is subtracted from "issued at" to tolerate minor clock drift
// between this host and GitHub's servers, per GitHub's own App-auth guidance.
const jwtClockSkew = 60 * time.Second

// jwtValidity is how long the minted JWT is valid for. GitHub caps this at 10
// minutes; the JWT is only used to mint an installation token, so a short
// lifetime is sufficient and safer.
const jwtValidity = 9 * time.Minute

// ParseAppPrivateKey parses a PEM-encoded RSA private key as used by a GitHub
// App (downloaded from the App's settings page). Both PKCS#1 ("BEGIN RSA
// PRIVATE KEY") and PKCS#8 ("BEGIN PRIVATE KEY") encodings are accepted,
// since GitHub's own download and common conversions (e.g. via openssl)
// produce either depending on tooling.
func ParseAppPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing private key (tried PKCS1 and PKCS8): %w", err)
	}
	rsaKey, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not an RSA key (got %T)", keyAny)
	}
	return rsaKey, nil
}

// base64URLEncode encodes data using unpadded base64url, as required by the
// JWT spec (RFC 7519).
func base64URLEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// BuildAppJWT constructs and signs a short-lived (9 minute) RS256 JWT
// asserting the given GitHub App ID, per GitHub's App-authentication flow
// (https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app).
// This JWT authenticates as the App itself — it is exchanged for a
// per-installation access token via MintInstallationToken, never used
// directly against ordinary REST/GraphQL endpoints.
//
// Hand-rolled rather than via a JWT library: GitHub's App-auth flow needs
// exactly one fixed-shape token (header.payload signed with RS256), which
// stdlib crypto/rsa + encoding/json + encoding/base64 covers directly,
// consistent with this module's "minimize external dependencies" convention.
func BuildAppJWT(appID int64, privateKey *rsa.PrivateKey) (string, error) {
	now := time.Now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]int64{
		"iat": now.Add(-jwtClockSkew).Unix(),
		"exp": now.Add(jwtValidity).Unix(),
		"iss": appID,
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshaling JWT header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshaling JWT claims: %w", err)
	}
	signingInput := base64URLEncode(headerJSON) + "." + base64URLEncode(claimsJSON)

	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("signing JWT: %w", err)
	}
	return signingInput + "." + base64URLEncode(sig), nil
}

// AppInstallation represents a single installation of a GitHub App, as
// returned by GET /app/installations.
type AppInstallation struct {
	ID      int64  // Installation ID, needed to mint an installation access token.
	Account string // Login of the org/user the App is installed on.
	// RepositorySelection is "all" or "selected". "selected" means the
	// installation only grants access to a subset of Account's repos — the
	// actual subset is only discoverable via FetchInstallationRepositories,
	// which requires an installation access token (not the App's JWT).
	RepositorySelection string
}

// appRequest performs a JWT-authenticated GitHub App API request (installation
// discovery, token minting, or self-identity lookup) and decodes the JSON
// response into result. baseURL allows tests to target an httptest server;
// an empty baseURL (the production case) falls back to defaultBaseURL, the
// same convention github.Client uses.
func appRequest(method, baseURL, path, jwt string, result interface{}) error {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	req, err := http.NewRequest(method, baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := appHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GitHub App API returned %d: %s%s", resp.StatusCode, string(body), authErrorHint(resp.StatusCode))
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// FetchAppInstallations lists every installation of the GitHub App
// authenticated by jwt via GET /app/installations. Used for dynamic
// installation discovery: Pruefer watches whatever repos the App is
// installed on, so adding a repo requires only a GitHub-side installation
// change, not a Pruefer config or code change (see ADR-1113).
func FetchAppInstallations(baseURL, jwt string) ([]AppInstallation, error) {
	var raw []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		RepositorySelection string `json:"repository_selection"`
	}
	if err := appRequest("GET", baseURL, "/app/installations", jwt, &raw); err != nil {
		return nil, fmt.Errorf("fetching app installations: %w", err)
	}
	out := make([]AppInstallation, len(raw))
	for i, inst := range raw {
		out[i] = AppInstallation{ID: inst.ID, Account: inst.Account.Login, RepositorySelection: inst.RepositorySelection}
	}
	return out, nil
}

// FetchInstallationRepositories lists the repositories an installation
// actually grants access to, via GET /installation/repositories. Only
// meaningful (and only ever called) for an installation whose
// RepositorySelection is "selected" — an "all" installation grants access to
// every current and future repo on the account, so there is nothing to
// enumerate. Unlike the App-JWT-authenticated calls above, this endpoint is
// scoped to the installation's own identity: it must be authenticated with
// that installation's access token, not the App's JWT.
func FetchInstallationRepositories(baseURL, installationToken string) ([]string, error) {
	var result struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := appRequest("GET", baseURL, "/installation/repositories", installationToken, &result); err != nil {
		return nil, fmt.Errorf("fetching installation repositories: %w", err)
	}
	out := make([]string, len(result.Repositories))
	for i, r := range result.Repositories {
		out[i] = r.FullName
	}
	return out, nil
}

// MintInstallationToken exchanges a JWT for a short-lived (~1 hour)
// installation access token via POST
// /app/installations/{installation_id}/access_tokens. The returned token is
// used as an ordinary Bearer token for REST/GraphQL calls scoped to that
// installation's repos and permissions; callers must refresh it before
// expiresAt (see Pruefer's auth.go refresh loop).
func MintInstallationToken(baseURL, jwt string, installationID int64) (token string, expiresAt time.Time, err error) {
	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if err := appRequest("POST", baseURL, path, jwt, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("minting installation token for installation %d: %w", installationID, err)
	}
	return result.Token, result.ExpiresAt, nil
}

// FetchAppSlug returns the App's own slug (e.g. "my-reviewer") via GET /app,
// JWT-authenticated. The App's bot identity login on issues/PRs/reviews is
// always "<slug>[bot]" — used by Pruefer to recognize its own comments and
// reviews (self-review skip, already-reviewed-at-SHA state).
func FetchAppSlug(baseURL, jwt string) (string, error) {
	var result struct {
		Slug string `json:"slug"`
	}
	if err := appRequest("GET", baseURL, "/app", jwt, &result); err != nil {
		return "", fmt.Errorf("fetching app identity: %w", err)
	}
	if result.Slug == "" {
		return "", fmt.Errorf("github: /app response missing slug")
	}
	return result.Slug, nil
}
