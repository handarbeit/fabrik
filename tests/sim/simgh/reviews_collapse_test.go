package simgh

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestLatestReviewsByAuthor_MatchesProductionCollapse is the R5 parity check for
// #2588 (ADR 2582): it pins the same history shapes as
// github.TestFetchPRReviews_CollapseShapes. Only APPROVED and CHANGES_REQUESTED
// are sticky against a later COMMENTED; a DISMISSED ends the verdict. The
// stickyDismissed column is the neutralisation seam restoring the old rule.
func TestLatestReviewsByAuthor_MatchesProductionCollapse(t *testing.T) {
	cases := []struct {
		name       string
		history    []string
		want       string
		wantSticky string
	}{
		{"dismissed then commented", []string{"DISMISSED", "COMMENTED"}, "COMMENTED", "DISMISSED"},
		{"changes requested then commented", []string{"CHANGES_REQUESTED", "COMMENTED"}, "CHANGES_REQUESTED", "CHANGES_REQUESTED"},
		{"approved then commented", []string{"APPROVED", "COMMENTED"}, "APPROVED", "APPROVED"},
		{"dismissed, commented, changes requested", []string{"DISMISSED", "COMMENTED", "CHANGES_REQUESTED"}, "CHANGES_REQUESTED", "CHANGES_REQUESTED"},
		{"changes requested, dismissed, commented", []string{"CHANGES_REQUESTED", "DISMISSED", "COMMENTED"}, "COMMENTED", "DISMISSED"},
		{"commented then dismissed", []string{"COMMENTED", "DISMISSED"}, "DISMISSED", "DISMISSED"},
		{"lone dismissed", []string{"DISMISSED"}, "DISMISSED", "DISMISSED"},
		{"dismissed then approved", []string{"DISMISSED", "APPROVED"}, "APPROVED", "APPROVED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in []gh.PRReview
			for i, st := range tc.history {
				in = append(in, gh.PRReview{Author: "bot", State: st, DatabaseID: i + 1})
			}
			for _, sticky := range []bool{false, true} {
				want := tc.want
				if sticky {
					want = tc.wantSticky
				}
				got := latestReviewsByAuthor(in, sticky)
				if len(got) != 1 || got[0].State != want {
					t.Errorf("sticky=%v: collapsed = %+v, want one %s entry", sticky, got, want)
				}
			}
		})
	}
}
