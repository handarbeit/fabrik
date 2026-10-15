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

// pausedTrainMembers reads each member's labels live (e.client.FetchLabels —
// never the boardcache-backed readClient) and returns the numbers of the members
// that carry fabrik:paused. When a live read fails it falls back to the labels the
// member was snapshotted with at batch formation: a transient read error then
// leaves the check no weaker than before this guard existed, instead of stalling
// every landing on it (the landing is retried on the next poll either way).
func (e *Engine) pausedTrainMembers(owner, repo string, members []trainMember) (paused []int) {
	for _, m := range members {
		labels, err := e.client.FetchLabels(owner, repo, m.item.Number)
		if err != nil {
			e.logfRepo(owner+"/"+repo, "pause-guard", "could not re-read labels of #%d (%v) — checking its batch-formation snapshot instead\n", m.item.Number, err)
			labels = m.item.Labels
		}
		if hasLabel(labels, "fabrik:paused") {
			paused = append(paused, m.item.Number)
		}
	}
	return paused
}

// refuseIfMembersPaused is the merge-train R3 checkpoint, called strictly before
// the irreversible merge of a trial: a batch lands as one integration PR holding
// every survivor's changes, so a member paused after its batch formed cannot be
// left out of an assembled PR. The landing is refused, every member stays in
// Queued, and nothing is ejected, counted, paused or recorded. The paused
// member is already excluded from new batches by groupQueuedItemsByRepoUnordered,
// so the next poll re-forms a batch without it.
func (e *Engine) refuseIfMembersPaused(owner, repo string, members []trainMember) bool {
	if e.pauseGuardOff.Load() {
		return false
	}
	paused := e.pausedTrainMembers(owner, repo, members)
	if len(paused) == 0 {
		return false
	}
	nums := make([]string, len(paused))
	for i, n := range paused {
		nums[i] = fmt.Sprintf("#%d", n)
	}
	repoKey := owner + "/" + repo
	e.logfRepo(repoKey, "pause-guard", "REFUSING to land for %s: fabrik:paused is set on %s — leaving members in Queued\n",
		repoKey, strings.Join(nums, ", "))
	return true
}
