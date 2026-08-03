package pruefer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeYAMLConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writing config file: %v", err)
	}
	return path
}

func TestLoadConfig_DefaultsWhenNothingSet(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig([]string{"-config", filepath.Join(dir, "missing.yaml")})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PollInterval != DefaultPollInterval {
		t.Errorf("PollInterval = %v, want %v", cfg.PollInterval, DefaultPollInterval)
	}
	if cfg.Model != DefaultModel {
		t.Errorf("Model = %q, want %q", cfg.Model, DefaultModel)
	}
	if cfg.Effort != DefaultEffort {
		t.Errorf("Effort = %q, want %q", cfg.Effort, DefaultEffort)
	}
	if cfg.ConcurrencyCap != DefaultConcurrencyCap {
		t.Errorf("ConcurrencyCap = %d, want %d", cfg.ConcurrencyCap, DefaultConcurrencyCap)
	}
	if cfg.MaxDiffBytes != DefaultMaxDiffBytes {
		t.Errorf("MaxDiffBytes = %d, want %d", cfg.MaxDiffBytes, DefaultMaxDiffBytes)
	}
	if cfg.MaxWallTime != 0 {
		t.Errorf("MaxWallTime = %v, want 0 (uncapped by default)", cfg.MaxWallTime)
	}
	if cfg.AppPrivateKeyPath != DefaultPrivateKeyPath {
		t.Errorf("AppPrivateKeyPath = %q, want %q", cfg.AppPrivateKeyPath, DefaultPrivateKeyPath)
	}
	if cfg.AppStatePath != DefaultAppStatePath {
		t.Errorf("AppStatePath = %q, want %q", cfg.AppStatePath, DefaultAppStatePath)
	}
	if cfg.NoBrowser {
		t.Errorf("NoBrowser = true, want false (default)")
	}
	if len(cfg.WatchedRepos) != 0 {
		t.Errorf("WatchedRepos = %v, want empty", cfg.WatchedRepos)
	}
	if !cfg.TUI {
		t.Errorf("TUI = false, want true (default on)")
	}
}

func TestLoadConfig_TUIPrecedence(t *testing.T) {
	dir := t.TempDir()

	// YAML can disable it.
	path := writeYAMLConfig(t, dir, `tui: false`)
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.TUI {
		t.Errorf("TUI = true, want false (from YAML)")
	}

	// Env overrides YAML.
	t.Setenv("PRUEFER_TUI", "true")
	cfg, err = LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.TUI {
		t.Errorf("TUI = false, want true (env should override YAML)")
	}

	// -notui flag overrides env.
	cfg, err = LoadConfig([]string{"-config", path, "-notui"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.TUI {
		t.Errorf("TUI = true, want false (-notui flag should override env)")
	}
}

func TestLoadConfig_YAMLOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `
watched_repos:
  - handarbeit/fabrik
  - handarbeit/other
poll_interval_seconds: 60
model: opus
effort: high
concurrency_cap: 5
max_diff_bytes: 100000
excluded_authors:
  - dependabot[bot]
excluded_labels:
  - skip-review
`)
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PollInterval != 60*time.Second {
		t.Errorf("PollInterval = %v, want 60s", cfg.PollInterval)
	}
	if cfg.Model != "opus" {
		t.Errorf("Model = %q, want opus", cfg.Model)
	}
	if cfg.Effort != "high" {
		t.Errorf("Effort = %q, want high", cfg.Effort)
	}
	if cfg.ConcurrencyCap != 5 {
		t.Errorf("ConcurrencyCap = %d, want 5", cfg.ConcurrencyCap)
	}
	if cfg.MaxDiffBytes != 100000 {
		t.Errorf("MaxDiffBytes = %d, want 100000", cfg.MaxDiffBytes)
	}
	if len(cfg.WatchedRepos) != 2 || cfg.WatchedRepos[0] != "handarbeit/fabrik" {
		t.Errorf("WatchedRepos = %v", cfg.WatchedRepos)
	}
	if len(cfg.ExcludedAuthors) != 1 || cfg.ExcludedAuthors[0] != "dependabot[bot]" {
		t.Errorf("ExcludedAuthors = %v", cfg.ExcludedAuthors)
	}
	if len(cfg.ExcludedLabels) != 1 || cfg.ExcludedLabels[0] != "skip-review" {
		t.Errorf("ExcludedLabels = %v", cfg.ExcludedLabels)
	}
}

func TestLoadConfig_MaxWallTimePrecedence(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `max_wall_time_seconds: 600`)

	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MaxWallTime != 600*time.Second {
		t.Errorf("MaxWallTime = %v, want 600s (from YAML)", cfg.MaxWallTime)
	}

	t.Setenv("PRUEFER_MAX_WALL_TIME", "900")
	cfg, err = LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MaxWallTime != 900*time.Second {
		t.Errorf("MaxWallTime = %v, want 900s (env should override YAML)", cfg.MaxWallTime)
	}

	cfg, err = LoadConfig([]string{"-config", path, "-max-wall-time", "1200"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MaxWallTime != 1200*time.Second {
		t.Errorf("MaxWallTime = %v, want 1200s (flag should override env)", cfg.MaxWallTime)
	}
}

func TestLoadConfig_EnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `
model: opus
concurrency_cap: 5
`)
	t.Setenv("PRUEFER_MODEL", "haiku")
	t.Setenv("PRUEFER_CONCURRENCY", "7")

	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Model != "haiku" {
		t.Errorf("Model = %q, want haiku (env should override YAML)", cfg.Model)
	}
	if cfg.ConcurrencyCap != 7 {
		t.Errorf("ConcurrencyCap = %d, want 7 (env should override YAML)", cfg.ConcurrencyCap)
	}
}

func TestLoadConfig_FlagOverridesEnv(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `model: opus`)
	t.Setenv("PRUEFER_MODEL", "haiku")

	cfg, err := LoadConfig([]string{"-config", path, "-model", "sonnet"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Model != "sonnet" {
		t.Errorf("Model = %q, want sonnet (flag should override env)", cfg.Model)
	}
}

func TestLoadConfig_PrueferConfigEnvSelectsFile(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `model: opus`)
	t.Setenv("PRUEFER_CONFIG", path)

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Model != "opus" {
		t.Errorf("Model = %q, want opus (PRUEFER_CONFIG should select the file)", cfg.Model)
	}
}

func TestLoadConfig_ExplicitFlagBeatsPrueferConfigEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := writeYAMLConfig(t, dir, `model: opus`)
	flagPath := filepath.Join(dir, "flag-config.yaml")
	if err := os.WriteFile(flagPath, []byte(`model: haiku`), 0600); err != nil {
		t.Fatalf("writing flag config: %v", err)
	}
	t.Setenv("PRUEFER_CONFIG", envPath)

	cfg, err := LoadConfig([]string{"-config", flagPath})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Model != "haiku" {
		t.Errorf("Model = %q, want haiku (-config flag should win over PRUEFER_CONFIG)", cfg.Model)
	}
}

func TestLoadConfig_RequestChangesThresholdDefaultOff(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig([]string{"-config", filepath.Join(dir, "missing.yaml")})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RequestChangesThreshold != "" {
		t.Errorf("RequestChangesThreshold = %q, want empty (off by default)", cfg.RequestChangesThreshold)
	}
}

func TestLoadConfig_RequestChangesThresholdPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `request_changes_threshold: high`)

	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RequestChangesThreshold != SeverityHigh {
		t.Errorf("RequestChangesThreshold = %q, want high (from YAML)", cfg.RequestChangesThreshold)
	}

	t.Setenv("PRUEFER_REQUEST_CHANGES_THRESHOLD", "critical")
	cfg, err = LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RequestChangesThreshold != SeverityCritical {
		t.Errorf("RequestChangesThreshold = %q, want critical (env should override YAML)", cfg.RequestChangesThreshold)
	}

	cfg, err = LoadConfig([]string{"-config", path, "-request-changes-threshold", "medium"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RequestChangesThreshold != SeverityMedium {
		t.Errorf("RequestChangesThreshold = %q, want medium (flag should override env)", cfg.RequestChangesThreshold)
	}
}

func TestLoadConfig_RequestChangesThresholdRejectsUnrecognizedValue(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `request_changes_threshold: hgih`)

	if _, err := LoadConfig([]string{"-config", path}); err == nil {
		t.Fatal("LoadConfig: expected an error for an unrecognized request_changes_threshold value, got nil")
	}
}

func TestLoadConfig_AppStatePathAndNoBrowserPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `
github_app_state_path: /yaml/app-state.json
no_browser: true
`)

	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AppStatePath != "/yaml/app-state.json" {
		t.Errorf("AppStatePath = %q, want /yaml/app-state.json (from YAML)", cfg.AppStatePath)
	}
	if !cfg.NoBrowser {
		t.Errorf("NoBrowser = false, want true (from YAML)")
	}

	t.Setenv("PRUEFER_GITHUB_APP_STATE_PATH", "/env/app-state.json")
	t.Setenv("PRUEFER_NO_BROWSER", "false")
	cfg, err = LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AppStatePath != "/env/app-state.json" {
		t.Errorf("AppStatePath = %q, want /env/app-state.json (env should override YAML)", cfg.AppStatePath)
	}
	if cfg.NoBrowser {
		t.Errorf("NoBrowser = true, want false (env should override YAML)")
	}

	cfg, err = LoadConfig([]string{"-config", path, "-github-app-state-path", "/flag/app-state.json", "-no-browser"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AppStatePath != "/flag/app-state.json" {
		t.Errorf("AppStatePath = %q, want /flag/app-state.json (flag should override env)", cfg.AppStatePath)
	}
	if !cfg.NoBrowser {
		t.Errorf("NoBrowser = false, want true (flag should override env)")
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ,c ", []string{"a", "b", "c"}},
		{"a,,b", []string{"a", "b"}},
	}
	for _, tc := range cases {
		got := splitCSV(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitCSV(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}
