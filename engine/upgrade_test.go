package engine

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
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
	}
	for _, tc := range tests {
		got := SemverGreater(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("SemverGreater(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
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
	eng := testEngine(client, &mockClaudeInvoker{})
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
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.AutoUpgrade = true
	eng.cfg.Version = "v0.0.1"

	// Should log "no matching asset" warning and return without calling Exec.
	eng.checkReleaseUpgrade()
}

// TestCheckReleaseUpgrade_RateLimitSuppressesSecondCall verifies that a second
// call within 30 minutes does not invoke FetchLatestRelease again.
func TestCheckReleaseUpgrade_RateLimitSuppressesSecondCall(t *testing.T) {
	callCount := 0
	client := &mockGitHubClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			callCount++
			return &gh.LatestRelease{TagName: "v0.0.1"}, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.AutoUpgrade = true
	eng.cfg.Version = "v0.0.1"

	eng.checkReleaseUpgrade()
	eng.checkReleaseUpgrade() // should be rate-limited

	if callCount != 1 {
		t.Errorf("FetchLatestRelease called %d times, want 1 (second call should be rate-limited)", callCount)
	}
}

// TestCheckReleaseUpgrade_RateLimitExpiry verifies that after the rate-limit
// interval, the second call does invoke FetchLatestRelease.
func TestCheckReleaseUpgrade_RateLimitExpiry(t *testing.T) {
	callCount := 0
	client := &mockGitHubClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			callCount++
			return &gh.LatestRelease{TagName: "v0.0.1"}, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.AutoUpgrade = true
	eng.cfg.Version = "v0.0.1"

	eng.checkReleaseUpgrade()

	// Backdate lastReleaseCheck to simulate expiry.
	eng.mu.Lock()
	eng.lastReleaseCheck = time.Now().Add(-(releaseCheckInterval + time.Second))
	eng.mu.Unlock()

	eng.checkReleaseUpgrade()

	if callCount != 2 {
		t.Errorf("FetchLatestRelease called %d times after rate-limit expiry, want 2", callCount)
	}
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
	matchingAsset := fmt.Sprintf("fabrik_v9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

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
	eng := testEngine(client, &mockClaudeInvoker{})
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

	matchingAsset := fmt.Sprintf("fabrik_v9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
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
