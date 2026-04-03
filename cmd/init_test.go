package cmd

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/verveguy/fabrik/stages"
)

func TestRunInit_WritesFiles(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	if err := runInit([]string{}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, ".fabrik", "stages"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	// Count embedded source files to verify all were written.
	embedded, err := fs.ReadDir(stages.DefaultStages, "examples")
	if err != nil {
		t.Fatalf("reading embedded stages: %v", err)
	}
	embeddedFiles := 0
	for _, e := range embedded {
		if !e.IsDir() {
			embeddedFiles++
		}
	}
	writtenFiles := 0
	for _, e := range entries {
		if !e.IsDir() {
			writtenFiles++
		}
	}
	if writtenFiles != embeddedFiles {
		t.Fatalf("expected %d file(s) written, got %d", embeddedFiles, writtenFiles)
	}

	// Verify each written file matches the embedded source exactly.
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		written, err := os.ReadFile(filepath.Join(dir, ".fabrik", "stages", e.Name()))
		if err != nil {
			t.Fatalf("reading written file %s: %v", e.Name(), err)
		}
		source, err := stages.DefaultStages.ReadFile("examples/" + e.Name())
		if err != nil {
			t.Fatalf("reading embedded source %s: %v", e.Name(), err)
		}
		if string(written) != string(source) {
			t.Errorf("file %s: content mismatch", e.Name())
		}
	}
}

func TestRunInit_SkipsExistingFiles(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	// First init — writes all files.
	if err := runInit([]string{}); err != nil {
		t.Fatalf("first runInit: %v", err)
	}

	// Overwrite one file with sentinel content.
	stagesDir := filepath.Join(dir, ".fabrik", "stages")
	entries, err := os.ReadDir(stagesDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no files written by first init")
	}
	sentinel := []byte("sentinel content")
	targetPath := filepath.Join(stagesDir, entries[0].Name())
	if err := os.WriteFile(targetPath, sentinel, 0644); err != nil {
		t.Fatal(err)
	}

	// Second init — should skip the existing file.
	if err := runInit([]string{}); err != nil {
		t.Fatalf("second runInit: %v", err)
	}

	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sentinel) {
		t.Errorf("existing file was overwritten; want sentinel, got %q", string(got))
	}
}

func TestRunInit_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	// First init.
	if err := runInit([]string{}); err != nil {
		t.Fatalf("first runInit: %v", err)
	}

	stagesDir := filepath.Join(dir, ".fabrik", "stages")
	entries, err := os.ReadDir(stagesDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no files written by first init")
	}

	// Overwrite one file with sentinel.
	sentinel := []byte("sentinel content")
	targetPath := filepath.Join(stagesDir, entries[0].Name())
	if err := os.WriteFile(targetPath, sentinel, 0644); err != nil {
		t.Fatal(err)
	}

	// Second init with --force — should overwrite.
	if err := runInit([]string{"--force"}); err != nil {
		t.Fatalf("force runInit: %v", err)
	}

	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == string(sentinel) {
		t.Error("--force did not overwrite existing file")
	}
}

func TestRunInit_WritesConfigYAML(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	if err := runInit([]string{}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".fabrik", "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml not written: %v", err)
	}
	content := string(data)
	// All required fields should be commented out in non-interactive mode
	if !strings.Contains(content, "# owner:") {
		t.Error("expected '# owner:' in config.yaml template")
	}
	if !strings.Contains(content, "# repo:") {
		t.Error("expected '# repo:' in config.yaml template")
	}
	if !strings.Contains(content, "# project:") {
		t.Error("expected '# project:' in config.yaml template")
	}
	if !strings.Contains(content, "# user:") {
		t.Error("expected '# user:' in config.yaml template")
	}
}

func TestRunInit_ConfigYAMLNotOverwrittenWithoutForce(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	// First init writes the template
	if err := runInit([]string{}); err != nil {
		t.Fatalf("first runInit: %v", err)
	}

	// Overwrite with sentinel
	configPath := filepath.Join(dir, ".fabrik", "config.yaml")
	sentinel := []byte("owner: sentinel\n")
	if err := os.WriteFile(configPath, sentinel, 0644); err != nil {
		t.Fatal(err)
	}

	// Second init without --force should skip
	if err := runInit([]string{}); err != nil {
		t.Fatalf("second runInit: %v", err)
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sentinel) {
		t.Errorf("config.yaml was overwritten without --force; want sentinel, got %q", string(got))
	}
}

func TestRunInit_ConfigYAMLOverwrittenWithForce(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	// Write sentinel first
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	configPath := filepath.Join(dir, ".fabrik", "config.yaml")
	if err := os.WriteFile(configPath, []byte("owner: sentinel\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Init with --force should overwrite
	if err := runInit([]string{"--force"}); err != nil {
		t.Fatalf("force runInit: %v", err)
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "owner: sentinel\n" {
		t.Error("--force did not overwrite config.yaml")
	}
	if !strings.Contains(string(got), "# owner:") {
		t.Error("expected template content after --force overwrite")
	}
}

func TestRunInit_RejectsPositionalArgs(t *testing.T) {
	if err := runInit([]string{"unexpected"}); err == nil {
		t.Fatal("expected error for unexpected positional argument, got nil")
	}
}

func TestRunInit_IdempotentDestDir(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	// Running init twice should not error even if .fabrik/stages already exists.
	if err := runInit([]string{}); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	if err := runInit([]string{}); err != nil {
		t.Fatalf("second runInit: %v", err)
	}
}
