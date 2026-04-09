// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSaveSessionIDDirect_EmptyID_IsNoop verifies that an empty session ID is skipped.
func TestSaveSessionIDDirect_EmptyID_IsNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions", "session.txt")
	saveSessionIDDirect(path, "")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file should not be created for empty session ID")
	}
}

// TestSaveSessionIDDirect_WritesSID verifies that a non-empty session ID is written.
func TestSaveSessionIDDirect_WritesSID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions", "session.txt")
	saveSessionIDDirect(path, "test-session-id")

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("session file not written: %v", err)
	}
	if string(content) != "test-session-id" {
		t.Errorf("session content = %q, want %q", content, "test-session-id")
	}
}

// TestSaveDebugLog_WritesFile verifies that saveDebugLog creates a log file.
func TestSaveDebugLog_WritesFile(t *testing.T) {
	// Change to a temp dir so .fabrik/debug is created there
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origDir)

	saveDebugLog(1, "Research", "some debug output")

	debugDir := filepath.Join(tmpDir, ".fabrik", "debug")
	entries, err := os.ReadDir(debugDir)
	if err != nil {
		t.Fatalf("debug dir not created: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected at least one debug log file")
	}
}
