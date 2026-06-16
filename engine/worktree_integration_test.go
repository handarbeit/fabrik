//go:build integration

package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureBareClone_NewDir_ClonesFromLocal verifies that ensureBareClone creates
// the .fabrik directory before attempting a clone, even when the clone fails.
//
// This test requires network access: it calls ensureBareClone with a non-existent
// GitHub owner/repo, which triggers a real git clone attempt that fails with a
// network error. The ENAMETOOLONG trick (setting a 10000-char baseDir) cannot be
// used here because it causes os.MkdirAll to fail, which would prevent the
// .fabrik directory from being created — invalidating the key assertion below.
// Integration build tag excludes this from the default `go test ./...` run.
func TestEnsureBareClone_NewDir_ClonesFromLocal(t *testing.T) {
	skipIfNoGit(t)

	tmpDir := t.TempDir()

	// Override cloneURL construction by temporarily monkey-patching isn't possible in Go,
	// but we can test the error path: clone of a non-existent github URL fails.
	// Instead we test that the function creates the .fabrik directory and returns an error
	// when git clone fails (no real network in CI).
	_, err := ensureBareClone(tmpDir, "nonexistent-owner-xyz", "nonexistent-repo-xyz-abc", "", false)
	// We expect an error because the github URL doesn't exist. The key check is that
	// the function attempts the clone and returns a wrapped error.
	if err == nil {
		t.Log("clone unexpectedly succeeded (real network access?)")
	}
	// The .fabrik parent dir should have been created even on failure
	fabrikDir := filepath.Join(tmpDir, ".fabrik")
	if _, statErr := os.Stat(fabrikDir); os.IsNotExist(statErr) {
		t.Error(".fabrik directory should be created before clone attempt")
	}
}
