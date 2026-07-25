package engine

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/warnings"
)

// TestSemverGreater covers the basic semver comparison logic including the
// 0.0.2 vs 0.0.10 edge case where string comparison would produce wrong results.
func TestSemverGreater(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"v0.0.2", "v0.0.1", true},
		{"v0.0.1", "v0.0.2", false},
		{"v0.0.2", "v0.0.2", false}, // equal
		{"v0.0.10", "v0.0.2", true}, // integer comparison: 10 > 2
		{"v0.0.2", "v0.0.10", false},
		{"v1.0.0", "v0.9.9", true},
		{"v0.9.9", "v1.0.0", false},
		{"v1.2.3", "v1.2.3", false},
		{"v2.0.0", "v1.99.99", true},
		// No v prefix
		{"0.0.2", "0.0.1", true},
		{"0.0.10", "0.0.2", true},
		// Mismatched segment counts
		{"1.0", "0.9.9", true},
		// Suffixed versions (#1074): a non-numeric suffix must not defeat the
		// comparison — only the numeric MAJOR.MINOR.PATCH core is compared.
		{"v0.0.75", "0.0.73+dirty", true},                                             // +dirty release stamp on b
		{"v0.0.75", "0.0.73-abc1234", true},                                           // SHA suffix on b
		{"v0.0.75", "v0.0.72-0.20260716173320-6198e8102f90+dirty", true},              // go install @main pseudo-version on b
		{"v0.0.74-0.20260716173320-6198e8102f90+dirty", "v0.0.75", false},             // suffixed a, not greater
		{"0.0.73+dirty", "0.0.73-abc1234", false},                                     // equal numeric core, both suffixed
		{"v0.0.73-0.20260716173320-aaa+dirty", "v0.0.73-0.20260716173320-bbb", false}, // equal core, both suffixed, different sha
		{"garbage", "0.0.1", false},                                                   // non-numeric core, no panic
		{"0.0.1", "garbage", false},                                                   // non-numeric core on b, no panic
	}
	for _, tc := range tests {
		got := SemverGreater(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("SemverGreater(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

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

// TestExtractBinaryFromTarball verifies that ExtractBinaryFromTarball correctly
// extracts the "fabrik" binary from a .tar.gz archive and returns an executable
// temp file in the specified destination directory.
func TestExtractBinaryFromTarball(t *testing.T) {
	const binaryContent = "#!/bin/sh\necho hello\n"

	// Build a minimal tar.gz containing a file named "fabrik".
	tarball, err := os.CreateTemp(t.TempDir(), "test-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	tarballPath := tarball.Name()

	gw := gzip.NewWriter(tarball)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{
		Name:     "fabrik",
		Typeflag: tar.TypeReg,
		Size:     int64(len(binaryContent)),
		Mode:     0755,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(binaryContent)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tarball.Close(); err != nil {
		t.Fatal(err)
	}

	destDir := t.TempDir()
	outPath, err := ExtractBinaryFromTarball(tarballPath, destDir)
	if err != nil {
		t.Fatalf("ExtractBinaryFromTarball returned error: %v", err)
	}

	// Verify the output file is inside destDir.
	if filepath.Dir(outPath) != destDir {
		t.Errorf("output path %q is not inside destDir %q", outPath, destDir)
	}

	// Verify the content matches.
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if string(got) != binaryContent {
		t.Errorf("content = %q, want %q", got, binaryContent)
	}

	// Verify the file is executable.
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("stat output file: %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Errorf("output file mode %o is not executable", info.Mode())
	}
}

// TestExtractBinaryFromTarball_NotFound verifies that ExtractBinaryFromTarball
// returns an error when no entry named "fabrik" exists in the archive.
func TestExtractBinaryFromTarball_NotFound(t *testing.T) {
	tarball, err := os.CreateTemp(t.TempDir(), "test-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	tarballPath := tarball.Name()

	gw := gzip.NewWriter(tarball)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{
		Name:     "other-binary",
		Typeflag: tar.TypeReg,
		Size:     4,
		Mode:     0755,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()
	tarball.Close()

	_, err = ExtractBinaryFromTarball(tarballPath, t.TempDir())
	if err == nil {
		t.Error("expected error when fabrik binary not in tarball, got nil")
	}
}

// TestPerformReleaseUpgrade_UpToDate verifies that when the running version
// equals the latest release, PerformReleaseUpgrade returns without attempting
// a download.
func TestPerformReleaseUpgrade_UpToDate(t *testing.T) {
	fetched := false
	client := &mockGitHubClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			fetched = true
			return &gh.LatestRelease{TagName: "v1.2.3"}, nil
		},
	}
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	PerformReleaseUpgrade(client, "v1.2.3", "", nil, logf)

	if !fetched {
		t.Error("expected FetchLatestRelease to be called")
	}
	if len(logs) != 0 {
		t.Errorf("expected no log output for up-to-date version, got: %v", logs)
	}
}

// TestPerformReleaseUpgrade_NoMatchingAsset verifies that when a newer release
// exists but no asset matches the current GOOS/GOARCH, PerformReleaseUpgrade
// logs a warning and returns without attempting a download.
func TestPerformReleaseUpgrade_NoMatchingAsset(t *testing.T) {
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
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	PerformReleaseUpgrade(client, "v0.0.1", "", nil, logf)

	found := false
	for _, l := range logs {
		if strings.Contains(l, "no matching asset") || strings.Contains(l, "skipping") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'no matching asset' warning in logs, got: %v", logs)
	}
}

// TestPerformReleaseUpgrade_DownloadAttempted verifies that when a newer
// release exists and a platform-matching asset is found, the download server
// is hit. The server returns 500 so the upgrade fails gracefully (no exec).
func TestPerformReleaseUpgrade_DownloadAttempted(t *testing.T) {
	downloaded := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloaded = true
		http.Error(w, "test server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

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
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	PerformReleaseUpgrade(client, "v0.0.1", "", nil, logf)

	if !downloaded {
		t.Error("download server was not hit even though a matching asset was provided")
	}
}

// TestPerformReleaseUpgrade_SuffixedVersionUpgrades is the #1074 regression
// test: a daemon whose running version carries a non-numeric suffix must
// still upgrade to a newer release. This covers the exact confirmed
// real-world exposure — a `go install …@main`/branch pseudo-version running
// string, live since v0.0.72 — where SemverGreater previously choked on the
// suffixed segment and silently reported "up to date," so the download path
// was never reached.
func TestPerformReleaseUpgrade_SuffixedVersionUpgrades(t *testing.T) {
	downloaded := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloaded = true
		http.Error(w, "test server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	matchingAsset := fmt.Sprintf("fabrik_0.0.75_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	client := &mockGitHubClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			return &gh.LatestRelease{
				TagName: "v0.0.75",
				Assets: []gh.ReleaseAsset{
					{Name: matchingAsset, BrowserDownloadURL: srv.URL + "/asset.tar.gz"},
				},
			}, nil
		},
	}
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	// The confirmed real-world exposure: a go install …@main/branch pseudo-version.
	runningVersion := "v0.0.72-0.20260716173320-6198e8102f90+dirty"

	PerformReleaseUpgrade(client, runningVersion, "", nil, logf)

	if !downloaded {
		t.Errorf("expected download to be attempted for suffixed running version %q vs newer release v0.0.75, got logs: %v", runningVersion, logs)
	}
}

// TestPerformReleaseUpgrade_SuffixedVersionNotEagerlyUpgraded is the
// companion guard for the above: when the suffixed running version's
// numeric core is NOT older than the release tag, no download must be
// attempted. This guards against an over-eager regression where suffix
// stripping makes SemverGreater too permissive (e.g. always upgrading).
func TestPerformReleaseUpgrade_SuffixedVersionNotEagerlyUpgraded(t *testing.T) {
	downloaded := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloaded = true
		http.Error(w, "test server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := &mockGitHubClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			return &gh.LatestRelease{TagName: "v0.0.75"}, nil
		},
	}
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	// Suffixed but equal-or-newer numeric core than the release tag — must not upgrade.
	runningVersion := "v0.0.75-0.20260716173320-6198e8102f90+dirty"

	PerformReleaseUpgrade(client, runningVersion, "", nil, logf)

	if downloaded {
		t.Errorf("expected no download for running version %q whose numeric core is not older than release v0.0.75", runningVersion)
	}
}

// buildTestTarball writes a minimal .tar.gz containing a single "fabrik"
// entry with the given content to a temp file in dir, and returns its path.
func buildTestTarball(t *testing.T, dir, content string) string {
	t.Helper()
	tarball, err := os.CreateTemp(dir, "release-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer tarball.Close()
	gw := gzip.NewWriter(tarball)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{Name: "fabrik", Typeflag: tar.TypeReg, Size: int64(len(content)), Mode: 0755}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return tarball.Name()
}

// TestPerformReleaseUpgrade_ExecFailureLogsAndReturnsError drives
// PerformReleaseUpgrade through a full successful download+extract+replace,
// then overrides upgradeExecFn to simulate a re-exec failure (e.g. the
// macOS AMFI SIGKILL scenario). Asserts the strengthened CRITICAL log line
// appears and PerformReleaseUpgrade returns a non-nil error without
// panicking — i.e. the daemon survives a failed re-exec rather than dying
// or silently continuing on the old binary with no signal.
func TestPerformReleaseUpgrade_ExecFailureLogsAndReturnsError(t *testing.T) {
	dir := t.TempDir()
	scratchExe := filepath.Join(dir, "fabrik-under-test")
	if err := os.WriteFile(scratchExe, []byte("old binary content"), 0755); err != nil {
		t.Fatal(err)
	}

	origExecutableFn := upgradeExecutableFn
	upgradeExecutableFn = func() (string, error) { return scratchExe, nil }
	defer func() { upgradeExecutableFn = origExecutableFn }()

	origExecFn := upgradeExecFn
	execErr := errors.New("simulated exec failure (e.g. AMFI SIGKILL)")
	var execCalled bool
	upgradeExecFn = func(argv0 string, argv []string, envv []string) error {
		execCalled = true
		return execErr
	}
	defer func() { upgradeExecFn = origExecFn }()

	matchingAsset := fmt.Sprintf("fabrik_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tarballPath := buildTestTarball(t, t.TempDir(), "new binary content")
		data, err := os.ReadFile(tarballPath)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data) //nolint:errcheck
	}))
	defer srv.Close()

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
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	err := PerformReleaseUpgrade(client, "v0.0.1", "", nil, logf)

	if !execCalled {
		t.Fatal("upgradeExecFn was never called — the test did not reach the exec step")
	}
	if err == nil {
		t.Error("expected a non-nil error when re-exec fails")
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "CRITICAL") && strings.Contains(l, "still running the OLD binary") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a CRITICAL 'still running the OLD binary' log line, got: %v", logs)
	}

	// The binary on disk should have been replaced despite the exec failure.
	got, err := os.ReadFile(scratchExe)
	if err != nil {
		t.Fatalf("reading scratch exe after upgrade: %v", err)
	}
	if string(got) != "new binary content" {
		t.Errorf("expected scratch exe to contain the new binary content, got %q", got)
	}
}

// TestResignDarwinBinary_NoopOnNonDarwin verifies that resignDarwinBinary is
// a safe no-op (no logf calls, no error) on any non-darwin GOOS — this is
// what actually runs in this sandbox and in Linux CI.
func TestResignDarwinBinary_NoopOnNonDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this test verifies non-darwin no-op behavior; skipping on darwin")
	}
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	resignDarwinBinary(filepath.Join(t.TempDir(), "fabrik"), logf)
	if len(logs) != 0 {
		t.Errorf("expected no log output on non-darwin GOOS, got: %v", logs)
	}
}

// TestPerformReleaseUpgrade_PrefersAPIURL verifies that when both APIURL and
// BrowserDownloadURL are set, the APIURL is used (required for private repos).
// Also checks that the Accept: application/octet-stream header is sent.
func TestPerformReleaseUpgrade_PrefersAPIURL(t *testing.T) {
	var gotURL string
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		http.Error(w, "test", http.StatusInternalServerError)
	}))
	defer srv.Close()

	matchingAsset := fmt.Sprintf("fabrik_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	client := &mockGitHubClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			return &gh.LatestRelease{
				TagName: "v9.9.9",
				Assets: []gh.ReleaseAsset{
					{
						Name:               matchingAsset,
						BrowserDownloadURL: srv.URL + "/browser-url",
						APIURL:             srv.URL + "/api-url",
					},
				},
			}, nil
		},
	}
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	PerformReleaseUpgrade(client, "v0.0.1", "", nil, logf)

	if gotURL != "/api-url" {
		t.Errorf("expected request to /api-url (APIURL), got %q — BrowserDownloadURL was used instead", gotURL)
	}
	if gotAccept != "application/octet-stream" {
		t.Errorf("expected Accept: application/octet-stream header, got %q", gotAccept)
	}
}

// TestPerformReleaseUpgrade_FallsBackToBrowserURL verifies that when APIURL
// is empty, BrowserDownloadURL is used as fallback.
func TestPerformReleaseUpgrade_FallsBackToBrowserURL(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.Path
		http.Error(w, "test", http.StatusInternalServerError)
	}))
	defer srv.Close()

	matchingAsset := fmt.Sprintf("fabrik_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	client := &mockGitHubClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			return &gh.LatestRelease{
				TagName: "v9.9.9",
				Assets: []gh.ReleaseAsset{
					{
						Name:               matchingAsset,
						BrowserDownloadURL: srv.URL + "/browser-url",
						APIURL:             "", // empty — should fall back
					},
				},
			}, nil
		},
	}
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	PerformReleaseUpgrade(client, "v0.0.1", "", nil, logf)

	if gotURL != "/browser-url" {
		t.Errorf("expected fallback to /browser-url, got %q", gotURL)
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

	origExecutableFn := upgradeExecutableFn
	upgradeExecutableFn = func() (string, error) { return scriptPath, nil }
	defer func() { upgradeExecutableFn = origExecutableFn }()

	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	defer func() { warnings.WarningsPathOverride = "" }()

	key := "version_skew:" + resolvedPath
	if err := warnings.Record(warnings.Entry{Key: key, Type: "version_skew", Title: "stale"}); err != nil {
		t.Fatalf("seeding stale entry: %v", err)
	}

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Version = "v1.2.3"

	eng.checkVersionSkew()

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

	origExecutableFn := upgradeExecutableFn
	upgradeExecutableFn = func() (string, error) { return scriptPath, nil }
	defer func() { upgradeExecutableFn = origExecutableFn }()

	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	defer func() { warnings.WarningsPathOverride = "" }()

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Version = "v1.2.3"

	eng.checkVersionSkew()

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
	origExecutableFn := upgradeExecutableFn
	upgradeExecutableFn = func() (string, error) { return "", errors.New("boom") }
	defer func() { upgradeExecutableFn = origExecutableFn }()

	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	defer func() { warnings.WarningsPathOverride = "" }()

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Version = "v1.2.3"

	eng.checkVersionSkew() // must not panic

	entries, err := warnings.Load()
	if err != nil {
		t.Fatalf("loading warnings: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no warnings entries on executable-path error, got %v", entries)
	}
}
