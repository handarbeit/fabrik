package pruefer

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadConfig_RepoPluginSettings(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig([]string{"-config", filepath.Join(dir, "missing.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RepoPlugins || len(cfg.RepoPluginMarketplaces) != 0 {
		t.Errorf("defaults = %v %v, want off and empty", cfg.RepoPlugins, cfg.RepoPluginMarketplaces)
	}

	path := writeYAMLConfig(t, dir, "repo_plugins: true\nrepo_plugin_marketplaces:\n  - acme/tools\n  - org/*\n")
	cfg, err = LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RepoPlugins || !slices.Equal(cfg.RepoPluginMarketplaces, []string{"acme/tools", "org/*"}) {
		t.Errorf("yaml = %v %v", cfg.RepoPlugins, cfg.RepoPluginMarketplaces)
	}

	t.Setenv("PRUEFER_REPO_PLUGINS", "false")
	t.Setenv("PRUEFER_REPO_PLUGIN_MARKETPLACES", "a/b, c/d")
	cfg, err = LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RepoPlugins || !slices.Equal(cfg.RepoPluginMarketplaces, []string{"a/b", "c/d"}) {
		t.Errorf("env = %v %v, want env to override yaml", cfg.RepoPlugins, cfg.RepoPluginMarketplaces)
	}

	cfg, err = LoadConfig([]string{"-config", path, "-repo-plugins", "-repo-plugin-marketplaces", "x/y"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RepoPlugins || !slices.Equal(cfg.RepoPluginMarketplaces, []string{"x/y"}) {
		t.Errorf("flags = %v %v, want flags to override env", cfg.RepoPlugins, cfg.RepoPluginMarketplaces)
	}
}

// An unparseable PRUEFER_REPO_PLUGINS must never switch code loading on.
func TestLoadConfig_UnparseableRepoPluginsEnvFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, "repo_plugins: true\n")
	t.Setenv("PRUEFER_REPO_PLUGINS", "ture")
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RepoPlugins {
		t.Error("an unparseable PRUEFER_REPO_PLUGINS must fail closed to off")
	}
}

func TestReload_RepoPluginSettingsAreLive(t *testing.T) {
	old := Config{}
	cand := Config{RepoPlugins: true, RepoPluginMarketplaces: []string{"a/b"}}
	merged, diff := applyConfigReload(old, cand)
	if !merged.RepoPlugins || !slices.Equal(merged.RepoPluginMarketplaces, cand.RepoPluginMarketplaces) {
		t.Errorf("merged = %+v", merged)
	}
	got := map[string]bool{}
	for _, c := range diff.FieldsChanged {
		got[c.Field] = true
	}
	for _, f := range []string{"RepoPlugins", "RepoPluginMarketplaces"} {
		if !got[f] {
			t.Errorf("%s not applied live", f)
		}
	}
	if len(diff.RestartOnlyChanged) != 0 {
		t.Errorf("restart-only changes: %+v", diff.RestartOnlyChanged)
	}
	// Turning it off takes effect on the next reload.
	merged, _ = applyConfigReload(merged, Config{})
	if merged.RepoPlugins {
		t.Error("turning repo_plugins off must apply live")
	}
}
