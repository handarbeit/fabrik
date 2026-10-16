package engine

import (
	"errors"
	"fmt"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// Pause guard (#2341, ADR 2341). A fabrik:paused label is the operator's stop
// button: it must win over every automatic pre-merge advance or landing
// decision, using the labels the deciding site has already read live. The guard
// sits at the decision sites, never inside advanceToNextStage — that primitive
// also moves an already-merged item to Done, which a pause must not strand.
//
// A refusal is a clean no-op: one log line, no label write, no comment, no
// cooldown and no retry increment. A human comment lifts the pause (ADR 1813),
// the label removal re-admits the item, and the decision is taken again on a
// later poll.

// errLandingPaused is returned by advanceToQueued, which only returns an error,
// when it refused because the item is paused. landValidated maps it to the
// deferred shape; it is never surfaced to an operator.
var errLandingPaused = errors.New("landing refused: fabrik:paused")

// SetPauseGuardDisabledForTest neutralises the pause guard so a test can show
// its refusal assertions bite (the non-vacuity seam).
func (e *Engine) SetPauseGuardDisabledForTest(disabled bool) {
	e.pauseGuardOff.Store(disabled)
}

// SetPauseGuardUnreadFailOpenForTest makes the stage-path and merge_gate checks treat
// an unreadable label set as "not paused" again (the pre-fail-closed behaviour). It
// exists only so the many unrelated tests whose mock leaves FetchLabels unconfigured
// (which errors by design, see mocks_test.go) keep exercising what they were written
// for; the fail-closed tests build their engine without it. The merge-train check has
// no such seam.
func (e *Engine) SetPauseGuardUnreadFailOpenForTest(failOpen bool) {
	e.pauseGuardUnreadOK.Store(failOpen)
}

// refuseIfPaused reports whether the item's labels carry fabrik:paused, logging
// the refusal once. site names the decision being refused ("landing",
// "advancing"). It touches no label, comment, cooldown or retry counter.
func (e *Engine) refuseIfPaused(item gh.ProjectItem, site string) bool {
	return e.refuseIfPausedLabels(item.Labels, item.Number, site)
}

// isPaused is the silent form of the check: true when the guard is enabled and
// the labels carry fabrik:paused. Callers log the refusal at the point they act on it.
func (e *Engine) isPaused(labels []string) bool {
	return !e.pauseGuardOff.Load() && hasLabel(labels, "fabrik:paused")
}

// refuseIfPausedLabels is refuseIfPaused over an already-extracted label set.
func (e *Engine) refuseIfPausedLabels(labels []string, number int, site string) bool {
	if !e.isPaused(labels) {
		return false
	}
	e.logf(number, "pause-guard", "not %s: fabrik:paused is set — leaving the item as it is until a human comment lifts the pause\n", site)
	return true
}

// pausedOrUnread is the stage-path form of the check: true when the guard is
// enabled and either the labels carry fabrik:paused or they are not live. A
// pause is the operator's stop button, so labels that could not be re-read are
// treated as "pause cannot be ruled out" — never as "not paused" (fail closed).
func (e *Engine) pausedOrUnread(labels []string, live bool) bool {
	if e.pauseGuardOff.Load() {
		return false
	}
	return (!live && !e.pauseGuardUnreadOK.Load()) || hasLabel(labels, "fabrik:paused")
}

// refuseIfPausedOrUnread reports whether the decision must be refused because the
// item is paused or its labels could not be re-read, logging the refusal once. It
// touches no label, comment, cooldown or retry counter, so the decision is simply
// taken again on a later poll.
func (e *Engine) refuseIfPausedOrUnread(item gh.ProjectItem, live bool, site string) bool {
	if !e.pausedOrUnread(item.Labels, live) {
		return false
	}
	if !live && !hasLabel(item.Labels, "fabrik:paused") {
		e.logf(item.Number, "pause-guard", "not %s: could not re-read the labels live, so fabrik:paused cannot be ruled out — leaving the item as it is until a later poll\n", site)
		return true
	}
	return e.refuseIfPaused(item, site)
}

// refuseIfPausedLive is the check for sites that hold no live labels of their own
// (the reinvoke and convergence merge/enqueue paths): it reads them with
// e.client.FetchLabels — never the boardcache-backed readClient — and refuses on a
// pause or on a failed read.
func (e *Engine) refuseIfPausedLive(item gh.ProjectItem, site string) bool {
	if e.pauseGuardOff.Load() {
		return false
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	labels, err := e.client.FetchLabels(owner, repo, item.Number)
	if err != nil && e.pauseGuardUnreadOK.Load() {
		return false
	}
	if err != nil {
		e.logf(item.Number, "pause-guard", "not %s: could not re-read the labels of %s/%s#%d (%v), so fabrik:paused cannot be ruled out — leaving the item as it is until a later poll\n", site, owner, repo, item.Number, err)
		return true
	}
	return e.refuseIfPausedLabels(labels, item.Number, site)
}

// pausedTrainMembers reads each member's labels live (e.client.FetchLabels —
// never the boardcache-backed readClient) and returns the numbers of the members
// that carry fabrik:paused, and of those whose labels could not be read. A read
// failure is NOT a fallback to the batch-formation snapshot: that snapshot can never
// carry fabrik:paused (groupQueuedItemsByRepoUnordered excludes paused members at
// formation), so it would always read as "not paused". The caller treats an
// unreadable member as a refusal for this poll.
func (e *Engine) pausedTrainMembers(owner, repo string, members []trainMember) (paused, unread []int) {
	for _, m := range members {
		labels, err := e.client.FetchLabels(owner, repo, m.item.Number)
		if err != nil {
			e.logfRepo(owner+"/"+repo, "pause-guard", "could not re-read labels of #%d (%v) — refusing to land this poll, fabrik:paused cannot be ruled out\n", m.item.Number, err)
			unread = append(unread, m.item.Number)
			continue
		}
		if hasLabel(labels, "fabrik:paused") {
			paused = append(paused, m.item.Number)
		}
	}
	return paused, unread
}

// refuseIfMembersPaused is the merge-train R3 checkpoint, called strictly before
// the irreversible merge of a trial: a batch lands as one integration PR holding
// every survivor's changes, so a member paused after its batch formed cannot be
// left out of an assembled PR. The landing is refused when any member is paused or
// has labels that could not be read (fail closed); every member stays in Queued,
// and nothing is ejected, counted, paused or recorded. The paused member is already
// excluded from new batches by groupQueuedItemsByRepoUnordered, so the next poll
// re-forms a batch without it; an unreadable member is simply retried.
func (e *Engine) refuseIfMembersPaused(owner, repo string, members []trainMember) bool {
	if e.pauseGuardOff.Load() {
		return false
	}
	paused, unread := e.pausedTrainMembers(owner, repo, members)
	if len(paused) == 0 && len(unread) == 0 {
		return false
	}
	repoKey := owner + "/" + repo
	if len(paused) > 0 {
		e.logfRepo(repoKey, "pause-guard", "REFUSING to land for %s: fabrik:paused is set on %s — leaving members in Queued\n",
			repoKey, formatMemberRefs(paused))
	}
	if len(unread) > 0 {
		e.logfRepo(repoKey, "pause-guard", "REFUSING to land for %s: labels of %s could not be read — leaving members in Queued, retrying next poll\n",
			repoKey, formatMemberRefs(unread))
	}
	return true
}

func formatMemberRefs(nums []int) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(parts, ", ")
}
