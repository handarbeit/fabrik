package pruefer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shellAdded are variables a POSIX shell adds to its own environment; they are
// not inherited from the daemon.
var shellAdded = map[string]bool{"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true}

var daemonSecrets = map[string]string{
	"GH_TOKEN":                    "ghs_secret_gh",
	"GITHUB_TOKEN":                "ghs_secret_github",
	"FABRIK_TOKEN":                "secret_fabrik",
	"PRUEFER_GITHUB_APP_ID":       "12345",
	"PRUEFER_APP_PRIVATE_KEY":     "-----BEGIN PRIVATE KEY-----",
	"HOOKDECK_API_KEY":            "secret_hookdeck",
	"GITHUB_APP_PRIVATE_KEY_PATH": "/secrets/app.pem",
	"DOTENV_ONLY_SECRET":          "from-dot-env", // not allowlisted: dropped by default
}

func setDaemonSecrets(t *testing.T) {
	t.Helper()
	for k, v := range daemonSecrets {
		t.Setenv(k, v)
	}
}

// writeEnvPlugin writes an operator plugin whose hook and MCP server each
// record the environment they start with, and returns its directory.
func writeEnvPlugin(t *testing.T) string {
	t.Helper()
	script := "#!/bin/sh\nenv > \"$1\"\n"
	dir := filepath.Join(t.TempDir(), "envplug")
	files := []struct {
		path, data string
		mode       os.FileMode
	}{
		{".claude-plugin/plugin.json", `{"name":"envplug","version":"1"}`, 0o644},
		{"hooks/hooks.json", `{"hooks":{}}`, 0o644},
		{"hooks/record.sh", script, 0o755},
		{".mcp.json", `{"mcpServers":{"rec":{"command":"${CLAUDE_PLUGIN_ROOT}/mcp/server.sh"}}}`, 0o644},
		{"mcp/server.sh", script, 0o755},
	}
	for _, f := range files {
		target := filepath.Join(dir, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(f.data), f.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, f.mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// pluginChildrenScript is a fake claude that does what the real CLI does with a
// plugin: it starts the plugin's hook and MCP server as child processes of
// itself, so they inherit exactly the environment the review process was given.
func pluginChildrenScript(rec string) string {
	return `prev=""
for a in "$@"; do
  if [ "$prev" = "--plugin-dir" ]; then
    [ -x "$a/hooks/record.sh" ] && "$a/hooks/record.sh" "` + rec + `.hook"
    [ -x "$a/mcp/server.sh" ] && "$a/mcp/server.sh" "` + rec + `.mcp"
  fi
  prev="$a"
done
echo '` + resultLine + `'
`
}

func readEnvFile(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the plugin child never ran: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func envHas(env []string, name string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

// R6 (#2307): everything an operator plugin starts — a hook, an MCP server —
// runs in the scrubbed review environment: no GitHub token, App key or .env secret.
func TestPluginHooksAndMCPServersRunInTheScrubbedReviewEnvironment(t *testing.T) {
	setDaemonSecrets(t)
	rec := filepath.Join(t.TempDir(), "rec")
	writeFakeClaude(t, pluginChildrenScript(rec))

	res, err := (&RealClaudeInvoker{}).Review(context.Background(), ReviewRequest{
		PRNumber: 1, WorkDir: t.TempDir(), OperatorPluginDirs: []string{writeEnvPlugin(t)},
	})
	if err != nil || res.Text == "" {
		t.Fatalf("review failed: %v", err)
	}
	for _, which := range []string{"hook", "mcp"} {
		env := readEnvFile(t, rec+"."+which)
		for name := range daemonSecrets {
			if envHas(env, name) {
				t.Errorf("the plugin %s inherited %s", which, name)
			}
		}
		for _, kv := range env {
			for _, secret := range daemonSecrets {
				if strings.Contains(kv, secret) {
					t.Errorf("the plugin %s environment contains a daemon secret value: %s", which, kv)
				}
			}
		}
		if err := checkReviewEnvScrubbed(env, shellAdded); err != nil {
			t.Errorf("the plugin %s environment is not the scrubbed one: %v", which, err)
		}
	}
}

// Non-vacuity: the same plugin children, started from an unscrubbed environment,
// do see the secrets, and the scrub assertions do flag them.
func TestPluginEnvironmentTest_FailsAgainstAnUnscrubbedEnvironment(t *testing.T) {
	setDaemonSecrets(t)
	dir := writeEnvPlugin(t)
	rec := filepath.Join(t.TempDir(), "rec")
	cmd := exec.Command(filepath.Join(dir, "hooks", "record.sh"), rec+".hook")
	cmd.Env = os.Environ() // what the daemon holds: not scrubbed
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	env := readEnvFile(t, rec+".hook")
	if !envHas(env, "GH_TOKEN") || !envHas(env, "FABRIK_TOKEN") {
		t.Fatal("the fixture hook does not record the environment it was given")
	}
	if checkReviewEnvScrubbed(env, shellAdded) == nil {
		t.Error("checkReviewEnvScrubbed accepted an unscrubbed environment")
	}
}

// A plugin that tries to reach GitHub by itself — as the code-review command
// does with gh — has no credential to do it with.
func TestPluginCannotReachGitHubWithTheDaemonsCredentials(t *testing.T) {
	setDaemonSecrets(t)
	rec := filepath.Join(t.TempDir(), "rec")
	writeFakeClaude(t, `echo "GH_TOKEN=${GH_TOKEN:-unset} GITHUB_TOKEN=${GITHUB_TOKEN:-unset}" > "`+rec+`"
echo '`+resultLine+`'
`)
	if _, err := (&RealClaudeInvoker{}).Review(context.Background(), ReviewRequest{PRNumber: 1, WorkDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(mustRead(t, rec))); got != "GH_TOKEN=unset GITHUB_TOKEN=unset" {
		t.Errorf("review process saw %q", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
