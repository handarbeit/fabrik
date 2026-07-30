package githubauth

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	gh "github.com/handarbeit/fabrik/github"
)

// Credentials is the reconciler-owned, non-secret-key metadata persisted at
// an AppStatePath (default .pruefer/app-state.json): everything the manifest
// exchange produces except the private key itself, which is always stored
// separately as a PEM file (see loadPrivateKey/savePrivateKey) so the two
// files can carry different sensitivity/handling even though both are
// secrets. WebhookSecret and the client ID/secret pair are not used by
// Pruefer V1 (polling-only, no webhook delivery — see ADR-1113 §1/ADR-032)
// but are persisted for forward compatibility with a future webhook-transport
// issue, per the issue's own credential-model requirement.
type Credentials struct {
	AppID         int64  `json:"app_id"`
	Slug          string `json:"slug"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
	ClientID      string `json:"client_id,omitempty"`
	ClientSecret  string `json:"client_secret,omitempty"`
	// InstallationRepoCache is a best-effort cache of the last known owner
	// -> installation-accessible-repos mapping, for human-readable
	// diagnostics only. Never authoritative: Reconcile always re-verifies
	// live against GitHub before granting access to a repo: a stale or
	// missing cache entry never grants access, and a stale entry claiming
	// access can never substitute for the live check either.
	InstallationRepoCache map[string][]string `json:"installation_repo_cache,omitempty"`
}

// loadCredentials reads path, returning a zero-value Credentials (no error)
// if the file does not exist — mirroring pruefer/config.go's
// loadYAMLConfig "absent file" convention. A file that exists but fails to
// parse is a corrupt-state error, distinguished from "absent" so callers can
// tell "nothing here yet" from "something here is broken" (see the issue's
// three-way credential-problem branching).
func loadCredentials(path string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Credentials{}, nil
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return Credentials{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return c, nil
}

// saveCredentials writes c to path as indented JSON, creating any missing
// parent directory (0700, owner-only) and writing the file itself 0600
// (owner-only) — this file never holds the App's private key, but it does
// hold a webhook secret and OAuth client secret, both of which warrant the
// same handling as the PEM.
func saveCredentials(path string, c Credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling app state: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// savePrivateKey writes raw PEM bytes to path, creating any missing parent
// directory (0700) and writing the file itself 0600 (owner-only) — the
// minimum protection the issue's credential-model requirement asks for.
func savePrivateKey(path string, pemBytes []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, pemBytes, 0600); err != nil {
		return fmt.Errorf("writing private key %s: %w", path, err)
	}
	return nil
}

// loadPrivateKey reads and parses the PEM-encoded RSA private key at path.
// Unlike loadCredentials, a missing file is returned as an ordinary error —
// the caller (loadOrBootstrapCredentials) is responsible for distinguishing
// "never had one" from "had one, now missing/corrupt" using its own
// knowledge of whether an AppID was already known.
func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := gh.ParseAppPrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parsing private key %s: %w", path, err)
	}
	return key, nil
}
