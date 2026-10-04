// Package testenv makes a test immune to everything a Fabrik stage worker (or
// an operator's shell) leaves in the process environment (#2027, ADR-2027).
//
// A worker runs the repo's own test suite with the daemon's environment
// inherited: the App/PAT/webhook credentials, GH_TOKEN, and the ADR-1846
// GIT_CONFIG_COUNT/KEY_n/VALUE_n credential-helper entries. CI has none of
// them, so a test that reads any of them passes in CI and fails (or hangs) in
// a worker. These helpers unset every one — truly unset, not set to "", since
// LookupEnv callers tell the two apart — and restore the original on cleanup.
//
// The package is a regular (non-_test) package so other packages' tests can
// import it; it must be imported from _test.go files only. It depends only on
// internal/workerenv, so it cannot create an import cycle with engine or cmd.
package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/workerenv"
)

// fixedNames are the non-FABRIK names a worker may inherit that tests must not
// see: the sanctioned GitHub access (R2) and the fixed git-config selectors.
var fixedNames = []string{
	"GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
	"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
}

// Names returns every variable Isolate clears, given the environment to
// inspect: the fixed names, every scrubbed Fabrik variable and every
// GIT_CONFIG_KEY_<n>/GIT_CONFIG_VALUE_<n> present in environ (enumerated, not
// hardcoded to a count).
func Names(environ []string) []string {
	names := append([]string{}, fixedNames...)
	names = append(names, workerenv.ScrubbedNames()...)
	names = append(names, workerenv.DefaultHookdeckAPIKeyEnv, workerenv.DefaultHookdeckWebhookSecretEnv)
	for _, kv := range environ {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		k := kv[:i]
		if strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			names = append(names, k)
		}
	}
	return names
}

// Isolate unsets every variable a worker inherits for the duration of the
// test and restores the original values on cleanup. A test that needs one of
// them sets it explicitly with t.Setenv afterwards.
func Isolate(t testing.TB) {
	t.Helper()
	for _, k := range Names(os.Environ()) {
		// t.Setenv records the prior state (set or unset) for restore on
		// cleanup; the Unsetenv then makes the variable genuinely absent.
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("testenv: unsetting %s: %v", k, err)
		}
	}
}

// IsolateGit is Isolate plus an ambient-git-config cut-off: the global config
// is a test-local file holding globalContents (e.g. a url insteadOf rewrite)
// and the system config is disabled. Command-scope GIT_CONFIG_* entries — which
// outrank the global file — are removed by Isolate.
func IsolateGit(t testing.TB, globalContents string) {
	t.Helper()
	Isolate(t)
	cfgPath := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfgPath, []byte(globalContents), 0o644); err != nil {
		t.Fatalf("testenv: writing isolated git config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfgPath)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// ScrubProcess unsets the same variables once, for a package's TestMain, where
// there is no *testing.T. Nothing is restored: the process is a test binary.
// Tests that need a variable set it with t.Setenv.
func ScrubProcess() {
	for _, k := range Names(os.Environ()) {
		_ = os.Unsetenv(k) // best-effort in TestMain; Unsetenv only fails on an invalid name
	}
}
