package pruefer

import (
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// reviewCommand is the PR comment command that forces a fresh review of the
// current head, bypassing only the already-reviewed-at-this-SHA check (not
// draft/self-authored/exclusion checks — see select.go's Eligible).
const reviewCommand = "/pruefer review"

// GitHubCommenter is the subset of *github.Client's comment/reaction methods
// this file needs. A narrow interface — rather than depending on the full
// client — keeps these functions testable without HTTP mocking. AddComment
// (posting a new issue/PR-conversation comment) backs notice.go's too-large
// notice — distinct from AddCommentReaction, which only reacts to an
// existing comment.
type GitHubCommenter interface {
	FetchIssueComments(owner, repo string, issueNumber int) ([]gh.Comment, error)
	AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error
	AddComment(owner, repo string, issueNumber int, body string) (int, error)
}

// isReviewCommand reports whether body contains the /pruefer review
// command, matched case-insensitively — consistent with how Fabrik
// recognizes its own comment commands.
func isReviewCommand(body string) bool {
	return strings.Contains(strings.ToLower(body), reviewCommand)
}

// pendingReviewComments filters an already-fetched comment list down to
// unprocessed "/pruefer review" commands (no ROCKET reaction yet) — the same
// 👀/🚀 reaction-based idempotency convention Fabrik uses for its own comment
// processing (see engine/comments.go). Pure: takes the comment list rather
// than fetching it, so ReviewPR can fetch comments once and reuse the same
// slice for both this check and notice.go's alreadyNoticedTooLarge, instead
// of paying a second FetchIssueComments round-trip per PR per poll.
func pendingReviewComments(comments []gh.Comment) []gh.Comment {
	var out []gh.Comment
	for _, c := range comments {
		if isReviewCommand(c.Body) && !c.HasReaction("ROCKET") {
			out = append(out, c)
		}
	}
	return out
}

// unprocessedReviewCommands fetches owner/repo#prNumber's comments and
// returns the unprocessed "/pruefer review" commands among them. A thin
// fetch-then-filter wrapper around pendingReviewComments for callers that
// don't already have a comment list in hand.
func unprocessedReviewCommands(client GitHubCommenter, owner, repo string, prNumber int) ([]gh.Comment, error) {
	comments, err := client.FetchIssueComments(owner, repo, prNumber)
	if err != nil {
		return nil, err
	}
	return pendingReviewComments(comments), nil
}

// PendingForceReview reports whether owner/repo#prNumber has an unprocessed
// "/pruefer review" comment.
func PendingForceReview(client GitHubCommenter, owner, repo string, prNumber int) (bool, error) {
	pending, err := unprocessedReviewCommands(client, owner, repo, prNumber)
	if err != nil {
		return false, err
	}
	return len(pending) > 0, nil
}

// AcknowledgeForceReview adds the 👀 (eyes) reaction to every unprocessed
// "/pruefer review" comment, signaling that Pruefer has picked it up and is
// acting on it. Call before invoking Claude; a failure here is non-fatal to
// the review itself (the caller should log and continue).
func AcknowledgeForceReview(client GitHubCommenter, owner, repo string, prNumber int) error {
	pending, err := unprocessedReviewCommands(client, owner, repo, prNumber)
	if err != nil {
		return err
	}
	for _, c := range pending {
		if c.HasReaction("EYES") {
			continue
		}
		if err := client.AddCommentReaction(owner, repo, c.DatabaseID, "eyes"); err != nil {
			return err
		}
	}
	return nil
}

// MarkForceReviewsProcessed adds the 🚀 (rocket) reaction to every
// unprocessed "/pruefer review" comment on owner/repo#prNumber, so a
// subsequent poll does not treat them as still-pending. Call only after a
// review has actually been submitted for the forced request (see review.go)
// — this is the durable "don't reprocess this comment" marker.
func MarkForceReviewsProcessed(client GitHubCommenter, owner, repo string, prNumber int) error {
	pending, err := unprocessedReviewCommands(client, owner, repo, prNumber)
	if err != nil {
		return err
	}
	for _, c := range pending {
		if err := client.AddCommentReaction(owner, repo, c.DatabaseID, "rocket"); err != nil {
			return err
		}
	}
	return nil
}
