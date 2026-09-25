package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// isolateGitConfigEnv blanks the GIT_CONFIG_COUNT family for the test, so
// applyGitConfigEnv's os.Setenv/os.Unsetenv calls are undone by t.Setenv's
// cleanup rather than leaking into later tests.
func isolateGitConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
		"GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1", "GIT_CONFIG_KEY_2", "GIT_CONFIG_VALUE_2"} {
		t.Setenv(k, "")
	}
}

func TestGitConfigEnvEdit(t *testing.T) {
	ours := appGitCredentialHelper("/x/token")
	oldOurs := appGitCredentialHelper("/old/token")
	add := [][2]string{{appGitCredentialKey, ""}, {appGitCredentialKey, ours}}
	foreign := []string{"GIT_CONFIG_KEY_0=core.autocrlf", "GIT_CONFIG_VALUE_0=false"}

	tests := []struct {
		name        string
		environ     []string
		add         [][2]string
		wantChanged bool
		wantUnset   []string
		wantSet     []string
		wantErr     bool
	}{
		{name: "nothing to do", environ: []string{"HOME=/h"}, wantChanged: false},
		{name: "empty count treated as zero", environ: []string{"GIT_CONFIG_COUNT="}, wantChanged: false},
		{
			name: "inject into empty env", add: add, wantChanged: true,
			wantSet: []string{"GIT_CONFIG_KEY_0=" + appGitCredentialKey, "GIT_CONFIG_VALUE_0=",
				"GIT_CONFIG_KEY_1=" + appGitCredentialKey, "GIT_CONFIG_VALUE_1=" + ours, "GIT_CONFIG_COUNT=2"},
		},
		{
			name: "foreign entries preserved ahead of ours", environ: append([]string{"GIT_CONFIG_COUNT=1"}, foreign...),
			add: add, wantChanged: true,
			wantSet: []string{"GIT_CONFIG_KEY_0=core.autocrlf", "GIT_CONFIG_VALUE_0=false",
				"GIT_CONFIG_KEY_1=" + appGitCredentialKey, "GIT_CONFIG_VALUE_1=",
				"GIT_CONFIG_KEY_2=" + appGitCredentialKey, "GIT_CONFIG_VALUE_2=" + ours, "GIT_CONFIG_COUNT=3"},
		},
		{
			name: "previous exec's pair replaced, not stacked",
			environ: []string{"GIT_CONFIG_COUNT=2",
				"GIT_CONFIG_KEY_0=" + appGitCredentialKey, "GIT_CONFIG_VALUE_0=",
				"GIT_CONFIG_KEY_1=" + appGitCredentialKey, "GIT_CONFIG_VALUE_1=" + oldOurs},
			add: add, wantChanged: true,
			wantSet: []string{"GIT_CONFIG_KEY_0=" + appGitCredentialKey, "GIT_CONFIG_VALUE_0=",
				"GIT_CONFIG_KEY_1=" + appGitCredentialKey, "GIT_CONFIG_VALUE_1=" + ours, "GIT_CONFIG_COUNT=2"},
		},
		{
			name: "strip only, foreign entry after ours compacts down",
			environ: []string{"GIT_CONFIG_COUNT=3",
				"GIT_CONFIG_KEY_0=" + appGitCredentialKey, "GIT_CONFIG_VALUE_0=",
				"GIT_CONFIG_KEY_1=" + appGitCredentialKey, "GIT_CONFIG_VALUE_1=" + oldOurs,
				"GIT_CONFIG_KEY_2=core.autocrlf", "GIT_CONFIG_VALUE_2=false"},
			wantChanged: true,
			wantUnset:   []string{"GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1", "GIT_CONFIG_KEY_2", "GIT_CONFIG_VALUE_2"},
			wantSet:     []string{"GIT_CONFIG_KEY_0=core.autocrlf", "GIT_CONFIG_VALUE_0=false", "GIT_CONFIG_COUNT=1"},
		},
		{
			name: "strip only, nothing left removes the count",
			environ: []string{"GIT_CONFIG_COUNT=2",
				"GIT_CONFIG_KEY_0=" + appGitCredentialKey, "GIT_CONFIG_VALUE_0=",
				"GIT_CONFIG_KEY_1=" + appGitCredentialKey, "GIT_CONFIG_VALUE_1=" + oldOurs},
			wantChanged: true,
			wantUnset: []string{"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1",
				"GIT_CONFIG_COUNT"},
		},
		{
			name: "a foreign empty reset for the same key is kept when not followed by ours",
			environ: []string{"GIT_CONFIG_COUNT=1",
				"GIT_CONFIG_KEY_0=" + appGitCredentialKey, "GIT_CONFIG_VALUE_0="},
			wantChanged: false,
		},
		{name: "invalid count refused", environ: []string{"GIT_CONFIG_COUNT=two"}, add: add, wantErr: true},
		{name: "negative count refused", environ: []string{"GIT_CONFIG_COUNT=-1"}, add: add, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unset, set, changed, err := gitConfigEnvEdit(tt.environ, tt.add)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if changed != tt.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tt.wantChanged)
			}
			if !reflect.DeepEqual(unset, tt.wantUnset) {
				t.Errorf("unset = %q, want %q", unset, tt.wantUnset)
			}
			if !reflect.DeepEqual(set, tt.wantSet) {
				t.Errorf("set = %q, want %q", set, tt.wantSet)
			}
		})
	}
}

// credentialFill runs `git credential fill` for github.com under env and
// returns the password line's value ("" when git produced none). Failure
// messages below never print it: on a misconfigured host it can be a real
// credential from the ambient helper chain.
func credentialFill(t *testing.T, env []string) string {
	t.Helper()
	cmd := exec.Command("git", "credential", "fill")
	cmd.Dir = t.TempDir() // outside any repo: a checkout's local config may carry its own helper
	cmd.Env = env
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=handarbeit/fabrik.git\n\n")
	out, _ := cmd.Output() // no answer → non-zero exit with GIT_TERMINAL_PROMPT=0; the password check covers it
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "password="); ok {
			return v
		}
	}
	return ""
}

// TestAppGitCredentialHelper_ServesTokenFileOverAmbientHelper drives real
// git: an ambient github.com helper in the (isolated) global config must be
// overridden, the token must be read from the file at lookup time (so a
// rotation is picked up without re-exporting anything), and a missing file
// must produce no credential rather than falling back to the ambient one.
func TestAppGitCredentialHelper_ServesTokenFileOverAmbientHelper(t *testing.T) {
	skipIfNoGit(t)
	isolateGitConfig(t, "[credential \"https://github.com\"]\n\thelper = \"!f() { echo username=ambient; echo password=ambient-token; }; f\"\n")

	dir := filepath.Join(t.TempDir(), "it's a dir") // space and quote exercise shellSingleQuote
	tokenPath := filepath.Join(dir, "token")
	base := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	if got := credentialFill(t, base); got != "ambient-token" {
		t.Fatalf("precondition: ambient helper did not answer without injection (value withheld; empty=%v)", got == "")
	}

	_, set, _, err := gitConfigEnvEdit(nil, [][2]string{{appGitCredentialKey, ""}, {appGitCredentialKey, appGitCredentialHelper(tokenPath)}})
	if err != nil {
		t.Fatal(err)
	}
	env := append(append([]string{}, base...), set...)

	if err := writeAppGitToken(tokenPath, "ghs_first"); err != nil {
		t.Fatal(err)
	}
	if got := credentialFill(t, env); got != "ghs_first" {
		t.Fatalf("password is not the token file's value (withheld; empty=%v)", got == "")
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}

	if err := writeAppGitToken(tokenPath, "ghs_rotated"); err != nil {
		t.Fatal(err)
	}
	if got := credentialFill(t, env); got != "ghs_rotated" {
		t.Fatalf("after rotation, password is not the rotated token (withheld; empty=%v)", got == "")
	}

	if err := os.Remove(tokenPath); err != nil {
		t.Fatal(err)
	}
	if got := credentialFill(t, env); got != "" {
		t.Fatal("missing token file: git still produced a password (withheld); want none, never the ambient helper's")
	}
}

// TestRun_GitHubAppAuth_HTTPS_InjectsCredentialHelper replaces #1756's
// startup refusal: App auth + HTTPS git now starts, with the helper
// injected into the process environment and the token file written.
func TestRun_GitHubAppAuth_HTTPS_InjectsCredentialHelper(t *testing.T) {
	isolateGitConfig(t, "")
	isolateGitConfigEnv(t)
	eng := newAppAuthTestEngine(t)
	eng.hostClient = gh.NewClient("ghs_installation_token")

	runEngineUntilShutdownWith(t, eng, func() {
		// ReadyCh closes before Run()'s git preflight; wait for it.
		waitFor(t, func() bool { _, err := os.Stat(AppGitTokenPath(eng.fabrikDir)); return err == nil })
		if got := os.Getenv("GIT_CONFIG_COUNT"); got != "2" {
			t.Errorf("GIT_CONFIG_COUNT = %q, want 2", got)
		}
		if got := os.Getenv("GIT_CONFIG_KEY_1"); got != appGitCredentialKey {
			t.Errorf("GIT_CONFIG_KEY_1 = %q, want %q", got, appGitCredentialKey)
		}
		if got := os.Getenv("GIT_CONFIG_VALUE_1"); !strings.Contains(got, appGitCredentialMarker) {
			t.Errorf("GIT_CONFIG_VALUE_1 = %q, want the app git credential helper", got)
		}
		data, err := os.ReadFile(AppGitTokenPath(eng.fabrikDir))
		if err != nil {
			t.Fatalf("reading token file: %v", err)
		}
		if string(data) != "ghs_installation_token" {
			t.Errorf("token file = %q, want the installation token", data)
		}
	})
}

// TestRun_GitHubAppAuth_GitSSH_StripsStaleHelper: a previous exec (SIGHUP
// or self-upgrade, which keep the environment) left the helper behind;
// with git_ssh the new exec must strip it rather than keep serving the
// installation token.
func TestRun_GitHubAppAuth_GitSSH_StripsStaleHelper(t *testing.T) {
	isolateGitConfig(t, "")
	isolateGitConfigEnv(t)
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", appGitCredentialKey)
	t.Setenv("GIT_CONFIG_KEY_1", appGitCredentialKey)
	t.Setenv("GIT_CONFIG_VALUE_1", appGitCredentialHelper("/stale/token"))
	eng := newAppAuthTestEngine(t)
	eng.hostClient = gh.NewClient("ghs_installation_token")
	eng.cfg.GitSSH = true

	runEngineUntilShutdownWith(t, eng, func() {
		// ReadyCh closes before Run()'s git preflight; wait for it.
		waitFor(t, func() bool { _, ok := os.LookupEnv("GIT_CONFIG_COUNT"); return !ok })
		if _, ok := os.LookupEnv("GIT_CONFIG_COUNT"); ok {
			t.Errorf("GIT_CONFIG_COUNT still set (%q); stale helper not stripped", os.Getenv("GIT_CONFIG_COUNT"))
		}
		if _, err := os.Stat(AppGitTokenPath(eng.fabrikDir)); !os.IsNotExist(err) {
			t.Errorf("token file written under git_ssh (stat err %v)", err)
		}
	})
}

// waitFor polls cond for up to 5s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return // the caller's assertions report what is missing
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// httpsGitAppPerms is the grant an installation needs when git runs over
// HTTPS: the base set plus contents:write.
func httpsGitAppPerms() map[string]string {
	p := RequiredGitHubAppPermissions(false)
	p["contents"] = "write"
	return p
}

func TestSetUpGitHubAppAuth_HTTPSGitRequiresContentsWrite(t *testing.T) {
	tests := []struct {
		name    string
		gitSSH  bool
		rewrite string
		perms   map[string]string
		wantErr bool
	}{
		{name: "https, contents:read refused", perms: RequiredGitHubAppPermissions(false), wantErr: true},
		{name: "https, contents:write allowed", perms: httpsGitAppPerms()},
		{name: "git_ssh, contents:read allowed", gitSSH: true, perms: RequiredGitHubAppPermissions(false)},
		{name: "ssh rewrite, contents:read allowed", rewrite: sshRewriteGitConfig, perms: RequiredGitHubAppPermissions(false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateGitConfig(t, tt.rewrite)
			dir := t.TempDir()
			keyPath := writeEngineTestAppKey(t, dir)
			srv := newFakeGitHubAppServer(t, 999, "handarbeit", "organization", tt.perms)
			cfg := Config{
				Owner: "handarbeit", Repo: "fabrik", GitSSH: tt.gitSSH,
				GitHubAppID: 42, GitHubAppPrivateKeyPath: keyPath, GitHubAppInstallationID: 999,
			}
			_, _, err := setUpGitHubAppAuth(context.Background(), cfg, dir, srv.URL)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a grant-shortfall error")
			}
			for _, want := range []string{"contents", "git_ssh"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err.Error(), want)
				}
			}
		})
	}
}
