package engine

import (
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// checkReviewGate inspects item.LinkedPRReviewRequests and determines whether
// auto-advance is gated by outstanding PR reviewer requests.
//
// This function is only called from the catch-up path (Path 2) in poll.go,
// where item.LinkedPRReviewRequests contains fresh data from FetchItemDetails.
// Path 1 (handleStageComplete) always applies fabrik:awaiting-review directly
// because reviewer assignment happens after MarkPRReady, so data would be stale.
//
// Returns true if the issue should not advance yet (gate is blocking),
// false if it should proceed.
//
// Side effects when blocking:
//   - Logs a message listing the pending reviewers.
//   - Adds fabrik:awaiting-review label on first block transition (idempotent).
//
// Side effects when unblocking:
//   - Removes fabrik:awaiting-review label if present (idempotent).
func (e *Engine) checkReviewGate(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) bool {
	// Gate is opt-in — only active when wait_for_reviews: true.
	if stage.WaitForReviews == nil || !*stage.WaitForReviews {
		return false
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Build set of reviewers who have already submitted any review (APPROVED,
	// CHANGES_REQUESTED, or COMMENTED). A dismissed review means the reviewer
	// is back in reviewRequests, so we rely on the GraphQL data directly:
	// if they appear in reviewRequests, they're outstanding; if not, they're done.
	// latestReviews is informational for logging.
	outstanding := make([]string, 0, len(item.LinkedPRReviewRequests))
	for _, rr := range item.LinkedPRReviewRequests {
		if rr.Login != "" {
			outstanding = append(outstanding, rr.Login)
		}
	}

	if len(outstanding) == 0 {
		// No outstanding reviewers — remove label if present and allow advance.
		e.removeAwaitingReviewLabel(owner, repo, item)
		return false
	}

	// Check timeout. If fabrik:awaiting-review was applied more than
	// ReviewWaitTimeout ago, log a warning and advance anyway.
	timeout := e.cfg.ReviewWaitTimeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	for _, l := range item.Labels {
		if l == "fabrik:awaiting-review" {
			appliedAt, err := e.client.FetchLabelAppliedAt(owner, repo, item.Number, "fabrik:awaiting-review")
			if err != nil {
				e.logf(item.Number, "warn", "could not fetch awaiting-review label timestamp: %v\n", err)
			} else if !appliedAt.IsZero() && time.Since(appliedAt) >= timeout {
				e.logf(item.Number, "warn", "review wait timeout elapsed; advancing despite pending reviewers: %s\n",
					strings.Join(outstanding, ", "))
				e.removeAwaitingReviewLabel(owner, repo, item)
				return false
			}
			break
		}
	}

	e.logf(item.Number, "awaiting-review", "waiting for reviewers: %s\n", strings.Join(outstanding, ", "))

	// Apply label on first block transition.
	alreadyWaiting := false
	for _, l := range item.Labels {
		if l == "fabrik:awaiting-review" {
			alreadyWaiting = true
			break
		}
	}
	if !alreadyWaiting {
		if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-review"); err != nil {
			e.logf(item.Number, "warn", "could not add fabrik:awaiting-review label: %v\n", err)
		}
	}

	return true
}

// removeAwaitingReviewLabel removes fabrik:awaiting-review if present on the item.
func (e *Engine) removeAwaitingReviewLabel(owner, repo string, item gh.ProjectItem) {
	for _, l := range item.Labels {
		if l == "fabrik:awaiting-review" {
			if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:awaiting-review"); err != nil {
				e.logf(item.Number, "warn", "could not remove fabrik:awaiting-review label: %v\n", err)
			}
			return
		}
	}
}
