//go:build e2e

package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecideAppLegRun(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		identity   string
		wantRun    bool
		wantReason string // substring; "" = none expected
		wantErr    bool
	}{
		{name: "pat skips even with an identity", mode: "pat", identity: "fabrik-bed[bot]", wantReason: "PAT auth leg"},
		{name: "pat skips without an identity", mode: "pat", wantReason: "PAT auth leg"},
		{name: "app with identity runs", mode: "app", identity: "fabrik-bed[bot]", wantRun: true},
		{name: "app without identity fails loudly", mode: "app", wantErr: true},
		{name: "unset with identity runs", mode: "", identity: "fabrik-bed[bot]", wantRun: true},
		{name: "unset without identity skips as undeterminable", mode: "", wantReason: "cannot determine"},
		{name: "unknown mode is an error", mode: "both", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run, reason, err := decideAppLegRun(tc.mode, tc.identity)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if run != tc.wantRun {
				t.Errorf("run = %v, want %v", run, tc.wantRun)
			}
			if tc.wantReason == "" && reason != "" {
				t.Errorf("reason = %q, want none", reason)
			}
			if tc.wantReason != "" && !strings.Contains(reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", reason, tc.wantReason)
			}
		})
	}
}

func TestParseAppIdentity(t *testing.T) {
	const base = "/beds/fabrik-test"

	t.Run("all set, relative key resolved against the bed dir", func(t *testing.T) {
		got, err := parseAppIdentity("123", "keys/app.pem", "456", base)
		if err != nil {
			t.Fatal(err)
		}
		want := appIdentity{AppID: 123, KeyPath: filepath.Join(base, "keys/app.pem"), InstallationID: 456}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
	t.Run("absolute key kept, whitespace trimmed", func(t *testing.T) {
		got, err := parseAppIdentity(" 7 ", " /etc/app.pem ", " 8 ", base)
		if err != nil {
			t.Fatal(err)
		}
		if got.KeyPath != "/etc/app.pem" || got.AppID != 7 || got.InstallationID != 8 {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("nothing set is the unset sentinel", func(t *testing.T) {
		if _, err := parseAppIdentity("", "", "  ", base); !errors.Is(err, errAppIdentityUnset) {
			t.Errorf("err = %v, want errAppIdentityUnset", err)
		}
	})
	t.Run("partial set names what is missing", func(t *testing.T) {
		_, err := parseAppIdentity("123", "", "", base)
		if err == nil || errors.Is(err, errAppIdentityUnset) {
			t.Fatalf("err = %v, want a hard partial-config error", err)
		}
		for _, key := range []string{"E2E_APP_PRIVATE_KEY_PATH", "E2E_APP_INSTALLATION_ID"} {
			if !strings.Contains(err.Error(), key) {
				t.Errorf("err %q does not name %s", err, key)
			}
		}
	})
	t.Run("non-integers rejected", func(t *testing.T) {
		for _, tc := range [][3]string{{"abc", "k.pem", "1"}, {"1", "k.pem", "x"}, {"0", "k.pem", "1"}, {"1", "k.pem", "-5"}} {
			if _, err := parseAppIdentity(tc[0], tc[1], tc[2], base); err == nil {
				t.Errorf("parseAppIdentity(%q, %q, %q) = nil error, want invalid", tc[0], tc[1], tc[2])
			}
		}
	})
}

func TestRedactSecret(t *testing.T) {
	const tok = "ghs_SECRETTOKEN123"
	tests := []struct{ name, in, secret, want string }{
		{"single occurrence", "Authorization: " + tok, tok, "Authorization: [REDACTED]"},
		{"repeated", tok + " and " + tok, tok, "[REDACTED] and [REDACTED]"},
		{"absent", "nothing here", tok, "nothing here"},
		{"empty secret is a no-op", "abc", "", "abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactSecret(tc.in, tc.secret); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if err := redactedErr(errors.New("boom "+tok), tok); strings.Contains(err.Error(), tok) {
		t.Errorf("redactedErr leaked the secret: %v", err)
	}
}

// TestMintAppInstallationTokenErrors pins that an unreadable or
// malformed key fails with the key path, never with key material.
func TestMintAppInstallationTokenErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := mintAppInstallationToken(appIdentity{AppID: 1, KeyPath: filepath.Join(dir, "missing.pem"), InstallationID: 2}); err == nil {
		t.Error("missing key: want error")
	}
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("-----BEGIN RSA PRIVATE KEY-----\nnot a key\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := mintAppInstallationToken(appIdentity{AppID: 1, KeyPath: bad, InstallationID: 2})
	if err == nil {
		t.Fatal("malformed key: want error")
	}
	if strings.Contains(err.Error(), "not a key") {
		t.Errorf("error echoed key material: %v", err)
	}
}
