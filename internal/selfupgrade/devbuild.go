package selfupgrade

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
	// PreExecHook, if non-nil, runs immediately before the re-exec — the last
	// point a caller can flush state an exec would otherwise lose (an exec runs
	// no deferred cleanup). It must not tear anything down: if the exec fails the
	// process keeps running.
	PreExecHook func()
	// ExtraEnv is appended to the environment for the re-exec (e.g. a restart
	// marker the new process reads at startup); nil for no extras.
	ExtraEnv []string
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
	// ("dev(<sha>)", optionally followed by a suffix such as "+dirty"), or ""
	// if Version isn't in that form.
	BinarySHA string

	// BinaryDirty is true when the version string carries the "+dirty" suffix
	// — the binary was built from a tree with uncommitted or untracked files.
	// It only words the same-SHA note in Reason; it never affects whether a
	// rebuild is needed (see the R2 decision on CompareDevBuild).
	BinaryDirty bool

	// Reason is a one-line explanation of why no rebuild is needed, set only
	// when Applicable and NeedsRebuild are such that CheckAndRebuildDev would
	// otherwise exit silently (not a source checkout, up to date, local ahead
	// or diverged). Empty whenever a rebuild is needed or an error is
	// returned. CompareDevBuild fills it in without logging so read-only
	// reporters (checkSourceStaleness) gain no new log output; only
	// CheckAndRebuildDev logs it.
	Reason string

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
//
// R2 (#2234): a "+dirty" binary at the same SHA as HEAD is NOT rebuilt. The
// decision keys on the SHA alone, so such a build is compared exactly like a
// clean one. Rebuilding would pick up no committed change and the result
// would be stamped dirty again — the dirt in the checkout, not the binary, is
// what needs fixing. Instead the outcome line (Reason) says so, and
// CheckAndRebuildDev reports the dirty paths once.
func CompareDevBuild(cfg DevBuildConfig) (DevBuildStatus, error) {
	var status DevBuildStatus
	if !IsSourceCheckout(cfg.Dir, cfg.ExpectedRemote) {
		status.Reason = fmt.Sprintf("self-upgrade not applicable: %s is not a source checkout of %s", cfg.Dir, cfg.ExpectedRemote)
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

	status.BinarySHA, status.BinaryDirty = parseDevVersion(cfg.Version)
	// notes are appended to whichever "no rebuild" outcome follows, so each
	// check still logs exactly one line.
	var notes string
	if status.BinarySHA == "" {
		notes = fmt.Sprintf("; note: version %q has no parseable commit SHA, binary-vs-HEAD check skipped", cfg.Version)
	}
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
	if status.BinaryDirty && status.BinarySHA != "" {
		notes += "; binary was built from a dirty tree at the current commit, rebuilding would not clear that"
	}
	if localRef == remoteRef {
		cfg.statusClear()
		status.Reason = fmt.Sprintf("up to date with origin/%s at %s%s", cfg.BaseBranch, shortRef(localRef), notes)
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
		status.Reason = fmt.Sprintf("local %s is ahead of or diverged from origin/%s (%s), nothing to pull%s",
			shortRef(localRef), cfg.BaseBranch, shortRef(remoteRef), notes)
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
		cfg.Logf("%s\n", status.Reason)
		return
	}
	reportDirtyCheckout(cfg)

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
		// Up to date, or local ahead of/diverged from remote — nothing to do,
		// but say so: a silent "checking ..." hid #2234.
		cfg.Logf("%s\n", status.Reason)
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

	if cfg.PreExecHook != nil {
		cfg.PreExecHook()
	}
	if err := execFn(exe, os.Args, append(os.Environ(), cfg.ExtraEnv...)); err != nil {
		cfg.Logf("exec failed: %v\n", err)
	}
}

// extractBinarySHA extracts the short SHA from a dev version string like
// "dev(abc1234)" or "dev(abc1234)+dirty". Returns "" if the version is not a
// dev build or has no SHA.
func extractBinarySHA(version string) string {
	sha, _ := parseDevVersion(version)
	return sha
}

// parseDevVersion parses "dev(<sha>)<suffix>". The SHA is everything up to the
// first ")"; the suffix after it is opaque (the build stamps "+dirty" when the
// tree had uncommitted or untracked files), and dirty reports whether it
// contains "+dirty". An empty SHA, a missing ")" or a non-dev version yields
// "" — an empty SHA must stay unparseable, since every ref has "" as a prefix
// and the binary-vs-HEAD check would otherwise never fire.
func parseDevVersion(version string) (sha string, dirty bool) {
	rest, ok := strings.CutPrefix(version, "dev(")
	if !ok {
		return "", false
	}
	sha, suffix, ok := strings.Cut(rest, ")")
	if !ok || sha == "" {
		return "", false
	}
	return sha, strings.Contains(suffix, "+dirty")
}

func shortRef(ref string) string {
	if len(ref) > 7 {
		return ref[:7]
	}
	return ref
}

// maxDirtyPathsListed caps the dirty paths named in the one-time report.
const maxDirtyPathsListed = 10

// dirtyReported makes the dirty-checkout report once per process lifetime.
// DevBuildConfig is rebuilt by value on every call in both daemons, so
// caller-owned state would need shared pointers in each.
var dirtyReported atomic.Bool

func resetDirtyReportedForTest() { dirtyReported.Store(false) }

// dirtyPaths returns the paths `git status --porcelain` lists in dir, or nil
// on any error. Untracked directories are collapsed to one entry. Unscoped on
// purpose: a dirty file anywhere in the repo is what stamps a build +dirty.
func dirtyPaths(dir string) []string {
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=normal")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) > 3 {
			paths = append(paths, line[3:])
		}
	}
	return paths
}

// reportDirtyCheckout logs, once per process, that the source checkout has
// uncommitted or untracked changes and names the first few paths — the usual
// reason a dev daemon stops upgrading (#2234). A clean checkout or a git
// failure logs nothing and does not consume the once-only report.
func reportDirtyCheckout(cfg DevBuildConfig) {
	if dirtyReported.Load() {
		return
	}
	paths := dirtyPaths(cfg.Dir)
	if len(paths) == 0 || !dirtyReported.CompareAndSwap(false, true) {
		return
	}
	listed := paths
	more := ""
	if len(listed) > maxDirtyPathsListed {
		more = fmt.Sprintf(", and %d more", len(listed)-maxDirtyPathsListed)
		listed = listed[:maxDirtyPathsListed]
	}
	cfg.Logf("source checkout has uncommitted/untracked changes (%d paths): %s%s — builds from it are stamped +dirty and may not self-upgrade\n",
		len(paths), strings.Join(listed, ", "), more)
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
