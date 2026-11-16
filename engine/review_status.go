package engine

import (
	"strings"

	"github.com/handarbeit/fabrik/stages"
)

// ReviewStatusEnabled reports whether the `fabrik/review` commit status (#2471,
// ADR 2471) is enabled anywhere: the repo-wide review_status config key, or any
// stage's own review_status: true. It is the one decision the startup
// permission check and `fabrik init --github-app` share, so they cannot
// disagree about when `statuses: write` is demanded. It deliberately ignores
// per-item labels and wait_for_reviews: a mis-set stage is reported separately,
// and over-asking for the grant is the safe direction.
func ReviewStatusEnabled(repoWide bool, stgs []*stages.Stage) bool {
	if repoWide {
		return true
	}
	for _, s := range stgs {
		if s != nil && s.ReviewStatus != nil && *s.ReviewStatus {
			return true
		}
	}
	return false
}

// reviewStatusState is a value of the fabrik/review commit status.
const (
	reviewStatusPending = "pending"
	reviewStatusSuccess = "success"
	reviewStatusFailure = "failure"
)

// reviewStatusResult is the status a head should carry.
type reviewStatusResult struct {
	State       string
	Description string
}

// reviewStatusVerdict is the fabrik/review status for head, derived from the
// per-reviewer standings (reviewerStandings, #2465) of the declared reviewers —
// never from a second collapse of the review history (FR-003). The caller
// supplies a known head; an empty head yields ok=false (no status is written:
// an unknown head never produces success).
//
// Per declared reviewer:
//
//	changes requested                       → failure
//	awaiting re-review (verdict on old head) → pending
//	clean                                   → success
//	no verdict, COMMENTED at head           → success (how a comment-only bot such
//	                                          as Pruefer expresses a clean pass; a
//	                                          review with findings makes Fabrik push
//	                                          a fix, i.e. a new head and a pending
//	                                          status)
//	no verdict, no comment at head          → pending
//	absent from the history                 → pending
//
// Overall: any failure wins, then any pending, otherwise success. Undeclared
// reviewers never affect the status.
func reviewStatusVerdict(standings []reviewerStanding, declared []string, head string) (reviewStatusResult, bool) {
	if head == "" || len(declared) == 0 {
		return reviewStatusResult{}, false
	}
	var failed, pending, clean []string
	for _, d := range declared {
		var st *reviewerStanding
		for i := range standings {
			if reviewerIdentityMatches(d, standings[i].Author) {
				st = &standings[i]
				break
			}
		}
		switch {
		case st == nil:
			pending = append(pending, d)
		case st.Kind == standingChangesRequested:
			failed = append(failed, d)
		case st.Kind == standingAwaitingReReview:
			pending = append(pending, d)
		case st.Kind == standingClean, st.Kind == standingNoVerdict && st.CommentAtHead:
			clean = append(clean, d)
		default:
			pending = append(pending, d)
		}
	}
	sha := standingSHA(head)
	switch {
	case len(failed) > 0:
		return reviewStatusResult{reviewStatusFailure, fitStatusDescription("Changes requested on " + sha + " by " + strings.Join(failed, ", "))}, true
	case len(pending) > 0:
		return reviewStatusResult{reviewStatusPending, fitStatusDescription("Waiting for review of " + sha + " from " + strings.Join(pending, ", "))}, true
	default:
		return reviewStatusResult{reviewStatusSuccess, fitStatusDescription("Reviewed " + sha + ": no changes requested by " + strings.Join(clean, ", "))}, true
	}
}

// fitStatusDescription keeps a description within GitHub's 140-character limit.
func fitStatusDescription(s string) string {
	if r := []rune(s); len(r) > 140 {
		return string(r[:139]) + "…"
	}
	return s
}
