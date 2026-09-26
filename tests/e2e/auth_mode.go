//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Auth-mode legs (#1861): scripts/e2e/run.sh runs the suite once with the bed
// engine on a PAT and once as the GitHub App installation. The mode is applied
// through the bed's .env, never config.yaml, by TestSwitchTrainMode's restart.

// bedAppAuthEnvKeys are the engine's App-auth env vars, in the order written.
var bedAppAuthEnvKeys = []string{
	"FABRIK_GITHUB_APP_ID",
	"FABRIK_GITHUB_APP_PRIVATE_KEY_PATH",
	"FABRIK_GITHUB_APP_INSTALLATION_ID",
}

// bedAppIdentityEnvKeys are the bed-local sources for bedAppAuthEnvKeys, index
// for index. They live in the bed's .env under names Fabrik never reads, so the
// App identity stays configured while a pat leg runs.
var bedAppIdentityEnvKeys = []string{
	"E2E_APP_ID",
	"E2E_APP_PRIVATE_KEY_PATH",
	"E2E_APP_INSTALLATION_ID",
}

// appAuthBannerPrefix/appAuthBannerSuffix bracket the startup line
// engine.setUpGitHubAppAuth prints once App auth is fully set up.
const (
	appAuthBannerPrefix = "[startup] authenticated as "
	appAuthBannerSuffix = "(GitHub App installation"
)

// configGitHubAppKeyRe matches an uncommented github_app_* auth key in
// config.yaml. Such a key would make every pat leg run as the App.
var configGitHubAppKeyRe = regexp.MustCompile(`(?m)^\s*github_app_(id|private_key_path|installation_id)\s*:`)

// normalizeAuthMode validates E2E_AUTH_MODE. Empty means "leave the bed's
// auth as it is" (a switch step invoked outside run.sh's auth legs).
func normalizeAuthMode(raw string) (string, error) {
	switch m := strings.ToLower(strings.TrimSpace(raw)); m {
	case "", "pat", "app":
		return m, nil
	default:
		return "", fmt.Errorf("E2E_AUTH_MODE=%q is invalid (must be pat or app)", raw)
	}
}

// bedRunLogPath is where the bed's stdout/stderr go (run.sh's
// preflight_bed_start and StartFabrikTestBed both write it).
func bedRunLogPath(env *Env) string {
	return filepath.Join(env.FabrikTestDir, "bed-run.log")
}

// applyBedAuthMode writes the bed's .env for mode ("pat" or "app") and
// refuses a config.yaml that sets github_app_* itself.
func applyBedAuthMode(bedDir, mode string) error {
	cfgPath := filepath.Join(bedDir, ".fabrik", "config.yaml")
	if data, err := os.ReadFile(cfgPath); err == nil && configGitHubAppKeyRe.Match(data) {
		return fmt.Errorf("%s sets github_app_* keys; auth mode is applied per leg through .env, so move them to %s in %s/.env",
			cfgPath, strings.Join(bedAppIdentityEnvKeys, "/"), bedDir)
	}
	envFile := filepath.Join(bedDir, ".env")
	for i, key := range bedAppAuthEnvKeys {
		value := ""
		if mode == "app" {
			v, err := readEnvFileValue(envFile, bedAppIdentityEnvKeys[i])
			if err != nil || v == "" {
				return fmt.Errorf("app auth leg needs %s in %s (err: %v)", bedAppIdentityEnvKeys[i], envFile, err)
			}
			value = v
		}
		if err := writeEnvFileValue(envFile, key, value); err != nil {
			return fmt.Errorf("writing %s to %s: %w", key, envFile, err)
		}
	}
	return nil
}

// bedAuthIdentity reports the App login from a bed stdout log's startup
// banner, or "" when the bed started without App auth.
func bedAuthIdentity(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), appAuthBannerPrefix); ok && strings.Contains(rest, appAuthBannerSuffix) {
			login, _, _ := strings.Cut(rest, " ")
			return login
		}
	}
	return ""
}

// verifyBedAuthIdentity fails the test unless the running bed started in
// mode. It relies on New() printing the App banner before Run() takes the
// lock StartFabrikTestBed waits for, so the banner is already in the log.
func verifyBedAuthIdentity(t *testing.T, env *Env, mode string) {
	t.Helper()
	data, err := os.ReadFile(bedRunLogPath(env))
	if err != nil {
		t.Fatalf("reading bed stdout %s to verify auth mode %q: %v", bedRunLogPath(env), mode, err)
	}
	login := bedAuthIdentity(string(data))
	switch {
	case mode == "app" && login == "":
		t.Fatalf("bed restarted for the app auth leg but its startup shows no GitHub App identity — bed stdout:\n%s", tailLines(string(data), 30))
	case mode == "pat" && login != "":
		t.Fatalf("bed restarted for the pat auth leg but authenticated as App %s — bed stdout:\n%s", login, tailLines(string(data), 30))
	case login != "":
		t.Logf("bed auth identity verified: GitHub App %s", login)
	default:
		t.Logf("bed auth identity verified: PAT (no GitHub App banner)")
	}
}

// tailLines returns the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
