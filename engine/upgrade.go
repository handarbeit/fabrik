package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/selfupgrade"
	"github.com/handarbeit/fabrik/warnings"
)

// fabrikOwner and fabrikRepo are the canonical owner/repo for fabrik itself.
// These are used when checking the GitHub Releases API for a newer binary —
// the release always targets handarbeit/fabrik, not the user's managed project.
const (
	fabrikOwner = "handarbeit"
	fabrikRepo  = "fabrik"
)

// checkAndUpgrade selects the upgrade path based on the running version:
//   - dev builds (version starts with "dev"): git pull → go build → re-exec
//   - release builds (all other versions): GitHub Releases API → download → atomic replace → re-exec
//
// The actual logic lives in internal/selfupgrade, which neither daemon owns —
// see that package's doc comment and adrs/1196-extract-self-upgrade-package.md.
func (e *Engine) checkAndUpgrade() {
	if !strings.HasPrefix(e.cfg.Version, "dev") {
		e.checkReleaseUpgrade()
		return
	}

	logf := func(format string, args ...any) {
		e.logf(0, "upgrade", format, args...)
	}

	selfupgrade.CheckAndRebuildDev(selfupgrade.DevBuildConfig{
		Dir:            e.fabrikDir,
		Version:        e.cfg.Version,
		BaseBranch:     "main",
		ExpectedRemote: fabrikOwner + "/" + fabrikRepo,
		Logf:           logf,
		StatusFn:       pollStatus,
		StatusClearFn:  pollStatusClear,
		PostBuildHook: func(exe, dir string) error {
			// Refresh plugin skills from the new binary.
			e.logf(0, "upgrade", "refreshing plugin skills\n")
			upgradeCmd := exec.Command(exe, "upgrade")
			upgradeCmd.Dir = dir
			if out, err := upgradeCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%w\n%s", err, out)
			}
			return nil
		},
	})
}

// releaseUpgradeToken returns the token to authenticate self-upgrade's
// github.com requests (both the release-metadata lookup and the asset
// download) with. cfg.Token authenticates whatever host the engine is
// configured against; when that's a GHES host, cfg.Token is a credential
// scoped to the GHES instance and is not valid on github.com — sending it
// to api.github.com fails with "Bad credentials" (401) rather than
// succeeding unauthenticated. handarbeit/fabrik's releases are public, so
// self-upgrade works fine unauthenticated (just more tightly rate-limited);
// dropping the token entirely when a GHES host is configured is safer than
// sending one guaranteed to be rejected.
func releaseUpgradeToken(cfg Config) string {
	if cfg.GHESHost != "" {
		return ""
	}
	return cfg.Token
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
	// Error discarded intentionally: failures are logged by
	// selfupgrade.PerformReleaseUpgrade itself via logf, and this caller's
	// contract is non-fatal — the poll loop continues regardless (unlike the
	// foreground `fabrik upgrade` command).
	_ = selfupgrade.PerformReleaseUpgrade(selfupgrade.ReleaseConfig{
		Client:     e.releaseClient,
		Owner:      fabrikOwner,
		Repo:       fabrikRepo,
		BinaryName: "fabrik",
		Version:    e.cfg.Version,
		Token:      releaseUpgradeToken(e.cfg),
		ExtraEnv:   []string{"FABRIK_AUTO_UPGRADED=1"},
		Logf:       logf,
	})
}

// versionSkewExecutableFn resolves the path to the currently-running
// executable, for checkVersionSkew only. A private seam (distinct from
// internal/selfupgrade's own executableFn) since checkVersionSkew is out of
// scope for the selfupgrade extraction and has no other reason to depend on
// that package's internals.
var versionSkewExecutableFn = os.Executable

// versionSkewExecCommandFn is a seam over exec.CommandContext so tests can
// stub the on-disk binary's `--version` output without spawning a real
// process. Production code leaves this as exec.CommandContext.
var versionSkewExecCommandFn = exec.CommandContext

// checkVersionSkew compares the on-disk binary's reported version (via a
// throttled `<exe> --version` subprocess) against the running process's
// version, recording a persistent warnings/ entry on mismatch — see #1074.
// This is a general safety net for "the binary on disk has moved on but this
// process hasn't," whatever the cause: a SemverGreater bug, a failed
// syscall.Exec re-exec, or a manual/external replacement (e.g. a fleet
// sharing ~/go/bin/fabrik). Non-fatal: any error resolving the path or
// running the subprocess is logged and the check is skipped for this poll.
//
// Unlike allow_auto_merge/stage_drift/undeclared_reviewers (#1348), this
// warning needs no separate stale-sweep. Its key's subject — the resolved
// on-disk executable path — is re-derived fresh on every idle-upgrade check
// (poll.go), not looked up against a shrinking discovered set (board repos,
// configured stages). The Clear branch below is therefore reachable on
// every single evaluation, so the warning can never outlive the condition
// that produced it.
func (e *Engine) checkVersionSkew(ctx context.Context) {
	exe, err := versionSkewExecutableFn()
	if err != nil {
		e.logf(0, "upgrade", "version-skew check: could not determine executable path: %v\n", err)
		return
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		e.logf(0, "upgrade", "version-skew check: could not resolve symlinks for executable: %v\n", err)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := versionSkewExecCommandFn(ctx, exe, "--version").Output()
	if err != nil {
		e.logf(0, "upgrade", "version-skew check: running %s --version: %v\n", exe, err)
		return
	}

	diskVersion := strings.TrimSpace(string(out))
	running := e.cfg.Version
	key := "version_skew:" + exe

	if diskVersion == running {
		_ = warnings.Clear(key)
		return
	}

	e.logf(0, "upgrade", "WARNING: on-disk binary version (%s) differs from running version (%s) — a pending upgrade may be stuck; restart with 'kill -HUP %d' to pick it up\n", diskVersion, running, os.Getpid())
	_ = warnings.Record(warnings.Entry{
		Key:       key,
		Type:      "version_skew",
		Title:     "Running version differs from on-disk binary",
		Detail:    fmt.Sprintf("The daemon is running version %s but the binary on disk at %s reports %s. This can happen when an upgrade replaced the binary but the process failed to re-exec, or when another process replaced the binary externally.\n\nFix: kill -HUP %d (restarts in place, picking up the on-disk binary).", running, exe, diskVersion, os.Getpid()),
		FixAction: "shell_command",
		FixParams: map[string]string{"cmd": fmt.Sprintf("kill -HUP %d", os.Getpid())},
	})
}
