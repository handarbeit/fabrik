package claudeprofile

import (
	"os"
	"path/filepath"
	"testing"
)

func writeProfile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAccount_ReadsEmailFromConfigDir(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, `{"oauthAccount":{"emailAddress":"a@example.com"}}`)
	if got := Account(dir); got != "a@example.com" {
		t.Errorf("Account(%q) = %q, want a@example.com", dir, got)
	}
}

func TestAccount_EmptyDirReadsHomeDefaultProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeProfile(t, home, `{"oauthAccount":{"emailAddress":"default@example.com"}}`)
	if got := Account(""); got != "default@example.com" {
		t.Errorf(`Account("") = %q, want default@example.com`, got)
	}
}

func TestAccount_FailuresReturnEmpty(t *testing.T) {
	missing := t.TempDir()
	malformed := t.TempDir()
	writeProfile(t, malformed, `{not json`)
	noAccount := t.TempDir()
	writeProfile(t, noAccount, `{}`)
	for name, dir := range map[string]string{"missing": missing, "malformed": malformed, "no account": noAccount} {
		if got := Account(dir); got != "" {
			t.Errorf("%s: Account = %q, want empty", name, got)
		}
	}
}
