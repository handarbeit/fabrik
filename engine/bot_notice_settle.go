package engine

import (
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// settleBotServiceNotices is the per-poll settle scan that watermarks
// non-actionable bot service-notice comments (#1083/#1088) excluded by
// findNewComments' isBotServiceNotice check. It cannot live inside
// findNewComments (which must stay a pure, client-free function reused by
// several test call sites) or inside processComments (a bot-notice-only
// backlog never reaches processComments once findNewComments excludes it —
// itemNeedsWork sees zero new comments and never dispatches). Runs
// unconditionally every poll over the raw board snapshot, mirroring
// settleMergeTrainMemberCloses: a missed reaction/watermark this poll is
// harmless and simply retries next poll, since the comment remains both
// unprocessed and unreacted — no retry/escalate machinery is warranted for
// a cosmetic, idempotent side effect.
func (e *Engine) settleBotServiceNotices(board *gh.ProjectBoard) {
	for _, item := range board.Items {
		e.settleBotServiceNoticesForItem(item)
	}
}

// settleBotServiceNoticesForItem watermarks (🚀 reaction + CommentProcessed)
// every not-yet-processed bot service-notice comment on item. Comments that
// don't classify as a bot service-notice, are already rocket-reacted, or are
// already recorded as CommentProcessed are left untouched.
func (e *Engine) settleBotServiceNoticesForItem(item gh.ProjectItem) {
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	snap, _ := e.store.Get(repoStr, item.Number)
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	for _, c := range item.Comments {
		if !isBotServiceNotice(c) {
			continue
		}
		if !snap.CommentProcessed(c.ID).IsZero() {
			continue
		}
		if c.HasReaction("ROCKET") {
			// Already reacted (e.g. by a previous settle pass whose store write
			// didn't persist) — just record the watermark, no duplicate reaction.
			e.store.Apply(itemstate.CommentProcessed{Repo: repoStr, Number: item.Number, CommentID: c.ID, At: time.Now()})
			continue
		}
		if c.DatabaseID == 0 {
			e.logf(item.Number, "debug", "skipping bot-notice watermark for synthetic comment %s (no DatabaseID)\n", c.ID)
			continue
		}

		var reactErr error
		if c.ReviewThreadID != "" {
			reactErr = e.client.AddPRReviewCommentReaction(owner, repo, c.DatabaseID, "rocket")
		} else {
			reactErr = e.client.AddCommentReaction(owner, repo, c.DatabaseID, "rocket")
		}
		if reactErr != nil {
			e.logf(item.Number, "warn", "could not add 🚀 to bot service-notice comment %s: %v\n", c.ID, reactErr)
			continue
		}
		e.store.Apply(itemstate.CommentProcessed{Repo: repoStr, Number: item.Number, CommentID: c.ID, At: time.Now()})
		e.logf(item.Number, "comments", "watermarked bot service-notice comment %s (author %s) — no worker spawned, no reply posted\n", c.ID, c.Author)
	}
}
