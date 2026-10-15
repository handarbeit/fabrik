package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/handarbeit/fabrik/config"
	"github.com/handarbeit/fabrik/internal/claudeprofile"
)

// claudeProfileExecute runs Execute in a fresh project dir with the given
// ambient, shell-FABRIK, .env body, YAML body and extra args, and returns the
// resolution it recorded.
func claudeProfileExecute(t *testing.T, ambient, shellFabrik, dotenv, yaml string, extra ...string) claudeprofile.Resolution {
	t.Helper()
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	if dotenv != "" {
		os.WriteFile(filepath.Join(dir, ".env"), []byte(dotenv), 0600)
	}
	if yaml != "" {
		os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
		os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte(yaml), 0644)
	}
	resetFlags()
	t.Cleanup(claudeprofile.Reset)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv(claudeprofile.BareEnv, ambient)
	t.Setenv(claudeprofile.DeliberateEnv, shellFabrik)
	os.Args = append([]string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}, extra...)
	executeWithConfigHook(t)
	return claudeprofile.Current()
}

func TestExecute_ClaudeProfile_DotenvBeatsAmbient(t *testing.T) {
	r := claudeProfileExecute(t, "/a", "", "CLAUDE_CONFIG_DIR=/b\n", "")
	if r.Dir != "/b" || r.Winner != claudeprofile.LayerDotenv || !r.Disagree() {
		t.Fatalf("got %+v", r)
	}
}

func TestExecute_ClaudeProfile_Layers(t *testing.T) {
	if r := claudeProfileExecute(t, "/a", "", "", "claude_config_dir: /c\n"); r.Dir != "/c" {
		t.Errorf("yaml over ambient: %q", r.Dir)
	}
	if r := claudeProfileExecute(t, "/a", "/d", "", "claude_config_dir: /c\n"); r.Dir != "/d" {
		t.Errorf("FABRIK_ env over yaml: %q", r.Dir)
	}
	if r := claudeProfileExecute(t, "/a", "/d", "FABRIK_CLAUDE_CONFIG_DIR=/e\n", "claude_config_dir: /c\n", "--claude-config-dir", "/f"); r.Dir != "/f" || r.Winner != claudeprofile.LayerFlag {
		t.Errorf("flag over all: %+v", r)
	}
	if r := claudeProfileExecute(t, "", "", "CLAUDE_CONFIG_DIR=/g\nFABRIK_CLAUDE_CONFIG_DIR=/h\n", ""); r.Dir != "/h" || !r.Disagree() {
		t.Errorf(".env FABRIK_ over bare: %+v", r)
	}
}

func TestExecute_ClaudeProfile_AmbientOnly(t *testing.T) {
	r := claudeProfileExecute(t, "/a", "", "", "")
	if r.Dir != "/a" || r.Winner != claudeprofile.LayerAmbient || r.Disagree() {
		t.Fatalf("got %+v", r)
	}
}

// SC-003: a re-exec'd process receives os.Environ() of the first one (that is
// exactly what performSighupRestart and selfupgrade pass). Run Execute twice in
// one process, with no environment reset in between, and require the same
// result — and that .env's value never became ambient.
func TestExecute_ClaudeProfile_ProvenanceSurvivesReexec(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("CLAUDE_CONFIG_DIR=/b\nFABRIK_CLAUDE_CONFIG_DIR=/bf\nFABRIK_UNRELATED_FOR_TEST=1\n"), 0600)
	t.Cleanup(claudeprofile.Reset)
	t.Cleanup(func() { os.Unsetenv("FABRIK_UNRELATED_FOR_TEST") })
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv(claudeprofile.BareEnv, "/a")
	t.Setenv(claudeprofile.DeliberateEnv, "")

	run := func() claudeprofile.Resolution {
		resetFlags()
		os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
		executeWithConfigHook(t)
		return claudeprofile.Current()
	}
	first := run()
	if got := os.Getenv(claudeprofile.BareEnv); got != "/a" {
		t.Fatalf("CLAUDE_CONFIG_DIR in env after start = %q, want the untouched ambient /a", got)
	}
	if got := os.Getenv(claudeprofile.DeliberateEnv); got != "" {
		t.Fatalf("FABRIK_CLAUDE_CONFIG_DIR leaked into env: %q", got)
	}
	if os.Getenv("FABRIK_UNRELATED_FOR_TEST") != "1" {
		t.Error("other .env keys must still load")
	}
	second := run()
	if first.Dir != "/bf" || second.Dir != first.Dir || second.Winner != first.Winner || second.Fingerprint() != first.Fingerprint() {
		t.Errorf("re-exec changed the outcome:\nfirst  %+v\nsecond %+v", first, second)
	}
	for _, s := range second.Sources {
		if s.Layer == claudeprofile.LayerAmbient && s.Dir != "/a" {
			t.Errorf("ambient source reports %q, want /a", s.Dir)
		}
	}
}

// SC-006 (cmd half): the footer account is derived from the same resolution.
func TestBuildProjectInfo_ClaudeAccountFromResolution(t *testing.T) {
	prof := t.TempDir()
	os.WriteFile(filepath.Join(prof, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"foot@example.com"}}`), 0600)
	t.Setenv(claudeprofile.BareEnv, "/some/other/ambient")
	t.Cleanup(claudeprofile.Reset)
	claudeprofile.Set(claudeprofile.Resolve(claudeprofile.Inputs{YAML: prof}))
	info := buildProjectInfo(&Config{}, config.ProjectConfig{})
	if info.ClaudeAccount != "foot@example.com" {
		t.Errorf("ClaudeAccount = %q", info.ClaudeAccount)
	}
}

// `fabrik watch` records the same resolution as the daemon, so its inline
// resume cannot open claude under a stray exported account (#1807): ambient =
// /a, .env = /b resolves to /b, and the launch environment carries it.
func TestLoadWatchConfig_ResolvesClaudeProfile(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("CLAUDE_CONFIG_DIR=/b\n"), 0600)
	t.Cleanup(claudeprofile.Reset)
	t.Setenv(claudeprofile.BareEnv, "/a")
	t.Setenv(claudeprofile.DeliberateEnv, "")

	loadWatchConfig()

	r := claudeprofile.Current()
	if r.Dir != "/b" || r.Winner != claudeprofile.LayerDotenv {
		t.Fatalf("got %+v", r)
	}
	env := claudeprofile.ApplyToEnv([]string{"CLAUDE_CONFIG_DIR=/a"})
	if len(env) != 1 || env[0] != "CLAUDE_CONFIG_DIR=/b" {
		t.Fatalf("launch env = %v", env)
	}
	if got := os.Getenv(claudeprofile.BareEnv); got != "/a" {
		t.Errorf(".env value leaked into the process environment: %q", got)
	}
}
