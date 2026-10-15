package github

// CheckRunStatus is the aggregate classification of a set of check runs.
type CheckRunStatus int

const (
	// CheckRunsReady means every latest-per-name run completed successfully
	// (or there are no check runs at all).
	CheckRunsReady CheckRunStatus = iota
	// CheckRunsPending means at least one latest-per-name run is still
	// queued/in_progress. Pending always takes precedence over failed —
	// a fresh rerun in progress must never be shadowed by a stale failure
	// of a different check (or of the same check under an older ID).
	CheckRunsPending
	// CheckRunsFailed means at least one latest-per-name run completed with
	// a failing conclusion and none are pending.
	CheckRunsFailed
	// CheckRunsNonPassing means at least one latest-per-name run completed
	// with a conclusion that is neither passing (success/neutral/skipped) nor
	// a failure (failure/timed_out/action_required): cancelled, stale, an
	// empty conclusion on a completed run, or a value GitHub adds later. The
	// check was supposed to run and produced no result, so it is never ready —
	// but it is also not a code failure, and callers (the merge train above
	// all) must not charge, eject or pause for it. Appended after Failed so
	// existing values keep their meaning; a genuine failure in the same set
	// still classifies as CheckRunsFailed (the stronger disposition wins).
	CheckRunsNonPassing
)

// IsPassingConclusion reports whether a completed check run's conclusion
// counts as passing: success, neutral or skipped. A skipped check means the
// workflow decided the check does not apply to this head (a path filter);
// everything else — including cancelled, stale, empty and unrecognised
// values — has no passing result.
func IsPassingConclusion(conclusion string) bool {
	switch conclusion {
	case "success", "neutral", "skipped":
		return true
	}
	return false
}

// IsFailureConclusion reports whether a completed check run's conclusion is a
// genuine failure: failure, timed_out or action_required.
func IsFailureConclusion(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "action_required":
		return true
	}
	return false
}

// ClassifyCheckRuns reduces checkRuns to the latest run per check name
// (highest ID wins — GitHub check-run IDs are monotonically increasing, so
// this discards a stale completed/failed entry left behind when a check is
// rerun under a new ID) and classifies the result. Any pending run, at any
// name, takes global precedence over any failed run: a check still running
// is never outweighed by a different check (or a superseded run of the same
// check) that has already failed.
//
// Precedence is pending > failed > non-passing > ready. A completed run is
// passing only for success/neutral/skipped (IsPassingConclusion); a failure
// conclusion (IsFailureConclusion) lands in failed; every other completed
// conclusion — cancelled, stale, empty, unrecognised — is non-passing and is
// reported by NonPassingCheckRuns, not here, so the three-value signature
// stays stable.
//
// This is the single source of truth for check-run classification, shared by
// settlePRMergeState (engine/pr_settle.go) and checkCIGate (engine/ci.go) so
// the two call sites cannot drift out of agreement.
func ClassifyCheckRuns(checkRuns []CheckRun) (status CheckRunStatus, pending, failed []CheckRun) {
	nonPassing := false
	for _, cr := range latestCheckRunsByName(checkRuns) {
		switch cr.Status {
		case "queued", "in_progress":
			pending = append(pending, cr)
		case "completed":
			if IsFailureConclusion(cr.Conclusion) {
				failed = append(failed, cr)
			} else if !IsPassingConclusion(cr.Conclusion) {
				nonPassing = true
			}
		}
	}

	switch {
	case len(pending) > 0:
		status = CheckRunsPending
	case len(failed) > 0:
		status = CheckRunsFailed
	case nonPassing:
		status = CheckRunsNonPassing
	default:
		status = CheckRunsReady
	}
	return status, pending, failed
}

// NonPassingCheckRuns returns the latest-per-name completed runs whose
// conclusion is neither passing nor a failure (cancelled, stale, empty,
// unrecognised), in first-appearance order. Pending runs are not included.
func NonPassingCheckRuns(checkRuns []CheckRun) []CheckRun {
	var out []CheckRun
	for _, cr := range latestCheckRunsByName(checkRuns) {
		if cr.Status == "completed" && !IsPassingConclusion(cr.Conclusion) && !IsFailureConclusion(cr.Conclusion) {
			out = append(out, cr)
		}
	}
	return out
}

// LatestCheckRuns is the exported form of latestCheckRunsByName, for callers
// that need the latest-per-name reduction without ClassifyCheckRuns's
// pending/failed split (engine/ci_status.go's "all passed" check).
func LatestCheckRuns(checkRuns []CheckRun) []CheckRun {
	return latestCheckRunsByName(checkRuns)
}

// latestCheckRunsByName reduces checkRuns to one entry per Name, keeping the
// run with the highest ID (GitHub check-run IDs are monotonically increasing,
// so the highest ID is the most recent rerun of that check name). Output
// order follows each name's first appearance in checkRuns — Go map iteration
// order is randomized, so this keeps ClassifyCheckRuns's pending/failed
// slices (and anything logged or rendered from them) deterministic.
func latestCheckRunsByName(checkRuns []CheckRun) []CheckRun {
	byName := make(map[string]CheckRun, len(checkRuns))
	names := make([]string, 0, len(checkRuns))
	for _, cr := range checkRuns {
		existing, ok := byName[cr.Name]
		if !ok {
			names = append(names, cr.Name)
			byName[cr.Name] = cr
		} else if cr.ID > existing.ID {
			byName[cr.Name] = cr
		}
	}
	out := make([]CheckRun, 0, len(byName))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}
