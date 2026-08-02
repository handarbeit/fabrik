package engine

import "github.com/handarbeit/fabrik/warnings"

// sweepStaleAllowAutoMergeWarnings clears any recorded allow_auto_merge
// warning whose subject repo is no longer present on the current board
// (#1348). checkAllowAutoMerge's own Clear call is only reachable when a
// repo is re-checked AND auto-merge is now enabled — a repo that has left
// the board entirely is never re-checked at all, so its warning is
// otherwise immortal, the same durable-state-leak shape as the orphaned
// stage:*:in_progress labels (#1135) and the fabrik:claude-limit sweep
// (ADR-1183).
//
// seenRepos is the poll()-local set already computed from board.Items for
// the label-seeding path (R2: no new fetch) — this sweep must be called
// with that same set, not an independently recomputed one.
//
// The engine's own configured repo (single-repo mode only) is unioned in
// before comparing: checkAllowAutoMerge(e.cfg.Owner, e.cfg.Repo) fires
// unconditionally at Run() startup regardless of whether the board
// currently has any open items for that repo, but seenRepos is built
// purely from board.Items. Without this exemption, a transient
// zero-open-items poll for the operator's own configured repo (e.g.
// everything currently in Done) would durably clear a legitimate warning —
// and because checkedAutoMergeRepos never re-fires for that repo after the
// first startup call, the warning would not come back until a process
// restart. Multi-repo mode (e.cfg.Repo == "") has no equivalent
// always-present repo, so defaultRepo() returning "" is a correct no-op
// here.
//
// Runs every poll (cheap: warnings.ClearMissing performs a single load, and
// only saves when something was actually stale — R4/AC5). No API calls.
//
// present is required to be non-empty before any clearing is attempted: in
// multi-repo mode, retryOnEmpty (github/project.go) can still exhaust its
// retries and return a genuinely zero-item board (e.g. a persistent indexer
// hiccup, or a project that's transiently empty of open items across every
// configured repo). Unlike single-repo mode, multi-repo has no defaultRepo()
// to fall back on, so an empty seenRepos would otherwise make every
// allow_auto_merge warning for every repo look "absent" and wipe them all in
// one pass. Skipping the sweep entirely when present is empty mirrors the
// same "never destructively act on zero signal" reasoning behind the
// single-repo defaultRepo() exemption above.
func (e *Engine) sweepStaleAllowAutoMergeWarnings(seenRepos map[string]bool) {
	present := make(map[string]bool, len(seenRepos)+1)
	for r := range seenRepos {
		present[r] = true
	}
	if dr := e.defaultRepo(); dr != "" {
		present[dr] = true
	}
	if len(present) == 0 {
		return
	}
	cleared, err := warnings.ClearMissing("allow_auto_merge", present)
	if err != nil {
		e.logf(0, "warn", "stale allow_auto_merge warning sweep failed: %v\n", err)
		return
	}
	for _, key := range cleared {
		e.logf(0, "startup", "cleared stale warning %s: repo no longer on the board\n", key)
	}
}
