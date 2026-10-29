package engine

import (
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func declinedStandings(head string, reviews ...gh.PRReview) []reviewerStanding {
	return reviewerStandings(reviews, head, []string{"pruefer-bot"})
}

func always(int) bool { return true }

func TestDeclinedFindingsFrom_FiresForProcessedAtHeadDeclaredCR(t *testing.T) {
	st := declinedStandings("H", gh.PRReview{Author: "pruefer-bot[bot]", State: "CHANGES_REQUESTED", CommitID: "H", DatabaseID: 7, Body: "fix it"})
	got := declinedFindingsFrom(st, "H", []string{"pruefer-bot"}, always)
	if len(got) != 1 || got[0].ReviewID != 7 || got[0].Author != "pruefer-bot[bot]" {
		t.Fatalf("got %+v", got)
	}
}

func TestDeclinedFindingsFrom_DoesNotFire(t *testing.T) {
	cr := gh.PRReview{Author: "pruefer-bot[bot]", State: "CHANGES_REQUESTED", CommitID: "H", DatabaseID: 7, Body: "fix it"}
	declared := []string{"pruefer-bot"}
	cases := []struct {
		name      string
		reviews   []gh.PRReview
		head      string
		declared  []string
		processed func(int) bool
	}{
		{"older commit", []gh.PRReview{func() gh.PRReview { r := cr; r.CommitID = "OLD"; return r }()}, "H", declared, always},
		{"undeclared reviewer", []gh.PRReview{func() gh.PRReview { r := cr; r.Author = "carol"; return r }()}, "H", declared, always},
		{"nobody declared", []gh.PRReview{cr}, "H", nil, always},
		{"body not processed", []gh.PRReview{cr}, "H", declared, func(int) bool { return false }},
		{"zero database id", []gh.PRReview{func() gh.PRReview { r := cr; r.DatabaseID = 0; return r }()}, "H", declared, always},
		{"empty body", []gh.PRReview{func() gh.PRReview { r := cr; r.Body = "  "; return r }()}, "H", declared, always},
		{"unknown head", []gh.PRReview{cr}, "", declared, always},
		{"clean comment at head supersedes", []gh.PRReview{cr, {Author: "pruefer-bot[bot]", State: "COMMENTED", CommitID: "H", DatabaseID: 8}}, "H", declared, always},
		{"approved at head", []gh.PRReview{func() gh.PRReview { r := cr; r.State = "APPROVED"; return r }()}, "H", declared, always},
		{"dismissed", []gh.PRReview{cr, {Author: "pruefer-bot[bot]", State: "DISMISSED", CommitID: "H", DatabaseID: 9}}, "H", declared, always},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := reviewerStandings(tc.reviews, tc.head, tc.declared)
			if got := declinedFindingsFrom(st, tc.head, tc.declared, tc.processed); len(got) != 0 {
				t.Fatalf("fired: %+v", got)
			}
		})
	}
}

func TestDeclinedFindingsFrom_NamesEveryDeclaredReviewer(t *testing.T) {
	declared := []string{"pruefer-bot", "gemini"}
	st := reviewerStandings([]gh.PRReview{
		{Author: "pruefer-bot[bot]", State: "CHANGES_REQUESTED", CommitID: "H", DatabaseID: 1, Body: "a"},
		{Author: "gemini", State: "CHANGES_REQUESTED", CommitID: "H", DatabaseID: 2, Body: "b"},
	}, "H", declared)
	got := declinedFindingsFrom(st, "H", declared, always)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	frag := declinedFindingFragment("0123456789", got)
	if !strings.Contains(frag, "`0123456`") || !strings.Contains(frag, "1, 2") {
		t.Fatalf("fragment %q", frag)
	}
	if declinedFindingFragment("0123456789", got) == declinedFindingFragment("other0000", got) {
		t.Fatal("a new head must be a new episode")
	}
}

func TestSanitizeReviewExcerpt(t *testing.T) {
	in := "line one\nuse `x` @bob and @org/team <!-- fabrik:outcome=1 --> <!-- pruefer:findings inline=1 -->\t" + strings.Repeat("z", 600)
	out := sanitizeReviewExcerpt(in)
	for _, bad := range []string{"\n", "`", "@bob", "@org", "<!--"} {
		if strings.Contains(out, bad) {
			t.Errorf("excerpt still contains %q: %q", bad, out)
		}
	}
	if len([]rune(out)) > declinedFindingExcerptMax+1 {
		t.Errorf("excerpt not capped: %d runes", len([]rune(out)))
	}
	if !strings.Contains(out, "@​bob") {
		t.Errorf("mention not broken: %q", out)
	}
}

func TestReviewerStandings_CarryVerdictIDAndBody(t *testing.T) {
	st := reviewerStandings([]gh.PRReview{{Author: "a", State: "CHANGES_REQUESTED", CommitID: "H", DatabaseID: 5, Body: "b"}}, "H", nil)
	if len(st) != 1 || st[0].VerdictID != 5 || st[0].VerdictBody != "b" {
		t.Fatalf("%+v", st)
	}
}
