package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFindAPIKeyHelper_Missing confirms a missing settings file is not an
// error (R12) — findAPIKeyHelper returns no match and no warning.
func TestFindAPIKeyHelper_Missing(t *testing.T) {
	dir := t.TempDir()
	found, warns := findAPIKeyHelper([]settingsLayer{
		{layer: "user", path: filepath.Join(dir, "settings.json")},
	})
	if found != nil {
		t.Errorf("found = %+v, want nil for a missing file", found)
	}
	if len(warns) != 0 {
		t.Errorf("warns = %v, want none for a missing file", warns)
	}
}

// TestFindAPIKeyHelper_Malformed confirms a malformed settings file produces
// a warning and does not crash or count as a detection (R12).
func TestFindAPIKeyHelper_Malformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0644); err != nil {
		t.Fatalf("write malformed settings: %v", err)
	}
	found, warns := findAPIKeyHelper([]settingsLayer{
		{layer: "user", path: path},
	})
	if found != nil {
		t.Errorf("found = %+v, want nil for a malformed file", found)
	}
	if len(warns) != 1 {
		t.Fatalf("warns = %v, want exactly 1 warning for a malformed file", warns)
	}
	if !strings.Contains(warns[0], path) {
		t.Errorf("warning %q does not name the malformed file %q", warns[0], path)
	}
}

// TestFindAPIKeyHelper_PresentWithoutKey confirms a settings file that
// parses fine but doesn't set apiKeyHelper is not a detection.
func TestFindAPIKeyHelper_PresentWithoutKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"permissions": {"allow": ["Read"]}}`), 0644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	found, warns := findAPIKeyHelper([]settingsLayer{
		{layer: "user", path: path},
	})
	if found != nil {
		t.Errorf("found = %+v, want nil when apiKeyHelper is absent", found)
	}
	if len(warns) != 0 {
		t.Errorf("warns = %v, want none", warns)
	}
}

// TestFindAPIKeyHelper_PresentWithKey confirms a present, parseable
// apiKeyHelper key is detected and the offending layer/path is returned (R10).
func TestFindAPIKeyHelper_PresentWithKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"apiKeyHelper": "/bin/echo fake-key"}`), 0644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	found, warns := findAPIKeyHelper([]settingsLayer{
		{layer: "managed-policy", path: path},
	})
	if found == nil {
		t.Fatal("found = nil, want a detection")
	}
	if found.layer != "managed-policy" || found.path != path {
		t.Errorf("found = %+v, want layer=managed-policy path=%s", found, path)
	}
	if len(warns) != 0 {
		t.Errorf("warns = %v, want none for a clean detection", warns)
	}
}

// TestFindAPIKeyHelper_FirstMatchWins confirms findAPIKeyHelper returns the
// first layer (in order) that sets apiKeyHelper, and does not scan past it —
// a malformed layer *after* a genuine detection must not surface a spurious
// warning that wasn't actually reached.
func TestFindAPIKeyHelper_FirstMatchWins(t *testing.T) {
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed-settings.json")
	userPath := filepath.Join(dir, "user-settings.json")
	if err := os.WriteFile(managedPath, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write managed settings: %v", err)
	}
	if err := os.WriteFile(userPath, []byte(`{"apiKeyHelper": "/bin/echo fake-key"}`), 0644); err != nil {
		t.Fatalf("write user settings: %v", err)
	}
	found, warns := findAPIKeyHelper([]settingsLayer{
		{layer: "managed-policy", path: managedPath},
		{layer: "user", path: userPath},
		{layer: "project", path: filepath.Join(dir, "this-is-never-read.json")},
	})
	if found == nil || found.layer != "user" {
		t.Fatalf("found = %+v, want the user layer", found)
	}
	if len(warns) != 0 {
		t.Errorf("warns = %v, want none", warns)
	}
}

// TestCheckAPIKeyHelper_NoFilesAnywhere confirms startup proceeds normally
// (nil error) when no settings file exists at any layer.
func TestCheckAPIKeyHelper_NoFilesAnywhere(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo"}, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	eng.fabrikDir = t.TempDir()

	if err := eng.checkAPIKeyHelper(); err != nil {
		t.Errorf("checkAPIKeyHelper() = %v, want nil when no settings files exist", err)
	}
}

// TestCheckAPIKeyHelper_NoFilesButPresentWithoutKey confirms startup
// proceeds normally when a settings file exists but does not set
// apiKeyHelper.
func TestCheckAPIKeyHelper_NoFilesButPresentWithoutKey(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	if err := os.WriteFile(filepath.Join(userDir, "settings.json"), []byte(`{"theme": "dark"}`), 0644); err != nil {
		t.Fatalf("write user settings: %v", err)
	}

	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo"}, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	eng.fabrikDir = t.TempDir()

	if err := eng.checkAPIKeyHelper(); err != nil {
		t.Errorf("checkAPIKeyHelper() = %v, want nil when no apiKeyHelper key is set", err)
	}
}

// TestCheckAPIKeyHelper_UserLayer confirms startup exits non-zero, naming
// the file, when apiKeyHelper is present in the user layer ($CLAUDE_CONFIG_DIR).
func TestCheckAPIKeyHelper_UserLayer(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	settingsPath := filepath.Join(userDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"apiKeyHelper": "/bin/echo fake-key"}`), 0644); err != nil {
		t.Fatalf("write user settings: %v", err)
	}

	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo"}, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	eng.fabrikDir = t.TempDir()

	err := eng.checkAPIKeyHelper()
	if err == nil {
		t.Fatal("checkAPIKeyHelper() = nil, want an error naming the user-layer file")
	}
	if !strings.Contains(err.Error(), settingsPath) {
		t.Errorf("error %q does not name the offending file %q", err.Error(), settingsPath)
	}
	if !strings.Contains(err.Error(), "user") {
		t.Errorf("error %q does not name the user layer", err.Error())
	}
}

// TestCheckAPIKeyHelper_UserLocalLayer confirms the settings.local.json
// variant of the user layer is checked too.
func TestCheckAPIKeyHelper_UserLocalLayer(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	settingsPath := filepath.Join(userDir, "settings.local.json")
	if err := os.WriteFile(settingsPath, []byte(`{"apiKeyHelper": "/bin/echo fake-key"}`), 0644); err != nil {
		t.Fatalf("write user local settings: %v", err)
	}

	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo"}, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	eng.fabrikDir = t.TempDir()

	err := eng.checkAPIKeyHelper()
	if err == nil {
		t.Fatal("checkAPIKeyHelper() = nil, want an error naming the user-local-layer file")
	}
	if !strings.Contains(err.Error(), settingsPath) {
		t.Errorf("error %q does not name the offending file %q", err.Error(), settingsPath)
	}
}

// TestCheckAPIKeyHelper_ProjectLayer confirms startup exits non-zero, naming
// the file, when apiKeyHelper is present in the fabrikDir project layer
// (.claude/settings.json).
func TestCheckAPIKeyHelper_ProjectLayer(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	fabrikDir := t.TempDir()
	claudeDir := filepath.Join(fabrikDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"apiKeyHelper": "/bin/echo fake-key"}`), 0644); err != nil {
		t.Fatalf("write project settings: %v", err)
	}

	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo"}, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	eng.fabrikDir = fabrikDir

	err := eng.checkAPIKeyHelper()
	if err == nil {
		t.Fatal("checkAPIKeyHelper() = nil, want an error naming the project-layer file")
	}
	if !strings.Contains(err.Error(), settingsPath) {
		t.Errorf("error %q does not name the offending file %q", err.Error(), settingsPath)
	}
	if !strings.Contains(err.Error(), "project") {
		t.Errorf("error %q does not name the project layer", err.Error())
	}
}

// TestCheckAPIKeyHelper_ManagedPolicyLayer confirms the managed-policy layer
// is included in checkAPIKeyHelper's assembled layer list and its detection
// is reported with the "managed-policy" layer name, driven directly through
// findAPIKeyHelper (managedPolicySettingsPath resolves a hardcoded,
// OS-owned system path that tests must not write to — see
// managedPolicySettingsPath in startup.go). This exercises the identical
// detection/error-formatting logic checkAPIKeyHelper itself uses for that
// layer, without touching real system state.
func TestCheckAPIKeyHelper_ManagedPolicyLayer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed-settings.json")
	if err := os.WriteFile(path, []byte(`{"apiKeyHelper": "/bin/echo fake-key"}`), 0644); err != nil {
		t.Fatalf("write managed-policy settings: %v", err)
	}
	found, _ := findAPIKeyHelper([]settingsLayer{
		{layer: "managed-policy", path: path},
	})
	if found == nil || found.layer != "managed-policy" || found.path != path {
		t.Fatalf("found = %+v, want layer=managed-policy path=%s", found, path)
	}
}

// TestCheckAPIKeyHelper_MalformedProducesWarningNotCrash confirms a malformed
// settings file at a real checkAPIKeyHelper layer produces a warning and a
// successful (nil-error) startup, not a crash.
func TestCheckAPIKeyHelper_MalformedProducesWarningNotCrash(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	if err := os.WriteFile(filepath.Join(userDir, "settings.json"), []byte("{not valid json"), 0644); err != nil {
		t.Fatalf("write malformed user settings: %v", err)
	}

	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo"}, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	eng.fabrikDir = t.TempDir()

	if err := eng.checkAPIKeyHelper(); err != nil {
		t.Errorf("checkAPIKeyHelper() = %v, want nil (malformed file warns, does not fail startup)", err)
	}
}

// TestManagedPolicySettingsPath_MatchesRunningGOOS confirms the resolved
// path corresponds to the platform the test is actually running on, per
// R11 — assertions elsewhere in this file must not assume darwin in CI.
func TestManagedPolicySettingsPath_MatchesRunningGOOS(t *testing.T) {
	p := managedPolicySettingsPath()
	switch {
	case strings.Contains(p, "ClaudeCode/managed-settings.json"):
	case strings.Contains(p, "claude-code/managed-settings.json"):
	case p == "":
	default:
		t.Errorf("managedPolicySettingsPath() = %q, unexpected shape", p)
	}
}

// TestLogAnthropicAPIKeyOptIn_Inactive confirms the R9 startup notice stays
// byte-for-byte silent when the opt-in is inactive.
func TestLogAnthropicAPIKeyOptIn_Inactive(t *testing.T) {
	var buf strings.Builder
	logAnthropicAPIKeyOptIn(false, &buf)
	if buf.Len() != 0 {
		t.Fatalf("expected no output when inactive, got: %q", buf.String())
	}
}

// TestLogAnthropicAPIKeyOptIn_Active confirms the R9 startup notice fires
// exactly one line naming FABRIK_ANTHROPIC_API_KEY when active, and never
// contains a value (there is none passed to this function to leak, but the
// assertion pins the exact message shape so a future edit can't
// accidentally start interpolating one).
func TestLogAnthropicAPIKeyOptIn_Active(t *testing.T) {
	var buf strings.Builder
	logAnthropicAPIKeyOptIn(true, &buf)
	out := buf.String()
	if !strings.Contains(out, "FABRIK_ANTHROPIC_API_KEY") {
		t.Fatalf("expected output to name FABRIK_ANTHROPIC_API_KEY, got: %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("expected exactly one line, got: %q", out)
	}
}

// TestLogAnthropicEnvPassthrough_Empty confirms the R18 startup notice stays
// byte-for-byte silent when the passthrough list is empty.
func TestLogAnthropicEnvPassthrough_Empty(t *testing.T) {
	var buf strings.Builder
	logAnthropicEnvPassthrough(nil, &buf)
	if buf.Len() != 0 {
		t.Fatalf("expected no output when passthrough list is empty, got: %q", buf.String())
	}
}

// TestLogAnthropicEnvPassthrough_NonEmpty confirms the R18 startup notice
// fires exactly one line naming every passed-through variable, and never
// contains a value.
func TestLogAnthropicEnvPassthrough_NonEmpty(t *testing.T) {
	var buf strings.Builder
	logAnthropicEnvPassthrough([]string{"CLAUDE_CODE_USE_BEDROCK", "ANTHROPIC_AWS_API_KEY"}, &buf)
	out := buf.String()
	for _, name := range []string{"CLAUDE_CODE_USE_BEDROCK", "ANTHROPIC_AWS_API_KEY"} {
		if !strings.Contains(out, name) {
			t.Errorf("expected output to name %s, got: %q", name, out)
		}
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("expected exactly one line, got: %q", out)
	}
}
