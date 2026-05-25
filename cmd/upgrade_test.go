package cmd

import (
	"bytes"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fabrikplugin "github.com/handarbeit/fabrik/plugin"
)

// buildPluginDir creates a temp dir with the full set of embedded plugin files
// and returns the path. It is equivalent to what refreshPlugin() writes.
func buildPluginDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugin")
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	err := fs.WalkDir(fabrikplugin.FabrikPlugin, "fabrik-workflows", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel("fabrik-workflows", p)
		dest := filepath.Join(pluginDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0755)
		}
		data, err := fabrikplugin.FabrikPlugin.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return pluginDir
}

func TestCheckPluginSkillsWithReader_NoDirNoOp(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugin") // does not exist

	var stderr bytes.Buffer
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	err := checkPluginSkillsWithReader(pluginDir, false, strings.NewReader(""))

	w.Close()
	os.Stderr = origStderr
	stderr.ReadFrom(r)

	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr output, got: %q", stderr.String())
	}
}

func TestCheckPluginSkillsWithReader_AllMatchNoOp(t *testing.T) {
	pluginDir := buildPluginDir(t)

	var stderr bytes.Buffer
	r, w, _ := os.Pipe()
	origStderr := os.Stderr
	os.Stderr = w

	err := checkPluginSkillsWithReader(pluginDir, false, strings.NewReader(""))

	w.Close()
	os.Stderr = origStderr
	stderr.ReadFrom(r)

	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr output for matching files, got: %q", stderr.String())
	}
}

// writeInstalledForDir writes an .installed-version seeded from the current disk state
// and temporarily registers the disk hash as a known embedded version so that
// CheckPluginState treats it as a legitimate installed version (upgradeNeeded=true)
// rather than a corrupted migration (customWorkflow=true).
//
// Note: not parallel-safe due to KnownEmbeddedVersions mutation.
func writeInstalledForDir(t *testing.T, pluginDir string) {
	t.Helper()
	diskVer, err := fabrikplugin.ComputeDiskVersion(pluginDir)
	if err != nil {
		t.Fatalf("ComputeDiskVersion: %v", err)
	}
	if err := fabrikplugin.WriteVersionHash(pluginDir, diskVer); err != nil {
		t.Fatalf("WriteVersionHash: %v", err)
	}
	// Register diskVer as a known embedded version so CheckPluginState treats
	// disk==installed as a legitimate auto-refresh case, not a corrupted migration.
	orig := fabrikplugin.KnownEmbeddedVersions
	fabrikplugin.KnownEmbeddedVersions = append(fabrikplugin.KnownEmbeddedVersions, diskVer)
	t.Cleanup(func() { fabrikplugin.KnownEmbeddedVersions = orig })
}

// TestCheckPluginSkillsWithReader_CustomWorkflow_NonTTYWarning verifies that
// when disk files differ from the recorded installed-version (operator customization),
// the non-TTY path emits a warning and does NOT auto-refresh.
func TestCheckPluginSkillsWithReader_CustomWorkflow_NonTTYWarning(t *testing.T) {
	pluginDir := buildPluginDir(t)
	// Seed installed = embedded (disk matches embedded at this point).
	if err := fabrikplugin.WriteInstalledVersion(pluginDir); err != nil {
		t.Fatal(err)
	}
	// Now modify a disk file — disk diverges from installed (customWorkflow case).
	entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
	if err != nil || len(entries) == 0 {
		t.Fatal("no SKILL.md files found in test plugin dir")
	}
	if err := os.WriteFile(entries[0], []byte("operator customization"), 0644); err != nil {
		t.Fatal(err)
	}

	r, w, _ := os.Pipe()
	origStderr := os.Stderr
	os.Stderr = w

	callErr := checkPluginSkillsWithReader(pluginDir, false, strings.NewReader(""))

	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	if callErr != nil {
		t.Fatalf("expected nil error, got %v", callErr)
	}
	if !strings.Contains(output, "local customizations") {
		t.Fatalf("expected customization warning on stderr, got: %q", output)
	}
	// File must NOT have been overwritten.
	data, _ := os.ReadFile(entries[0])
	if string(data) != "operator customization" {
		t.Fatal("custom-workflow non-TTY path should NOT overwrite operator customizations")
	}
}

// TestCheckPluginSkillsWithReader_AutoRefresh_NonTTY verifies that when disk==installed
// and embedded has changed (upgradeNeeded), non-TTY silently auto-refreshes.
func TestCheckPluginSkillsWithReader_AutoRefresh_NonTTY(t *testing.T) {
	pluginDir := buildPluginDir(t)
	// Simulate "old" installed state: write old content and seed installed from it.
	entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
	if err != nil || len(entries) == 0 {
		t.Fatal("no SKILL.md files found in test plugin dir")
	}
	if err := os.WriteFile(entries[0], []byte("old content"), 0644); err != nil {
		t.Fatal(err)
	}
	// Seed installed = current disk (which has old content). So disk == installed.
	writeInstalledForDir(t, pluginDir)
	// Now embedded != disk (because we wrote old content to disk).

	r, w, _ := os.Pipe()
	origStderr := os.Stderr
	os.Stderr = w

	callErr := checkPluginSkillsWithReader(pluginDir, false, strings.NewReader(""))

	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	if callErr != nil {
		t.Fatalf("expected nil error, got %v", callErr)
	}
	if !strings.Contains(output, "auto-refreshed") {
		t.Fatalf("expected auto-refresh message on stderr, got: %q", output)
	}
	// File must have been refreshed to match embedded.
	rel, _ := filepath.Rel(pluginDir, entries[0])
	embeddedData, _ := fabrikplugin.FabrikPlugin.ReadFile(filepath.Join("fabrik-workflows", rel))
	diskData, _ := os.ReadFile(entries[0])
	if !bytes.Equal(embeddedData, diskData) {
		t.Fatal("non-TTY auto-refresh path should overwrite disk with embedded content")
	}
}

// TestCheckPluginSkillsWithReader_CustomWorkflow_TTYRedirects verifies that
// when operator customizations are detected, the TTY path shows a redirect
// message (--force / --reconcile) instead of the y/N prompt.
func TestCheckPluginSkillsWithReader_CustomWorkflow_TTYRedirects(t *testing.T) {
	pluginDir := buildPluginDir(t)
	if err := fabrikplugin.WriteInstalledVersion(pluginDir); err != nil {
		t.Fatal(err)
	}
	entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
	if err != nil || len(entries) == 0 {
		t.Fatal("no SKILL.md files found in test plugin dir")
	}
	modifiedFile := entries[0]
	if err := os.WriteFile(modifiedFile, []byte("operator customization"), 0644); err != nil {
		t.Fatal(err)
	}

	r, w, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = w

	callErr := checkPluginSkillsWithReader(pluginDir, true, strings.NewReader("y\n"))

	w.Close()
	os.Stdout = origStdout
	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	if callErr != nil {
		t.Fatalf("expected nil error, got %v", callErr)
	}
	if !strings.Contains(output, "--force") {
		t.Fatalf("expected redirect to --force in output, got: %q", output)
	}
	// File must NOT have been overwritten.
	data, _ := os.ReadFile(modifiedFile)
	if string(data) != "operator customization" {
		t.Fatal("TTY custom-workflow path should NOT overwrite operator customizations")
	}
}

// TestCheckPluginSkillsWithReader_AutoRefresh_TTYYes verifies the TTY y answer
// triggers auto-refresh when upgradeNeeded (disk==installed, embedded changed).
func TestCheckPluginSkillsWithReader_AutoRefresh_TTYYes(t *testing.T) {
	pluginDir := buildPluginDir(t)
	entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
	if err != nil || len(entries) == 0 {
		t.Fatal("no SKILL.md files found in test plugin dir")
	}
	modifiedFile := entries[0]
	if err := os.WriteFile(modifiedFile, []byte("old content"), 0644); err != nil {
		t.Fatal(err)
	}
	// Seed installed = current disk (old content). So disk == installed, embedded != installed.
	writeInstalledForDir(t, pluginDir)

	r, w, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = w

	callErr := checkPluginSkillsWithReader(pluginDir, true, strings.NewReader("y\n"))

	w.Close()
	os.Stdout = origStdout
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if callErr != nil {
		t.Fatalf("expected nil error, got %v", callErr)
	}
	// File must now match embedded version.
	rel, _ := filepath.Rel(pluginDir, modifiedFile)
	embeddedData, _ := fabrikplugin.FabrikPlugin.ReadFile(filepath.Join("fabrik-workflows", rel))
	diskData, _ := os.ReadFile(modifiedFile)
	if !bytes.Equal(embeddedData, diskData) {
		t.Fatal("expected file to be refreshed with embedded content after y answer")
	}
}

// TestCheckPluginSkillsWithReader_AutoRefresh_TTYNo verifies that answering n
// keeps files unmodified when upgradeNeeded (disk==installed, embedded changed).
func TestCheckPluginSkillsWithReader_AutoRefresh_TTYNo(t *testing.T) {
	for _, answer := range []string{"n\n", "N\n", "\n"} {
		t.Run(answer, func(t *testing.T) {
			pluginDir := buildPluginDir(t)
			entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
			if err != nil || len(entries) == 0 {
				t.Fatal("no SKILL.md files found in test plugin dir")
			}
			modifiedFile := entries[0]
			if err := os.WriteFile(modifiedFile, []byte("old content"), 0644); err != nil {
				t.Fatal(err)
			}
			writeInstalledForDir(t, pluginDir)

			r, w, _ := os.Pipe()
			origStdout := os.Stdout
			os.Stdout = w

			callErr := checkPluginSkillsWithReader(pluginDir, true, strings.NewReader(answer))

			w.Close()
			os.Stdout = origStdout
			r.Close()

			if callErr != nil {
				t.Fatalf("expected nil error for answer %q, got %v", answer, callErr)
			}
			data, _ := os.ReadFile(modifiedFile)
			if string(data) != "old content" {
				t.Fatalf("answer %q: expected file to remain unmodified", answer)
			}
		})
	}
}

func TestRunUpgrade_HelpFlag(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	err := runUpgrade([]string{"--help"})
	if err != flag.ErrHelp {
		t.Errorf("expected flag.ErrHelp, got %v", err)
	}
}

// buildFabrikPluginDir writes embedded plugin files to .fabrik/plugin/ in cwd.
// The caller must have already chdirTest'd to a temp dir.
func buildFabrikPluginDir(t *testing.T) string {
	t.Helper()
	pluginDir := ".fabrik/plugin"
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	err := fs.WalkDir(fabrikplugin.FabrikPlugin, "fabrik-workflows", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel("fabrik-workflows", p)
		dest := filepath.Join(pluginDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0755)
		}
		data, err := fabrikplugin.FabrikPlugin.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return pluginDir
}

func TestRunUpgrade_Reconcile(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	r, w, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = w

	err := runUpgrade([]string{"--reconcile"})

	w.Close()
	os.Stdout = origStdout
	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !strings.Contains(output, "reconcile") {
		t.Fatalf("expected reconciliation prompt in output, got: %q", output)
	}
}

func TestRunUpgrade_Force(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	pluginDir := buildFabrikPluginDir(t)

	if err := fabrikplugin.WriteInstalledVersion(pluginDir); err != nil {
		t.Fatal(err)
	}
	entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
	if err != nil || len(entries) == 0 {
		t.Fatal("no SKILL.md files found")
	}
	modifiedFile := entries[0]
	if err := os.WriteFile(modifiedFile, []byte("operator customization"), 0644); err != nil {
		t.Fatal(err)
	}

	callErr := runUpgrade([]string{"--force"})
	if callErr != nil {
		t.Fatalf("expected nil error, got %v", callErr)
	}

	rel, _ := filepath.Rel(pluginDir, modifiedFile)
	embeddedData, _ := fabrikplugin.FabrikPlugin.ReadFile(filepath.Join("fabrik-workflows", rel))
	diskData, _ := os.ReadFile(modifiedFile)
	if !bytes.Equal(embeddedData, diskData) {
		t.Fatal("--force should overwrite operator customization with embedded content")
	}

	installedVer, readErr := fabrikplugin.ReadInstalledVersion(pluginDir)
	if readErr != nil || installedVer == "" {
		t.Fatalf("expected non-empty installed version after --force, err=%v", readErr)
	}
}

func TestRunUpgrade_CustomWorkflowError(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	pluginDir := buildFabrikPluginDir(t)

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

	callErr := runUpgrade([]string{})
	if callErr == nil {
		t.Fatal("expected error for custom-workflow state, got nil")
	}
	if !strings.Contains(callErr.Error(), "local customizations") {
		t.Fatalf("expected 'local customizations' in error, got: %v", callErr)
	}
}
