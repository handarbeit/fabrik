package pruefer

import (
	"os"
	"path/filepath"
	"reflect"
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
	if len(cfg.WatchedRepos) != 0 {
		t.Errorf("WatchedRepos = %v, want empty", cfg.WatchedRepos)
	}
	if !cfg.TUI {
		t.Errorf("TUI = false, want true (default on)")
	}
	if cfg.LogFile != DefaultLogPath {
		t.Errorf("LogFile = %q, want %q", cfg.LogFile, DefaultLogPath)
	}
	if cfg.EventSource != DefaultEventSource {
		t.Errorf("EventSource = %q, want %q", cfg.EventSource, DefaultEventSource)
	}
	if cfg.HookdeckAPIKeyEnv != DefaultHookdeckAPIKeyEnv {
		t.Errorf("HookdeckAPIKeyEnv = %q, want %q", cfg.HookdeckAPIKeyEnv, DefaultHookdeckAPIKeyEnv)
	}
	if cfg.HookdeckWebhookSecretEnv != DefaultHookdeckWebhookSecretEnv {
		t.Errorf("HookdeckWebhookSecretEnv = %q, want %q", cfg.HookdeckWebhookSecretEnv, DefaultHookdeckWebhookSecretEnv)
	}
	if cfg.ReconciliationStartup != DefaultReconciliationStartup {
		t.Errorf("ReconciliationStartup = %v, want %v", cfg.ReconciliationStartup, DefaultReconciliationStartup)
	}
	if cfg.ReconciliationFallbackInterval != DefaultReconciliationFallbackInterval {
		t.Errorf("ReconciliationFallbackInterval = %v, want %v", cfg.ReconciliationFallbackInterval, DefaultReconciliationFallbackInterval)
	}
}

func TestLoadConfig_LogFilePrecedence(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(t.TempDir(), "custom.log")
	fromEnv := filepath.Join(t.TempDir(), "env.log")
	fromFlag := filepath.Join(t.TempDir(), "flag.log")

	// YAML override.
	path := writeYAMLConfig(t, dir, "log_file: "+custom)
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.LogFile != custom {
		t.Errorf("LogFile = %q, want %q (from YAML)", cfg.LogFile, custom)
	}

	// Env overrides YAML.
	t.Setenv("PRUEFER_LOG_FILE", fromEnv)
	cfg, err = LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.LogFile != fromEnv {
		t.Errorf("LogFile = %q, want %q (env should override YAML)", cfg.LogFile, fromEnv)
	}

	// Flag overrides env.
	cfg, err = LoadConfig([]string{"-config", path, "-log-file", fromFlag})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.LogFile != fromFlag {
		t.Errorf("LogFile = %q, want %q (flag should override env)", cfg.LogFile, fromFlag)
	}
}

func TestLoadConfig_LogFileYAMLExplicitEmptyDisables(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `log_file: ""`)
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.LogFile != "" {
		t.Errorf("LogFile = %q, want empty (YAML log_file: \"\" should disable file logging)", cfg.LogFile)
	}
}

func TestLoadConfig_LogFileEnvExplicitEmptyDisables(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, "log_file: "+filepath.Join(t.TempDir(), "custom.log"))
	t.Setenv("PRUEFER_LOG_FILE", "")

	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.LogFile != "" {
		t.Errorf("LogFile = %q, want empty (PRUEFER_LOG_FILE= should disable file logging, overriding YAML)", cfg.LogFile)
	}
}

func TestLoadConfig_LogFileFlagExplicitEmptyDisables(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, "log_file: "+filepath.Join(t.TempDir(), "custom.log"))

	cfg, err := LoadConfig([]string{"-config", path, "-log-file", ""})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.LogFile != "" {
		t.Errorf("LogFile = %q, want empty (-log-file \"\" should disable file logging, overriding YAML)", cfg.LogFile)
	}
}

// TestLoadConfig_EventSourceSurfaceUnchangedWhenUnset is the issue's
// "byte-for-byte unchanged" regression guard: loading a config with none of
// the new event_source/hookdeck/reconciliation keys set must produce
// exactly the same values as before this surface existed for every
// pre-existing field, plus the documented defaults for the new ones.
func TestLoadConfig_EventSourceSurfaceUnchangedWhenUnset(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `
watched_repos:
  - handarbeit/fabrik
model: opus
concurrency_cap: 5
`)
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.EventSource != EventSourcePoll {
		t.Errorf("EventSource = %q, want %q (poll-only mode is the unaffected default)", cfg.EventSource, EventSourcePoll)
	}
	if cfg.Model != "opus" || cfg.ConcurrencyCap != 5 {
		t.Errorf("pre-existing fields affected: Model=%q ConcurrencyCap=%d", cfg.Model, cfg.ConcurrencyCap)
	}
	if len(cfg.WatchedRepos) != 1 || cfg.WatchedRepos[0] != "handarbeit/fabrik" {
		t.Errorf("WatchedRepos = %v", cfg.WatchedRepos)
	}
}

func TestLoadConfig_EventSourceValidation(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `event_source: not-a-real-source`)
	if _, err := LoadConfig([]string{"-config", path}); err == nil {
		t.Fatal("expected an error for an invalid event_source value")
	}
}

func TestLoadConfig_EventSourceYAML(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `event_source: hookdeck`)
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.EventSource != EventSourceHookdeck {
		t.Errorf("EventSource = %q, want %q", cfg.EventSource, EventSourceHookdeck)
	}
}

func TestLoadConfig_EventSourcePrecedence(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `event_source: hookdeck`)

	// Env overrides YAML.
	t.Setenv("PRUEFER_EVENT_SOURCE", "poll")
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.EventSource != EventSourcePoll {
		t.Errorf("EventSource = %q, want %q (env should override YAML)", cfg.EventSource, EventSourcePoll)
	}

	// Flag overrides env.
	cfg, err = LoadConfig([]string{"-config", path, "-event-source", "hookdeck"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.EventSource != EventSourceHookdeck {
		t.Errorf("EventSource = %q, want %q (flag should override env)", cfg.EventSource, EventSourceHookdeck)
	}
}

func TestLoadConfig_HookdeckYAML(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `
event_source: hookdeck
hookdeck:
  api_key_env: MY_HOOKDECK_KEY
  webhook_secret_env: MY_WEBHOOK_SECRET
`)
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.HookdeckAPIKeyEnv != "MY_HOOKDECK_KEY" {
		t.Errorf("HookdeckAPIKeyEnv = %q, want MY_HOOKDECK_KEY", cfg.HookdeckAPIKeyEnv)
	}
	if cfg.HookdeckWebhookSecretEnv != "MY_WEBHOOK_SECRET" {
		t.Errorf("HookdeckWebhookSecretEnv = %q, want MY_WEBHOOK_SECRET", cfg.HookdeckWebhookSecretEnv)
	}
}

func TestLoadConfig_HookdeckEnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `
hookdeck:
  api_key_env: FROM_YAML
`)
	t.Setenv("PRUEFER_HOOKDECK_API_KEY_ENV", "FROM_ENV")

	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.HookdeckAPIKeyEnv != "FROM_ENV" {
		t.Errorf("HookdeckAPIKeyEnv = %q, want FROM_ENV (env should override YAML)", cfg.HookdeckAPIKeyEnv)
	}
}

func TestLoadConfig_HookdeckEnvFlagsEmptyRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, ``)

	if _, err := LoadConfig([]string{"-config", path, "-hookdeck-api-key-env="}); err == nil {
		t.Fatal("expected an error for an empty -hookdeck-api-key-env value")
	}
	if _, err := LoadConfig([]string{"-config", path, "-hookdeck-webhook-secret-env="}); err == nil {
		t.Fatal("expected an error for an empty -hookdeck-webhook-secret-env value")
	}
}

func TestLoadConfig_ReconciliationYAML(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `
reconciliation:
  startup: false
  fallback_interval: 5m
`)
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ReconciliationStartup {
		t.Error("ReconciliationStartup = true, want false (from YAML)")
	}
	if cfg.ReconciliationFallbackInterval != 5*time.Minute {
		t.Errorf("ReconciliationFallbackInterval = %v, want 5m", cfg.ReconciliationFallbackInterval)
	}
}

func TestLoadConfig_ReconciliationFallbackIntervalInvalid(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `
reconciliation:
  fallback_interval: not-a-duration
`)
	if _, err := LoadConfig([]string{"-config", path}); err == nil {
		t.Fatal("expected an error for an invalid reconciliation.fallback_interval value")
	}
}

func TestLoadConfig_ReconciliationPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, `
reconciliation:
  startup: true
  fallback_interval: 5m
`)

	// Env overrides YAML.
	t.Setenv("PRUEFER_RECONCILIATION_STARTUP", "false")
	t.Setenv("PRUEFER_RECONCILIATION_FALLBACK_INTERVAL", "10m")
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ReconciliationStartup {
		t.Error("ReconciliationStartup = true, want false (env should override YAML)")
	}
	if cfg.ReconciliationFallbackInterval != 10*time.Minute {
		t.Errorf("ReconciliationFallbackInterval = %v, want 10m (env should override YAML)", cfg.ReconciliationFallbackInterval)
	}

	// Flag overrides env.
	cfg, err = LoadConfig([]string{"-config", path, "-reconciliation-startup=true", "-reconciliation-fallback-interval", "15m"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.ReconciliationStartup {
		t.Error("ReconciliationStartup = false, want true (flag should override env)")
	}
	if cfg.ReconciliationFallbackInterval != 15*time.Minute {
		t.Errorf("ReconciliationFallbackInterval = %v, want 15m (flag should override env)", cfg.ReconciliationFallbackInterval)
	}
}

func TestLoadConfig_ReconciliationFallbackIntervalFlagInvalid(t *testing.T) {
	dir := t.TempDir()
	path := writeYAMLConfig(t, dir, ``)
	if _, err := LoadConfig([]string{"-config", path, "-reconciliation-fallback-interval", "not-a-duration"}); err == nil {
		t.Fatal("expected an error for an invalid -reconciliation-fallback-interval value")
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

func TestLoadConfig_AutoUpgradeDefaultsOff(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig([]string{"-config", filepath.Join(dir, "missing.yaml")})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AutoUpgrade {
		t.Errorf("AutoUpgrade = true, want false by default")
	}
}

func TestLoadConfig_AutoUpgradePrecedence(t *testing.T) {
	dir := t.TempDir()

	// YAML can enable it.
	path := writeYAMLConfig(t, dir, `auto_upgrade: true`)
	cfg, err := LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.AutoUpgrade {
		t.Errorf("AutoUpgrade = false, want true (from YAML)")
	}

	// Env overrides YAML.
	t.Setenv("PRUEFER_AUTO_UPGRADE", "false")
	cfg, err = LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AutoUpgrade {
		t.Errorf("AutoUpgrade = true, want false (env should override YAML)")
	}

	// --auto-upgrade flag overrides env.
	t.Setenv("PRUEFER_AUTO_UPGRADE", "false")
	cfg, err = LoadConfig([]string{"-config", path, "-auto-upgrade"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.AutoUpgrade {
		t.Errorf("AutoUpgrade = false, want true (--auto-upgrade flag should override env)")
	}
}

func TestLoadConfig_VersionRequestedShortCircuits(t *testing.T) {
	// --version must work even with no config file present and no other
	// required flags set, so it short-circuits before YAML/env resolution.
	cfg, err := LoadConfig([]string{"-version"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.VersionRequested {
		t.Errorf("VersionRequested = false, want true")
	}
	if cfg.AppID != 0 || len(cfg.WatchedRepos) != 0 {
		t.Errorf("expected short-circuited zero-value Config aside from VersionRequested, got %+v", cfg)
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

// TestConfig_AllFieldsClassified asserts every field in Config carries a
// recognized `reload` struct tag (#1640, R2) — a field added without one
// fails this test rather than silently falling through applyConfigReload's
// conservative "treat as restart-only" default. Non-vacuous: adding a field
// with no tag, or a misspelled one, to a scratch copy of Config fails this
// exact assertion (verified by hand while writing this test, against a
// throwaway extra field).
func TestConfig_AllFieldsClassified(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("reload")
		switch reloadClass(tag) {
		case reloadLive, reloadRestart, reloadSkip:
			// classified
		default:
			t.Errorf("Config.%s has no recognized reload tag (got %q) — every field must be tagged live, restart, or skip", f.Name, tag)
		}
	}
}

func TestApplyConfigReload_WatchedReposAddedAndRemoved(t *testing.T) {
	old := Config{WatchedRepos: []string{"a/one", "a/two"}}
	cand := Config{WatchedRepos: []string{"a/one", "b/three"}}

	merged, diff := applyConfigReload(old, cand)

	if len(diff.ReposAdded) != 1 || diff.ReposAdded[0] != "b/three" {
		t.Errorf("ReposAdded = %v, want [b/three]", diff.ReposAdded)
	}
	if len(diff.ReposRemoved) != 1 || diff.ReposRemoved[0] != "a/two" {
		t.Errorf("ReposRemoved = %v, want [a/two]", diff.ReposRemoved)
	}
	if len(merged.WatchedRepos) != 2 || merged.WatchedRepos[0] != "a/one" || merged.WatchedRepos[1] != "b/three" {
		t.Errorf("merged.WatchedRepos = %v, want [a/one b/three]", merged.WatchedRepos)
	}
}

func TestApplyConfigReload_LiveFieldIsApplied(t *testing.T) {
	old := Config{Model: "sonnet", PollInterval: 120 * time.Second}
	cand := Config{Model: "opus", PollInterval: 120 * time.Second}

	merged, diff := applyConfigReload(old, cand)

	if merged.Model != "opus" {
		t.Errorf("merged.Model = %q, want opus", merged.Model)
	}
	if len(diff.FieldsChanged) != 1 || diff.FieldsChanged[0].Field != "Model" {
		t.Fatalf("FieldsChanged = %v, want exactly one entry for Model", diff.FieldsChanged)
	}
	if diff.FieldsChanged[0].Old != "sonnet" || diff.FieldsChanged[0].New != "opus" {
		t.Errorf("FieldsChanged[0] = %+v, want Old=sonnet New=opus", diff.FieldsChanged[0])
	}
	if len(diff.RestartOnlyChanged) != 0 {
		t.Errorf("RestartOnlyChanged = %v, want empty", diff.RestartOnlyChanged)
	}
}

func TestApplyConfigReload_RestartOnlyFieldIsReportedNotApplied(t *testing.T) {
	old := Config{AppID: 1, LogFile: "old.log"}
	cand := Config{AppID: 2, LogFile: "new.log"}

	merged, diff := applyConfigReload(old, cand)

	// AC4: reported...
	if len(diff.RestartOnlyChanged) != 2 {
		t.Fatalf("RestartOnlyChanged = %v, want 2 entries (AppID, LogFile)", diff.RestartOnlyChanged)
	}
	var names []string
	for _, c := range diff.RestartOnlyChanged {
		names = append(names, c.Field)
	}
	for _, want := range []string{"AppID", "LogFile"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("RestartOnlyChanged = %v, missing %q", names, want)
		}
	}
	// ...but never applied (AC4: "not applied", literally true of merged).
	if merged.AppID != 1 {
		t.Errorf("merged.AppID = %d, want 1 (unchanged — restart-only)", merged.AppID)
	}
	if merged.LogFile != "old.log" {
		t.Errorf("merged.LogFile = %q, want old.log (unchanged — restart-only)", merged.LogFile)
	}
	if len(diff.FieldsChanged) != 0 {
		t.Errorf("FieldsChanged = %v, want empty — no live fields changed", diff.FieldsChanged)
	}
}

func TestApplyConfigReload_NoChangeProducesEmptyDiff(t *testing.T) {
	cfg := Config{Model: "sonnet", WatchedRepos: []string{"a/one"}, AppID: 1}
	merged, diff := applyConfigReload(cfg, cfg)

	if !diff.Empty() {
		t.Errorf("diff = %+v, want Empty()", diff)
	}
	if !reflect.DeepEqual(merged, cfg) {
		t.Errorf("merged = %+v, want unchanged %+v", merged, cfg)
	}
}

func TestApplyConfigReload_MixOfLiveAndRestartOnlyBothReported(t *testing.T) {
	old := Config{Model: "sonnet", AppID: 1}
	cand := Config{Model: "opus", AppID: 2}

	merged, diff := applyConfigReload(old, cand)

	// The live field is applied even though a restart-only field also
	// changed in the same reload (my reading of R2: a restart-only change
	// is reported, not a reason to withhold an otherwise-safe field).
	if merged.Model != "opus" {
		t.Errorf("merged.Model = %q, want opus (a restart-only change elsewhere must not block a live field)", merged.Model)
	}
	if merged.AppID != 1 {
		t.Errorf("merged.AppID = %d, want 1 (restart-only, not applied)", merged.AppID)
	}
	if len(diff.FieldsChanged) != 1 || len(diff.RestartOnlyChanged) != 1 {
		t.Errorf("FieldsChanged=%v RestartOnlyChanged=%v, want exactly one of each", diff.FieldsChanged, diff.RestartOnlyChanged)
	}
}

func TestDiffRepos_OrderInsensitive(t *testing.T) {
	added, removed := diffRepos([]string{"a/one", "a/two"}, []string{"a/two", "a/one"})
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("diffRepos with reordered-only input = added:%v removed:%v, want both empty", added, removed)
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
