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
// client — keeps these functions testable without HTTP mocking.
type GitHubCommenter interface {
	FetchIssueComments(owner, repo string, issueNumber int) ([]gh.Comment, error)
	AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error
	// AddComment posts a comment on the PR (issue-numbered) and returns its
	// database ID. Used by notice.go to post the diff-unavailable notice
	// (R4) when a 406 too_large diff and its files-API fallback both fail.
	AddComment(owner, repo string, issueNumber int, body string) (int, error)
}

// isReviewCommand reports whether body contains the /pruefer review
// command, matched case-insensitively — consistent with how Fabrik
// recognizes its own comment commands.
func isReviewCommand(body string) bool {
	return strings.Contains(strings.ToLower(body), reviewCommand)
}

// unprocessedReviewCommands returns the "/pruefer review" comments on
// owner/repo#prNumber that have not yet been marked processed (no ROCKET
// reaction) — the same 👀/🚀 reaction-based idempotency convention Fabrik
// uses for its own comment processing (see engine/comments.go).
func unprocessedReviewCommands(client GitHubCommenter, owner, repo string, prNumber int) ([]gh.Comment, error) {
	comments, err := client.FetchIssueComments(owner, repo, prNumber)
	if err != nil {
		return nil, err
	}
	var out []gh.Comment
	for _, c := range comments {
		if isReviewCommand(c.Body) && !c.HasReaction("ROCKET") {
			out = append(out, c)
		}
	}
	return out, nil
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
