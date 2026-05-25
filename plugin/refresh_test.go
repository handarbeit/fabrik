package plugin

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestComputeEmbeddedVersion_Deterministic(t *testing.T) {
	v1 := ComputeEmbeddedVersion()
	v2 := ComputeEmbeddedVersion()
	if v1 == "" {
		t.Fatal("ComputeEmbeddedVersion returned empty string")
	}
	if v1 != v2 {
		t.Errorf("ComputeEmbeddedVersion not deterministic: %q != %q", v1, v2)
	}
}

func TestComputeDiskVersion_NonExistentDir(t *testing.T) {
	dir := t.TempDir()
	ver, err := ComputeDiskVersion(filepath.Join(dir, "nonexistent"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ver != "" {
		t.Errorf("expected empty string for nonexistent dir, got %q", ver)
	}
}

func TestComputeDiskVersion_MatchesEmbedded(t *testing.T) {
	dir := t.TempDir()
	if err := populatePluginDir(dir); err != nil {
		t.Fatal(err)
	}

	embeddedVer := ComputeEmbeddedVersion()
	diskVer, err := ComputeDiskVersion(dir)
	if err != nil {
		t.Fatalf("ComputeDiskVersion error: %v", err)
	}
	if diskVer != embeddedVer {
		t.Errorf("disk version %q != embedded version %q", diskVer, embeddedVer)
	}
}

func TestComputeDiskVersion_SkipsInstalledVersionFile(t *testing.T) {
	dir := t.TempDir()
	if err := populatePluginDir(dir); err != nil {
		t.Fatal(err)
	}

	vBefore, err := ComputeDiskVersion(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Write .installed-version — must not affect the disk hash.
	if err := WriteVersionHash(dir, "somehash"); err != nil {
		t.Fatal(err)
	}

	vAfter, err := ComputeDiskVersion(dir)
	if err != nil {
		t.Fatal(err)
	}
	if vBefore != vAfter {
		t.Errorf("ComputeDiskVersion changed after writing .installed-version: %q -> %q", vBefore, vAfter)
	}
}

func TestWriteReadInstalledVersion_Roundtrip(t *testing.T) {
	dir := t.TempDir()

	// Missing file returns ("", nil).
	v, err := ReadInstalledVersion(dir)
	if err != nil {
		t.Fatalf("ReadInstalledVersion (missing) error: %v", err)
	}
	if v != "" {
		t.Errorf("expected empty string for missing file, got %q", v)
	}

	if err := WriteInstalledVersion(dir); err != nil {
		t.Fatalf("WriteInstalledVersion error: %v", err)
	}

	v2, err := ReadInstalledVersion(dir)
	if err != nil {
		t.Fatalf("ReadInstalledVersion error: %v", err)
	}
	if v2 != ComputeEmbeddedVersion() {
		t.Errorf("ReadInstalledVersion = %q, want %q", v2, ComputeEmbeddedVersion())
	}
}

func TestCheckPluginState_Migration(t *testing.T) {
	dir := t.TempDir()
	if err := populatePluginDir(dir); err != nil {
		t.Fatal(err)
	}

	cw, up, err := CheckPluginState(dir)
	if err != nil {
		t.Fatalf("CheckPluginState error: %v", err)
	}
	if cw || up {
		t.Errorf("migration: want (false,false), got (%v,%v)", cw, up)
	}
	// .installed-version must now exist and equal disk version.
	installedVer, _ := ReadInstalledVersion(dir)
	diskVer, _ := ComputeDiskVersion(dir)
	if installedVer != diskVer {
		t.Errorf("migration: installedVer %q != diskVer %q", installedVer, diskVer)
	}
}

func TestCheckPluginState_NoOp(t *testing.T) {
	dir := t.TempDir()
	if err := populatePluginDir(dir); err != nil {
		t.Fatal(err)
	}
	// Seed installedVer from embedded (disk == embedded == installed).
	if err := WriteInstalledVersion(dir); err != nil {
		t.Fatal(err)
	}

	cw, up, err := CheckPluginState(dir)
	if err != nil {
		t.Fatalf("CheckPluginState error: %v", err)
	}
	if cw || up {
		t.Errorf("no-op: want (false,false), got (%v,%v)", cw, up)
	}
}

func TestCheckPluginState_AutoRefresh(t *testing.T) {
	// Use an empty dir so disk == installed (both "empty") while embedded differs.
	// We must inject the fake diskVer into the known list so the corrupted-state
	// guard treats it as a legitimate installedVer (and returns upgradeNeeded=true).
	dir2 := t.TempDir()
	embeddedVer := ComputeEmbeddedVersion()
	diskVer2, _ := ComputeDiskVersion(dir2)
	if err := WriteVersionHash(dir2, diskVer2); err != nil {
		t.Fatal(err)
	}
	// embedded != diskVer2, disk == installed → auto-refresh.
	if embeddedVer == diskVer2 {
		t.Skip("embedded matches empty disk — cannot test auto-refresh")
	}

	// Inject diskVer2 as a "known" version so the guard passes.
	cw, up, err := checkPluginState(dir2, []string{diskVer2})
	if err != nil {
		t.Fatalf("checkPluginState error: %v", err)
	}
	if cw {
		t.Errorf("auto-refresh: customWorkflow should be false")
	}
	if !up {
		t.Errorf("auto-refresh: upgradeNeeded should be true")
	}
}

func TestCheckPluginState_CustomWorkflow(t *testing.T) {
	dir := t.TempDir()
	if err := populatePluginDir(dir); err != nil {
		t.Fatal(err)
	}
	// Write installed == current disk.
	diskVer, _ := ComputeDiskVersion(dir)
	if err := WriteVersionHash(dir, diskVer); err != nil {
		t.Fatal(err)
	}
	// Now mutate a disk file so diskVer != installedVer.
	entries, _ := filepath.Glob(filepath.Join(dir, "skills", "*", "SKILL.md"))
	if len(entries) == 0 {
		t.Fatal("no SKILL.md files found")
	}
	if err := os.WriteFile(entries[0], []byte("operator customization"), 0644); err != nil {
		t.Fatal(err)
	}

	cw, up, err := CheckPluginState(dir)
	if err != nil {
		t.Fatalf("CheckPluginState error: %v", err)
	}
	if !cw {
		t.Errorf("custom-workflow: customWorkflow should be true")
	}
	if up {
		t.Errorf("custom-workflow: upgradeNeeded should be false")
	}
}

// TestCheckPluginState_MigrationCustomised verifies that when .installed-version
// is absent and diskVer != embeddedVer (pre-v0.0.64 customized install), the
// migration path returns customWorkflow=true and does NOT write .installed-version.
func TestCheckPluginState_MigrationCustomised(t *testing.T) {
	dir := t.TempDir()
	if err := populatePluginDir(dir); err != nil {
		t.Fatal(err)
	}
	// Modify a skill file so disk differs from embedded.
	entries, _ := filepath.Glob(filepath.Join(dir, "skills", "*", "SKILL.md"))
	if len(entries) == 0 {
		t.Fatal("no SKILL.md files found")
	}
	if err := os.WriteFile(entries[0], []byte("operator customization"), 0644); err != nil {
		t.Fatal(err)
	}
	// No .installed-version file present (simulating pre-v0.0.64).

	cw, up, err := CheckPluginState(dir)
	if err != nil {
		t.Fatalf("CheckPluginState error: %v", err)
	}
	if !cw {
		t.Errorf("migration-customised: customWorkflow should be true")
	}
	if up {
		t.Errorf("migration-customised: upgradeNeeded should be false")
	}
	// .installed-version must NOT have been created.
	installedPath := filepath.Join(dir, ".installed-version")
	if _, statErr := os.Stat(installedPath); !os.IsNotExist(statErr) {
		t.Errorf("migration-customised: .installed-version must NOT be created for customized install")
	}
}

// TestCheckPluginState_CorruptedMigration verifies that when installedVer is
// present but is not a known embedded hash (i.e., written by the buggy v0.0.64
// migration), the engine returns customWorkflow=true and upgradeNeeded=false.
func TestCheckPluginState_CorruptedMigration(t *testing.T) {
	dir := t.TempDir()
	if err := populatePluginDir(dir); err != nil {
		t.Fatal(err)
	}
	// Modify a skill to create a "custom" disk state.
	entries, _ := filepath.Glob(filepath.Join(dir, "skills", "*", "SKILL.md"))
	if len(entries) == 0 {
		t.Fatal("no SKILL.md files found")
	}
	if err := os.WriteFile(entries[0], []byte("operator customization"), 0644); err != nil {
		t.Fatal(err)
	}
	// Simulate the buggy v0.0.64 migration: write installedVer = customized disk hash.
	diskVer, err := ComputeDiskVersion(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteVersionHash(dir, diskVer); err != nil {
		t.Fatal(err)
	}
	// Verify setup: disk == installed, but installedVer is not in KnownEmbeddedVersions.
	for _, known := range KnownEmbeddedVersions {
		if diskVer == known {
			t.Skip("diskVer accidentally matches a known embedded version — cannot test corrupted-migration path")
		}
	}

	// Now simulate a new embedded version shipping (embedded != installedVer).
	// Use checkPluginState with an empty known list so installedVer is not recognized.
	embeddedVer := ComputeEmbeddedVersion()
	if embeddedVer == diskVer {
		t.Skip("embedded matches customized disk — cannot test corrupted-migration path")
	}

	cw, up, err := checkPluginState(dir, []string{})
	if err != nil {
		t.Fatalf("checkPluginState error: %v", err)
	}
	if !cw {
		t.Errorf("corrupted-migration: customWorkflow should be true")
	}
	if up {
		t.Errorf("corrupted-migration: upgradeNeeded should be false")
	}
}

// TestCheckPluginState_KnownEmbeddedAutoRefresh verifies that when installed is
// a known embedded hash and disk==installed but embedded has changed, the engine
// returns upgradeNeeded=true (safe auto-refresh path).
func TestCheckPluginState_KnownEmbeddedAutoRefresh(t *testing.T) {
	dir := t.TempDir()
	if err := populatePluginDir(dir); err != nil {
		t.Fatal(err)
	}
	// Modify a skill to simulate an "old" disk state distinct from embedded.
	entries, _ := filepath.Glob(filepath.Join(dir, "skills", "*", "SKILL.md"))
	if len(entries) == 0 {
		t.Fatal("no SKILL.md files found")
	}
	if err := os.WriteFile(entries[0], []byte("old embedded content"), 0644); err != nil {
		t.Fatal(err)
	}
	// Compute the "old" disk hash (this simulates a previous embedded version).
	oldHash, err := ComputeDiskVersion(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Write installedVer = oldHash (disk == installed).
	if err := WriteVersionHash(dir, oldHash); err != nil {
		t.Fatal(err)
	}
	// Sanity: current embedded must differ from the old hash.
	if ComputeEmbeddedVersion() == oldHash {
		t.Skip("embedded matches old hash — cannot test known-embedded auto-refresh")
	}
	// Inject oldHash into known list → should return upgradeNeeded=true.
	cw, up, err := checkPluginState(dir, []string{oldHash})
	if err != nil {
		t.Fatalf("checkPluginState error: %v", err)
	}
	if cw {
		t.Errorf("known-embedded-auto-refresh: customWorkflow should be false")
	}
	if !up {
		t.Errorf("known-embedded-auto-refresh: upgradeNeeded should be true")
	}
}

// populatePluginDir writes the embedded plugin files to pluginDir.
func populatePluginDir(pluginDir string) error {
	return fs.WalkDir(FabrikPlugin, "fabrik-workflows", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel("fabrik-workflows", p)
		dest := filepath.Join(pluginDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0755)
		}
		data, err := FabrikPlugin.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0644)
	})
}
