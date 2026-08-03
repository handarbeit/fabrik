package githubauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadCredentials_MissingFileReturnsZeroValueNoError(t *testing.T) {
	c, err := loadCredentials(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}
	if c.AppID != 0 || c.Slug != "" || c.InstallationRepoCache != nil {
		t.Errorf("expected zero-value Credentials, got %+v", c)
	}
}

func TestLoadCredentials_CorruptFileReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-state.json")
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatalf("writing corrupt state file: %v", err)
	}
	if _, err := loadCredentials(path); err == nil {
		t.Fatal("expected an error for a corrupt app-state file")
	}
}

func TestSaveCredentials_RoundTripAndPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file permissions not applicable on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "app-state.json")
	want := Credentials{
		AppID: 123, Slug: "my-pruefer", WebhookSecret: "whsec",
		ClientID: "cid", ClientSecret: "csecret",
		InstallationRepoCache: map[string][]string{"acme": {"acme/repo"}},
	}
	if err := saveCredentials(path, want); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file perm = %o, want 0600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0700 {
		t.Errorf("dir perm = %o, want 0700", perm)
	}

	got, err := loadCredentials(path)
	if err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}
	if got.AppID != want.AppID || got.Slug != want.Slug || got.WebhookSecret != want.WebhookSecret ||
		got.ClientID != want.ClientID || got.ClientSecret != want.ClientSecret {
		t.Errorf("round-tripped Credentials = %+v, want %+v", got, want)
	}
	if got.InstallationRepoCache["acme"][0] != "acme/repo" {
		t.Errorf("InstallationRepoCache not round-tripped: %+v", got.InstallationRepoCache)
	}
}

func TestSaveCredentials_TightensPermsOnExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file permissions not applicable on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "app-state.json")
	// Simulate a pre-existing file with wider-than-expected permissions,
	// e.g. left over from an older/looser version or a manual copy.
	if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
		t.Fatalf("seeding existing file: %v", err)
	}

	if err := saveCredentials(path, Credentials{AppID: 42}); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file perm = %o, want 0600 (saveCredentials must tighten perms on an existing file, not just a newly created one)", perm)
	}
}

func writeTestPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generating test RSA key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func TestSavePrivateKey_RoundTripAndPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file permissions not applicable on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "app-private-key.pem")
	pemBytes := writeTestPrivateKeyPEM(t)

	if err := savePrivateKey(path, pemBytes); err != nil {
		t.Fatalf("savePrivateKey: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file perm = %o, want 0600", perm)
	}

	key, err := loadPrivateKey(path)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	if key == nil {
		t.Fatal("expected a parsed private key")
	}
}

func TestSavePrivateKey_TightensPermsOnExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file permissions not applicable on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "app-private-key.pem")
	// Simulate a pre-existing PEM with wider-than-expected permissions,
	// e.g. copied in manually under a default umask, or from a re-bootstrap
	// after "App deleted externally" overwriting a file left at 0644.
	if err := os.WriteFile(path, []byte("placeholder"), 0644); err != nil {
		t.Fatalf("seeding existing file: %v", err)
	}
	pemBytes := writeTestPrivateKeyPEM(t)

	if err := savePrivateKey(path, pemBytes); err != nil {
		t.Fatalf("savePrivateKey: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file perm = %o, want 0600 (savePrivateKey must tighten perms on an existing file, not just a newly created one)", perm)
	}
}

func TestSaveCredentials_AtomicNoLeftoverTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app-state.json")

	if err := saveCredentials(path, Credentials{AppID: 1}); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}
	if err := saveCredentials(path, Credentials{AppID: 2}); err != nil {
		t.Fatalf("saveCredentials (second write): %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "app-state.json" {
		t.Fatalf("expected exactly one file (app-state.json) after two atomic writes, got %v", entries)
	}
}

func TestSaveCredentials_PreservesOldContentOnWriteFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app-state.json")
	if err := saveCredentials(path, Credentials{AppID: 1, Slug: "original"}); err != nil {
		t.Fatalf("seeding original saveCredentials: %v", err)
	}

	// Make the directory read-only so the temp-file rename inside a second
	// saveCredentials call fails partway through — atomicWriteFile must
	// leave the original file completely untouched rather than partially
	// overwritten, since it only ever mutates via a same-directory rename,
	// never in place.
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based read-only directory simulation not reliable on windows")
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("chmod dir read-only: %v", err)
	}
	defer os.Chmod(dir, 0700)

	err := saveCredentials(path, Credentials{AppID: 2, Slug: "should-not-land"})
	if err == nil {
		t.Fatal("expected saveCredentials to fail against a read-only directory")
	}

	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatalf("restoring dir perms: %v", err)
	}
	got, err := loadCredentials(path)
	if err != nil {
		t.Fatalf("loadCredentials after failed write: %v", err)
	}
	if got.AppID != 1 || got.Slug != "original" {
		t.Errorf("original content was not preserved after a failed atomic write: got %+v", got)
	}
}

func TestLoadPrivateKey_MissingFile(t *testing.T) {
	if _, err := loadPrivateKey(filepath.Join(t.TempDir(), "nope.pem")); err == nil {
		t.Fatal("expected an error for a missing private key file")
	}
}

func TestLoadPrivateKey_CorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.pem")
	if err := os.WriteFile(path, []byte("not a pem"), 0600); err != nil {
		t.Fatalf("writing corrupt key file: %v", err)
	}
	if _, err := loadPrivateKey(path); err == nil {
		t.Fatal("expected an error for a corrupt private key file")
	}
}
