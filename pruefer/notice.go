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
// Shared verbatim by both notice bodies in this file — the diff-unavailable
// case (#1427, GitHub's 406 too_large) and the diff-obtained-but-oversized
// case (#1462, buildDiffTooLargeAfterFetchNoticeBody below) — so
// alreadyNoticedTooLarge treats either as already having notified a human
// about this head SHA: at most one notice ever exists per head SHA across
// both paths (R5, adrs/1462-pruefer-per-file-diff-exclusion-and-trim.md).
// #1427 originally duplicated this marker format from #1274's then-unmerged
// branch rather than importing it, anticipating exactly this reconciliation;
// #1462 is that reconciliation landing in the same file.
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
// Unlike buildDiffTooLargeAfterFetchNoticeBody below, there is no measured
// diff size to report here: GitHub refused the .diff media type outright,
// and the fallback that would otherwise supply a path list also failed.
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

// buildDiffTooLargeAfterFetchNoticeBody renders the PR comment posted when a
// diff *was* obtained but still exceeded max_diff_bytes after per-file
// exclusion and, if necessary, trimming (R4, R5) — the sibling of
// buildDiffUnavailableNoticeBody above for the case where GitHub did render
// the diff. reviewed distinguishes the two possible outcomes: true means the
// review proceeded on the disclosed remainder (excludedPaths and/or
// trimmedPaths omitted, everything else reviewed); false means nothing
// reviewable survived exclusion and trimming, so the PR was skipped
// entirely (the same terminal SkipDiffTooLarge disposition the pre-existing
// bare-size skip already produced). Shares diffTooLargeMarker with
// buildDiffUnavailableNoticeBody so postDiffTooLargeAfterFetchNoticeOnce
// below and postDiffUnavailableNoticeOnce are mutually idempotent against
// each other's marker — at most one notice exists per head SHA regardless
// of which path produced it (R5).
func buildDiffTooLargeAfterFetchNoticeBody(headSHA string, totalBytes, maxBytes int64, excludedPaths, trimmedPaths []string, reviewed bool) string {
	var b strings.Builder
	if reviewed {
		fmt.Fprintf(&b, "Pruefer's diff for this pull request at %s was %d bytes, over the configured max_diff_bytes=%d. It reviewed the remaining files after omitting the ones below.\n\n", headSHA, totalBytes, maxBytes)
	} else {
		fmt.Fprintf(&b, "Pruefer could not review this pull request at %s: its diff is %d bytes, over the configured max_diff_bytes=%d, and nothing reviewable remained after omitting the files below.\n\n", headSHA, totalBytes, maxBytes)
	}
	if len(excludedPaths) > 0 {
		b.WriteString("Omitted by `excluded_paths`:\n")
		for _, p := range excludedPaths {
			fmt.Fprintf(&b, "- %s\n", p)
		}
		b.WriteString("\n")
	}
	if len(trimmedPaths) > 0 {
		b.WriteString("Omitted by size trimming (largest files first):\n")
		for _, p := range trimmedPaths {
			fmt.Fprintf(&b, "- %s\n", p)
		}
		b.WriteString("\n")
	}
	b.WriteString(diffTooLargeMarker(headSHA))
	return b.String()
}

// postDiffTooLargeAfterFetchNoticeOnce posts
// buildDiffTooLargeAfterFetchNoticeBody's notice unless a notice for this
// exact headSHA already exists — checked the same way
// postDiffUnavailableNoticeOnce is, and against the same marker, so a PR
// that hit the diff-unavailable path on one poll and this path on another
// (or vice versa) never accumulates two notices for one head SHA (R5). A
// comment-fetch error fails closed: no notice is posted.
func postDiffTooLargeAfterFetchNoticeOnce(client GitHubCommenter, owner, repo string, prNumber int, headSHA string, totalBytes, maxBytes int64, excludedPaths, trimmedPaths []string, reviewed bool) error {
	comments, err := client.FetchIssueComments(owner, repo, prNumber)
	if err != nil {
		return fmt.Errorf("fetching comments to check too-large notice idempotency: %w", err)
	}
	if alreadyNoticedTooLarge(comments, headSHA) {
		return nil
	}
	body := buildDiffTooLargeAfterFetchNoticeBody(headSHA, totalBytes, maxBytes, excludedPaths, trimmedPaths, reviewed)
	if _, err := client.AddComment(owner, repo, prNumber, body); err != nil {
		return fmt.Errorf("posting diff-too-large-after-fetch notice: %w", err)
	}
	return nil
}
