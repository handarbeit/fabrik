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
		for _, p := range detail.OmittedPaths {
			fmt.Fprintf(&b, "- `%s`\n", p)
		}
		b.WriteString("\n")
	}
	if len(detail.DominantPaths) > 0 {
		b.WriteString("Largest remaining contributors:\n\n")
		for _, p := range detail.DominantPaths {
			fmt.Fprintf(&b, "- `%s` (%s)\n", p.Path, humanBytes(p.Bytes))
		}
		b.WriteString("\n")
	}
	b.WriteString("To get this PR reviewed, add the path(s) above to `excluded_paths`, or split the PR so the reviewable code isn't bundled with the oversized file(s).\n\n")
	b.WriteString(diffTooLargeMarker(headSHA))
	b.WriteString("\n")
	return b.String()
}
