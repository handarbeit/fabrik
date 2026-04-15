package cmd

import (
	"bytes"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fabrikplugin "github.com/verveguy/fabrik/plugin"
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
	err := fs.WalkDir(fabrikplugin.FabrikPlugin, "fabrik-plugin", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel("fabrik-plugin", p)
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

func TestCheckPluginSkillsWithReader_DifferNonTTYWarning(t *testing.T) {
	pluginDir := buildPluginDir(t)

	// Corrupt one file on disk.
	entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
	if err != nil || len(entries) == 0 {
		t.Fatal("no SKILL.md files found in test plugin dir")
	}
	if err := os.WriteFile(entries[0], []byte("modified content"), 0644); err != nil {
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
	if !strings.Contains(output, "auto-refreshed") {
		t.Fatalf("expected auto-refresh message on stderr, got: %q", output)
	}

	// File must have been refreshed (non-TTY silently updates).
	data, _ := os.ReadFile(entries[0])
	if string(data) == "modified content" {
		t.Fatal("non-TTY path should auto-refresh differing files")
	}
}

func TestCheckPluginSkillsWithReader_DifferTTYYes(t *testing.T) {
	pluginDir := buildPluginDir(t)

	entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
	if err != nil || len(entries) == 0 {
		t.Fatal("no SKILL.md files found in test plugin dir")
	}
	modifiedFile := entries[0]
	if err := os.WriteFile(modifiedFile, []byte("modified content"), 0644); err != nil {
		t.Fatal(err)
	}

	// Use a pipe for stdout capture.
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

	// The modified file must now match the embedded version.
	rel, _ := filepath.Rel(pluginDir, modifiedFile)
	embeddedData, _ := fabrikplugin.FabrikPlugin.ReadFile(filepath.Join("fabrik-plugin", rel))
	diskData, _ := os.ReadFile(modifiedFile)
	if !bytes.Equal(embeddedData, diskData) {
		t.Fatal("expected modified file to be overwritten with embedded content")
	}
}

func TestCheckPluginSkillsWithReader_DifferTTYNo(t *testing.T) {
	for _, answer := range []string{"n\n", "N\n", "\n"} {
		t.Run(answer, func(t *testing.T) {
			pluginDir := buildPluginDir(t)

			entries, err := filepath.Glob(filepath.Join(pluginDir, "skills", "*", "SKILL.md"))
			if err != nil || len(entries) == 0 {
				t.Fatal("no SKILL.md files found in test plugin dir")
			}
			modifiedFile := entries[0]
			if err := os.WriteFile(modifiedFile, []byte("modified content"), 0644); err != nil {
				t.Fatal(err)
			}

			// Suppress stdout.
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
			if string(data) != "modified content" {
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
