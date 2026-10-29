package engine

import (
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// pauseCause is why the engine paused an item (#2487, ADR 2487). It is a closed
// vocabulary: every pause the engine writes carries exactly one of these as a
// `fabrik:pause:<cause>` label, written right after fabrik:paused and removed
// with it, so an operator can tell a question from a timeout from the board.
//
// The cause label is presentation and classification only. Nothing in
// resumeAuthorised (ADR-1813), itemNeedsWork, processItem, the pause guards or
// admission may read it: a paused item resumes exactly as before whatever its
// cause, and fabrik:awaiting-input stays on every pause the engine applies
// (ADR 2487 decision 1).
type pauseCause string

const (
	// pauseCauseNone is the unset zero value. pauseIssue treats it as a bug
	// (logged, falls back to pauseCauseOther); the static scan keeps it out of
	// the tree.
	pauseCauseNone pauseCause = ""
	// pauseCauseQuestion: a question was put to a human (FABRIK_BLOCKED_ON_INPUT,
	// an ungrantable-denial question, a spawn servability question).
	pauseCauseQuestion pauseCause = "question"
	// pauseCauseReviewTimeout: the review gate timed out.
	pauseCauseReviewTimeout pauseCause = "review-timeout"
	// pauseCauseCITimeout: the CI gate, its backstop or the merge-state wait timed out.
	pauseCauseCITimeout pauseCause = "ci-timeout"
	// pauseCauseCycleLimit: a bounded loop (review, CI-fix, rebase, enqueue,
	// slice, tools-denied, stage dispatch) reached its limit.
	pauseCauseCycleLimit pauseCause = "cycle-limit"
	// pauseCauseDaemonStop: a clean daemon stop interrupted the item.
	pauseCauseDaemonStop pauseCause = "daemon-stop"
	// pauseCauseBreaker: a comment circuit breaker or the merge-train runaway guard fired.
	pauseCauseBreaker pauseCause = "breaker"
	// pauseCauseFailed: a stage failed or a settle scan's retries were exhausted.
	pauseCauseFailed pauseCause = "failed"
	// pauseCauseOther: anything else (a manual stop, a refusal, a spawn failure, ...).
	pauseCauseOther pauseCause = "other"
)

// pauseCauseLabelPrefix is the one shared prefix of the cause label family.
const pauseCauseLabelPrefix = "fabrik:pause:"

// allPauseCauses is the closed vocabulary, in documentation order.
var allPauseCauses = []pauseCause{
	pauseCauseQuestion, pauseCauseReviewTimeout, pauseCauseCITimeout, pauseCauseCycleLimit,
	pauseCauseDaemonStop, pauseCauseBreaker, pauseCauseFailed, pauseCauseOther,
}

// label is the `fabrik:pause:<cause>` label name.
func (c pauseCause) label() string { return pauseCauseLabelPrefix + string(c) }

// valid reports whether c is a member of the closed vocabulary.
func (c pauseCause) valid() bool {
	for _, v := range allPauseCauses {
		if c == v {
			return true
		}
	}
	return false
}

// isPauseCauseLabel reports whether label belongs to the cause family.
func isPauseCauseLabel(label string) bool {
	return strings.HasPrefix(label, pauseCauseLabelPrefix)
}

// pauseCauseFromLabels returns the cause named by the first cause label in
// labels, or pauseCauseNone.
func pauseCauseFromLabels(labels []string) pauseCause {
	for _, l := range labels {
		if isPauseCauseLabel(l) {
			return pauseCause(strings.TrimPrefix(l, pauseCauseLabelPrefix))
		}
	}
	return pauseCauseNone
}

// currentPauseCauseLabels returns every cause label the item carries, from the
// caller's snapshot and the Store (the snapshot can predate a write-through).
func (e *Engine) currentPauseCauseLabels(item gh.ProjectItem) []string {
	var out []string
	seen := map[string]bool{}
	add := func(labels []string) {
		for _, l := range labels {
			if isPauseCauseLabel(l) && !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	add(item.Labels)
	if snap, err := e.store.Get(itemOwnerRepoString(item, e.defaultRepo()), item.Number); err == nil {
		add(snap.Labels())
	}
	return out
}

// applyPauseCause writes cause's label, removing any other cause label first so
// at most one is ever present. A failed write is logged and non-fatal: the pause
// stands (an item without a cause label is the same state as one paused before
// the upgrade).
func (e *Engine) applyPauseCause(item gh.ProjectItem, cause pauseCause, echo bool) {
	if !cause.valid() {
		e.logf(item.Number, "warn", "pause with no valid cause %q; recording %q\n", string(cause), string(pauseCauseOther))
		cause = pauseCauseOther
	}
	want := cause.label()
	for _, l := range e.currentPauseCauseLabels(item) {
		if l != want {
			e.applyLabelRemove(item, l, echo)
		}
	}
	e.applyLabelAdd(item, want, echo)
}

// clearPauseCause removes every cause label the item carries. It is called on
// every path that lifts a pause, so a cause label never outlives fabrik:paused.
func (e *Engine) clearPauseCause(item gh.ProjectItem) {
	for _, l := range e.currentPauseCauseLabels(item) {
		e.applyLabelRemove(item, l, true)
	}
}

// applyPauseLabels is the one write of the pause label set for the sites that do
// not go through pauseIssue (they post no comment or post it themselves):
// fabrik:paused, then fabrik:awaiting-input when asked, then the cause label.
// Labels are applied with the engine echo, like addLabel.
func (e *Engine) applyPauseLabels(item gh.ProjectItem, cause pauseCause, awaitingInput bool) {
	e.applyLabelAdd(item, "fabrik:paused", true)
	if awaitingInput {
		e.applyLabelAdd(item, "fabrik:awaiting-input", true)
	}
	e.applyPauseCause(item, cause, true)
}

// pauseCauseLabels is every label of the family, in vocabulary order.
func pauseCauseLabels() []string {
	out := make([]string, 0, len(allPauseCauses))
	for _, c := range allPauseCauses {
		out = append(out, c.label())
	}
	return out
}
