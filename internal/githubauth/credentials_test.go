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
