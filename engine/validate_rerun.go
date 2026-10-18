package engine

import (
	gh "github.com/handarbeit/fabrik/github"
)

// validateRerunLabel is the durable marker of a forced Validate re-run (#2378,
// ADR 2378): the engine stripped stage:Validate:complete so that Validate runs again
// (R3 of ADR 2343, the head-moved scan, fabrik:revalidate and the merge train's
// auto-repair). While it is present only a Validate *stage* run may restore the
// completion label; a Validate comment-review pass handles its comments but never
// completes the stage. It is a label because the signal has to survive a daemon
// restart (FR-004) and nothing else the engine keeps does.
const validateRerunLabel = "fabrik:validate-rerun"

// SetValidateRerunGuardDisabledForTest restores the pre-#2378 behaviour (no marker
// is set, a Validate comment pass may complete the stage, routing is unchanged) so a
// test can show its assertions bite (the non-vacuity seam).
func (e *Engine) SetValidateRerunGuardDisabledForTest(disabled bool) {
	e.validateRerunGuardOff.Store(disabled)
}

// markValidateRerun records a forced Validate re-run. It must run before the
// completion label is removed: a crash between the two writes then leaves "marker
// and label", which is safe, instead of "label gone, no marker". It reports false
// when the marker could not be written, and the caller then defers without
// stripping anything.
func (e *Engine) markValidateRerun(item gh.ProjectItem) bool {
	if e.validateRerunGuardOff.Load() {
		return true
	}
	if err := e.addLabelChecked(item, validateRerunLabel); err != nil {
		e.logf(item.Number, "warn", "validate-rerun: could not add %s: %v\n", validateRerunLabel, err)
		return false
	}
	return true
}

// validateRerunPending reports whether a forced Validate re-run is outstanding for
// item. The labels are read live (e.client, never the board cache) and it fails
// closed: an unreadable label set counts as pending, because the opposite error
// lets a comment pass restore the label and an unvalidated head land. The cost of
// a wrong "pending" is a completion that the next Validate stage run supplies.
func (e *Engine) validateRerunPending(item gh.ProjectItem) bool {
	if e.validateRerunGuardOff.Load() {
		return false
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	labels, err := e.client.FetchLabels(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "validate-rerun", "could not re-read the labels (%v), so a forced Validate re-run cannot be ruled out — treating it as outstanding\n", err)
		return true
	}
	return hasLabel(labels, validateRerunLabel)
}

// clearValidateRerun drops the marker once a Validate stage run has produced a
// durable completion signal (the label, or fabrik:awaiting-ci on a wait_for_ci
// stage). live reports that item.Labels were re-read this call, so an absent
// marker needs no write; with a stale snapshot the removal is attempted anyway
// (an already-absent label is a no-op).
func (e *Engine) clearValidateRerun(item gh.ProjectItem, live bool) {
	if e.validateRerunGuardOff.Load() {
		return
	}
	if live && !hasLabel(item.Labels, validateRerunLabel) {
		return
	}
	if err := e.removeLabelChecked(item, validateRerunLabel); err != nil {
		e.logf(item.Number, "warn", "validate-rerun: could not remove %s: %v\n", validateRerunLabel, err)
		return
	}
	e.logf(item.Number, "validate-rerun", "Validate stage run completed — forced re-run marker cleared\n")
}
