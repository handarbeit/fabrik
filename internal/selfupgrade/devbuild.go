package selfupgrade

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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

// DevBuildStatus is the result of comparing a dev build's running SHA and
// local git checkout against its upstream branch — the read-only comparison
// CheckAndRebuildDev performs before deciding whether to act. Computed by
// CompareDevBuild, which never calls `git pull`, `go build`, or execFn; only
// CheckAndRebuildDev's own code (downstream of CompareDevBuild) does that.
type DevBuildStatus struct {
	// Applicable is false when Dir is not a source checkout matching
	// ExpectedRemote (IsSourceCheckout) — every other field is zero-valued.
	Applicable bool

	// LocalRef is the resolved SHA of local HEAD.
	LocalRef string

	// LocalCommitTime is the commit timestamp of LocalRef — how old the code
	// actually running is. Left zero if it could not be determined; this is
	// non-fatal since it's an incidental reporting detail, not the applicability
	// or rebuild-need decision.
	LocalCommitTime time.Time

	// BinarySHA is the SHA embedded in the running version string
	// ("dev(<sha>)"), or "" if Version isn't in that form.
	BinarySHA string

	// ShaMismatch is true when BinarySHA is non-empty and does not prefix
	// LocalRef — the running binary predates the checkout's own HEAD. When
	// true, the remote comparison below is skipped entirely (no git fetch),
	// mirroring CheckAndRebuildDev's pre-extraction no-fetch shortcut;
	// RemoteRef, RemoteAhead, and CommitsBehind are left zero-valued.
	ShaMismatch bool

	// RemoteRef is the resolved SHA of origin/BaseBranch, populated only when
	// the remote comparison ran (i.e. ShaMismatch is false and the fetch
	// succeeded).
	RemoteRef string

	// RemoteAhead is true when RemoteRef is a descendant of LocalRef (origin
	// has commits the checkout doesn't) — as opposed to LocalRef being ahead
	// of or diverged from RemoteRef, in which case nothing is pulled.
	RemoteAhead bool

	// CommitsBehind is the number of commits LocalRef is behind RemoteRef.
	// Only meaningful when RemoteAhead is true; left 0 when the count itself
	// could not be computed (non-fatal, same reasoning as LocalCommitTime).
	CommitsBehind int

	// NeedsRebuild is true when either ShaMismatch or RemoteAhead is true —
	// i.e. CheckAndRebuildDev would proceed to pull/build/exec.
	NeedsRebuild bool
}

// CompareDevBuild performs the read-only comparison CheckAndRebuildDev needs
// to decide whether a dev-build source checkout is stale: local HEAD vs. the
// running binary's embedded SHA, and (when that alone is inconclusive) local
// HEAD vs. origin/BaseBranch after a `git fetch`. It never mutates the
// checkout beyond that fetch, and never calls `git pull`, `go build`, or
// execFn — CheckAndRebuildDev is the only caller that takes those actions,
// based on the DevBuildStatus this returns. This split lets a caller that
// only wants to *report* staleness (e.g. a busy-board warning, see #1464) do
// so without any risk of triggering a rebuild.
//
// A non-nil error means the comparison could not be completed (HEAD or
// origin/BaseBranch unresolvable, or the fetch itself failed); the returned
// status is the partial result up to that point.
func CompareDevBuild(cfg DevBuildConfig) (DevBuildStatus, error) {
	var status DevBuildStatus
	if !IsSourceCheckout(cfg.Dir, cfg.ExpectedRemote) {
		return status, nil
	}
	status.Applicable = true

	// Check local HEAD first — detects local commits that haven't been pushed.
	localRef, err := gitRevParse(cfg.Dir, "HEAD")
	if err != nil {
		return status, fmt.Errorf("could not resolve HEAD: %w", err)
	}
	status.LocalRef = localRef
	if t, terr := gitCommitTime(cfg.Dir, localRef); terr == nil {
		status.LocalCommitTime = t
	}

	status.BinarySHA = extractBinarySHA(cfg.Version)
	status.ShaMismatch = status.BinarySHA != "" && !strings.HasPrefix(localRef, status.BinarySHA)
	if status.ShaMismatch {
		status.NeedsRebuild = true
		return status, nil
	}

	// Also check remote for new upstream commits.
	cfg.status("[upgrade] checking origin/%s ...", cfg.BaseBranch)

	fetchCmd := exec.Command("git", "fetch", "origin", cfg.BaseBranch)
	fetchCmd.Dir = cfg.Dir
	if out, ferr := fetchCmd.CombinedOutput(); ferr != nil {
		return status, fmt.Errorf("git fetch failed: %w\n%s", ferr, out)
	}

	remoteRef, err := gitRevParse(cfg.Dir, "origin/"+cfg.BaseBranch)
	if err != nil {
		return status, fmt.Errorf("could not resolve origin/%s: %w", cfg.BaseBranch, err)
	}
	status.RemoteRef = remoteRef
	if localRef == remoteRef {
		cfg.statusClear()
		return status, nil // up to date
	}
	// Only "remote ahead" if remote is ahead of local. If local is ahead
	// (unpushed commits) or diverged, we already checked the binary SHA
	// against local HEAD above.
	mergeBaseCmd := exec.Command("git", "merge-base", "--is-ancestor", localRef, remoteRef)
	mergeBaseCmd.Dir = cfg.Dir
	if merr := mergeBaseCmd.Run(); merr != nil {
		// localRef is not an ancestor of remoteRef — local is ahead or diverged.
		// Either way, nothing to pull. The binary SHA check above already
		// handled whether a rebuild is needed.
		cfg.statusClear()
		return status, nil
	}

	status.RemoteAhead = true
	status.NeedsRebuild = true
	if count, cerr := gitRevListCount(cfg.Dir, localRef, remoteRef); cerr == nil {
		status.CommitsBehind = count
	}
	return status, nil
}

// CheckAndRebuildDev implements the dev-build self-upgrade path: compare the
// running binary's embedded SHA (and, failing that, the local checkout's
// remote) against upstream via CompareDevBuild, rebuild from source with
// `go build`, run the optional PostBuildHook, and re-exec. Requires cfg.Dir
// to be a git source checkout matching cfg.ExpectedRemote (IsSourceCheckout)
// — a dev-mode daemon can only self-upgrade when run from its own source
// tree. All failures are logged via cfg.Logf and non-fatal: the function
// simply returns without upgrading.
func CheckAndRebuildDev(cfg DevBuildConfig) {
	status, err := CompareDevBuild(cfg)
	if err != nil {
		cfg.Logf("%v\n", err)
		return
	}
	if !status.Applicable {
		return
	}

	switch {
	case status.ShaMismatch:
		cfg.Logf("binary built from %s but HEAD is %s — rebuilding\n", status.BinarySHA, status.LocalRef[:7])
	case status.RemoteAhead:
		cfg.Logf("new commits on origin/%s — pulling\n", cfg.BaseBranch)
		pullCmd := exec.Command("git", "pull", "--ff-only", "origin", cfg.BaseBranch)
		pullCmd.Dir = cfg.Dir
		if out, err := pullCmd.CombinedOutput(); err != nil {
			cfg.Logf("git pull --ff-only failed (local changes?): %v\n%s\n", err, out)
			return
		}
	default:
		// Up to date, or local ahead of/diverged from remote — nothing to do.
		return
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

// gitRevListCount returns the number of commits reachable from "to" but not
// from "from" in the git checkout at dir — i.e. `git rev-list --count
// from..to`.
func gitRevListCount(dir, from, to string) (int, error) {
	cmd := exec.Command("git", "rev-list", "--count", from+".."+to)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("parsing rev-list count: %w", err)
	}
	return n, nil
}

// gitCommitTime returns the commit timestamp of ref in the git checkout at
// dir.
func gitCommitTime(dir, ref string) (time.Time, error) {
	cmd := exec.Command("git", "log", "-1", "--format=%cI", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
}
