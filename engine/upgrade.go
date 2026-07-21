package engine

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// fabrikOwner and fabrikRepo are the canonical owner/repo for fabrik itself.
// These are used when checking the GitHub Releases API for a newer binary —
// the release always targets handarbeit/fabrik, not the user's managed project.
const (
	fabrikOwner = "handarbeit"
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
// logf is always called with a warning on failure; the returned error lets
// callers decide whether a failure should be fatal (e.g. a foreground `fabrik
// upgrade` command halting before plugin refresh) or non-fatal (e.g. the
// background poll loop, which must continue regardless — see
// engine/poll.go's checkReleaseUpgrade). "Already up to date" and "no release
// object" are not failures and return nil.
func PerformReleaseUpgrade(client GitHubClient, version, token string, extraEnv []string, logf func(string, ...any)) error {
	release, err := client.FetchLatestRelease(fabrikOwner, fabrikRepo)
	if err != nil {
		logf("could not fetch latest release: %v\n", err)
		return fmt.Errorf("fetching latest release: %w", err)
	}
	if release == nil {
		return nil
	}

	latestTag := release.TagName
	if !SemverGreater(latestTag, version) {
		// Up to date; log nothing.
		return nil
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
		return fmt.Errorf("no matching release asset for %s/%s (want %s)", runtime.GOOS, runtime.GOARCH, wantName)
	}

	// Determine current executable path.
	exe, err := os.Executable()
	if err != nil {
		logf("could not determine executable path: %v\n", err)
		return fmt.Errorf("determining executable path: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		logf("could not resolve symlinks for executable: %v\n", err)
		return fmt.Errorf("resolving executable symlinks: %w", err)
	}

	logf("downloading %s\n", downloadURL)

	// Download to a temp file in the same directory as the binary to ensure
	// os.Rename works (same filesystem).
	tarballTmp, err := os.CreateTemp(filepath.Dir(exe), "fabrik-download-*")
	if err != nil {
		logf("could not create download temp file: %v\n", err)
		return fmt.Errorf("creating download temp file: %w", err)
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
		return fmt.Errorf("downloading release asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		tarballTmp.Close()
		logf("download returned HTTP %d\n", resp.StatusCode)
		return fmt.Errorf("downloading release asset: HTTP %d", resp.StatusCode)
	}
	if _, err := io.Copy(tarballTmp, resp.Body); err != nil {
		tarballTmp.Close()
		logf("writing download: %v\n", err)
		return fmt.Errorf("writing downloaded asset: %w", err)
	}
	if err := tarballTmp.Close(); err != nil {
		logf("closing download: %v\n", err)
		return fmt.Errorf("closing downloaded asset: %w", err)
	}

	// Extract the binary from the tarball.
	newBin, err := ExtractBinaryFromTarball(tarballPath, filepath.Dir(exe))
	if err != nil {
		logf("extracting binary: %v\n", err)
		return fmt.Errorf("extracting binary from tarball: %w", err)
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
		return fmt.Errorf("replacing running binary: %w", err)
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
		return fmt.Errorf("re-executing upgraded binary: %w", err)
	}
	return nil
}

// checkAndUpgrade selects the upgrade path based on the running version:
//   - dev builds (version starts with "dev"): git pull → go build → re-exec
//   - release builds (all other versions): GitHub Releases API → download → atomic replace → re-exec
func (e *Engine) checkAndUpgrade() {
	if !strings.HasPrefix(e.cfg.Version, "dev") {
		e.checkReleaseUpgrade()
		return
	}

	dir := e.fabrikDir

	// Only auto-upgrade if we're running from a Fabrik source checkout.
	if !isFabrikSourceCheckout(dir) {
		return
	}

	baseBranch := "main"

	// Check local HEAD first — detects local commits that haven't been pushed.
	localRef, err := gitRevParse(dir, "HEAD")
	if err != nil {
		e.logf(0, "upgrade", "could not resolve HEAD: %v\n", err)
		return
	}
	binarySHA := extractBinarySHA(e.cfg.Version)
	needsRebuild := binarySHA != "" && !strings.HasPrefix(localRef, binarySHA)
	if needsRebuild {
		e.logf(0, "upgrade", "binary built from %s but HEAD is %s — rebuilding\n", binarySHA, localRef[:7])
	}

	// Also check remote for new upstream commits.
	if !needsRebuild {
		pollStatus("[upgrade] checking origin/%s ...", baseBranch)

		fetchCmd := exec.Command("git", "fetch", "origin", baseBranch)
		fetchCmd.Dir = dir
		if out, err := fetchCmd.CombinedOutput(); err != nil {
			e.logf(0, "upgrade", "git fetch failed: %v\n%s\n", err, out)
			return
		}

		remoteRef, err := gitRevParse(dir, "origin/"+baseBranch)
		if err != nil {
			e.logf(0, "upgrade", "could not resolve origin/%s: %v\n", baseBranch, err)
			return
		}
		if localRef == remoteRef {
			pollStatusClear()
			return // up to date
		}
		// Only pull if remote is ahead of local. If local is ahead (unpushed
		// commits), we already checked the binary SHA against local HEAD above.
		mergeBaseCmd := exec.Command("git", "merge-base", "--is-ancestor", localRef, remoteRef)
		mergeBaseCmd.Dir = dir
		if err := mergeBaseCmd.Run(); err != nil {
			// localRef is not an ancestor of remoteRef — local is ahead or diverged.
			// Either way, nothing to pull. The binary SHA check above already
			// handled whether a rebuild is needed.
			pollStatusClear()
			return
		}
		needsRebuild = true
		e.logf(0, "upgrade", "new commits on origin/%s — pulling\n", baseBranch)

		pullCmd := exec.Command("git", "pull", "--ff-only", "origin", baseBranch)
		pullCmd.Dir = dir
		if out, err := pullCmd.CombinedOutput(); err != nil {
			e.logf(0, "upgrade", "git pull --ff-only failed (local changes?): %v\n%s\n", err, out)
			return
		}
	}

	exe, err := os.Executable()
	if err != nil {
		e.logf(0, "upgrade", "could not determine executable path: %v\n", err)
		return
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		e.logf(0, "upgrade", "could not resolve symlinks for executable: %v\n", err)
		return
	}

	e.logf(0, "upgrade", "rebuilding binary: %s\n", exe)

	buildCmd := exec.Command("go", "build", "-o", exe, ".")
	buildCmd.Dir = dir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		e.logf(0, "upgrade", "build failed: %v\n%s\n", err, out)
		return
	}

	// Refresh plugin skills from the new binary.
	e.logf(0, "upgrade", "refreshing plugin skills\n")
	upgradeCmd := exec.Command(exe, "upgrade")
	upgradeCmd.Dir = dir
	if out, err := upgradeCmd.CombinedOutput(); err != nil {
		e.logf(0, "upgrade", "fabrik upgrade failed: %v\n%s\n", err, out)
		// Non-fatal — continue with re-exec, old skills still work
	}

	e.logf(0, "upgrade", "re-executing new binary\n")

	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		e.logf(0, "upgrade", "exec failed: %v\n", err)
	}
}

// extractBinarySHA extracts the short SHA from a dev version string like
// "dev(abc1234)". Returns "" if the version is not a dev build or has no SHA.
func extractBinarySHA(version string) string {
	if !strings.HasPrefix(version, "dev(") || !strings.HasSuffix(version, ")") {
		return ""
	}
	return version[4 : len(version)-1]
}

// isFabrikSourceCheckout reports whether dir is a git checkout of the fabrik
// source repo (handarbeit/fabrik). Returns false on any error (no git, no
// remote, wrong remote, etc.).
func isFabrikSourceCheckout(dir string) bool {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	url := strings.TrimSuffix(strings.TrimSpace(string(out)), ".git")
	return strings.Contains(url, "handarbeit/fabrik")
}

// checkReleaseUpgrade is the release-based upgrade path. It checks the GitHub
// Releases API for a version newer than the running binary, downloads the
// matching platform asset, atomically replaces the running binary, and re-execs.
//
// All failures are non-fatal: a warning is logged and the poll loop continues.
func (e *Engine) checkReleaseUpgrade() {
	logf := func(format string, args ...any) {
		e.logf(0, "upgrade", format, args...)
	}
	// Error discarded intentionally: failures are logged by PerformReleaseUpgrade
	// itself via logf, and this caller's contract is non-fatal — the poll loop
	// continues regardless (unlike the foreground `fabrik upgrade` command).
	_ = PerformReleaseUpgrade(e.client, e.cfg.Version, e.cfg.Token, []string{"FABRIK_AUTO_UPGRADED=1"}, logf)
}
