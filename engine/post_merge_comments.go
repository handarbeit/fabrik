package engine

import (
	"fmt"
	"strconv"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// postMergeCommentMarkerPrefix opens the durable dedupe marker embedded in the
// "not applied" reply (#1862, ADR-1862). The guard deliberately leaves the
// triggering comment without 🚀, so the comment stays "new" to findNewComments;
// without a durable record of having answered it, every poll on a merged-but-
// still-open item would re-dispatch and re-reply. The marker rides in the reply
// itself and is scanned back from item.Comments (the hasPauseComment precedent),
// so it survives a restart — unlike the in-memory CommentProcessed watermark,
// which would also hide the comment until the restart and answer it twice after.
const postMergeCommentMarkerPrefix = "<!-- fabrik:post-merge-comment:"

func postMergeCommentMarker(commentID string) string {
	return postMergeCommentMarkerPrefix + commentID + " -->"
}

// hasPostMergeReply reports whether item already carries a reply answering the
// given comment ID.
func hasPostMergeReply(item gh.ProjectItem, commentID string) bool {
	marker := postMergeCommentMarker(commentID)
	for _, c := range item.Comments {
		if strings.Contains(c.Body, marker) {
			return true
		}
	}
	return false
}

// itemPRAlreadyLanded reports whether the item's work has already landed — the
// PR merged, or (merge-train member) landed via an integration PR while the
// member's own PR stays closed-not-merged — and returns the PR number to answer
// on (0 when unknown). It acts only on positive evidence: any read error, no PR,
// an open PR, or a human-closed PR with no landing marker all report false, so
// the guard can never refuse a legitimate comment on unknown state.
//
// The merge-train case is read from the durable landing labels
// (fabrik:credited-pr:<N> / fabrik:awaiting-landing-verification, ADR-1616);
// once verification clears them the issue is closed and ADR-1387 already keeps
// comments away from workers. The ordinary path reads e.client (never the
// boardcache-backed readClient) and, for a closed-not-merged PR, confirms with
// FetchPRMerged because the list endpoint's merged flag lags a merge by seconds
// — the same discipline as advanceValidateTerminalItem.
func (e *Engine) itemPRAlreadyLanded(item gh.ProjectItem) (bool, int) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	labelLanded := hasLabel(item.Labels, "fabrik:awaiting-landing-verification")
	creditedPR := 0
	for _, l := range item.Labels {
		if n, ok := strings.CutPrefix(l, "fabrik:credited-pr:"); ok {
			labelLanded = true
			if v, err := strconv.Atoi(n); err == nil {
				creditedPR = v
			}
		}
	}
	if labelLanded {
		if creditedPR != 0 {
			return true, creditedPR
		}
		return true, item.LinkedPRNumber
	}

	pr, err := e.client.FetchLinkedPR(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "post-merge guard: could not fetch linked PR: %v — processing normally\n", err)
		return false, 0
	}
	if pr == nil || pr.Number == 0 {
		return false, 0
	}
	if pr.Merged {
		return true, pr.Number
	}
	if pr.State == "closed" {
		merged, mErr := e.client.FetchPRMerged(owner, repo, pr.Number)
		if mErr != nil {
			e.logf(item.Number, "warn", "post-merge guard: could not confirm PR #%d merged state: %v — processing normally\n", pr.Number, mErr)
			return false, 0
		}
		if merged {
			return true, pr.Number
		}
	}
	return false, 0
}

// postMergeCommentGuard is the post-merge guard on comment processing (#1862,
// ADR-1862). Comment processing must never commit to or push a branch whose PR
// already merged: the change is orphaned there, and the 🚀 that follows makes
// the comment look handled when it was not.
//
// When the work already landed it returns true and the caller must skip the
// worker entirely. For each human-authored, not-yet-answered comment in the
// batch it posts one reply — on the issue and on the PR — saying the work
// already landed, the change was NOT applied, and that it needs a new issue.
// It never reacts 🚀 to the triggering comment (it must not look processed), and
// dedupes on a durable marker in the reply (see postMergeCommentMarkerPrefix).
// Bot-only and synthetic batches (reinvoke review-body comments carry no
// DatabaseID) are skipped silently: there is no commenter to tell.
//
// Runs before recordCommentBreakerInvocation, so a guarded cycle never feeds the
// comment breakers, and is unaffected by resetCommentBreaker.
func (e *Engine) postMergeCommentGuard(item gh.ProjectItem, comments []gh.Comment) bool {
	landed, prNumber := e.itemPRAlreadyLanded(item)
	if !landed {
		return false
	}

	var unanswered []gh.Comment
	for _, c := range filterHuman(comments) {
		if c.DatabaseID == 0 || hasPostMergeReply(item, c.ID) {
			continue
		}
		unanswered = append(unanswered, c)
	}
	if len(unanswered) == 0 {
		e.logf(item.Number, "post-merge", "work already landed — comment processing skipped, nothing new to answer\n")
		return true
	}

	e.logf(item.Number, "post-merge", "work already landed — not applying %d comment(s); replying instead\n", len(unanswered))
	var markers strings.Builder
	for _, c := range unanswered {
		markers.WriteString(postMergeCommentMarker(c.ID))
		markers.WriteString("\n")
	}
	prRef := "the PR"
	if prNumber > 0 {
		prRef = fmt.Sprintf("PR #%d", prNumber)
	}
	body := fmt.Sprintf("🏭 **Fabrik — comment not applied**\n\n"+
		"%s already landed, so your comment arrived after the work was merged. "+
		"The requested change was **not** applied — Fabrik does not push to a branch that has already merged. "+
		"If you still want it, please open a new issue describing the change.\n\n%s",
		prRef, markers.String())

	e.postItemComment(item, body, false)
	if prNumber > 0 {
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		// no write-through: excluded — posts to prNumber (PR comment thread, not issue cache)
		if _, err := e.client.AddComment(owner, repo, prNumber, body); err != nil {
			e.logf(item.Number, "warn", "post-merge guard: could not post reply to PR #%d: %v\n", prNumber, err)
		}
	}
	return true
}
