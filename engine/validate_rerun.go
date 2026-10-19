package engine

import (
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
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

// SetValidateReworkRestoreGuardDisabledForTest neutralises only the rework-bracket
// guard (a non-completing exit of a comment pass restoring the completion label while
// a forced re-run is outstanding), leaving every other #2378 guard on, so a test can
// show that guard on its own bites.
func (e *Engine) SetValidateReworkRestoreGuardDisabledForTest(disabled bool) {
	e.validateReworkRestoreGuardOff.Store(disabled)
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
	pending, err := e.readValidateRerun(item)
	if err != nil {
		e.logf(item.Number, "validate-rerun", "could not re-read the labels (%v), so a forced Validate re-run cannot be ruled out — treating it as outstanding\n", err)
		return true
	}
	return pending
}

// readValidateRerun is the live read behind validateRerunPending: whether the marker
// is on the item, or the read error (the caller decides how to fail).
func (e *Engine) readValidateRerun(item gh.ProjectItem) (bool, error) {
	pending, _, err := e.readValidateRerunState(item)
	return pending, err
}

// readValidateRerunState is readValidateRerun that also reports whether the live
// label set carries stage:Validate:complete.
func (e *Engine) readValidateRerunState(item gh.ProjectItem) (pending, complete bool, err error) {
	if e.validateRerunGuardOff.Load() {
		return false, false, nil
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	labels, err := e.liveLabels(owner, repo, item.Number)
	if err != nil {
		return false, false, err
	}
	return hasLabel(labels, validateRerunLabel), hasLabel(labels, "stage:Validate:complete"), nil
}

// healStaleValidateRerun reports whether the marker is left over from a Validate stage
// run whose marker clear failed (handleStageComplete clears it best-effort, once): the
// completion label is on the live labels and the Store holds a Validate completion SHA.
// The strip clears that SHA (ValidateCompletedAtSHACleared) and only a Validate stage
// run records one again (a comment pass never reaches handleStageComplete while the
// marker is set), so the pair means a stage run completed after the strip. It then
// retries the clear. The landing path still re-checks the head against that SHA.
func (e *Engine) healStaleValidateRerun(item gh.ProjectItem) bool {
	snap, err := e.store.Get(itemOwnerRepoString(item, e.defaultRepo()), item.Number)
	if err != nil || snap.ValidateCompletedSHA() == "" {
		return false
	}
	if err := e.removeLabelChecked(item, validateRerunLabel); err != nil {
		e.logf(item.Number, "warn", "validate-rerun: could not remove stale %s: %v\n", validateRerunLabel, err)
		return false
	}
	e.logf(item.Number, "validate-rerun", "Validate stage run already completed (completion label and SHA present) — cleared the leftover forced re-run marker\n")
	return true
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

// refuseIfValidateRerun is the landing-side form of validateRerunPending: it refuses
// (true) while a forced Validate re-run is outstanding or the labels cannot be
// re-read, logging once and recording the merge-unsettled admission cooldown so the
// decision is taken again on a later poll. It writes no label or comment, pauses
// nothing and touches no retry, cycle or ejection counter.
func (e *Engine) refuseIfValidateRerun(item gh.ProjectItem, site string) bool {
	pending, complete, err := e.readValidateRerunState(item)
	if err != nil {
		// Fail closed, with the pause guard's test seam for mocks whose FetchLabels is
		// unconfigured (it errors by design).
		if e.pauseGuardUnreadOK.Load() {
			return false
		}
		e.logf(item.Number, "validate-rerun", "could not re-read the labels (%v), so a forced Validate re-run cannot be ruled out\n", err)
	} else if !pending {
		return false
	} else if complete && e.healStaleValidateRerun(item) {
		return false
	}
	e.logf(item.Number, "validate-rerun", "not %s: a forced Validate re-run is outstanding (%s) — a Validate stage run must verify the current head first\n", site, validateRerunLabel)
	e.store.Apply(itemstate.CooldownRecorded{
		Repo:   itemOwnerRepoString(item, e.defaultRepo()),
		Number: item.Number,
		Reason: mergeUnsettledCooldownReason,
		Until:  e.now().Add(e.mergeGateRecheckInterval()),
	})
	return true
}
