package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExecute_UpgradeSubcommand invokes `fabrik upgrade` via Execute().
func TestExecute_UpgradeSubcommand(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	resetFlags()
	os.Args = []string{"fabrik", "upgrade"}
	if err := Execute(); err != nil {
		t.Fatalf("Execute upgrade: %v", err)
	}
	// Plugin dir should have been created
	if _, err := os.Stat(filepath.Join(dir, ".fabrik", "plugin")); os.IsNotExist(err) {
		t.Error(".fabrik/plugin not created by upgrade subcommand")
	}
}

// TestExecute_InitSubcommand invokes `fabrik init` via Execute().
func TestExecute_InitSubcommand(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	resetFlags()
	os.Args = []string{"fabrik", "init"}
	if err := Execute(); err != nil {
		t.Fatalf("Execute init: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".fabrik", "stages")); os.IsNotExist(err) {
		t.Error(".fabrik/stages not created by init subcommand")
	}
}

// TestRunInit_RunTwiceSkipsExisting verifies that without --force, existing files are skipped.
func TestRunInit_RunTwiceSkipsExisting(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	// First init writes files
	if err := runInit([]string{}); err != nil {
		t.Fatalf("first runInit: %v", err)
	}

	// Second init without force should skip all existing files
	if err := runInit([]string{}); err != nil {
		t.Fatalf("second runInit: %v", err)
	}
}

// TestRunInit_UnexpectedArgs returns an error for positional arguments.
func TestRunInit_UnexpectedArgs(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	err := runInit([]string{"unexpected"})
	if err == nil {
		t.Fatal("expected error for unexpected positional args")
	}
}

// TestWriteConfigTemplate_SkipsIfExists verifies that an existing config is skipped without --force.
func TestWriteConfigTemplate_SkipsIfExists(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	// Create .fabrik/config.yaml
	if err := os.MkdirAll(".fabrik", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".fabrik/config.yaml", []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := writeConfigTemplate(false); err != nil {
		t.Fatalf("writeConfigTemplate: %v", err)
	}

	// Should still have original content
	content, _ := os.ReadFile(".fabrik/config.yaml")
	if string(content) != "existing" {
		t.Errorf("content overwritten, expected 'existing', got %q", content)
	}
}

// TestWriteConfigTemplate_ForceOverwrites verifies that --force overwrites the config.
func TestWriteConfigTemplate_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	if err := os.MkdirAll(".fabrik", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".fabrik/config.yaml", []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := writeConfigTemplate(true); err != nil {
		t.Fatalf("writeConfigTemplate(force): %v", err)
	}

	content, _ := os.ReadFile(".fabrik/config.yaml")
	if string(content) == "old" {
		t.Error("config should have been overwritten with --force")
	}
}
