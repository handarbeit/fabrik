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
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrAppUnauthorized is returned by appRequest when GitHub responds 401 or
// 403 to an App-JWT-authenticated call: the JWT was rejected outright,
// distinct from a transient network failure or 5xx. Together with
// ErrNotFound (rest.go, reused here for 404), callers can use errors.Is to
// distinguish "the App's identity was actively rejected" — worth treating as
// a sign the App may have been deleted — from a transient failure that
// merely warrants a retry.
var ErrAppUnauthorized = errors.New("app unauthorized")

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
		switch resp.StatusCode {
		case 401, 403:
			return fmt.Errorf("GitHub App API returned %d: %s%s: %w", resp.StatusCode, string(body), authErrorHint(resp.StatusCode), ErrAppUnauthorized)
		case 404:
			return fmt.Errorf("GitHub App API returned 404: %s: %w", string(body), ErrNotFound)
		}
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

// appFetchPageSize is the page size every paginated App-auth REST fetcher
// below requests — GitHub's max per_page (its own default is only 30).
const appFetchPageSize = 100

// appFetchMaxPages bounds FetchAppInstallations/FetchInstallationRepositories'
// pagination as a sanity ceiling (50 pages * 100/page = 5,000 items),
// distinct from Pruefer's own operator-facing max_derived_repos cap (R5):
// this only guards against an unbounded loop against a pathological or
// misbehaving server, not a deliberate operator choice about review scope.
const appFetchMaxPages = 50

// fetchAppInstallationsPage and fetchInstallationRepositoriesPage's shared
// page-loop shape: accumulate pages until one comes back short of
// appFetchPageSize (the last page) or appFetchMaxPages is reached (in which
// case truncated=true is returned instead of an error — see the two
// functions' doc comments for why silent-but-flagged truncation, not a hard
// failure, is the right trade-off once installation enumeration is the
// *primary* discovery path, not a verification check against a short list).
func fetchAppPaginated[T any](fetchPage func(page int) ([]T, error)) (items []T, truncated bool, err error) {
	for page := 1; page <= appFetchMaxPages; page++ {
		chunk, err := fetchPage(page)
		if err != nil {
			return nil, false, err
		}
		items = append(items, chunk...)
		if len(chunk) < appFetchPageSize {
			return items, false, nil
		}
	}
	return items, true, nil
}

// FetchAppInstallations lists every installation of the GitHub App
// authenticated by jwt via GET /app/installations. Used for dynamic
// installation discovery: Pruefer watches whatever repos the App is
// installed on, so adding a repo requires only a GitHub-side installation
// change, not a Pruefer config or code change (see ADR-1113).
//
// Follows GitHub's page-number pagination (not the Link header — every
// caller already builds page-numbered URLs directly) up to appFetchMaxPages;
// the returned truncated bool is true if that ceiling was hit before a short
// page was seen, i.e. more installations may exist beyond what was
// returned. Once installation enumeration is the primary repo-discovery
// path (handarbeit/fabrik#1641), silently returning an incomplete list here
// would make an owner beyond the ceiling look uninstalled and trigger a
// spurious guided-install prompt — callers must check truncated rather than
// assume completeness.
func FetchAppInstallations(baseURL, jwt string) ([]AppInstallation, bool, error) {
	type rawInstallation struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		RepositorySelection string `json:"repository_selection"`
	}
	raw, truncated, err := fetchAppPaginated(func(page int) ([]rawInstallation, error) {
		var chunk []rawInstallation
		path := fmt.Sprintf("/app/installations?per_page=%d&page=%d", appFetchPageSize, page)
		if err := appRequest("GET", baseURL, path, jwt, &chunk); err != nil {
			return nil, err
		}
		return chunk, nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("fetching app installations: %w", err)
	}
	if truncated {
		logf(0, "app", "FetchAppInstallations: hit the %d-page pagination ceiling (%d installations returned) — more installations may exist\n", appFetchMaxPages, len(raw))
	}
	out := make([]AppInstallation, len(raw))
	for i, inst := range raw {
		out[i] = AppInstallation{ID: inst.ID, Account: inst.Account.Login, RepositorySelection: inst.RepositorySelection}
	}
	return out, truncated, nil
}

// FetchInstallationRepositories lists the repositories an installation
// actually grants access to, via GET /installation/repositories. Called for
// every installation regardless of RepositorySelection: unlike an earlier
// revision of this function (which only bothered for "selected"-mode
// installations, since "all" was assumed to cover every current and future
// repo), the endpoint already returns the full accessible set for "all"
// installations too — calling it unconditionally is what lets
// installation-derived discovery (handarbeit/fabrik#1641) enumerate an
// "all"-mode installation's repos without a separate code path, and also
// makes newly-created repos under an "all" installation show up on the next
// call with no special casing. Unlike the App-JWT-authenticated calls
// above, this endpoint is scoped to the installation's own identity: it
// must be authenticated with that installation's access token, not the
// App's JWT.
//
// Follows GitHub's page-number pagination up to appFetchMaxPages, exactly
// like FetchAppInstallations above; the returned truncated bool reports
// whether the ceiling was hit. A "selected"-mode installation granting more
// than 100 repos is plausible for a larger org, and an "all"-mode
// installation on a large account even more so — callers must check
// truncated rather than assume the returned list is complete.
func FetchInstallationRepositories(baseURL, installationToken string) ([]string, bool, error) {
	type repoEntry struct {
		FullName string `json:"full_name"`
	}
	raw, truncated, err := fetchAppPaginated(func(page int) ([]repoEntry, error) {
		var result struct {
			Repositories []repoEntry `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=%d&page=%d", appFetchPageSize, page)
		if err := appRequest("GET", baseURL, path, installationToken, &result); err != nil {
			return nil, err
		}
		return result.Repositories, nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("fetching installation repositories: %w", err)
	}
	if truncated {
		logf(0, "app", "FetchInstallationRepositories: hit the %d-page pagination ceiling (%d repositories returned) — more accessible repositories may exist\n", appFetchMaxPages, len(raw))
	}
	out := make([]string, len(raw))
	for i, r := range raw {
		out[i] = r.FullName
	}
	return out, truncated, nil
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
