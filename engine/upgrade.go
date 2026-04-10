package engine

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// fabrikOwner and fabrikRepo are the canonical owner/repo for fabrik itself.
// These are used when checking the GitHub Releases API for a newer binary —
// releases are published to shadoworg/fabrik (the public distribution repo).
const (
	fabrikOwner = "shadoworg"
	fabrikRepo  = "fabrik"
)

// SemverGreater reports whether version a is greater than version b.
// Both versions may have a leading "v" which is stripped before comparison.
// Each version is split on "." and each segment is compared as an integer.
// Returns false (not an upgrade) on any parse error.
func SemverGreater(a, b string) bool {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	// Pad shorter slice with zeros.
	for len(aParts) < len(bParts) {
		aParts = append(aParts, "0")
	}
	for len(bParts) < len(aParts) {
		bParts = append(bParts, "0")
	}
	for i := range aParts {
		av, err := strconv.Atoi(aParts[i])
		if err != nil {
			return false
		}
		bv, err := strconv.Atoi(bParts[i])
		if err != nil {
			return false
		}
		if av != bv {
			return av > bv
		}
	}
	return false
}

// ExtractBinaryFromTarball extracts the "fabrik" binary from a .tar.gz archive
// at tarballPath and writes it to a temp file in destDir. Returns the path to the
// temp file. The caller is responsible for renaming or removing it.
func ExtractBinaryFromTarball(tarballPath, destDir string) (string, error) {
	f, err := os.Open(tarballPath)
	if err != nil {
		return "", fmt.Errorf("opening tarball: %w", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("creating gzip reader: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading tarball: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Match the binary by base name in case GoReleaser puts it in a subdir.
		if filepath.Base(hdr.Name) != "fabrik" {
			continue
		}
		tmp, err := os.CreateTemp(destDir, "fabrik-*")
		if err != nil {
			return "", fmt.Errorf("creating temp file: %w", err)
		}
		if err := tmp.Chmod(0755); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", fmt.Errorf("chmod temp file: %w", err)
		}
		if _, err := io.Copy(tmp, tr); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", fmt.Errorf("writing temp file: %w", err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			return "", fmt.Errorf("closing temp file: %w", err)
		}
		return tmp.Name(), nil
	}
	return "", fmt.Errorf("fabrik binary not found in tarball")
}

// PerformReleaseUpgrade fetches the latest release from GitHub, compares it to
// the running version, and — if a newer version is available — downloads the
// platform-matching tarball, atomically replaces the running binary, and
// re-execs with os.Args. extraEnv is appended to the environment for the
// re-exec (e.g. []string{"FABRIK_AUTO_UPGRADED=1"}); pass nil for no extras.
//
// All failures are non-fatal: logf is called with a warning and the function
// returns so the caller can fall through to its normal operation.
func PerformReleaseUpgrade(client GitHubClient, version, token string, extraEnv []string, logf func(string, ...any)) {
	release, err := client.FetchLatestRelease(fabrikOwner, fabrikRepo)
	if err != nil {
		logf("could not fetch latest release: %v\n", err)
		return
	}
	if release == nil {
		return
	}

	latestTag := release.TagName
	if !SemverGreater(latestTag, version) {
		// Up to date; log nothing.
		return
	}

	logf("new release available: %s (running %s) — upgrading\n", latestTag, version)

	// Find the platform-matching asset: fabrik_VERSION_GOOS_GOARCH.tar.gz
	wantName := fmt.Sprintf("fabrik_%s_%s_%s.tar.gz", strings.TrimPrefix(latestTag, "v"), runtime.GOOS, runtime.GOARCH)
	var downloadURL string
	for _, asset := range release.Assets {
		if asset.Name == wantName {
			// Use the API URL with Accept: application/octet-stream for private repos.
			// The browser_download_url redirects to S3 which rejects the auth header.
			if asset.APIURL != "" {
				downloadURL = asset.APIURL
			} else {
				downloadURL = asset.BrowserDownloadURL
			}
			break
		}
	}
	if downloadURL == "" {
		logf("no matching asset for %s/%s (want %s) — skipping\n", runtime.GOOS, runtime.GOARCH, wantName)
		return
	}

	// Determine current executable path.
	exe, err := os.Executable()
	if err != nil {
		logf("could not determine executable path: %v\n", err)
		return
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		logf("could not resolve symlinks for executable: %v\n", err)
		return
	}

	logf("downloading %s\n", downloadURL)

	// Download to a temp file in the same directory as the binary to ensure
	// os.Rename works (same filesystem).
	tarballTmp, err := os.CreateTemp(filepath.Dir(exe), "fabrik-download-*")
	if err != nil {
		logf("could not create download temp file: %v\n", err)
		return
	}
	tarballPath := tarballTmp.Name()
	defer os.Remove(tarballPath)

	resp, err := func() (*http.Response, error) {
		req, err := http.NewRequest("GET", downloadURL, nil)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		// Required for API URL downloads on private repos
		req.Header.Set("Accept", "application/octet-stream")
		return http.DefaultClient.Do(req)
	}()
	if err != nil {
		tarballTmp.Close()
		logf("download failed: %v\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		tarballTmp.Close()
		logf("download returned HTTP %d\n", resp.StatusCode)
		return
	}
	if _, err := io.Copy(tarballTmp, resp.Body); err != nil {
		tarballTmp.Close()
		logf("writing download: %v\n", err)
		return
	}
	if err := tarballTmp.Close(); err != nil {
		logf("closing download: %v\n", err)
		return
	}

	// Extract the binary from the tarball.
	newBin, err := ExtractBinaryFromTarball(tarballPath, filepath.Dir(exe))
	if err != nil {
		logf("extracting binary: %v\n", err)
		return
	}

	// Atomically replace the running binary; only remove newBin if rename fails.
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(newBin)
		}
	}()
	if err := os.Rename(newBin, exe); err != nil {
		logf("replacing binary: %v\n", err)
		return
	}
	renamed = true

	logf("upgraded to %s\n", latestTag)

	// Clean up tarball before exec replaces the process (defers won't run).
	os.Remove(tarballPath)

	// Plugin skills are refreshed by the NEW binary after re-exec — the
	// FABRIK_AUTO_UPGRADED=1 env var (passed via extraEnv) triggers
	// RefreshPlugin() in root.go on startup.
	logf("re-executing\n")

	env := append(os.Environ(), extraEnv...)
	if err := syscall.Exec(exe, os.Args, env); err != nil {
		logf("exec failed: %v\n", err)
	}
}
