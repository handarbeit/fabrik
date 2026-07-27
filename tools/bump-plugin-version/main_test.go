package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
}

// runIn runs a command in dir, failing the test with its combined output on error.
func runIn(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %s: %v", name, args, out, err)
	}
	return string(out)
}

const testManifest = `{
  "name": "fabrik",
  "version": "0.2.0",
  "description": "test plugin"
}
`

// initRepoWithPlugin creates a temp git repo with a plugin/fabrik/ directory
// (a manifest plus one source file), tags the initial commit v0.1.0, and
// returns the repo dir.
func initRepoWithPlugin(t *testing.T) (dir string, prevTag string) {
	t.Helper()
	dir = t.TempDir()

	mustWrite := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	mustWrite("plugin/fabrik/.claude-plugin/plugin.json", testManifest)
	mustWrite("plugin/fabrik/skills/foo/SKILL.md", "# Foo skill\n")
	mustWrite("README.md", "# repo\n")

	runIn(t, dir, "git", "init", "-b", "main")
	runIn(t, dir, "git", "config", "user.email", "test@test.com")
	runIn(t, dir, "git", "config", "user.name", "Test")
	runIn(t, dir, "git", "add", ".")
	runIn(t, dir, "git", "commit", "-m", "initial")
	runIn(t, dir, "git", "tag", "v0.1.0")

	return dir, "v0.1.0"
}

func TestSourceChanged_FiresOnNonManifestChange(t *testing.T) {
	skipIfNoGit(t)
	dir, prevTag := initRepoWithPlugin(t)

	skillPath := filepath.Join(dir, "plugin/fabrik/skills/foo/SKILL.md")
	if err := os.WriteFile(skillPath, []byte("# Foo skill (updated)\n"), 0o644); err != nil {
		t.Fatalf("update SKILL.md: %v", err)
	}
	runIn(t, dir, "git", "add", ".")
	runIn(t, dir, "git", "commit", "-m", "update skill")

	changed, err := sourceChanged(dir, "plugin/fabrik", "plugin/fabrik/.claude-plugin/plugin.json", prevTag)
	if err != nil {
		t.Fatalf("sourceChanged() error = %v", err)
	}
	if !changed {
		t.Fatal("sourceChanged() = false, want true after non-manifest source change")
	}
}

func TestSourceChanged_NoBumpWhenUnrelatedFileChanges(t *testing.T) {
	skipIfNoGit(t)
	dir, prevTag := initRepoWithPlugin(t)

	readmePath := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmePath, []byte("# repo (updated)\n"), 0o644); err != nil {
		t.Fatalf("update README.md: %v", err)
	}
	runIn(t, dir, "git", "add", ".")
	runIn(t, dir, "git", "commit", "-m", "update readme")

	changed, err := sourceChanged(dir, "plugin/fabrik", "plugin/fabrik/.claude-plugin/plugin.json", prevTag)
	if err != nil {
		t.Fatalf("sourceChanged() error = %v", err)
	}
	if changed {
		t.Fatal("sourceChanged() = true, want false when only files outside plugin dir changed")
	}
}

func TestSourceChanged_ManifestOnlyChangeExcluded(t *testing.T) {
	skipIfNoGit(t)
	dir, prevTag := initRepoWithPlugin(t)

	manifestPath := filepath.Join(dir, "plugin/fabrik/.claude-plugin/plugin.json")
	bumped := strings.Replace(testManifest, `"version": "0.2.0"`, `"version": "0.2.1"`, 1)
	if err := os.WriteFile(manifestPath, []byte(bumped), 0o644); err != nil {
		t.Fatalf("update manifest: %v", err)
	}
	runIn(t, dir, "git", "add", ".")
	runIn(t, dir, "git", "commit", "-m", "manual bump")

	changed, err := sourceChanged(dir, "plugin/fabrik", "plugin/fabrik/.claude-plugin/plugin.json", prevTag)
	if err != nil {
		t.Fatalf("sourceChanged() error = %v", err)
	}
	if changed {
		t.Fatal("sourceChanged() = true, want false when only the manifest itself changed")
	}
}

func TestSourceChanged_NoPriorTagErrors(t *testing.T) {
	skipIfNoGit(t)
	dir, _ := initRepoWithPlugin(t)

	if _, err := sourceChanged(dir, "plugin/fabrik", "plugin/fabrik/.claude-plugin/plugin.json", ""); err == nil {
		t.Fatal("sourceChanged() with empty prevTag = nil error, want error")
	}
}

func TestBumpPatchVersion_Success(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "plugin.json")
	if err := os.WriteFile(manifestPath, []byte(testManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	oldVer, newVer, err := bumpPatchVersion(manifestPath)
	if err != nil {
		t.Fatalf("bumpPatchVersion() error = %v", err)
	}
	if oldVer != "0.2.0" || newVer != "0.2.1" {
		t.Fatalf("bumpPatchVersion() = (%q, %q), want (0.2.0, 0.2.1)", oldVer, newVer)
	}

	updated, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read updated manifest: %v", err)
	}
	if !strings.Contains(string(updated), `"version": "0.2.1"`) {
		t.Fatalf("updated manifest does not contain new version:\n%s", updated)
	}
	if !strings.Contains(string(updated), `"name": "fabrik"`) {
		t.Fatalf("updated manifest lost unrelated fields:\n%s", updated)
	}
}

func TestBumpPatchVersion_MalformedVersionErrors(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "plugin.json")
	content := `{"name": "fabrik", "version": "not-a-version"}`
	if err := os.WriteFile(manifestPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	if _, _, err := bumpPatchVersion(manifestPath); err == nil {
		t.Fatal("bumpPatchVersion() with malformed version = nil error, want error")
	}
}

func TestBumpPatchVersion_MissingVersionErrors(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "plugin.json")
	content := `{"name": "fabrik"}`
	if err := os.WriteFile(manifestPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	if _, _, err := bumpPatchVersion(manifestPath); err == nil {
		t.Fatal("bumpPatchVersion() with missing version field = nil error, want error")
	}
}

func TestBumpPatchVersion_MultipleFieldsErrors(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "plugin.json")
	content := `{"version": "0.2.0", "nested": {"version": "0.2.0"}}`
	if err := os.WriteFile(manifestPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	if _, _, err := bumpPatchVersion(manifestPath); err == nil {
		t.Fatal("bumpPatchVersion() with multiple version fields = nil error, want error")
	}
}

func TestEndToEnd_BumpFiresThenNoBumpOnNextRelease(t *testing.T) {
	skipIfNoGit(t)
	dir, prevTag := initRepoWithPlugin(t)

	// Simulate a release cycle: plugin source changes, then cut-release.sh
	// would call sourceChanged() (true) and bumpPatchVersion() (0.2.0 -> 0.2.1).
	skillPath := filepath.Join(dir, "plugin/fabrik/skills/foo/SKILL.md")
	if err := os.WriteFile(skillPath, []byte("# Foo skill (updated)\n"), 0o644); err != nil {
		t.Fatalf("update SKILL.md: %v", err)
	}
	runIn(t, dir, "git", "add", ".")
	runIn(t, dir, "git", "commit", "-m", "update skill")

	manifestPath := filepath.Join(dir, "plugin/fabrik/.claude-plugin/plugin.json")
	changed, err := sourceChanged(dir, "plugin/fabrik", "plugin/fabrik/.claude-plugin/plugin.json", prevTag)
	if err != nil {
		t.Fatalf("sourceChanged() error = %v", err)
	}
	if !changed {
		t.Fatal("expected source changed after skill update")
	}
	oldVer, newVer, err := bumpPatchVersion(manifestPath)
	if err != nil {
		t.Fatalf("bumpPatchVersion() error = %v", err)
	}
	if oldVer != "0.2.0" || newVer != "0.2.1" {
		t.Fatalf("bumpPatchVersion() = (%q, %q), want (0.2.0, 0.2.1)", oldVer, newVer)
	}

	// Commit the bump and tag the new release, mirroring cut-release.sh's
	// step 4b + step 5.
	runIn(t, dir, "git", "add", ".")
	runIn(t, dir, "git", "commit", "-m", "bump plugin version")
	runIn(t, dir, "git", "tag", "v0.1.1")

	// Next release cycle with no further plugin source changes: no bump.
	readmePath := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmePath, []byte("# repo (again)\n"), 0o644); err != nil {
		t.Fatalf("update README.md: %v", err)
	}
	runIn(t, dir, "git", "add", ".")
	runIn(t, dir, "git", "commit", "-m", "update readme again")

	changed, err = sourceChanged(dir, "plugin/fabrik", "plugin/fabrik/.claude-plugin/plugin.json", "v0.1.1")
	if err != nil {
		t.Fatalf("sourceChanged() error = %v", err)
	}
	if changed {
		t.Fatal("sourceChanged() = true after unrelated change following the bump commit, want false (manifest-only prior bump must not trigger a recursive bump)")
	}
}
