package githubauth

import (
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
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
	// PrivateKeyFingerprint is a SHA-256 hex digest of the PEM bytes this
	// AppID's private key is expected to hold, recorded at the same time
	// AppID itself is persisted (RunManifestFlow sets both in the same
	// saveCredentials call, before the PEM is written to disk). It exists
	// to close a narrow crash-window: App re-creation (self-heal after an
	// externally-deleted App) writes app-state.json first and the PEM
	// second — if the process is killed between the two writes, the state
	// file names the new App while PrivateKeyPath still holds the
	// *previous* App's key, a mismatch nothing else here would ever
	// notice (both files exist and parse fine). loadOrBootstrapCredentials
	// treats a fingerprint mismatch as a repair-needed error rather than
	// silently building a JWT with the wrong key pair and re-entering
	// self-heal on every subsequent restart. Left empty (and never
	// checked) for state files predating this field, and for a
	// first-ever bootstrap's own state read (nothing to compare against
	// yet).
	PrivateKeyFingerprint string `json:"private_key_fingerprint,omitempty"`
}

// privateKeyFingerprint returns a SHA-256 hex digest of raw PEM bytes, used
// to detect a private key file that doesn't match the AppID it's supposed
// to belong to (see Credentials.PrivateKeyFingerprint). Not a security
// boundary — just a cheap consistency check against a specific crash
// window, so a simple content hash is sufficient.
func privateKeyFingerprint(pemBytes []byte) string {
	sum := sha256.Sum256(pemBytes)
	return hex.EncodeToString(sum[:])
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
// same handling as the PEM. The write is atomic (see atomicWriteFile):
// saveInstallationRepoCache calls this on every successful Reconcile, not
// just first-run bootstrap, so an ordinary process kill/OOM/power loss
// during a routine restart must never leave a truncated, invalid JSON file
// behind — that would surface as loadOrBootstrapCredentials's "app state
// file exists but is corrupt" hard error, turning a routine interruption
// into a manual-repair incident.
func saveCredentials(path string, c Credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling app state: %w", err)
	}
	if err := atomicWriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// savePrivateKey writes raw PEM bytes to path, creating any missing parent
// directory (0700) and writing the file itself 0600 (owner-only) — the
// minimum protection the issue's credential-model requirement asks for. The
// write is atomic (see atomicWriteFile), for the same torn-write reason as
// saveCredentials.
func savePrivateKey(path string, pemBytes []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}
	if err := atomicWriteFile(path, pemBytes, 0600); err != nil {
		return fmt.Errorf("writing private key %s: %w", path, err)
	}
	return nil
}

// atomicWriteFile writes data to a temp file in path's own directory (so
// the subsequent rename is same-filesystem and therefore atomic), sets perm
// explicitly rather than relying on umask, and renames it over path. This
// means a reader (or a concurrent save) never observes a partially-written
// file: path either still holds its old complete contents or the new
// complete contents, never something truncated in between — unlike a direct
// os.WriteFile, which writes in place and can leave a torn file behind if
// the process is killed mid-write.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	// os.CreateTemp already creates the file 0600 on POSIX, but chmod
	// explicitly rather than relying on that default — perm is the actual
	// contract callers depend on, and an existing target file's current
	// permissions must never leak through to the replacement (unlike plain
	// os.WriteFile, a rename replaces the file's permissions entirely, so
	// this is the only place they're set).
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("setting permissions on temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming temp file into place: %w", err)
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
