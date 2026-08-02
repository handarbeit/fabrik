package pruefer

import (
	"path/filepath"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/selfupgrade"
)

// fabrikOwner and fabrikRepo are the canonical owner/repo Pruefer's releases
// come from — Pruefer ships from the fabrik monorepo's release, not a
// separate repo (see ADR-1197).
const (
	fabrikOwner = "handarbeit"
	fabrikRepo  = "fabrik"
)

// releaseAPIBaseURL overrides the GitHub API host used by
// checkReleaseUpgrade's dedicated release-check client. Empty (the
// production default) means the real GitHub API; tests point this at an
// httptest server so no real network call is made.
var releaseAPIBaseURL string

// checkAndUpgrade selects the upgrade path based on the running version,
// mirroring engine/upgrade.go's checkAndUpgrade:
//   - dev builds (version starts with "dev"): git pull → go build → re-exec
//   - release builds (all other versions): GitHub Releases API → download →
//     atomic replace → re-exec
//
// The actual logic lives in internal/selfupgrade, which neither daemon owns
// — see that package's doc comment and adrs/1196-extract-self-upgrade-package.md.
//
// Callers must only invoke this at a poll boundary when no review is in
// flight (see Daemon.Run) — re-exec'ing mid-review orphans the in-flight
// ephemeral clone and claude subprocess.
func (d *Daemon) checkAndUpgrade() {
	if !strings.HasPrefix(Version, "dev") {
		d.checkReleaseUpgrade()
		return
	}

	logfn := func(format string, args ...any) {
		logf(0, "upgrade", format, args...)
	}

	selfupgrade.CheckAndRebuildDev(selfupgrade.DevBuildConfig{
		Dir:            devBuildDir(d.FabrikDir),
		Version:        Version,
		BaseBranch:     "main",
		ExpectedRemote: fabrikOwner + "/" + fabrikRepo,
		Logf:           logfn,
	})
}

// devBuildDir resolves the source-checkout subdirectory CheckAndRebuildDev
// should operate in for Pruefer's dev-build path. Unlike fabrik, whose
// main.go lives at the checkout root, Pruefer's main package lives at
// cmd/pruefer — pointing Dir there makes both the git subcommands (which
// resolve correctly from any subdirectory of the checkout) and
// `go build -o exe .` (which builds whatever package "." resolves to
// relative to Dir) operate on the right package, with no changes needed to
// internal/selfupgrade itself. fabrikDir is d.FabrikDir; "" defaults to ".".
func devBuildDir(fabrikDir string) string {
	if fabrikDir == "" {
		fabrikDir = "."
	}
	return filepath.Join(fabrikDir, "cmd", "pruefer")
}

// checkReleaseUpgrade is the release-based upgrade path. It checks the
// GitHub Releases API for a version newer than the running binary, downloads
// the matching platform asset, atomically replaces the running binary, and
// re-execs. Unlike Pruefer's per-owner App installation tokens (scoped to
// Config.WatchedRepos, not guaranteed to cover handarbeit/fabrik), this uses
// a dedicated unauthenticated client — fabrik is a public repo, so this is
// sufficient for both the releases-API lookup and the asset download (see
// ADR-1197 for the tradeoff).
//
// All failures are non-fatal: a warning is logged and the poll loop continues.
func (d *Daemon) checkReleaseUpgrade() {
	logfn := func(format string, args ...any) {
		logf(0, "upgrade", format, args...)
	}
	// Error discarded intentionally: failures are logged by
	// selfupgrade.PerformReleaseUpgrade itself via logf, and this caller's
	// contract is non-fatal — the poll loop continues regardless.
	_ = selfupgrade.PerformReleaseUpgrade(selfupgrade.ReleaseConfig{
		Client:     gh.NewClientWithBaseURL("", releaseAPIBaseURL),
		Owner:      fabrikOwner,
		Repo:       fabrikRepo,
		BinaryName: "pruefer",
		Version:    Version,
		Logf:       logfn,
	})
}
