package engine

import (
	"context"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// postRunFeedbackAdmission admits an item on the next poll when a worker has
// just ended and feedback is already waiting in the cache (FR-001b, #2232).
//
// Feedback that arrived while the worker ran was read into the cache during the
// run, so nothing signals its arrival once the run ends; a bare WorkerExited
// wakes the poll but is excluded from cycleSetFlags (ADR 039), and the poll's
// periodic-re-eval then hides the item for 10 × PollSeconds. Setting
// e.mayNeedWork (as requeueDeferredDispatch does) makes the very next poll admit
// it through cycleSet, bypassing cooldowns; the entry is drained once.
//
// It is a cache-only check — the same definitions as unprocessedFeedback, pinned
// by TestCachedFeedbackMatchesLiveGate — so it costs no GitHub call, and it runs
// on the worker goroutine rather than in a WorkerExited observer so nothing slow
// sits on the store's apply path. Only known pending feedback admits the item:
// admitting every exit would undo ADR 039. Admission is not a dispatch: the
// pass that follows is still bounded by the comment circuit breakers and the
// review cycle limit, and a paused item is never admitted.
//
// Only feedback that arrived during the run (created at or after startedAt)
// admits. Feedback that was already pending when the run started is what the
// run was for: if it is still pending afterwards the pass failed, and
// admitting it again would bypass every cooldown for a pass that ended without
// clearing it and without pausing (an error, a cancel, a no-change reply) — the
// #1752/#1813 loop shape. That case takes the ordinary stageRetryBackoff path.
// startedAt is the worker's start on e.now(), the clock the cached GitHub
// timestamps are compared against.
func (e *Engine) postRunFeedbackAdmission(ctx context.Context, item gh.ProjectItem, startedAt time.Time) {
	if postRunFeedbackAdmissionDisabled.Load() || ctx.Err() != nil {
		return
	}
	repo := itemOwnerRepoString(item, e.defaultRepo())
	snap, ok := e.store.Peek(repo, item.Number)
	if !ok {
		return
	}
	st := snap.State()
	if st.IsPR || st.IsClosed {
		return
	}
	// A paused item (a breaker trip or escalation at the end of this very run)
	// only resumes on a human comment created after the pause, so the feedback
	// already cached cannot lift it: admitting it would cost a deep fetch for a
	// refusal, and a failing pass must not re-admit itself through here.
	if hasLabel(st.Labels, "fabrik:paused") {
		return
	}
	if s := stages.FindStage(e.cfg.Stages, st.Status); s != nil && s.CleanupWorktree {
		return
	}
	f := e.cachedFeedbackArrivedSince(&st, startedAt)
	if !f.any() {
		return
	}
	e.mayNeedWorkMu.Lock()
	e.mayNeedWork[issueKey(item, e.defaultRepo())] = true
	e.mayNeedWorkMu.Unlock()
	e.logf(item.Number, "post-run", "feedback arrived during the run (%d comment(s), %d thread(s), %d review body(ies)) — admitting on next poll\n", f.Comments, f.Threads, f.Bodies)
}

// cachedFeedbackArrivedSince is cachedPendingFeedbackOf restricted to feedback
// created at or after since: the same pending definitions (filterNewComments,
// filterReviewThreadComments + dropOutdatedThreadComments, reviewBodyCandidates
// and the durable review-ids-addressed marker), counting only items whose
// GitHub timestamp is known and not before since. An unknown (zero) timestamp
// is never counted — the conservative direction, since it leaves the item on
// the ordinary retry path.
func (e *Engine) cachedFeedbackArrivedSince(st *itemstate.ItemState, since time.Time) cachedFeedback {
	arrived := func(t time.Time) bool {
		return postRunArrivalRestrictionDisabled.Load() || (!t.IsZero() && !t.Before(since))
	}
	processed := func(id string) bool { return !st.StageState.ProcessedComments[id].IsZero() }
	var f cachedFeedback
	for _, c := range filterNewComments(st.Comments, processed) {
		if arrived(c.CreatedAt) {
			f.Comments++
		}
	}
	if lpr := st.LinkedPR; lpr != nil {
		for _, c := range dropOutdatedThreadComments(filterReviewThreadComments(lpr.ThreadComments, processed)) {
			if arrived(c.CreatedAt) {
				f.Threads++
			}
		}
		if cands := reviewBodyCandidates(lpr.Reviews, processed, e.cleanReviewSkipper(st.Status, st.Number, st.Labels)); len(cands) > 0 {
			durable := e.cachedAddressedReviewIDs(st)
			for _, c := range cands {
				if !durable[c.review.DatabaseID] && arrived(c.review.SubmittedAt) {
					f.Bodies++
				}
			}
		}
	}
	return f
}
