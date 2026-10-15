package pruefer

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// The patch-unchanged skip (#2230, adrs/2230-pruefer-patch-identity-skip.md).
//
// A head whose whole change against the merge base with the PR's base branch is
// patch-identical to the change at the last head Pruefer completed a review of
// carries nothing new to review: a rebase onto an updated base, a clean
// catch-up merge, a force-push back to an earlier state. The proof is computed
// from diff text GitHub serves (patchid.go) and trusts nothing the PR author
// controls — no marker, no commit message, no trailer.
//
// It fails toward reviewing. Anything missing, ambiguous or failed — no prior
// review, a prior review that was not complete (#2190), an unreachable old
// head, a binary or empty diff, a failed read — means "review as today". A
// tolerated transient failure additionally marks the outcome degraded so the
// daemon never memoises it as conclusive.

// maxPatchSkipChain bounds how many consecutive patch-unchanged skips are taken
// over one reviewed head before a real review is forced, so a long run of
// replays still gets looked at periodically. Same value as maxCatchUpChain. The
// run length is in memory (ReviewTracker): nothing in GitHub records a silent
// skip, so a restart resets it (accepted, see the ADR).
const maxPatchSkipChain = 4

var (
	coverageLineAnyRE      = regexp.MustCompile(`(?m)^PRUEFER_COVERAGE:.*$`)
	coverageLineCompleteRE = regexp.MustCompile(`^PRUEFER_COVERAGE:[ \t]*complete\b`)
)

// reviewBodyComplete reports whether a Pruefer review body ends in a
// `PRUEFER_COVERAGE: complete` line (#2190). appendCoverage moves any
// model-written lookalike off the start of the line, so a line-anchored match
// can only be Pruefer's own. The last such line decides; no line (a review from
// before the contract) or a malformed one is not complete.
func reviewBodyComplete(body string) bool {
	all := coverageLineAnyRE.FindAllString(body, -1)
	if len(all) == 0 {
		return false
	}
	return coverageLineCompleteRE.MatchString(strings.TrimRight(all[len(all)-1], "\r"))
}

// lastBotReview returns Pruefer's most recent review in reviews that names a
// commit. FetchPRReviews already collapses to the latest review per author;
// the SubmittedAt tie-break only matters for a caller passing an uncollapsed
// list.
func lastBotReview(reviews []gh.PRReview, botLogin string) (gh.PRReview, bool) {
	var best gh.PRReview
	found := false
	for _, r := range reviews {
		if r.CommitID == "" || !strings.EqualFold(r.Author, botLogin) {
			continue
		}
		if !found || r.SubmittedAt.After(best.SubmittedAt) {
			best, found = r, true
		}
	}
	return best, found
}

// patchVerdict is the result of a patch-identity decision.
type patchVerdict struct {
	Skip     bool
	Detail   string // human-readable, logged; empty when there was nothing to say
	Degraded bool   // a tolerated read failed, so the outcome must not be memoised
}

// fetchComparePrint fetches and fingerprints the change head carries relative
// to the merge base with base. ok is false when it cannot be compared;
// degraded is true when that is because of a (possibly) transient failure
// rather than a deterministic one (GitHub no longer has the head, the diff is
// too large to render, binary content).
func fetchComparePrint(client GitHubReviewer, owner, repo, base, head string) (pp patchPrint, ok, degraded bool, detail string) {
	diff, err := client.FetchCompareDiff(owner, repo, base, head)
	if err != nil {
		switch {
		case errors.Is(err, gh.ErrNotFound), errors.Is(err, gh.ErrUnprocessableEntity), errors.Is(err, gh.ErrDiffTooLarge):
			return patchPrint{}, false, false, fmt.Sprintf("reviewed head %s cannot be compared (%v)", shortSHA(head), err)
		}
		return patchPrint{}, false, true, fmt.Sprintf("reading the change at reviewed head %s failed: %v", shortSHA(head), err)
	}
	pp, ok = patchFingerprint(diff)
	if !ok {
		return patchPrint{}, false, false, fmt.Sprintf("the change at reviewed head %s is empty or not comparable", shortSHA(head))
	}
	return pp, true, false, ""
}

// patchUnchangedSkip decides whether pr's head can be skipped because its
// change is identical to the change at the last head Pruefer completed a
// review of. reviews are the PR's reviews already fetched by ReviewPR; diff is
// the PR's current raw diff (before exclusion or trimming), "" when it could
// not be obtained. Every uncertain input yields Skip=false.
func patchUnchangedSkip(client GitHubReviewer, cfg Config, botLogin, owner, repo string, pr gh.PRDetails, reviews []gh.PRReview, diff string, tracker *ReviewTracker) patchVerdict {
	if !cfg.PatchSkip || pr.HeadSHA == "" || pr.BaseRef == "" || diff == "" {
		return patchVerdict{}
	}
	last, ok := lastBotReview(reviews, botLogin)
	if !ok || last.CommitID == pr.HeadSHA {
		return patchVerdict{} // never reviewed, or this very head (not this check's business)
	}
	// FR-008: only a completed review of the earlier change makes an identical
	// change safe to skip. A partial one must not suppress the re-review.
	if !reviewBodyComplete(last.Body) {
		return patchVerdict{Detail: fmt.Sprintf("last review (head %s) is not marked PRUEFER_COVERAGE: complete", shortSHA(last.CommitID))}
	}
	if n := tracker.PatchSkips(owner, repo, pr.Number, last.CommitID); n >= maxPatchSkipChain {
		return patchVerdict{Detail: fmt.Sprintf("%d consecutive patch-unchanged skips over %s; forcing a real review", n, shortSHA(last.CommitID))}
	}
	cur, ok := patchFingerprint(diff)
	if !ok {
		return patchVerdict{Detail: "the current change is empty or not comparable"}
	}
	prev, ok, degraded, detail := fetchComparePrint(client, owner, repo, pr.BaseRef, last.CommitID)
	if !ok {
		return patchVerdict{Detail: detail, Degraded: degraded}
	}
	if prev.Whole != cur.Whole {
		return patchVerdict{Detail: fmt.Sprintf("the change differs from reviewed head %s", shortSHA(last.CommitID))}
	}
	n := tracker.NotePatchSkip(owner, repo, pr.Number, last.CommitID)
	return patchVerdict{Skip: true, Detail: fmt.Sprintf("head %s has the same patch as reviewed head %s (fp=%s, chain %d/%d)",
		shortSHA(pr.HeadSHA), shortSHA(last.CommitID), cur.Whole[:8], n, maxPatchSkipChain)}
}
