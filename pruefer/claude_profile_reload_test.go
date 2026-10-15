package pruefer

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/claudeprofile"
)

// reviewProfileFor runs the real review path (ReviewPR) with cfg and returns the
// CLAUDE_CONFIG_DIR the subprocess environment would hold for the request the
// invoker received; "" means none.
func reviewProfileFor(t *testing.T, cfg Config) string {
	t.Helper()
	client := newFakeReviewer()
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1", Title: "T"}
	if out := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr, nil); !out.Reviewed {
		t.Fatalf("outcome = %+v, want Reviewed", out)
	}
	calls := claude.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("invoker called %d times, want 1", len(calls))
	}
	daemonEnv := []string{"PATH=/usr/bin", claudeprofile.BareEnv + "=/daemon/inherited"}
	for _, kv := range reviewProcessEnv(daemonEnv, calls[0]) {
		if strings.HasPrefix(kv, claudeprofile.BareEnv+"=") {
			return strings.TrimPrefix(kv, claudeprofile.BareEnv+"=")
		}
	}
	return ""
}

// Reload (#2294 R4): changing the YAML value and reloading changes the next
// review's CLAUDE_CONFIG_DIR and logs the new effective account; a review that
// started earlier keeps its value; the ambient snapshot is not re-read.
func TestHandleReload_ClaudeProfile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // handleReload re-reads ./.env
	ambient := profileDir(t, "ambient", "ambient@example.com")
	y1 := profileDir(t, "y1", "y1@example.com")
	y2 := profileDir(t, "y2", "y2@example.com")
	profileEnv(t, ambient, "", nil)

	keyPath := writeTestPrivateKey(t, dir)
	cfgPath := filepath.Join(dir, "config.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("model: sonnet\nclaude_config_dir: " + y1 + "\n")
	args, _, daemon, _, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{{ID: 111, Account: "handarbeit"}})
	defer closeLog()
	snapshot := captureLogf(t)

	before := daemon.config()
	if before.ClaudeConfigDir != y1 {
		t.Fatalf("initial ClaudeConfigDir = %q, want %q", before.ClaudeConfigDir, y1)
	}
	if got := reviewProfileFor(t, before); got != y1 {
		t.Fatalf("review profile before reload = %q, want %q", got, y1)
	}

	// 1. YAML change + reload.
	write("model: sonnet\nclaude_config_dir: " + y2 + "\n")
	handleReload(context.Background(), args, daemon)
	waitForRederivationDone(t, daemon)

	after := daemon.config()
	if after.ClaudeConfigDir != y2 {
		t.Fatalf("ClaudeConfigDir after reload = %q, want %q", after.ClaudeConfigDir, y2)
	}
	if got := reviewProfileFor(t, after); got != y2 {
		t.Errorf("next review profile = %q, want %q", got, y2)
	}
	// A request built from the pre-reload config keeps the value it started with.
	if got := reviewProfileFor(t, before); got != y1 {
		t.Errorf("review started before the reload = %q, want %q", got, y1)
	}
	joined := strings.Join(snapshot(), "\n")
	for _, want := range []string{"y2@example.com", y2} {
		if !strings.Contains(joined, want) {
			t.Errorf("reload log missing the new effective account %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "disagree") {
		t.Errorf("reload log missing the disagreement warning (YAML vs ambient):\n%s", joined)
	}

	// 2. The ambient snapshot is not re-read: change the live environment and
	// drop the YAML key; the result is the start-time ambient, not the new env.
	later := profileDir(t, "later", "later@example.com")
	t.Setenv(claudeprofile.BareEnv, later)
	write("model: sonnet\n")
	handleReload(context.Background(), args, daemon)
	waitForRederivationDone(t, daemon)
	if got := daemon.config().ClaudeConfigDir; got != ambient {
		t.Errorf("ClaudeConfigDir = %q, want the start-time ambient %q (not the later env %q)", got, ambient, later)
	}
}

// Reload re-reads .env as data, applies it, and exports nothing.
func TestHandleReload_ReadsDotenvAsData(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ambient := profileDir(t, "ambient", "ambient@example.com")
	fromEnv := profileDir(t, "dotenv", "dotenv@example.com")
	profileEnv(t, ambient, "", nil)

	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("model: sonnet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, _, daemon, _, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{{ID: 111, Account: "handarbeit"}})
	defer closeLog()
	captureLogf(t)
	if got := daemon.config().ClaudeConfigDir; got != ambient {
		t.Fatalf("initial = %q, want ambient %q", got, ambient)
	}

	t.Setenv("PRUEFER_RELOAD_DOTENV_PROBE", "")
	os.Unsetenv("PRUEFER_RELOAD_DOTENV_PROBE")
	dotenv := "CLAUDE_CONFIG_DIR=" + fromEnv + "\nPRUEFER_RELOAD_DOTENV_PROBE=1\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(dotenv), 0o600); err != nil {
		t.Fatal(err)
	}
	handleReload(context.Background(), args, daemon)
	waitForRederivationDone(t, daemon)

	if got := daemon.config().ClaudeConfigDir; got != fromEnv {
		t.Errorf("ClaudeConfigDir = %q, want the .env value %q", got, fromEnv)
	}
	if _, set := os.LookupEnv("PRUEFER_RELOAD_DOTENV_PROBE"); set {
		t.Error("reload exported a .env variable into the process environment")
	}
	if os.Getenv(claudeprofile.BareEnv) != ambient {
		t.Error("reload changed the process's CLAUDE_CONFIG_DIR")
	}
}

// A reload that fails (here: a malformed YAML) leaves both the config and the
// .env data the next resolution uses untouched.
func TestHandleReload_FailedReloadKeepsProfile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	y1 := profileDir(t, "y1", "y1@example.com")
	profileEnv(t, "", "", map[string]string{"UNRELATED": "x"})
	keyPath := writeTestPrivateKey(t, dir)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("claude_config_dir: "+y1+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, _, daemon, _, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{{ID: 111, Account: "handarbeit"}})
	defer closeLog()
	captureLogf(t)
	before := daemon.config()

	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("CLAUDE_CONFIG_DIR=/elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("watched_repos: [unterminated"), 0o600); err != nil {
		t.Fatal(err)
	}
	handleReload(context.Background(), args, daemon)

	if got := daemon.config(); !reflect.DeepEqual(got, before) {
		t.Errorf("config changed by a failed reload")
	}
	if _, dotenv := profileSnapshotInputs(); dotenv["CLAUDE_CONFIG_DIR"] != "" {
		t.Errorf("failed reload left the new .env data in the snapshot: %v", dotenv)
	}
}

// A reviewed repo's .pruefer/config.yaml cannot choose the billing account: the
// key is operator-only, ignored with the existing warning, and the review
// environment is unchanged.
func TestRepoConfig_ClaudeConfigDirIsOperatorOnly(t *testing.T) {
	operatorDir := t.TempDir()
	profileEnv(t, "", "", nil)
	snapshot := captureLogf(t)

	client := newFakeReviewer()
	client.repoConfigData = []byte("claude_config_dir: /attacker/profile\ncadence: once\n")
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1", Title: "T", BaseRef: "main"}
	cfg := Config{ClaudeConfigDir: operatorDir}
	if out := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr, nil); !out.Reviewed {
		t.Fatalf("outcome = %+v, want Reviewed", out)
	}

	calls := claude.callsSnapshot()
	if len(calls) != 1 || calls[0].ClaudeConfigDir != operatorDir {
		t.Fatalf("request profile = %+v, want the operator's %q", calls, operatorDir)
	}
	env := reviewProcessEnv([]string{"PATH=/usr/bin"}, calls[0])
	if !containsEnv(env, claudeprofile.BareEnv+"="+operatorDir) || containsEnv(env, "/attacker/profile") {
		t.Errorf("review env = %v", env)
	}
	if joined := strings.Join(snapshot(), "\n"); !strings.Contains(joined, `key "claude_config_dir" is not repo-settable`) {
		t.Errorf("missing the operator-only-key warning:\n%s", joined)
	}
}

func containsEnv(env []string, needle string) bool {
	for _, kv := range env {
		if strings.Contains(kv, needle) {
			return true
		}
	}
	return false
}
