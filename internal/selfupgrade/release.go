package selfupgrade

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// ReleaseFetcher is the narrow interface PerformReleaseUpgrade needs from a
// GitHub client — just enough to resolve the latest release for an
// owner/repo. Any client type satisfying this (structurally, no import
// required) can be passed in; engine.GitHubClient's much larger method set
// already includes FetchLatestRelease and so satisfies this automatically.
type ReleaseFetcher interface {
	FetchLatestRelease(owner, repo string) (*gh.LatestRelease, error)
}

// ReleaseConfig bundles the caller-supplied identity and behavior
// PerformReleaseUpgrade needs. Every field is required except ExtraEnv
// (defaults to no extra env vars) and Token (empty means an unauthenticated
// download request).
type ReleaseConfig struct {
	Client     ReleaseFetcher
	Owner      string // GitHub owner of the release repo, e.g. "handarbeit"
	Repo       string // GitHub repo name, e.g. "fabrik"
	BinaryName string // binary name inside the release tarball and on disk, e.g. "fabrik"
	Version    string // the running version, compared against the latest release tag
	Token      string // GitHub token for authenticated asset downloads; "" for unauthenticated
	ExtraEnv   []string
	Logf       func(string, ...any)
}

// PerformReleaseUpgrade fetches the latest release from GitHub, compares it to
// the running version, and — if a newer version is available — downloads the
// platform-matching tarball, atomically replaces the running binary, and
// re-execs with os.Args. cfg.ExtraEnv is appended to the environment for the
// re-exec (e.g. []string{"FABRIK_AUTO_UPGRADED=1"}); nil for no extras.
//
// cfg.Logf is always called with a warning on failure; the returned error lets
// callers decide whether a failure should be fatal (e.g. a foreground `fabrik
// upgrade` command halting before plugin refresh) or non-fatal (e.g. a
// background poll loop, which must continue regardless). "Already up to date"
// and "no release object" are not failures and return nil.
func PerformReleaseUpgrade(cfg ReleaseConfig) error {
	release, err := cfg.Client.FetchLatestRelease(cfg.Owner, cfg.Repo)
	if err != nil {
		cfg.Logf("could not fetch latest release: %v\n", err)
		return fmt.Errorf("fetching latest release: %w", err)
	}
	if release == nil {
		return nil
	}

	latestTag := release.TagName
	if !SemverGreater(latestTag, cfg.Version) {
		// Up to date; log nothing.
		return nil
	}

	cfg.Logf("new release available: %s (running %s) — upgrading\n", latestTag, cfg.Version)

	// Find the platform-matching asset: <binaryName>_VERSION_GOOS_GOARCH.tar.gz
	wantName := fmt.Sprintf("%s_%s_%s_%s.tar.gz", cfg.BinaryName, strings.TrimPrefix(latestTag, "v"), runtime.GOOS, runtime.GOARCH)
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
		cfg.Logf("no matching asset for %s/%s (want %s) — skipping\n", runtime.GOOS, runtime.GOARCH, wantName)
		return fmt.Errorf("no matching release asset for %s/%s (want %s)", runtime.GOOS, runtime.GOARCH, wantName)
	}

	// Determine current executable path.
	exe, err := executableFn()
	if err != nil {
		cfg.Logf("could not determine executable path: %v\n", err)
		return fmt.Errorf("determining executable path: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		cfg.Logf("could not resolve symlinks for executable: %v\n", err)
		return fmt.Errorf("resolving executable symlinks: %w", err)
	}

	cfg.Logf("downloading %s\n", downloadURL)

	// Download to a temp file in the same directory as the binary to ensure
	// os.Rename works (same filesystem).
	tarballTmp, err := os.CreateTemp(filepath.Dir(exe), cfg.BinaryName+"-download-*")
	if err != nil {
		cfg.Logf("could not create download temp file: %v\n", err)
		return fmt.Errorf("creating download temp file: %w", err)
	}
	tarballPath := tarballTmp.Name()
	defer os.Remove(tarballPath)

	resp, err := func() (*http.Response, error) {
		req, err := http.NewRequest("GET", downloadURL, nil)
		if err != nil {
			return nil, err
		}
		if cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
		}
		// Required for API URL downloads on private repos
		req.Header.Set("Accept", "application/octet-stream")
		return http.DefaultClient.Do(req)
	}()
	if err != nil {
		tarballTmp.Close()
		cfg.Logf("download failed: %v\n", err)
		return fmt.Errorf("downloading release asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		tarballTmp.Close()
		cfg.Logf("download returned HTTP %d\n", resp.StatusCode)
		return fmt.Errorf("downloading release asset: HTTP %d", resp.StatusCode)
	}
	if _, err := io.Copy(tarballTmp, resp.Body); err != nil {
		tarballTmp.Close()
		cfg.Logf("writing download: %v\n", err)
		return fmt.Errorf("writing downloaded asset: %w", err)
	}
	if err := tarballTmp.Close(); err != nil {
		cfg.Logf("closing download: %v\n", err)
		return fmt.Errorf("closing downloaded asset: %w", err)
	}

	// Extract the binary from the tarball.
	newBin, err := ExtractBinaryFromTarball(tarballPath, filepath.Dir(exe), cfg.BinaryName)
	if err != nil {
		cfg.Logf("extracting binary: %v\n", err)
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
		cfg.Logf("replacing binary: %v\n", err)
		return fmt.Errorf("replacing running binary: %w", err)
	}
	renamed = true

	// Best-effort: on macOS, a freshly-materialized binary (written via a fresh
	// io.Copy + rename, not built in place) can trip the Apple Silicon AMFI
	// trust-cache and be SIGKILL'd on exec — see tests/e2e/README.md's documented
	// "copied binary may be SIGKILL'd" fix. Re-signing here mirrors that recipe.
	resignDarwinBinary(exe, cfg.Logf)

	cfg.Logf("upgraded to %s\n", latestTag)

	// Clean up tarball before exec replaces the process (defers won't run).
	os.Remove(tarballPath)

	// A plugin/skill refresh, if the caller needs one, happens after re-exec —
	// e.g. Fabrik's FABRIK_AUTO_UPGRADED=1 env var (passed via cfg.ExtraEnv)
	// triggers RefreshPlugin() in root.go on startup.
	cfg.Logf("re-executing\n")

	env := append(os.Environ(), cfg.ExtraEnv...)
	if err := execFn(exe, os.Args, env); err != nil {
		cfg.Logf("CRITICAL: upgrade succeeded (binary replaced with %s on disk) but re-exec failed: %v — process is still running the OLD binary; restart manually (e.g. kill -HUP %d) or the daemon will remain silently stale\n", latestTag, err, os.Getpid())
		return fmt.Errorf("re-executing upgraded binary: %w", err)
	}
	return nil
}

// resignDarwinBinary is a no-op on any OS other than darwin. On darwin, it
// best-effort clears the quarantine extended attribute and applies an ad-hoc
// code signature to path, mirroring the documented fix in
// tests/e2e/README.md for "on macOS/Apple Silicon a copied binary may be
// SIGKILL'd" (an Apple Silicon AMFI trust-cache quirk that can affect a
// binary materialized via a fresh write rather than built in place — exactly
// the shape ExtractBinaryFromTarball + os.Rename produces). Re-signing an
// already-validly-signed binary is idempotent and harmless, so this runs
// unconditionally on darwin rather than trying to detect whether it's
// needed. Missing xattr/codesign (e.g. minimal CI images) and any command
// failure are logged and treated as non-fatal — this is hardening, not a
// guaranteed fix, and cannot be verified outside real Apple Silicon
// hardware.
func resignDarwinBinary(path string, logf func(string, ...any)) {
	if runtime.GOOS != "darwin" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := exec.LookPath("xattr"); err == nil {
		if out, err := exec.CommandContext(ctx, "xattr", "-cr", path).CombinedOutput(); err != nil {
			logf("resigning upgraded binary: xattr -cr failed (non-fatal): %v\n%s\n", err, out)
		}
	} else {
		logf("resigning upgraded binary: xattr not found (non-fatal, skipping quarantine clear)\n")
	}
	if _, err := exec.LookPath("codesign"); err == nil {
		if out, err := exec.CommandContext(ctx, "codesign", "--force", "--sign", "-", path).CombinedOutput(); err != nil {
			logf("resigning upgraded binary: codesign failed (non-fatal): %v\n%s\n", err, out)
		}
	} else {
		logf("resigning upgraded binary: codesign not found (non-fatal, skipping ad-hoc sign)\n")
	}
}
