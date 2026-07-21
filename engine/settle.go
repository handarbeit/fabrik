package engine

import (
	"errors"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// recordSettleRetry increments the in-memory retry counter for a stalled
// settle-scan operation, keyed by the caller's dedicated retryStage constant
// (a synthetic, double-underscore-wrapped name — never a real configured
// stage — so it can't collide with a stage's own retry count). Escalates via
// onEscalate once e.cfg.MaxRetries is reached. Shared by the three settle
// scans (no-work-needed, child-placement, merge-train member-close); each
// passes its own retryStage constant and escalate function so the counters
// never conflate across scans.
func (e *Engine) recordSettleRetry(item gh.ProjectItem, retryStage string, onEscalate func(gh.ProjectItem)) {
	if e.cfg.MaxRetries <= 0 {
		return
	}
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.StageRetryIncremented{Repo: repoStr, Number: item.Number, StageName: retryStage})
	var count int
	if snap, err := e.store.Get(repoStr, item.Number); err == nil {
		count = snap.Attempts(retryStage)
	}
	if count >= e.cfg.MaxRetries {
		onEscalate(item)
	}
}

// clearSettleMarker removes a settle scan's durable marker label and clears
// its retry counter once the underlying operation has fully succeeded.
// Shared by the three settle scans; each passes its own marker label and
// retryStage constant.
func (e *Engine) clearSettleMarker(item gh.ProjectItem, owner, repo, markerLabel, retryStage string) {
	if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, markerLabel); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "could not remove %s marker: %v\n", markerLabel, err)
		return
	} else if err == nil {
		e.syncLabelRemoval(item, markerLabel, true)
	}

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: retryStage})
}

// escalateSettle is called when a settle scan's outstanding operation has
// failed MaxRetries times. It pauses the issue (fabrik:paused) so it stops
// being retried silently forever, removes the scan's durable marker label
// (dispatch/retry suppression is no longer needed once fabrik:paused takes
// over), invokes postComment to post an explanatory comment (a hook rather
// than a plain string so callers with divergent comment-posting behavior —
// e.g. escalateChildPlacementFailure's CreatedAt-omitting raw AddComment —
// can express that divergence at their own call site instead of leaking it
// into this shared helper), and records the pause in the Store. Shared by
// the three settle scans; each passes its own marker label, retryStage
// constant, and comment-posting closure.
func (e *Engine) escalateSettle(item gh.ProjectItem, markerLabel, retryStage string, postComment func(gh.ProjectItem)) {
	e.addLabel(item, "fabrik:paused")
	e.applyLabelRemove(item, markerLabel, true)
	postComment(item)

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.EnginePaused{Repo: repoStr, Number: item.Number, StageName: retryStage})
}
