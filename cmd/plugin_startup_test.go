package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fabrikplugin "github.com/handarbeit/fabrik/plugin"
)

// TestEvaluatePluginStartupState_NoOp verifies that a pristine, up-to-date
// plugin reports no customization and no staleness — steady state.
func TestEvaluatePluginStartupState_NoOp(t *testing.T) {
	pluginDir := buildPluginDir(t)
	if err := fabrikplugin.WriteInstalledVersion(pluginDir); err != nil {
		t.Fatal(err)
	}

	customWorkflow, upgradeNeeded, skillsStaleCount, err := evaluatePluginStartupState(pluginDir, false)
	if err != nil {
		t.Fatalf("evaluatePluginStartupState error: %v", err)
	}
	if customWorkflow || upgradeNeeded || skillsStaleCount != 0 {
		t.Errorf("no-op: want (false,false,0), got (%v,%v,%d)", customWorkflow, upgradeNeeded, skillsStaleCount)
	}
}

// TestEvaluatePluginStartupState_PristineStale is AC4: a pristine (not
// customized), stale plugin is unchanged by #1787 — it already auto-refreshes
// (upgradeNeeded=true), and the new skillsStaleCount is now populated too
// (this is the pre-existing behavior; #1787 only changes the customized case).
func TestEvaluatePluginStartupState_PristineStale(t *testing.T) {
	pluginDir := buildPluginDir(t)
	entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
	if err != nil || len(entries) == 0 {
		t.Fatal("no SKILL.md files found in test plugin dir")
	}
	if err := os.WriteFile(entries[0], []byte("old content"), 0644); err != nil {
		t.Fatal(err)
	}
	// Seed installed = current (old) disk state, and register it as known so
	// this is treated as a legitimate prior release, not a corrupted migration.
	writeInstalledForDir(t, pluginDir)

	customWorkflow, upgradeNeeded, skillsStaleCount, err := evaluatePluginStartupState(pluginDir, false)
	if err != nil {
		t.Fatalf("evaluatePluginStartupState error: %v", err)
	}
	if customWorkflow {
		t.Errorf("pristine-stale: customWorkflow should be false, got true")
	}
	if !upgradeNeeded {
		t.Errorf("pristine-stale: upgradeNeeded should be true, got false")
	}
	if skillsStaleCount == 0 {
		t.Errorf("pristine-stale: skillsStaleCount should be > 0, got 0")
	}
}

// TestEvaluatePluginStartupState_CustomizedOnly is AC3: a customized,
// up-to-date-when-installed plugin reports customization only — staleness
// must not be fabricated when installedVer == embeddedVer.
func TestEvaluatePluginStartupState_CustomizedOnly(t *testing.T) {
	pluginDir := buildPluginDir(t)
	// Seed installed == embedded (pristine at install time).
	if err := fabrikplugin.WriteInstalledVersion(pluginDir); err != nil {
		t.Fatal(err)
	}
	// Operator customizes disk after install.
	entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
	if err != nil || len(entries) == 0 {
		t.Fatal("no SKILL.md files found in test plugin dir")
	}
	if err := os.WriteFile(entries[0], []byte("operator customization"), 0644); err != nil {
		t.Fatal(err)
	}

	customWorkflow, upgradeNeeded, skillsStaleCount, err := evaluatePluginStartupState(pluginDir, false)
	if err != nil {
		t.Fatalf("evaluatePluginStartupState error: %v", err)
	}
	if !customWorkflow {
		t.Errorf("customized-only: customWorkflow should be true, got false")
	}
	if upgradeNeeded {
		t.Errorf("customized-only: upgradeNeeded should be false, got true")
	}
	if skillsStaleCount != 0 {
		t.Errorf("customized-only: skillsStaleCount should be 0 (installedVer == embeddedVer), got %d", skillsStaleCount)
	}
}

// TestEvaluatePluginStartupState_CustomizedAndStale is AC1, reproduced red
// against main first (the #1787 bug: today only the customization warning
// appears — skillsStaleCount silently stays 0 whenever customWorkflow is
// true, because it was only ever populated inside the upgradeNeeded branch).
func TestEvaluatePluginStartupState_CustomizedAndStale(t *testing.T) {
	pluginDir := buildPluginDir(t)
	entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
	if err != nil || len(entries) < 2 {
		t.Fatal("need at least 2 SKILL.md files for this test")
	}
	// Simulate an old installed version.
	if err := os.WriteFile(entries[0], []byte("old embedded content"), 0644); err != nil {
		t.Fatal(err)
	}
	writeInstalledForDir(t, pluginDir)
	// Now the operator customizes a *different* file on top of the already-stale install.
	if err := os.WriteFile(entries[1], []byte("operator customization"), 0644); err != nil {
		t.Fatal(err)
	}

	customWorkflow, upgradeNeeded, skillsStaleCount, err := evaluatePluginStartupState(pluginDir, false)
	if err != nil {
		t.Fatalf("evaluatePluginStartupState error: %v", err)
	}
	if !customWorkflow {
		t.Errorf("customized-and-stale: customWorkflow should be true, got false")
	}
	if upgradeNeeded {
		t.Errorf("customized-and-stale: upgradeNeeded should be false, got true")
	}
	if skillsStaleCount == 0 {
		t.Errorf("customized-and-stale: skillsStaleCount should be > 0, got 0 — this is the #1787 bug: staleness must be reported even when customized")
	}
}

// TestPluginCustomizationWarning_NamesBothSignalsWhenBothTrue verifies R1/R2/R4:
// when both customization and staleness are true, the warning names both
// facts — including the drifted file count — and points at
// 'fabrik upgrade --reconcile'.
func TestPluginCustomizationWarning_NamesBothSignalsWhenBothTrue(t *testing.T) {
	pluginDir := buildPluginDir(t)
	entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
	if err != nil || len(entries) < 2 {
		t.Fatal("need at least 2 SKILL.md files for this test")
	}
	if err := os.WriteFile(entries[0], []byte("old embedded content"), 0644); err != nil {
		t.Fatal(err)
	}
	writeInstalledForDir(t, pluginDir)
	if err := os.WriteFile(entries[1], []byte("operator customization"), 0644); err != nil {
		t.Fatal(err)
	}

	_, _, skillsStaleCount, err := evaluatePluginStartupState(pluginDir, false)
	if err != nil {
		t.Fatalf("evaluatePluginStartupState error: %v", err)
	}
	if skillsStaleCount == 0 {
		t.Fatal("expected non-zero skillsStaleCount for this fixture")
	}

	msg := pluginCustomizationWarning(pluginDir, skillsStaleCount)
	if !strings.Contains(msg, "local customizations") {
		t.Errorf("expected customization signal in message, got: %q", msg)
	}
	if !strings.Contains(msg, "stale") {
		t.Errorf("expected staleness signal in message, got: %q", msg)
	}
	if !strings.Contains(msg, "--reconcile") {
		t.Errorf("expected 'fabrik upgrade --reconcile' to be named in message, got: %q", msg)
	}
}

// TestPluginCustomizationWarning_CustomizedOnlyOmitsStaleness verifies AC3 at
// the message level: when staleCount is 0, no staleness clause is added.
func TestPluginCustomizationWarning_CustomizedOnlyOmitsStaleness(t *testing.T) {
	pluginDir := buildPluginDir(t)
	if err := fabrikplugin.WriteInstalledVersion(pluginDir); err != nil {
		t.Fatal(err)
	}
	entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
	if err != nil || len(entries) == 0 {
		t.Fatal("no SKILL.md files found")
	}
	if err := os.WriteFile(entries[0], []byte("operator customization"), 0644); err != nil {
		t.Fatal(err)
	}

	msg := pluginCustomizationWarning(pluginDir, 0)
	if !strings.Contains(msg, "local customizations") {
		t.Errorf("expected customization signal in message, got: %q", msg)
	}
	if strings.Contains(msg, "Also stale") {
		t.Errorf("did not expect a staleness clause when staleCount is 0, got: %q", msg)
	}
}
