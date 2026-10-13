package engine

import (
	"context"

	gh "github.com/handarbeit/fabrik/github"
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
func (e *Engine) postRunFeedbackAdmission(ctx context.Context, item gh.ProjectItem) {
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
	f := e.cachedPendingFeedbackOf(&st)
	if !f.any() {
		return
	}
	e.mayNeedWorkMu.Lock()
	e.mayNeedWork[issueKey(item, e.defaultRepo())] = true
	e.mayNeedWorkMu.Unlock()
	e.logf(item.Number, "post-run", "feedback pending (%d comment(s), %d thread(s), %d review body(ies)) — admitting on next poll\n", f.Comments, f.Threads, f.Bodies)
}
