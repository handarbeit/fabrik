package selfupgrade

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DevBuildConfig bundles the caller-supplied identity, source location, and
// optional hooks CheckAndRebuildDev needs. Dir, Version, BaseBranch,
// ExpectedRemote, and Logf are required; StatusFn, StatusClearFn, and
// PostBuildHook are optional (nil-safe no-ops when unset).
type DevBuildConfig struct {
	Dir            string // path to the source checkout to rebuild from
	Version        string // running version, expected to be a "dev(<sha>)" string
	BaseBranch     string // upstream branch to compare/pull against, e.g. "main"
	ExpectedRemote string // substring the origin remote URL must contain, e.g. "handarbeit/fabrik"
	Logf           func(string, ...any)
	// StatusFn, if non-nil, is called with transient "checking origin/..."
	// progress messages before any decision to rebuild is made.
	StatusFn func(format string, args ...any)
	// StatusClearFn, if non-nil, is called to end a transient status line once
	// CheckAndRebuildDev determines no rebuild is needed.
	StatusClearFn func()
	// PostBuildHook, if non-nil, runs after a successful rebuild and before
	// re-exec, given the freshly built executable's path and Dir. Its error
	// is logged via Logf and treated as non-fatal — re-exec proceeds
	// regardless (e.g. Fabrik's plugin-skill refresh, where old skills still
	// work if the refresh fails).
	PostBuildHook func(exe, dir string) error
}

func (cfg DevBuildConfig) status(format string, args ...any) {
	if cfg.StatusFn != nil {
		cfg.StatusFn(format, args...)
	}
}

func (cfg DevBuildConfig) statusClear() {
	if cfg.StatusClearFn != nil {
		cfg.StatusClearFn()
	}
}

// CheckAndRebuildDev implements the dev-build self-upgrade path: compare the
// running binary's embedded SHA (and, failing that, the local checkout's
// remote) against upstream, rebuild from source with `go build`, run the
// optional PostBuildHook, and re-exec. Requires cfg.Dir to be a git source
// checkout matching cfg.ExpectedRemote (IsSourceCheckout) — a dev-mode daemon
// can only self-upgrade when run from its own source tree. All failures are
// logged via cfg.Logf and non-fatal: the function simply returns without
// upgrading.
func CheckAndRebuildDev(cfg DevBuildConfig) {
	if !IsSourceCheckout(cfg.Dir, cfg.ExpectedRemote) {
		return
	}

	// Check local HEAD first — detects local commits that haven't been pushed.
	localRef, err := gitRevParse(cfg.Dir, "HEAD")
	if err != nil {
		cfg.Logf("could not resolve HEAD: %v\n", err)
		return
	}
	binarySHA := extractBinarySHA(cfg.Version)
	needsRebuild := binarySHA != "" && !strings.HasPrefix(localRef, binarySHA)
	if needsRebuild {
		cfg.Logf("binary built from %s but HEAD is %s — rebuilding\n", binarySHA, localRef[:7])
	}

	// Also check remote for new upstream commits.
	if !needsRebuild {
		cfg.status("[upgrade] checking origin/%s ...", cfg.BaseBranch)

		fetchCmd := exec.Command("git", "fetch", "origin", cfg.BaseBranch)
		fetchCmd.Dir = cfg.Dir
		if out, err := fetchCmd.CombinedOutput(); err != nil {
			cfg.Logf("git fetch failed: %v\n%s\n", err, out)
			return
		}

		remoteRef, err := gitRevParse(cfg.Dir, "origin/"+cfg.BaseBranch)
		if err != nil {
			cfg.Logf("could not resolve origin/%s: %v\n", cfg.BaseBranch, err)
			return
		}
		if localRef == remoteRef {
			cfg.statusClear()
			return // up to date
		}
		// Only pull if remote is ahead of local. If local is ahead (unpushed
		// commits), we already checked the binary SHA against local HEAD above.
		mergeBaseCmd := exec.Command("git", "merge-base", "--is-ancestor", localRef, remoteRef)
		mergeBaseCmd.Dir = cfg.Dir
		if err := mergeBaseCmd.Run(); err != nil {
			// localRef is not an ancestor of remoteRef — local is ahead or diverged.
			// Either way, nothing to pull. The binary SHA check above already
			// handled whether a rebuild is needed.
			cfg.statusClear()
			return
		}
		needsRebuild = true
		cfg.Logf("new commits on origin/%s — pulling\n", cfg.BaseBranch)

		pullCmd := exec.Command("git", "pull", "--ff-only", "origin", cfg.BaseBranch)
		pullCmd.Dir = cfg.Dir
		if out, err := pullCmd.CombinedOutput(); err != nil {
			cfg.Logf("git pull --ff-only failed (local changes?): %v\n%s\n", err, out)
			return
		}
	}

	exe, err := executableFn()
	if err != nil {
		cfg.Logf("could not determine executable path: %v\n", err)
		return
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		cfg.Logf("could not resolve symlinks for executable: %v\n", err)
		return
	}

	cfg.Logf("rebuilding binary: %s\n", exe)

	buildCmd := exec.Command("go", "build", "-o", exe, ".")
	buildCmd.Dir = cfg.Dir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		cfg.Logf("build failed: %v\n%s\n", err, out)
		return
	}

	if cfg.PostBuildHook != nil {
		if err := cfg.PostBuildHook(exe, cfg.Dir); err != nil {
			cfg.Logf("post-build hook failed (non-fatal): %v\n", err)
			// Non-fatal — continue with re-exec.
		}
	}

	cfg.Logf("re-executing new binary\n")

	if err := execFn(exe, os.Args, os.Environ()); err != nil {
		cfg.Logf("exec failed: %v\n", err)
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

// IsSourceCheckout reports whether dir is a git checkout whose origin remote
// URL contains expectedRemoteSubstring (e.g. "handarbeit/fabrik"). Returns
// false on any error (no git, no remote, wrong remote, etc.).
func IsSourceCheckout(dir, expectedRemoteSubstring string) bool {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	url := strings.TrimSuffix(strings.TrimSpace(string(out)), ".git")
	return strings.Contains(url, expectedRemoteSubstring)
}

// gitRevParse resolves ref to a commit SHA in the git checkout at dir.
func gitRevParse(dir, ref string) (string, error) {
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
