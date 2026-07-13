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
)

// ClassifyCheckRuns reduces checkRuns to the latest run per check name
// (highest ID wins — GitHub check-run IDs are monotonically increasing, so
// this discards a stale completed/failed entry left behind when a check is
// rerun under a new ID) and classifies the result. Any pending run, at any
// name, takes global precedence over any failed run: a check still running
// is never outweighed by a different check (or a superseded run of the same
// check) that has already failed.
//
// This is the single source of truth for check-run classification, shared by
// settlePRMergeState (engine/pr_settle.go) and checkCIGate (engine/ci.go) so
// the two call sites cannot drift out of agreement.
func ClassifyCheckRuns(checkRuns []CheckRun) (status CheckRunStatus, pending, failed []CheckRun) {
	for _, cr := range latestCheckRunsByName(checkRuns) {
		switch cr.Status {
		case "queued", "in_progress":
			pending = append(pending, cr)
		case "completed":
			switch cr.Conclusion {
			case "failure", "timed_out", "action_required":
				failed = append(failed, cr)
			}
		}
	}

	switch {
	case len(pending) > 0:
		status = CheckRunsPending
	case len(failed) > 0:
		status = CheckRunsFailed
	default:
		status = CheckRunsReady
	}
	return status, pending, failed
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
