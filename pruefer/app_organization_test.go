package pruefer

import (
	"strings"
	"testing"
)

// TestLoadConfig_AppOrganizationOmitted: unset keeps today's behavior — the
// manifest flow creates a user-owned App.
func TestLoadConfig_AppOrganizationOmitted(t *testing.T) {
	path := writeYAMLConfig(t, t.TempDir(), `github_app_id: 123`)
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AppOrganization != "" {
		t.Errorf("AppOrganization = %q, want empty when omitted", cfg.AppOrganization)
	}
}

// TestLoadConfig_AppOrganizationSet: the key is read and trimmed.
func TestLoadConfig_AppOrganizationSet(t *testing.T) {
	path := writeYAMLConfig(t, t.TempDir(), "github_app_organization: \"  shadoworg  \"\n")
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AppOrganization != "shadoworg" {
		t.Errorf("AppOrganization = %q, want %q", cfg.AppOrganization, "shadoworg")
	}
}

// TestLoadConfig_AppOrganizationRejectsInvalid: a malformed value would
// otherwise surface only mid-setup, as a GitHub 404 in the browser. It must
// fail at config load, naming the key.
func TestLoadConfig_AppOrganizationRejectsInvalid(t *testing.T) {
	for _, bad := range []string{
		"   ",
		"-leading",
		"trailing-",
		"double--hyphen",
		"has/slash",
		"has space",
		"under_score",
		strings.Repeat("a", 40),
	} {
		path := writeYAMLConfig(t, t.TempDir(), "github_app_organization: \""+bad+"\"\n")
		_, err := LoadConfig([]string{"-config", path})
		if err == nil {
			t.Errorf("github_app_organization %q: expected a config error, got none", bad)
			continue
		}
		if !strings.Contains(err.Error(), "github_app_organization") {
			t.Errorf("github_app_organization %q: error does not name the key: %v", bad, err)
		}
	}
}

// TestValidateAppOrganization_AcceptsRealNames covers the three accounts this
// change exists to serve, plus the length boundary.
func TestValidateAppOrganization_AcceptsRealNames(t *testing.T) {
	for _, ok := range []string{"handarbeit", "shadoworg", "liminisapp", "a", "a-b", strings.Repeat("a", 39)} {
		if _, err := validateAppOrganization(ok); err != nil {
			t.Errorf("validateAppOrganization(%q) = %v, want nil", ok, err)
		}
	}
}

// TestReconcileOptions_ForwardsAppOrganization is the regression test for the
// actual defect: githubauth.Options.AppOrganization existed and was forwarded
// to both manifest-flow sites, but Pruefer never set it, so an org-owned App
// could not be created. A config-parsing test alone would pass even with the
// forwarding missing — this asserts the mapping itself.
func TestReconcileOptions_ForwardsAppOrganization(t *testing.T) {
	opts := reconcileOptions(Config{AppOrganization: "shadoworg"})
	if opts.AppOrganization != "shadoworg" {
		t.Errorf("githubauth.Options.AppOrganization = %q, want %q", opts.AppOrganization, "shadoworg")
	}
}

// TestReconcileOptions_ForwardsSiblingFields guards the extraction: every
// field the call site passed before must still be passed.
func TestReconcileOptions_ForwardsSiblingFields(t *testing.T) {
	const id = int64(4408765)
	cfg := Config{
		AppID: id, AppName: "n", AppHomepageURL: "https://h",
		AppPrivateKeyPath: "k.pem", AppStatePath: "s.json",
		WatchedRepos: []string{"o/r"}, ServedAccounts: []string{"o"}, NoBrowser: true,
	}
	opts := reconcileOptions(cfg)
	if opts.AppID != id || opts.AppName != "n" || opts.AppHomepageURL != "https://h" ||
		opts.AppPrivateKeyPath != "k.pem" || opts.AppStatePath != "s.json" || !opts.NoBrowser ||
		len(opts.WatchedRepos) != 1 || len(opts.ServedAccounts) != 1 {
		t.Errorf("reconcileOptions dropped a field: %+v", opts)
	}
	if opts.RequiredPermissions == nil || opts.Logf == nil {
		t.Error("reconcileOptions dropped RequiredPermissions or Logf")
	}
}
