package pruefer

import (
	"fmt"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// diffTooLargeMarker returns the idempotency marker embedded in a too-large
// notice's body, keyed by head SHA — mirrors engine/merge_train.go's
// mergeTrainBatchMarker convention (a marker embedded in a posted GitHub
// artifact, scanned back out on the next pass, rather than any local
// persistence — see ADR-1113 Decision 2 and adrs/1274-*.md).
func diffTooLargeMarker(headSHA string) string {
	return fmt.Sprintf("<!-- pruefer:diff-too-large:%s -->", headSHA)
}

// alreadyNoticedTooLarge reports whether comments already contains a
// too-large notice for headSHA, so ReviewPR can recognize "already noticed,
// nothing new to do" (FR-2's per-SHA idempotency) and, called before
// FetchPRDiff, skip re-deriving that verdict on every poll (FR-4) without
// any local bookkeeping.
func alreadyNoticedTooLarge(comments []gh.Comment, headSHA string) bool {
	marker := diffTooLargeMarker(headSHA)
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			return true
		}
	}
	return false
}

// humanBytes formats n as a human-readable byte count (KB/MB/...), for the
// too-large notice's operator-facing size wording. Matches the same raw
// diff-byte accounting used throughout this package (see DiffSizeDetail) —
// never line counts, which the issue this notice exists for explicitly
// found misleading.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	units := "KMGTPE"
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), units[exp])
}

// noticePathListLimit caps how many paths any single path-list section of a
// Pruefer-authored PR comment renders explicitly — the too-large notice's
// OmittedPaths/TrimAttempted sections, the all-excluded notice, and (via
// boundedPathList) the submitted review body's own "Omitted from this
// review" section in findings.go. All of these are, unlike DominantPaths
// (already capped at dominantPathsLimit upstream), otherwise unbounded — a
// diff whose excluded_paths glob matches a whole vendor/ or generated/ tree,
// or whose trim pass drops many small files, could otherwise grow past
// GitHub's ~65536-character PR/issue comment limit. A silently-failed
// AddComment/SubmitPRReview leaves no marker on the PR (for the notice) or
// drops the review outright (for the submitted body), so the same failure
// repeats every poll — reproducing the exact silent, repeated-failure mode
// this notice mechanism exists to close.
const noticePathListLimit = 20

// boundedPathList truncates paths to at most noticePathListLimit entries,
// returning the entries to render and how many were left out — the shared
// truncation rule every path-list section in a Pruefer-authored comment
// applies, regardless of that section's own bullet formatting.
func boundedPathList(paths []string) (shown []string, truncated int) {
	if len(paths) > noticePathListLimit {
		return paths[:noticePathListLimit], len(paths) - noticePathListLimit
	}
	return paths, 0
}

// writePathList renders paths as a Markdown bullet list, truncated per
// boundedPathList with a summary line for the remainder.
func writePathList(b *strings.Builder, paths []string) {
	shown, truncated := boundedPathList(paths)
	for _, p := range shown {
		fmt.Fprintf(b, "- `%s`\n", p)
	}
	if truncated > 0 {
		fmt.Fprintf(b, "- _(and %d more)_\n", truncated)
	}
}

// writePathSizeList is writePathList's PathSize analog, rendering each
// entry with its human-readable byte size — used for TrimAttempted and
// DominantPaths, both of which (unlike OmittedPaths) carry sizes so the
// notice can satisfy FR-3's "dominant contributing paths" promise wherever
// it has size information to show.
func writePathSizeList(b *strings.Builder, paths []PathSize) {
	shown := paths
	truncated := 0
	if len(shown) > noticePathListLimit {
		truncated = len(shown) - noticePathListLimit
		shown = shown[:noticePathListLimit]
	}
	for _, p := range shown {
		fmt.Fprintf(b, "- `%s` (%s)\n", p.Path, humanBytes(p.Bytes))
	}
	if truncated > 0 {
		fmt.Fprintf(b, "- _(and %d more)_\n", truncated)
	}
}

// buildTooLargeNoticeBody renders the PR comment posted when a diff cannot
// be reviewed even after exclusion and FR-5's best-effort trim — FR-3's
// "measured size, cap, dominant contributing paths" plus something an
// operator can act on. Embeds diffTooLargeMarker(headSHA) so
// alreadyNoticedTooLarge recognizes it on a later poll against the same
// head commit.
func buildTooLargeNoticeBody(detail DiffSizeDetail, headSHA string) string {
	var b strings.Builder
	b.WriteString("**Pruefer could not review this pull request: the diff is too large.**\n\n")
	fmt.Fprintf(&b, "Measured %s of diff against a %s cap (`max_diff_bytes`).\n\n", humanBytes(detail.MeasuredBytes), humanBytes(detail.MaxBytes))
	if len(detail.OmittedPaths) > 0 {
		b.WriteString("Already excluded via `excluded_paths` (not enough on its own):\n\n")
		writePathList(&b, detail.OmittedPaths)
		b.WriteString("\n")
	}
	if len(detail.TrimAttempted) > 0 {
		b.WriteString("Pruefer also tried automatically dropping the largest remaining file(s), but the rest was still over the cap:\n\n")
		writePathSizeList(&b, detail.TrimAttempted)
		b.WriteString("\n")
	}
	if len(detail.DominantPaths) > 0 {
		b.WriteString("Largest remaining contributors:\n\n")
		writePathSizeList(&b, detail.DominantPaths)
		b.WriteString("\n")
	}
	b.WriteString("To get this PR reviewed, add the path(s) above to `excluded_paths`, or split the PR so the reviewable code isn't bundled with the oversized file(s).\n\n")
	b.WriteString(diffTooLargeMarker(headSHA))
	b.WriteString("\n")
	return b.String()
}

// buildAllExcludedNoticeBody renders the acknowledgment comment posted when
// a forced "/pruefer review" resolves to "every touched file matches
// excluded_paths" — the one terminal outcome of a forced review that would
// otherwise leave no human-readable comment behind (success, still-too-large,
// and already-noticed-too-large all post one; see ReviewPR). Unlike the
// too-large notice this carries no idempotency marker: it is only ever
// posted in direct response to an unprocessed "/pruefer review" comment,
// which ReviewPR marks processed in the same call, so it can never repeat
// for the same command.
func buildAllExcludedNoticeBody(excludedPaths []string) string {
	var b strings.Builder
	b.WriteString("**Pruefer could not review this pull request: every touched file matches `excluded_paths`.**\n\n")
	if len(excludedPaths) > 0 {
		b.WriteString("Excluded paths:\n\n")
		writePathList(&b, excludedPaths)
		b.WriteString("\n")
	}
	b.WriteString("Nothing left to review once exclusions are applied.\n")
	return b.String()
}
