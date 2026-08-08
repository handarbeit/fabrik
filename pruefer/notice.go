package pruefer

import (
	"fmt"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// diffTooLargeMarker returns the idempotency marker embedded in a "diff too
// large to review" PR notice, keyed to headSHA so a later push (new head)
// gets its own notice rather than being silently suppressed by a stale one.
//
// This exact format is shared with #1274's sibling "diff obtained but
// exceeds max_diff_bytes" notice mechanism (see
// adrs/1427-pruefer-diff-too-large-degrade-not-block.md) — deliberately
// duplicated here (rather than imported) because #1274's branch is unmerged
// as of this writing; when it lands, the two notice.go additions are
// expected to collide as an add/add conflict requiring explicit human
// reconciliation, rather than silently diverging.
func diffTooLargeMarker(headSHA string) string {
	return fmt.Sprintf("<!-- pruefer:diff-too-large:%s -->", headSHA)
}

// alreadyNoticedTooLarge reports whether comments already contains a
// too-large notice for headSHA — Pruefer's GitHub-derived idempotency check
// (no local state; see adrs/1113-pruefer-v1-architecture.md), mirroring
// ReviewPR's existing alreadyReviewedAtHead pattern for the review path.
func alreadyNoticedTooLarge(comments []gh.Comment, headSHA string) bool {
	marker := diffTooLargeMarker(headSHA)
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			return true
		}
	}
	return false
}

// buildDiffUnavailableNoticeBody renders the PR comment posted when
// FetchPRDiff returns gh.ErrDiffTooLarge and the files-API fallback (R3)
// also failed to produce a changed-path list — the terminal case where
// GitHub could not enumerate the PR's changes by any means Pruefer has.
// Unlike #1274's buildTooLargeNoticeBody, there is no measured diff size to
// report here: GitHub refused the .diff media type outright, and the
// fallback that would otherwise supply a path list also failed.
func buildDiffUnavailableNoticeBody(headSHA string) string {
	return fmt.Sprintf(`Pruefer could not review this pull request at %s.

GitHub refused to render this PR's diff (it exceeds the 20,000-line cap on the diff media type), and the fallback changed-files listing also failed, so Pruefer has no way to enumerate what changed. This PR will not be automatically re-attempted until a new commit is pushed.

%s`, headSHA, diffTooLargeMarker(headSHA))
}

// postDiffUnavailableNoticeOnce posts buildDiffUnavailableNoticeBody's
// notice on owner/repo#prNumber unless a notice for this exact headSHA has
// already been posted (checked by fetching current comments and scanning
// for the marker — GitHub-derived, not locally persisted, matching this
// package's existing idempotency convention). A comment-fetch error fails
// closed: no notice is posted, since posting without checking risks a
// duplicate on every poll of a permanently-406 PR.
func postDiffUnavailableNoticeOnce(client GitHubCommenter, owner, repo string, prNumber int, headSHA string) error {
	comments, err := client.FetchIssueComments(owner, repo, prNumber)
	if err != nil {
		return fmt.Errorf("fetching comments to check too-large notice idempotency: %w", err)
	}
	if alreadyNoticedTooLarge(comments, headSHA) {
		return nil
	}
	if _, err := client.AddComment(owner, repo, prNumber, buildDiffUnavailableNoticeBody(headSHA)); err != nil {
		return fmt.Errorf("posting diff-unavailable notice: %w", err)
	}
	return nil
}
