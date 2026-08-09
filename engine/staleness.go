package engine

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/selfupgrade"
	"github.com/handarbeit/fabrik/warnings"
)

// stalenessCheckPollInterval is how many poll() calls elapse between
// source-checkout staleness checks (R4). 30 polls at the default 30s --poll
// interval is ≈15 minutes — frequent enough that a busy board notices
// staleness well within a working day, infrequent enough that the git fetch
// it requires (network I/O) never becomes a meaningful fraction of a poll
// cycle's GitHub-adjacent traffic. Unlike idleUpgradeThreshold (consecutive
// *idle* polls), this is a flat poll count with no idleness precondition —
// that's what makes it reachable on a continuously busy board (R1).
const stalenessCheckPollInterval = 30

// sourceStalenessWarningKey is the single fixed warnings/ key for the
// source-checkout staleness notice. Unlike allow_auto_merge/version_skew
// (keyed per-repo/per-exe), there is exactly one source checkout per Fabrik
// process, so one fixed key is sufficient — no ClearMissing sweep is needed
// because checkSourceStaleness's Clear branch is reachable on every
// throttled evaluation (see its doc comment), the same reasoning
// checkVersionSkew already documents for its own key.
const sourceStalenessWarningKey = "source_staleness"

// maybeCheckSourceStaleness is the poll-count gate for checkSourceStaleness:
// it fires on the very first call (covering "check at startup" for free) and
// every stalenessCheckPollInterval calls thereafter. Called unconditionally
// from poll()'s per-poll settle-scan section — never nested inside the
// dispatched==0/idleCount block that gates the actual upgrade-exec path, so
// it stays reachable on a board that never goes idle (R1).
func (e *Engine) maybeCheckSourceStaleness() {
	if e.pollsUntilStalenessCheck > 0 {
		e.pollsUntilStalenessCheck--
		return
	}
	// stalenessCheckPollInterval-1, not stalenessCheckPollInterval: this call
	// itself is the first of the next interval's calls, so only
	// stalenessCheckPollInterval-1 more decrementing calls remain before the
	// next fire. Setting it to the full interval would make the actual gap
	// between fires stalenessCheckPollInterval+1 calls.
	e.pollsUntilStalenessCheck = stalenessCheckPollInterval - 1
	e.checkSourceStaleness()
}

// checkSourceStaleness reports (never acts on) how far the running dev build
// is behind origin/main, via selfupgrade.CompareDevBuild — the same
// comparison CheckAndRebuildDev uses to decide whether to rebuild, so this
// warning and the actual upgrade decision can never disagree (R4). It never
// calls git pull, go build, or exec: CompareDevBuild structurally stops
// before any of those, and checkSourceStaleness does nothing beyond read its
// result.
//
// Applicability mirrors checkAndUpgrade's own fork: only dev builds
// (Version starts with "dev") reach selfupgrade at all — a release build
// never calls into this package for this check, so it never runs a git
// fetch (R6). CompareDevBuild's own IsSourceCheckout gate then covers the
// remaining "is this actually the fabrik source tree" case.
//
// Uses the single fixed key sourceStalenessWarningKey: there is exactly one
// source checkout per Fabrik process, so (mirroring checkVersionSkew's
// documented reasoning) the Clear branch below is reachable on every
// throttled evaluation — the warning can never outlive the condition that
// produced it, and needs no separate stale-sweep.
func (e *Engine) checkSourceStaleness() {
	if !strings.HasPrefix(e.cfg.Version, "dev") {
		return
	}

	compare := e.stalenessCompareFn
	if compare == nil {
		compare = selfupgrade.CompareDevBuild
	}

	logf := func(format string, args ...any) {
		e.logf(0, "upgrade", format, args...)
	}

	status, err := compare(selfupgrade.DevBuildConfig{
		Dir:            e.fabrikDir,
		Version:        e.cfg.Version,
		BaseBranch:     "main",
		ExpectedRemote: fabrikOwner + "/" + fabrikRepo,
		Logf:           logf,
	})
	if err != nil {
		logf("staleness check: %v\n", err)
		return
	}
	if !status.Applicable {
		return
	}

	if !status.NeedsRebuild {
		_ = warnings.Clear(sourceStalenessWarningKey)
		return
	}

	title, detail, fixAction, fixParams := stalenessWarningContent(status, e.cfg.AutoUpgrade)
	logf("WARNING: %s\n", title)
	_ = warnings.Record(warnings.Entry{
		Key:       sourceStalenessWarningKey,
		Type:      "source_staleness",
		Title:     title,
		Detail:    detail,
		FixAction: fixAction,
		FixParams: fixParams,
	})
}

// stalenessWarningContent builds the Title/Detail/FixAction/FixParams for a
// stale-build warning. The fix action is conditional on AutoUpgrade: a
// SIGHUP re-execs the running binary in place (performSighupRestart), which
// re-enters main() and re-runs the startup-time checkAndUpgrade() call — for
// a dev build, that call pulls/rebuilds/re-execs onto current code, so
// "kill -HUP" is a genuine one-shot fix only when AutoUpgrade is enabled.
// When it's disabled, that startup check never runs either, so a SIGHUP just
// re-execs the same stale binary — offering it as a fix action there would
// be actively misleading, so FixAction/FixParams are left empty and the
// Detail spells out the manual steps instead.
func stalenessWarningContent(status selfupgrade.DevBuildStatus, autoUpgrade bool) (title, detail, fixAction string, fixParams map[string]string) {
	sha := status.LocalRef
	if len(sha) > 7 {
		sha = sha[:7]
	}

	var behind string
	switch {
	case status.RemoteAhead && status.CommitsBehind > 0:
		behind = fmt.Sprintf("%d commit", status.CommitsBehind)
		if status.CommitsBehind != 1 {
			behind += "s"
		}
		behind += " behind origin/main"
	case status.RemoteAhead:
		// RemoteAhead is only true when localRef is a proper ancestor of
		// remoteRef, which always means at least one commit is behind — a
		// CommitsBehind of 0 here can only mean gitRevListCount itself failed
		// (left at its zero value; see the field's doc comment), not a
		// genuine zero. Fall back to generic phrasing rather than rendering
		// the self-contradictory "0 commits behind" alongside a warning that
		// a rebuild is needed.
		behind = "running an older commit than origin/main"
	default:
		// ShaMismatch: a local commit exists that the running binary predates.
		// The SHA-mismatch shortcut skips the fetch, so there's no origin-relative
		// commit count to report — see CompareDevBuild's doc comment.
		behind = "running an older commit than the local checkout's HEAD"
	}

	var age string
	if !status.LocalCommitTime.IsZero() {
		age = fmt.Sprintf(", %s old", roundedSince(status.LocalCommitTime))
	}

	title = fmt.Sprintf("Running binary is %s", behind)

	var fix string
	if autoUpgrade {
		fix = fmt.Sprintf("Fix: kill -HUP %d — auto-upgrade is enabled, so restarting re-execs onto the pending build automatically.", os.Getpid())
		fixAction = "shell_command"
		fixParams = map[string]string{"cmd": fmt.Sprintf("kill -HUP %d", os.Getpid())}
	} else {
		fix = "Fix: auto-upgrade is disabled. Pull and rebuild manually (git pull && go build), then restart — kill -HUP alone re-execs the same stale binary, it does not pick up the new commits."
	}

	detail = fmt.Sprintf(
		"The running binary was built from %s%s. It is %s. %s",
		sha, age, behind, fix,
	)
	return title, detail, fixAction, fixParams
}

// roundedSince renders time.Since(t) at minute granularity for a
// human-readable "how old is this" detail (e.g. "15h32m0s" -> "15h32m").
func roundedSince(t time.Time) string {
	return time.Since(t).Round(time.Minute).String()
}
