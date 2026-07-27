package selfupgrade

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
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// stubReleaseFetcher is a minimal ReleaseFetcher for tests, avoiding any
// dependency on engine's much larger mockGitHubClient.
type stubReleaseFetcher struct {
	fetchLatestReleaseFn func(owner, repo string) (*gh.LatestRelease, error)
}

func (s *stubReleaseFetcher) FetchLatestRelease(owner, repo string) (*gh.LatestRelease, error) {
	return s.fetchLatestReleaseFn(owner, repo)
}

// baseReleaseConfig returns a ReleaseConfig with fabrik-shaped identity and
// the given client/version/logf, for tests that don't need to vary those
// fields.
func baseReleaseConfig(client ReleaseFetcher, version string, logf func(string, ...any)) ReleaseConfig {
	return ReleaseConfig{
		Client:     client,
		Owner:      "handarbeit",
		Repo:       "fabrik",
		BinaryName: "fabrik",
		Version:    version,
		Logf:       logf,
	}
}

// TestPerformReleaseUpgrade_UpToDate verifies that when the running version
// equals the latest release, PerformReleaseUpgrade returns without attempting
// a download.
func TestPerformReleaseUpgrade_UpToDate(t *testing.T) {
	fetched := false
	client := &stubReleaseFetcher{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			fetched = true
			return &gh.LatestRelease{TagName: "v1.2.3"}, nil
		},
	}
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	PerformReleaseUpgrade(baseReleaseConfig(client, "v1.2.3", logf))

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
	client := &stubReleaseFetcher{
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

	PerformReleaseUpgrade(baseReleaseConfig(client, "v0.0.1", logf))

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
	client := &stubReleaseFetcher{
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

	PerformReleaseUpgrade(baseReleaseConfig(client, "v0.0.1", logf))

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
	client := &stubReleaseFetcher{
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

	PerformReleaseUpgrade(baseReleaseConfig(client, runningVersion, logf))

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

	client := &stubReleaseFetcher{
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

	PerformReleaseUpgrade(baseReleaseConfig(client, runningVersion, logf))

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
// then overrides execFn to simulate a re-exec failure (e.g. the macOS AMFI
// SIGKILL scenario). Asserts the strengthened CRITICAL log line appears and
// PerformReleaseUpgrade returns a non-nil error without panicking — i.e. the
// daemon survives a failed re-exec rather than dying or silently continuing
// on the old binary with no signal.
func TestPerformReleaseUpgrade_ExecFailureLogsAndReturnsError(t *testing.T) {
	dir := t.TempDir()
	scratchExe := filepath.Join(dir, "fabrik-under-test")
	if err := os.WriteFile(scratchExe, []byte("old binary content"), 0755); err != nil {
		t.Fatal(err)
	}

	origExecutableFn := executableFn
	executableFn = func() (string, error) { return scratchExe, nil }
	defer func() { executableFn = origExecutableFn }()

	origExecFn := execFn
	execErr := errors.New("simulated exec failure (e.g. AMFI SIGKILL)")
	var execCalled bool
	execFn = func(argv0 string, argv []string, envv []string) error {
		execCalled = true
		return execErr
	}
	defer func() { execFn = origExecFn }()

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

	client := &stubReleaseFetcher{
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

	err := PerformReleaseUpgrade(baseReleaseConfig(client, "v0.0.1", logf))

	if !execCalled {
		t.Fatal("execFn was never called — the test did not reach the exec step")
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
	client := &stubReleaseFetcher{
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

	PerformReleaseUpgrade(baseReleaseConfig(client, "v0.0.1", logf))

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
	client := &stubReleaseFetcher{
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

	PerformReleaseUpgrade(baseReleaseConfig(client, "v0.0.1", logf))

	if gotURL != "/browser-url" {
		t.Errorf("expected fallback to /browser-url, got %q", gotURL)
	}
}
