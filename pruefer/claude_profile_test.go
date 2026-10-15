package pruefer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/claudeprofile"
)

// profileEnv isolates a test from the host's Claude profile variables and from
// any snapshot a previous test captured. It must not be combined with
// t.Parallel: the snapshot is process-wide.
func profileEnv(t *testing.T, ambient, shell string, dotenv map[string]string) {
	t.Helper()
	t.Setenv(claudeprofile.BareEnv, ambient)
	t.Setenv(PrueferClaudeConfigDirEnv, shell)
	resetProfileSnapshot()
	t.Cleanup(resetProfileSnapshot)
	captureProfileSnapshot(ambient, dotenv)
}

// profileDir makes a profile directory logged in as email.
func profileDir(t *testing.T, name, email string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"oauthAccount":{"emailAddress":"` + email + `"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func loadProfileConfig(t *testing.T, yaml string, extra ...string) (Config, claudeprofile.Resolution) {
	t.Helper()
	path := writeYAMLConfig(t, t.TempDir(), yaml)
	cfg, res, err := loadConfig(append([]string{"-config", path}, extra...))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return cfg, res
}

// Acceptance: ambient = A, .env = B, no YAML key resolves to B, and the review
// process environment holds exactly CLAUDE_CONFIG_DIR=B.
func TestClaudeProfile_DotenvBeatsAmbient_ReviewEnvHoldsExactlyB(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	profileEnv(t, a, "", map[string]string{claudeprofile.BareEnv: b})

	cfg, res := loadProfileConfig(t, "model: sonnet\n")
	if cfg.ClaudeConfigDir != b {
		t.Fatalf("ClaudeConfigDir = %q, want %q", cfg.ClaudeConfigDir, b)
	}
	if !res.Disagree() {
		t.Error("ambient A vs .env B should disagree")
	}

	// The daemon's own environment still holds A.
	daemon := []string{"PATH=/usr/bin", "HOME=/h", claudeprofile.BareEnv + "=" + a}
	env := reviewProcessEnv(daemon, ReviewRequest{ClaudeConfigDir: cfg.ClaudeConfigDir})
	var got []string
	for _, kv := range env {
		if strings.HasPrefix(kv, claudeprofile.BareEnv) {
			got = append(got, kv)
		}
	}
	if len(got) != 1 || got[0] != claudeprofile.BareEnv+"="+b {
		t.Errorf("review env CLAUDE_CONFIG_DIR entries = %v, want exactly [CLAUDE_CONFIG_DIR=%s]", got, b)
	}
}

// Ambient, .env and YAML all different: the documented order holds, and the
// flag and PRUEFER_CLAUDE_CONFIG_DIR each beat YAML.
func TestClaudeProfile_PrecedenceOrder(t *testing.T) {
	flagDir, shellDir, envDir, bareDir, yamlDir, ambientDir :=
		t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	yaml := "claude_config_dir: " + yamlDir + "\n"
	dotenv := map[string]string{PrueferClaudeConfigDirEnv: envDir, claudeprofile.BareEnv: bareDir}

	steps := []struct {
		name   string
		setup  func()
		args   []string
		yaml   string
		want   string
		winner claudeprofile.Layer
	}{
		{"flag", func() { profileEnv(t, ambientDir, shellDir, dotenv) }, []string{"-claude-config-dir", flagDir}, yaml, flagDir, claudeprofile.LayerFlag},
		{"shell env", func() { profileEnv(t, ambientDir, shellDir, dotenv) }, nil, yaml, shellDir, claudeprofile.LayerShellEnv},
		{".env PRUEFER_ name", func() { profileEnv(t, ambientDir, "", dotenv) }, nil, yaml, envDir, claudeprofile.LayerDotenv},
		{".env bare name", func() { profileEnv(t, ambientDir, "", map[string]string{claudeprofile.BareEnv: bareDir}) }, nil, yaml, bareDir, claudeprofile.LayerDotenv},
		{"yaml beats ambient", func() { profileEnv(t, ambientDir, "", nil) }, nil, yaml, yamlDir, claudeprofile.LayerYAML},
		{"ambient", func() { profileEnv(t, ambientDir, "", nil) }, nil, "model: sonnet\n", ambientDir, claudeprofile.LayerAmbient},
		{"unset", func() { profileEnv(t, "", "", nil) }, nil, "model: sonnet\n", "", ""},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			st.setup()
			cfg, res := loadProfileConfig(t, st.yaml, st.args...)
			if cfg.ClaudeConfigDir != st.want || res.Winner != st.winner {
				t.Errorf("got (%q, %q), want (%q, %q)", cfg.ClaudeConfigDir, res.Winner, st.want, st.winner)
			}
		})
	}
}

// A relative value is anchored at the daemon's directory: reviews run in a temp
// clone, where the same relative path would name a different directory.
func TestClaudeProfile_RelativePathIsAnchored(t *testing.T) {
	profileEnv(t, "", "", nil)
	cfg, _ := loadProfileConfig(t, "claude_config_dir: .claude-work\n")
	if !filepath.IsAbs(cfg.ClaudeConfigDir) || filepath.Base(cfg.ClaudeConfigDir) != ".claude-work" {
		t.Errorf("ClaudeConfigDir = %q, want an absolute path ending in .claude-work", cfg.ClaudeConfigDir)
	}
}

func TestClaudeProfile_NamesArePrueferNotFabrik(t *testing.T) {
	a := t.TempDir()
	profileEnv(t, a, "", map[string]string{claudeprofile.BareEnv: t.TempDir()})
	_, res := loadProfileConfig(t, "claude_config_dir: "+t.TempDir()+"\n", "-claude-config-dir", t.TempDir())
	line := formatClaudeProfileDisagreement(res)
	for _, want := range []string{"-claude-config-dir", ".env CLAUDE_CONFIG_DIR", ".pruefer/config.yaml claude_config_dir", "CLAUDE_CONFIG_DIR (environment)"} {
		if !strings.Contains(line, want) {
			t.Errorf("disagreement line missing %q: %s", want, line)
		}
	}
	for _, bad := range []string{"FABRIK", ".fabrik"} {
		if strings.Contains(line, bad) {
			t.Errorf("disagreement line leaks %q: %s", bad, line)
		}
	}
}

func TestFormatClaudeProfileLine(t *testing.T) {
	dir := profileDir(t, "work", "me@example.com")
	profileEnv(t, "", "", nil)
	_, res := loadProfileConfig(t, "claude_config_dir: "+dir+"\n")
	line := formatClaudeProfileLine(res)
	for _, want := range []string{dir, "me@example.com", ".pruefer/config.yaml claude_config_dir"} {
		if !strings.Contains(line, want) {
			t.Errorf("line missing %q: %s", want, line)
		}
	}

	noEmail := t.TempDir()
	_, res = loadProfileConfig(t, "claude_config_dir: "+noEmail+"\n")
	if line := formatClaudeProfileLine(res); !strings.Contains(line, noEmail) || strings.Contains(line, "()") {
		t.Errorf("unreadable-email line = %s", line)
	}

	// Unlike the engine, Pruefer says so when nothing resolved.
	_, res = loadProfileConfig(t, "model: sonnet\n")
	if line := formatClaudeProfileLine(res); !strings.Contains(line, "default profile") {
		t.Errorf("unset line = %s", line)
	}
}

func TestReportClaudeProfile_StartupLineAndDisagreementWarning(t *testing.T) {
	a := profileDir(t, "a", "a@example.com")
	b := profileDir(t, "b", "b@example.com")
	profileEnv(t, a, "", map[string]string{claudeprofile.BareEnv: b})
	snapshot := captureLogf(t)

	_, res := loadProfileConfig(t, "model: sonnet\n")
	reportClaudeProfile("startup", res, true)

	lines := snapshot()
	if len(lines) != 2 {
		t.Fatalf("logged %d lines, want 2: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], b) || !strings.Contains(lines[0], "b@example.com") {
		t.Errorf("startup line = %q", lines[0])
	}
	w := lines[1]
	for _, want := range []string{"warning", "disagree", a, b, "a@example.com", "b@example.com", ".env CLAUDE_CONFIG_DIR wins"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning missing %q: %s", want, w)
		}
	}
}

func TestReportClaudeProfile_NoDisagreementIsQuiet(t *testing.T) {
	d := t.TempDir()
	profileEnv(t, d, "", nil)
	snapshot := captureLogf(t)
	_, res := loadProfileConfig(t, "claude_config_dir: "+d+"\n")
	reportClaudeProfile("reload", res, false)
	if lines := snapshot(); len(lines) != 0 {
		t.Errorf("logged %q, want nothing", lines)
	}
}

// The ambient value is frozen at capture time; a later change to the process
// environment is not re-read, while the shell variable (which cannot change in
// a running process) is.
func TestProfileSnapshot_AmbientIsFrozen(t *testing.T) {
	a, later := t.TempDir(), t.TempDir()
	profileEnv(t, a, "", nil)
	t.Setenv(claudeprofile.BareEnv, later)
	cfg, _ := loadProfileConfig(t, "model: sonnet\n")
	if cfg.ClaudeConfigDir != a {
		t.Errorf("ClaudeConfigDir = %q, want the frozen ambient %q", cfg.ClaudeConfigDir, a)
	}
}

// Without a captured snapshot (LoadConfig called directly) the ambient value is
// read live and there is no .env data.
func TestProfileSnapshot_UncapturedReadsAmbientLive(t *testing.T) {
	resetProfileSnapshot()
	t.Cleanup(resetProfileSnapshot)
	d := t.TempDir()
	t.Setenv(claudeprofile.BareEnv, d)
	t.Setenv(PrueferClaudeConfigDirEnv, "")
	cfg, _ := loadProfileConfig(t, "model: sonnet\n")
	if cfg.ClaudeConfigDir != d {
		t.Errorf("ClaudeConfigDir = %q, want %q", cfg.ClaudeConfigDir, d)
	}
}
