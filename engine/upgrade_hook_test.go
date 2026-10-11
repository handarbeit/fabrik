package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The dev-build auto-upgrade runs `fabrik upgrade` unattended in the daemon's
// working tree. It must pass --no-migrate so it never moves plugin files or
// rewrites tracked stage YAML (#2139, FR-006's spirit).
func TestDevBuildPostBuildHook_RunsUpgradeWithNoMigrate(t *testing.T) {
	e, _ := channelEngine(t, 0, nil)
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	exe := filepath.Join(dir, "fake-fabrik")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.devBuildConfig().PostBuildHook(exe, dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "upgrade --no-migrate" {
		t.Fatalf("post-build hook ran %q, want \"upgrade --no-migrate\"", got)
	}
}
