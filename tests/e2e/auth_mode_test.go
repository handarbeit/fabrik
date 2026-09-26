//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeAuthMode(t *testing.T) {
	for raw, want := range map[string]string{"": "", "pat": "pat", "APP": "app", " app ": "app"} {
		got, err := normalizeAuthMode(raw)
		if err != nil || got != want {
			t.Errorf("normalizeAuthMode(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := normalizeAuthMode("both"); err == nil {
		t.Error("normalizeAuthMode(\"both\") = nil error, want invalid")
	}
}

// writeBed builds a minimal bed dir with the given .env and config.yaml.
func writeBed(t *testing.T, envFile, config string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(envFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const appIdentityEnv = "FABRIK_TOKEN=tok\nE2E_APP_ID=4960842\nE2E_APP_PRIVATE_KEY_PATH=.fabrik/github-app-key.pem\nE2E_APP_INSTALLATION_ID=162085522\n"

func TestApplyBedAuthMode(t *testing.T) {
	t.Run("app writes the engine vars from the bed identity", func(t *testing.T) {
		dir := writeBed(t, appIdentityEnv, "owner: handarbeit\n# github_app_id: 1\n")
		if err := applyBedAuthMode(dir, "app"); err != nil {
			t.Fatal(err)
		}
		envFile := filepath.Join(dir, ".env")
		for key, want := range map[string]string{
			"FABRIK_GITHUB_APP_ID":               "4960842",
			"FABRIK_GITHUB_APP_PRIVATE_KEY_PATH": ".fabrik/github-app-key.pem",
			"FABRIK_GITHUB_APP_INSTALLATION_ID":  "162085522",
			"FABRIK_TOKEN":                       "tok",
		} {
			if got, _ := readEnvFileValue(envFile, key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
	})

	t.Run("pat after app blanks the engine vars and keeps the identity", func(t *testing.T) {
		dir := writeBed(t, appIdentityEnv, "owner: handarbeit\n")
		if err := applyBedAuthMode(dir, "app"); err != nil {
			t.Fatal(err)
		}
		if err := applyBedAuthMode(dir, "pat"); err != nil {
			t.Fatal(err)
		}
		envFile := filepath.Join(dir, ".env")
		for _, key := range bedAppAuthEnvKeys {
			if got, _ := readEnvFileValue(envFile, key); got != "" {
				t.Errorf("%s = %q after pat, want empty", key, got)
			}
		}
		if got, _ := readEnvFileValue(envFile, "E2E_APP_ID"); got != "4960842" {
			t.Errorf("E2E_APP_ID = %q after pat, want it kept", got)
		}
	})

	t.Run("config.yaml github_app keys refused in either mode", func(t *testing.T) {
		dir := writeBed(t, appIdentityEnv, "owner: handarbeit\ngithub_app_id: 4960842\n")
		for _, mode := range []string{"pat", "app"} {
			err := applyBedAuthMode(dir, mode)
			if err == nil || !strings.Contains(err.Error(), "github_app_") {
				t.Errorf("%s: err = %v, want a github_app_* config refusal", mode, err)
			}
		}
	})

	t.Run("app without the bed identity refused", func(t *testing.T) {
		dir := writeBed(t, "FABRIK_TOKEN=tok\nE2E_APP_ID=4960842\n", "owner: handarbeit\n")
		err := applyBedAuthMode(dir, "app")
		if err == nil || !strings.Contains(err.Error(), "E2E_APP_PRIVATE_KEY_PATH") {
			t.Errorf("err = %v, want it to name the missing E2E_APP_PRIVATE_KEY_PATH", err)
		}
	})
}

func TestBedAuthIdentity(t *testing.T) {
	app := "Fabrik starting dev(abc)\n[startup] github-app: ✓ authenticated as fabrik-bed[bot]\n" +
		"[startup] authenticated as fabrik-bed[bot] (GitHub App installation, organization \"handarbeit\")\n"
	if got := bedAuthIdentity(app); got != "fabrik-bed[bot]" {
		t.Errorf("app banner: got %q, want fabrik-bed[bot]", got)
	}
	// The reconciler's own "github-app: ✓ authenticated as" line alone is not
	// the completed-setup banner.
	if got := bedAuthIdentity("[startup] github-app: ✓ authenticated as fabrik-bed[bot]\n"); got != "" {
		t.Errorf("partial App setup: got %q, want none", got)
	}
	if got := bedAuthIdentity("Fabrik starting dev(abc)\n[startup] warn: something\n"); got != "" {
		t.Errorf("pat stdout: got %q, want none", got)
	}
}
