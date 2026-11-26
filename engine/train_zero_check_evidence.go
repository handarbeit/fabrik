package engine

import (
	"fmt"
)

// memberCIEvidence is the positive-evidence rule for the zero-check-runs green path
// (#2593, ADR 2593). Every zero-check green path assumed that a head with no check runs,
// no workflow runs and no suites is a repository with no CI. During a GitHub Actions
// delay that is also exactly what a repository that DOES run CI looks like, so the trial
// (or its landing PR) was landed with no CI having run.
//
// The members' own PR heads are the positive signal: if any member's head carries check
// runs the repository runs check-run CI, so zero check runs on the trial means "CI has
// not started yet" and the caller must treat the head as pending (bounded by
// CIBackstopTimeout), never green. Any check run counts — cancelled, stale and
// non-Actions ones too (ADR 2296): they all show the repository runs CI.
//
// The read is live and uncached (e.client), one call per member, stopping at the first
// member with runs. If no member shows runs but a read failed, the evidence is unknown and
// the answer is hold (fail safe, FR-007): never land an unvalidated trial on a repository
// that may run CI. Every read succeeding with zero runs is the genuine no-CI repository
// and returns hold=false, so that case lands exactly as before (FR-004). With no members
// (callers that have none, such as the admission and fast-path classifiers) it never holds.
func (e *Engine) memberCIEvidence(owner, repo string, members []trainMember) (hold bool, why string) {
	if e.zeroCheckEvidenceOff.Load() || len(members) == 0 {
		return false, ""
	}
	var readErr error
	var failedPR int
	for _, m := range members {
		if m.headSHA == "" {
			continue
		}
		// failfast-audited: only the presence of check runs is read, never their conclusions
		// live-read-audited: positive CI evidence must be read at decision time from the
		// member's own PR head, never from a cached or formation-time value (FR-006)
		runs, err := e.client.FetchCheckRuns(owner, repo, m.headSHA)
		if err != nil {
			if readErr == nil {
				readErr, failedPR = err, m.prNum
			}
			continue
		}
		if len(runs) > 0 {
			return true, fmt.Sprintf("member PR #%d has check runs", m.prNum)
		}
	}
	if readErr != nil {
		return true, fmt.Sprintf("could not read check runs of member PR #%d (%v) — cannot rule out that the repository runs CI", failedPR, readErr)
	}
	return false, ""
}

// zeroCheckHoldLine is the one operator-facing line for a held zero-check head (R4,
// FR-005): the head, why, and that the engine is waiting.
func zeroCheckHoldLine(kind, sha, why string) string {
	return fmt.Sprintf("%s %s has zero check runs but %s — CI not started yet, waiting\n", kind, sha, why)
}

// SetZeroCheckMemberEvidenceDisabledForTest turns the #2593 member-evidence rule off for
// the trial and landing paths (the pre-fix reading: zero check runs can be green), so a
// test can show it depends on the rule. Per-Engine. Test-only.
func (e *Engine) SetZeroCheckMemberEvidenceDisabledForTest(v bool) { e.zeroCheckEvidenceOff.Store(v) }

// SetStageGateHadChecksShortcutDisabledForTest turns the #2593 R3 stage-gate rule off (the
// clean shortcut ignoring HasHadChecks). Per-Engine. Test-only.
func (e *Engine) SetStageGateHadChecksShortcutDisabledForTest(v bool) {
	e.stageGateHadChecksOff.Store(v)
}
