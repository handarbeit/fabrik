package plugin

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// TestFabrikPlugin_EmbedsLabelsMd is the direct assertion for #1339 AC1:
// LABELS.md must be present in the built binary's embed FS, asserted
// against plugin.FabrikPlugin itself (not the source tree, which
// TestRunInit_WritesPluginFiles-style tests would only indirectly cover).
func TestFabrikPlugin_EmbedsLabelsMd(t *testing.T) {
	content, err := FabrikPlugin.ReadFile("fabrik-workflows/LABELS.md")
	if err != nil {
		t.Fatalf("LABELS.md not present in FabrikPlugin embed: %v", err)
	}
	const minNonTrivialBytes = 1024
	if len(content) < minNonTrivialBytes {
		t.Errorf("LABELS.md embed content suspiciously small: %d bytes (want at least %d)", len(content), minNonTrivialBytes)
	}
}

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

	cw, up, stale, err := CheckPluginState(dir, false)
	if err != nil {
		t.Fatalf("CheckPluginState error: %v", err)
	}
	if cw || up || stale {
		t.Errorf("migration: want (false,false,false), got (%v,%v,%v)", cw, up, stale)
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

	cw, up, stale, err := CheckPluginState(dir, false)
	if err != nil {
		t.Fatalf("CheckPluginState error: %v", err)
	}
	if cw || up || stale {
		t.Errorf("no-op: want (false,false,false), got (%v,%v,%v)", cw, up, stale)
	}
}

func TestCheckPluginState_AutoRefresh(t *testing.T) {
	// Simulate a previous embedded version on disk: populate with embedded files,
	// modify one to get a hash distinct from the current embedded. Then seed
	// installedVer = diskVer (disk == installed != embedded) with the old hash in
	// the known list — verifies the auto-refresh path returns upgradeNeeded=true.
	dir2 := t.TempDir()
	if err := populatePluginDir(dir2); err != nil {
		t.Fatal(err)
	}
	entries, _ := filepath.Glob(filepath.Join(dir2, "skills", "*", "SKILL.md"))
	if len(entries) == 0 {
		t.Fatal("no SKILL.md files found")
	}
	if err := os.WriteFile(entries[0], []byte("old plugin content"), 0644); err != nil {
		t.Fatal(err)
	}
	diskVer2, err := ComputeDiskVersion(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteVersionHash(dir2, diskVer2); err != nil {
		t.Fatal(err)
	}
	embeddedVer := ComputeEmbeddedVersion()
	if embeddedVer == diskVer2 {
		t.Skip("embedded matches modified disk — cannot test auto-refresh")
	}

	// Inject diskVer2 as a "known" version so the corrupted-state guard passes.
	cw, up, stale, err := checkPluginState(dir2, []string{diskVer2}, false)
	if err != nil {
		t.Fatalf("checkPluginState error: %v", err)
	}
	if cw {
		t.Errorf("auto-refresh: customWorkflow should be false")
	}
	if !up {
		t.Errorf("auto-refresh: upgradeNeeded should be true")
	}
	if !stale {
		t.Errorf("auto-refresh: stale should be true (installedVer != embeddedVer)")
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

	cw, up, stale, err := CheckPluginState(dir, false)
	if err != nil {
		t.Fatalf("CheckPluginState error: %v", err)
	}
	if !cw {
		t.Errorf("custom-workflow: customWorkflow should be true")
	}
	if up {
		t.Errorf("custom-workflow: upgradeNeeded should be false")
	}
	if stale {
		t.Errorf("custom-workflow: stale should be false (installedVer == embeddedVer, only disk was mutated after)")
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

	cw, up, stale, err := CheckPluginState(dir, false)
	if err != nil {
		t.Fatalf("CheckPluginState error: %v", err)
	}
	if !cw {
		t.Errorf("migration-customised: customWorkflow should be true")
	}
	if up {
		t.Errorf("migration-customised: upgradeNeeded should be false")
	}
	if stale {
		t.Errorf("migration-customised: stale should be false (no installedVer to compare)")
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

	cw, up, stale, err := checkPluginState(dir, []string{}, false)
	if err != nil {
		t.Fatalf("checkPluginState error: %v", err)
	}
	if !cw {
		t.Errorf("corrupted-migration: customWorkflow should be true")
	}
	if up {
		t.Errorf("corrupted-migration: upgradeNeeded should be false")
	}
	if !stale {
		t.Errorf("corrupted-migration: stale should be true (installedVer != embeddedVer)")
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
	cw, up, stale, err := checkPluginState(dir, []string{oldHash}, false)
	if err != nil {
		t.Fatalf("checkPluginState error: %v", err)
	}
	if cw {
		t.Errorf("known-embedded-auto-refresh: customWorkflow should be false")
	}
	if !up {
		t.Errorf("known-embedded-auto-refresh: upgradeNeeded should be true")
	}
	if !stale {
		t.Errorf("known-embedded-auto-refresh: stale should be true (installedVer != embeddedVer)")
	}
}

// TestCheckPluginState_DevBuildUnlistedFingerprint verifies that when
// installedVer is present, disk==installed, embedded differs, and installedVer
// is not in knownVersions, a dev build (isDevBuild=true) still auto-refreshes
// (upgradeNeeded=true) rather than being misclassified as a custom workflow —
// the fix for issue #1297. Mirrors TestCheckPluginState_CorruptedMigration's
// setup exactly, differing only in isDevBuild and the expected outcome.
func TestCheckPluginState_DevBuildUnlistedFingerprint(t *testing.T) {
	dir := t.TempDir()
	if err := populatePluginDir(dir); err != nil {
		t.Fatal(err)
	}
	// Modify a skill to create a disk state distinct from the current embedded
	// content — this simulates a dev build's own (unlisted) embedded fingerprint
	// having previously been written as installedVer.
	entries, _ := filepath.Glob(filepath.Join(dir, "skills", "*", "SKILL.md"))
	if len(entries) == 0 {
		t.Fatal("no SKILL.md files found")
	}
	if err := os.WriteFile(entries[0], []byte("dev build content"), 0644); err != nil {
		t.Fatal(err)
	}
	diskVer, err := ComputeDiskVersion(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a prior dev-build refresh: installedVer = disk fingerprint, never
	// registered in KnownEmbeddedVersions (dev fingerprints structurally can't be).
	if err := WriteVersionHash(dir, diskVer); err != nil {
		t.Fatal(err)
	}
	for _, known := range KnownEmbeddedVersions {
		if diskVer == known {
			t.Skip("diskVer accidentally matches a known embedded version — cannot test dev-build path")
		}
	}
	embeddedVer := ComputeEmbeddedVersion()
	if embeddedVer == diskVer {
		t.Skip("embedded matches dev-build disk — cannot test dev-build path")
	}

	cw, up, stale, err := checkPluginState(dir, []string{}, true)
	if err != nil {
		t.Fatalf("checkPluginState error: %v", err)
	}
	if cw {
		t.Errorf("dev-build-unlisted-fingerprint: customWorkflow should be false, got true")
	}
	if !up {
		t.Errorf("dev-build-unlisted-fingerprint: upgradeNeeded should be true, got false")
	}
	if !stale {
		t.Errorf("dev-build-unlisted-fingerprint: stale should be true (installedVer != embeddedVer)")
	}
}

// TestCheckPluginState_CustomizedAndStale verifies AC1: a plugin that is both
// customized (diskVer != installedVer) AND stale (installedVer != embeddedVer)
// reports both facts independently — the fix for #1787. Before this fix,
// checkPluginState returned as soon as it detected diskVer != installedVer and
// never computed the embeddedVer comparison in that branch, so stale was always
// false whenever customWorkflow was true.
func TestCheckPluginState_CustomizedAndStale(t *testing.T) {
	dir := t.TempDir()
	if err := populatePluginDir(dir); err != nil {
		t.Fatal(err)
	}
	// Simulate an old installed version: mutate one file to get a hash distinct
	// from current embedded, then seed installedVer to that old hash (mirrors
	// TestCheckPluginState_AutoRefresh's setup).
	entries, _ := filepath.Glob(filepath.Join(dir, "skills", "*", "SKILL.md"))
	if len(entries) < 2 {
		t.Fatal("need at least 2 SKILL.md files for this test")
	}
	if err := os.WriteFile(entries[0], []byte("old embedded content"), 0644); err != nil {
		t.Fatal(err)
	}
	oldHash, err := ComputeDiskVersion(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteVersionHash(dir, oldHash); err != nil {
		t.Fatal(err)
	}
	embeddedVer := ComputeEmbeddedVersion()
	if embeddedVer == oldHash {
		t.Skip("embedded matches old hash — cannot test customized-and-stale")
	}

	// Now mutate a *different* file on disk so diskVer != installedVer too —
	// this is the operator customization on top of the already-stale install.
	if err := os.WriteFile(entries[1], []byte("operator customization"), 0644); err != nil {
		t.Fatal(err)
	}

	// Register oldHash as known so the corrupted-migration guard doesn't
	// misclassify this scenario — this test is specifically about the
	// diskVer != installedVer branch, not the corrupted-state guard.
	cw, up, stale, err := checkPluginState(dir, []string{oldHash}, false)
	if err != nil {
		t.Fatalf("checkPluginState error: %v", err)
	}
	if !cw {
		t.Errorf("customized-and-stale: customWorkflow should be true, got false")
	}
	if up {
		t.Errorf("customized-and-stale: upgradeNeeded should be false, got true")
	}
	if !stale {
		t.Errorf("customized-and-stale: stale should be true, got false — this is the #1787 bug: staleness must be reported even when customized")
	}
}

func TestVersionsBehind_Found(t *testing.T) {
	known := []string{"v1", "v2", "v3", "v4"}
	n, ok := versionsBehind("v2", "v4", known)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if n != 2 {
		t.Errorf("versionsBehind(v2, v4) = %d, want 2", n)
	}
}

func TestVersionsBehind_NotFound(t *testing.T) {
	known := []string{"v1", "v2", "v3"}
	n, ok := versionsBehind("unknown-dev-hash", "v3", known)
	if ok {
		t.Errorf("expected ok=false for unrecognized installedVer, got n=%d", n)
	}
}

func TestVersionsBehind_EmbeddedNewerThanKnownList(t *testing.T) {
	// embeddedVer itself is not in the known list (e.g. a dev build, or a
	// release cut between cut-release.sh runs) — treat it as newer than every
	// known entry.
	known := []string{"v1", "v2", "v3"}
	n, ok := versionsBehind("v1", "dev-build-hash-not-in-list", known)
	if !ok {
		t.Fatal("expected ok=true (installedVer v1 is known)")
	}
	if n != len(known) {
		t.Errorf("versionsBehind(v1, unlisted) = %d, want %d (len(known))", n, len(known))
	}
}

func TestVersionsBehind_SameVersion(t *testing.T) {
	known := []string{"v1", "v2", "v3"}
	n, ok := versionsBehind("v2", "v2", known)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if n != 0 {
		t.Errorf("versionsBehind(v2, v2) = %d, want 0", n)
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

// TestRefreshPluginTo_PrunesSkillsRenamedByEarlierReleases pins #2140: the
// skills lost their fabrik- prefix, and a refresh over a tree from any earlier
// release must end up with exactly the embed, not both skill sets.
func TestRefreshPluginTo_PrunesSkillsRenamedByEarlierReleases(t *testing.T) {
	for _, tag := range []string{"v0.0.64", "v0.0.70", "v0.0.77"} {
		t.Run(tag, func(t *testing.T) {
			dir := t.TempDir()
			copyTree(t, filepath.Join("testdata", "migration", tag, "plugin"), dir)
			if _, err := os.Stat(filepath.Join(dir, "skills", "fabrik-review", "SKILL.md")); err != nil {
				t.Fatalf("fixture precondition: %v", err)
			}
			if _, err := RefreshPluginTo(dir); err != nil {
				t.Fatalf("RefreshPluginTo: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "skills", "fabrik-review")); !os.IsNotExist(err) {
				t.Errorf("old skills/fabrik-review survived the refresh (err=%v)", err)
			}
			diskVer, err := ComputeDiskVersion(dir)
			if err != nil {
				t.Fatal(err)
			}
			if diskVer != ComputeEmbeddedVersion() {
				t.Errorf("tree after refresh differs from the embed")
			}
			// Second refresh is a no-op.
			if _, err := RefreshPluginTo(dir); err != nil {
				t.Fatal(err)
			}
			again, _ := ComputeDiskVersion(dir)
			if again != diskVer {
				t.Errorf("second refresh changed the tree")
			}
		})
	}
}

func TestRefreshPluginTo_KeepsEditedAndOperatorFiles(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("testdata", "migration", "v0.0.77", "plugin"), dir)
	edited := filepath.Join(dir, "skills", "fabrik-review", "SKILL.md")
	if err := os.WriteFile(edited, []byte("my own review rules\n"), 0644); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(dir, "skills", "my-skill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(own), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, []byte("mine\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := RefreshPluginTo(dir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{edited, own} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("operator file %s was removed: %v", p, err)
		}
	}
	// The unedited old skills are gone, and the tree is still customised.
	if _, err := os.Stat(filepath.Join(dir, "skills", "fabrik-plan")); !os.IsNotExist(err) {
		t.Errorf("unedited skills/fabrik-plan survived (err=%v)", err)
	}
	c, err := ClassifyDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Customised() {
		t.Errorf("tree with kept operator files must stay customised")
	}
}

// embedFileCount counts the files in the embedded plugin tree.
func embedFileCount(t *testing.T) int {
	t.Helper()
	n := 0
	err := fs.WalkDir(FabrikPlugin, "fabrik-workflows", func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr == nil && !d.IsDir() {
			n++
		}
		return walkErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// fileMtimes returns every regular file under dir keyed by relative path.
func fileMtimes(t *testing.T, dir string) map[string]time.Time {
	t.Helper()
	out := map[string]time.Time{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[rel] = info.ModTime()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRefreshPluginTo_CountsChangesNotWrites pins #2219 SC-001: the first run
// returns the embed's file count, the second returns 0 and rewrites nothing.
func TestRefreshPluginTo_CountsChangesNotWrites(t *testing.T) {
	dir := t.TempDir()
	n, err := RefreshPluginTo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := embedFileCount(t); n != want {
		t.Fatalf("first refresh = %d, want embed size %d", n, want)
	}
	// Age every file so a rewrite is visible whatever the mtime granularity.
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	for rel := range fileMtimes(t, dir) {
		if err := os.Chtimes(filepath.Join(dir, rel), old, old); err != nil {
			t.Fatal(err)
		}
	}
	before := fileMtimes(t, dir)
	n, err = RefreshPluginTo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("second refresh = %d, want 0", n)
	}
	for rel, mt := range fileMtimes(t, dir) {
		if !mt.Equal(before[rel]) {
			t.Errorf("%s was rewritten by a no-op refresh", rel)
		}
	}
}

// TestRefreshPluginTo_CountsOnlyAffectedFiles covers a deleted file, an edited
// file and an obsolete shipped file: three changes, and only those are touched.
func TestRefreshPluginTo_CountsOnlyAffectedFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := RefreshPluginTo(dir); err != nil {
		t.Fatal(err)
	}
	var files []string
	for rel := range fileMtimes(t, dir) {
		files = append(files, rel)
	}
	sort.Strings(files)
	var data []string
	for _, f := range files {
		if f != installedVersionFile {
			data = append(data, f)
		}
	}
	if len(data) < 3 {
		t.Fatalf("embed too small for the test: %v", data)
	}
	deleted, edited := data[0], data[1]
	if err := os.Remove(filepath.Join(dir, deleted)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, edited), []byte("edited\n"), 0644); err != nil {
		t.Fatal(err)
	}
	obsolete := filepath.Join("skills", "fabrik-review", "SKILL.md")
	src, err := os.ReadFile(filepath.Join("testdata", "migration", "v0.0.77", "plugin", obsolete))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, obsolete)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, obsolete), src, 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	for _, f := range data[2:] {
		if err := os.Chtimes(filepath.Join(dir, f), old, old); err != nil {
			t.Fatal(err)
		}
	}
	n, err := RefreshPluginTo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("refresh = %d, want 3 (created + changed + pruned)", n)
	}
	for _, f := range data[2:] {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(old) {
			t.Errorf("untouched file %s was rewritten", f)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, obsolete)); !os.IsNotExist(err) {
		t.Errorf("obsolete shipped file survived (err=%v)", err)
	}
}

func TestWriteVersionHash_SkipsMatchingFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteVersionHash(dir, "abc"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, installedVersionFile)
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if err := WriteVersionHash(dir, "abc"); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(p); !info.ModTime().Equal(old) {
		t.Errorf("matching .installed-version was rewritten")
	}
	if err := WriteVersionHash(dir, "def"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadInstalledVersion(dir); got != "def" {
		t.Errorf("stale .installed-version not updated: %q", got)
	}
}
