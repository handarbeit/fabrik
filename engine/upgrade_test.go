package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/warnings"
)

// TestCheckReleaseUpgrade_MultiRepoMode verifies that checkReleaseUpgrade proceeds
// even when no primary repo is configured (multi-repo mode). This is the regression
// test for the bug where the release upgrade was incorrectly guarded by primaryWorktrees().
func TestCheckReleaseUpgrade_MultiRepoMode(t *testing.T) {
	fetched := false
	client := &mockGitHubClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			fetched = true
			return &gh.LatestRelease{TagName: "v0.0.1"}, nil
		},
	}
	// Engine with no owner/repo set — simulates multi-repo mode.
	eng := NewWithDeps(
		Config{Owner: "", Repo: "", User: "u", Token: "t", AutoUpgrade: true, Version: "v0.0.1", Stages: testStages()},
		client, &mockClaudeInvoker{}, nil,
	)

	eng.checkReleaseUpgrade()

	if !fetched {
		t.Error("FetchLatestRelease was not called in multi-repo mode — release upgrade was incorrectly skipped")
	}
}

// TestCheckReleaseUpgrade_UpToDate verifies that when the running version equals
// the latest release, no upgrade is attempted and the call returns cleanly.
func TestCheckReleaseUpgrade_UpToDate(t *testing.T) {
	client := &mockGitHubClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			return &gh.LatestRelease{TagName: "v0.0.1"}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.AutoUpgrade = true
	eng.cfg.Version = "v0.0.1"

	// Should complete without panic or upgrade; there's no assertion beyond it
	// not calling os.Executable or syscall.Exec (both would fail in test).
	eng.checkReleaseUpgrade()
}

// TestCheckReleaseUpgrade_NoMatchingAsset verifies that when a newer release
// exists but no asset matches the current GOOS/GOARCH, the function logs a
// warning and returns without crashing.
func TestCheckReleaseUpgrade_NoMatchingAsset(t *testing.T) {
	client := &mockGitHubClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			return &gh.LatestRelease{
				TagName: "v9.9.9",
				Assets: []gh.ReleaseAsset{
					{Name: "fabrik_v9.9.9_plan9_arm.tar.gz", BrowserDownloadURL: "http://example.com/plan9.tar.gz"},
				},
			}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.AutoUpgrade = true
	eng.cfg.Version = "v0.0.1"

	// Should log "no matching asset" warning and return without calling Exec.
	eng.checkReleaseUpgrade()
}

// TestCheckReleaseUpgrade_DownloadAttempted verifies that when a newer release
// exists and an asset matching the current platform is found, the download is
// actually attempted. The HTTP server returns a 500 so the upgrade fails
// gracefully (no exec occurs).
func TestCheckReleaseUpgrade_DownloadAttempted(t *testing.T) {
	downloaded := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloaded = true
		// Return a 500 so the upgrade fails gracefully without extracting or exec-ing.
		http.Error(w, "test server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Construct an asset name that matches the running platform — the same
	// format the production code uses: fabrik_VERSION_GOOS_GOARCH.tar.gz.
	matchingAsset := fmt.Sprintf("fabrik_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	client := &mockGitHubClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			return &gh.LatestRelease{
				TagName: "v9.9.9",
				Assets: []gh.ReleaseAsset{
					{Name: matchingAsset, BrowserDownloadURL: srv.URL + "/asset.tar.gz"},
				},
			}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.AutoUpgrade = true
	eng.cfg.Version = "v0.0.1"

	eng.checkReleaseUpgrade()

	// A matching asset was found — the download server must have been hit.
	if !downloaded {
		t.Error("download server was not hit even though a matching asset was provided")
	}
}

// startupTestSetup creates a temp dir with a .fabrik/ subdirectory and sets
// eng.fabrikDir to it, satisfying Run()'s lock-file and log-file creation.
func startupTestSetup(t *testing.T, eng *Engine) {
	t.Helper()
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".fabrik"), 0755); err != nil {
		t.Fatalf("creating .fabrik dir: %v", err)
	}
	eng.fabrikDir = tmpDir
}

// TestStartupUpgradeCheck_FiresWhenEnabled verifies that when AutoUpgrade=true,
// the upgradeCheckFn is called during Run() startup, before the first poll cycle.
func TestStartupUpgradeCheck_FiresWhenEnabled(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	startupTestSetup(t, eng)
	eng.cfg.AutoUpgrade = true
	eng.cfg.PollSeconds = 300

	called := make(chan struct{}, 1)
	eng.upgradeCheckFn = func() { called <- struct{}{} }

	readyCh := make(chan struct{})
	eng.cfg.ReadyCh = readyCh

	done := make(chan error, 1)
	go func() { done <- eng.Run() }()

	<-readyCh

	// Block until the startup upgrade check fires (before first doPollCycle).
	select {
	case <-called:
		// hook fired as expected
	case <-time.After(5 * time.Second):
		t.Fatal("upgradeCheckFn was not called within 5s (AutoUpgrade=true)")
	}

	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGINT) //nolint:errcheck

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not shut down in time")
	}
}

// TestStartupUpgradeCheck_SkipsWhenDisabled verifies that when AutoUpgrade=false,
// the upgradeCheckFn is never called during startup.
func TestStartupUpgradeCheck_SkipsWhenDisabled(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	startupTestSetup(t, eng)
	eng.cfg.AutoUpgrade = false
	eng.cfg.PollSeconds = 300

	called := make(chan struct{}, 1)
	eng.upgradeCheckFn = func() { called <- struct{}{} }

	readyCh := make(chan struct{})
	eng.cfg.ReadyCh = readyCh

	done := make(chan error, 1)
	go func() { done <- eng.Run() }()

	<-readyCh
	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGINT) //nolint:errcheck

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not shut down in time")
	}

	// Assert the hook was never called.
	select {
	case <-called:
		t.Error("upgradeCheckFn was called but AutoUpgrade=false")
	default:
	}
}

// writeFakeVersionBinary writes an executable shell script to dir that prints
// version to stdout when invoked with any arguments (mimicking `fabrik
// --version`). Returns the script's path.
func writeFakeVersionBinary(t *testing.T, dir, version string) string {
	t.Helper()
	path := filepath.Join(dir, "fabrik-fake")
	script := fmt.Sprintf("#!/bin/sh\necho %s\n", version)
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCheckVersionSkew_MatchingVersion_NoWarningAndClearsExisting verifies
// that when the on-disk binary reports the same version as the running
// process, no new warning is recorded and any stale entry for this key is
// cleared.
func TestCheckVersionSkew_MatchingVersion_NoWarningAndClearsExisting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake version binary is a shell script; not supported on windows")
	}
	dir := t.TempDir()
	scriptPath := writeFakeVersionBinary(t, dir, "v1.2.3")
	resolvedPath, err := filepath.EvalSymlinks(scriptPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	origExecutableFn := versionSkewExecutableFn
	versionSkewExecutableFn = func() (string, error) { return scriptPath, nil }
	defer func() { versionSkewExecutableFn = origExecutableFn }()

	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	defer func() { warnings.WarningsPathOverride = "" }()

	key := "version_skew:" + resolvedPath
	if err := warnings.Record(warnings.Entry{Key: key, Type: "version_skew", Title: "stale"}); err != nil {
		t.Fatalf("seeding stale entry: %v", err)
	}

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Version = "v1.2.3"

	eng.checkVersionSkew(context.Background())

	entries, err := warnings.Load()
	if err != nil {
		t.Fatalf("loading warnings: %v", err)
	}
	for _, entry := range entries {
		if entry.Key == key {
			t.Errorf("expected stale entry %q to be cleared, still present: %+v", key, entry)
		}
	}
}

// TestCheckVersionSkew_MismatchedVersion_RecordsWarning verifies that when
// the on-disk binary reports a different version than the running process,
// a warnings entry is recorded with the expected key, type, and fix params.
func TestCheckVersionSkew_MismatchedVersion_RecordsWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake version binary is a shell script; not supported on windows")
	}
	dir := t.TempDir()
	scriptPath := writeFakeVersionBinary(t, dir, "v1.2.4")
	resolvedPath, err := filepath.EvalSymlinks(scriptPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	origExecutableFn := versionSkewExecutableFn
	versionSkewExecutableFn = func() (string, error) { return scriptPath, nil }
	defer func() { versionSkewExecutableFn = origExecutableFn }()

	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	defer func() { warnings.WarningsPathOverride = "" }()

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Version = "v1.2.3"

	eng.checkVersionSkew(context.Background())

	entries, err := warnings.Load()
	if err != nil {
		t.Fatalf("loading warnings: %v", err)
	}
	key := "version_skew:" + resolvedPath
	var found *warnings.Entry
	for i := range entries {
		if entries[i].Key == key {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatalf("expected a version_skew warning entry with key %q, got: %+v", key, entries)
	}
	if found.Type != "version_skew" {
		t.Errorf("Type = %q, want %q", found.Type, "version_skew")
	}
	if found.FixAction != "shell_command" {
		t.Errorf("FixAction = %q, want %q", found.FixAction, "shell_command")
	}
	wantCmd := fmt.Sprintf("kill -HUP %d", os.Getpid())
	if found.FixParams["cmd"] != wantCmd {
		t.Errorf("FixParams[cmd] = %q, want %q", found.FixParams["cmd"], wantCmd)
	}
}

// TestCheckVersionSkew_ExecutableFnError_NonFatal verifies that a failure
// resolving the executable path is logged and does not panic or record a
// spurious warning.
func TestCheckVersionSkew_ExecutableFnError_NonFatal(t *testing.T) {
	origExecutableFn := versionSkewExecutableFn
	versionSkewExecutableFn = func() (string, error) { return "", errors.New("boom") }
	defer func() { versionSkewExecutableFn = origExecutableFn }()

	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	defer func() { warnings.WarningsPathOverride = "" }()

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Version = "v1.2.3"

	eng.checkVersionSkew(context.Background()) // must not panic

	entries, err := warnings.Load()
	if err != nil {
		t.Fatalf("loading warnings: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no warnings entries on executable-path error, got %v", entries)
	}
}
