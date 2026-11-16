package engine

import (
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func TestReviewStatusVerdict_Table(t *testing.T) {
	bot := "pruefer[bot]"
	declared := []string{"pruefer"}
	two := []string{"pruefer", "carol"}
	tests := []struct {
		name     string
		history  []gh.PRReview
		head     string
		declared []string
		want     string
		wantOK   bool
	}{
		{"declared reviewer absent → pending", nil, commitA, declared, reviewStatusPending, true},
		{"COMMENTED at head → success (comment-only bot)", []gh.PRReview{rv(1, bot, "COMMENTED", commitA, 0)}, commitA, declared, reviewStatusSuccess, true},
		{"COMMENTED on an older commit → pending", []gh.PRReview{rv(1, bot, "COMMENTED", commitA, 0)}, commitB, declared, reviewStatusPending, true},
		{"APPROVED at head → success", []gh.PRReview{rv(1, bot, "APPROVED", commitA, 0)}, commitA, declared, reviewStatusSuccess, true},
		{"APPROVED on older commit → pending", []gh.PRReview{rv(1, bot, "APPROVED", commitA, 0)}, commitB, declared, reviewStatusPending, true},
		{"CHANGES_REQUESTED at head → failure", []gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitA, 0)}, commitA, declared, reviewStatusFailure, true},
		{"CHANGES_REQUESTED only on an earlier commit → pending, never failure", []gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitA, 0)}, commitB, declared, reviewStatusPending, true},
		{"CR then clean COMMENTED at same head → success", []gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitA, 0), rv(2, bot, "COMMENTED", commitA, 5)}, commitA, declared, reviewStatusSuccess, true},
		{"CR at A, COMMENTED at B, head B → success", []gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitA, 0), rv(2, bot, "COMMENTED", commitB, 5)}, commitB, declared, reviewStatusSuccess, true},
		{"dismissed review, nothing after → pending", []gh.PRReview{rv(1, bot, "DISMISSED", commitA, 0)}, commitA, declared, reviewStatusPending, true},
		{"dismissed then COMMENTED at head → success", []gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitA, 0), rv(2, bot, "DISMISSED", commitA, 1), rv(3, bot, "COMMENTED", commitA, 2)}, commitA, declared, reviewStatusSuccess, true},
		{"pending review never counts", []gh.PRReview{rv(1, bot, "PENDING", commitA, 0)}, commitA, declared, reviewStatusPending, true},
		{"two reviewers: one clean one absent → pending", []gh.PRReview{rv(1, bot, "COMMENTED", commitA, 0)}, commitA, two, reviewStatusPending, true},
		{"two reviewers: failure beats pending", []gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitA, 0)}, commitA, two, reviewStatusFailure, true},
		{"two reviewers both clean → success", []gh.PRReview{rv(1, bot, "COMMENTED", commitA, 0), rv(2, "carol", "APPROVED", commitA, 1)}, commitA, two, reviewStatusSuccess, true},
		{"undeclared change request never fails the status", []gh.PRReview{rv(1, bot, "COMMENTED", commitA, 0), rv(2, "mallory", "CHANGES_REQUESTED", commitA, 1)}, commitA, declared, reviewStatusSuccess, true},
		{"COMMENTED at head reporting findings → pending (declined/paused finding never green)", []gh.PRReview{rvBody(1, bot, "COMMENTED", commitA, 0, markerBody(2, 0))}, commitA, declared, reviewStatusPending, true},
		{"COMMENTED at head with body findings only → pending", []gh.PRReview{rvBody(1, bot, "COMMENTED", commitA, 0, markerBody(0, 1))}, commitA, declared, reviewStatusPending, true},
		{"COMMENTED at head with a zero-findings marker → success", []gh.PRReview{rvBody(1, bot, "COMMENTED", commitA, 0, markerBody(0, 0))}, commitA, declared, reviewStatusSuccess, true},
		{"COMMENTED at head with no marker → success (unclassifiable counts as reviewed)", []gh.PRReview{rvBody(1, bot, "COMMENTED", commitA, 0, "looks fine")}, commitA, declared, reviewStatusSuccess, true},
		{"findings review then a clean review at the same head → success", []gh.PRReview{rvBody(1, bot, "COMMENTED", commitA, 0, markerBody(1, 0)), rvBody(2, bot, "COMMENTED", commitA, 5, markerBody(0, 0))}, commitA, declared, reviewStatusSuccess, true},
		{"findings review at the old head, new head → pending (awaiting a review)", []gh.PRReview{rvBody(1, bot, "COMMENTED", commitA, 0, markerBody(2, 0))}, commitB, declared, reviewStatusPending, true},
		{"CR at head then COMMENTED with findings at head → pending", []gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitA, 0), rvBody(2, bot, "COMMENTED", commitA, 5, markerBody(1, 0))}, commitA, declared, reviewStatusPending, true},
		{"unknown head → no status", []gh.PRReview{rv(1, bot, "APPROVED", commitA, 0)}, "", declared, "", false},
		{"no declared reviewers → no status", []gh.PRReview{rv(1, bot, "APPROVED", commitA, 0)}, commitA, nil, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := reviewStatusVerdict(reviewerStandings(tc.history, tc.head, tc.declared), tc.declared, tc.head)
			if ok != tc.wantOK || got.State != tc.want {
				t.Fatalf("got (%q, ok=%v), want (%q, ok=%v)", got.State, ok, tc.want, tc.wantOK)
			}
			if ok && (got.Description == "" || len([]rune(got.Description)) > 140) {
				t.Errorf("description %q is empty or over 140 characters", got.Description)
			}
		})
	}
}

func TestReviewStatusVerdict_DescriptionNamesReviewersAndHead(t *testing.T) {
	declared := []string{"pruefer"}
	got, _ := reviewStatusVerdict(nil, declared, commitB)
	if !strings.Contains(got.Description, "pruefer") || !strings.Contains(got.Description, "bbbbbbb") {
		t.Errorf("pending description %q should name the reviewer and head", got.Description)
	}
	long := []string{strings.Repeat("a", 100), strings.Repeat("b", 100)}
	got, _ = reviewStatusVerdict(nil, long, commitB)
	if n := len([]rune(got.Description)); n > 140 {
		t.Errorf("long reviewer list description is %d runes, want <= 140", n)
	}
}

// FR-003: the status and the gate read one standing per reviewer, so they can
// never disagree about the same head. For every history below: if any declared
// reviewer's standing blocks the gate the status is never success, and a failure
// status always has a changes-requested standing behind it.
func TestReviewStatusVerdict_AgreesWithGateStandings(t *testing.T) {
	bot := "pruefer[bot]"
	declared := []string{"pruefer"}
	histories := [][]gh.PRReview{
		nil,
		{rv(1, bot, "COMMENTED", commitA, 0)},
		{rv(1, bot, "CHANGES_REQUESTED", commitA, 0)},
		{rv(1, bot, "CHANGES_REQUESTED", commitA, 0), rv(2, bot, "COMMENTED", commitB, 5)},
		{rv(1, bot, "APPROVED", commitA, 0)},
		{rv(1, bot, "APPROVED", commitA, 0), rv(2, bot, "CHANGES_REQUESTED", commitB, 3)},
		{rv(1, bot, "DISMISSED", commitA, 0)},
	}
	for _, head := range []string{commitA, commitB} {
		for i, h := range histories {
			standings := reviewerStandings(h, head, declared)
			got, ok := reviewStatusVerdict(standings, declared, head)
			if !ok {
				t.Fatalf("history %d head %s: no verdict", i, head)
			}
			blocking, anyBlocks := firstBlockingStanding(standings)
			if anyBlocks && got.State == reviewStatusSuccess {
				t.Errorf("history %d head %s: gate blocks (%v) but status is success", i, head, blocking.reason())
			}
			if got.State == reviewStatusFailure && (!anyBlocks || blocking.Kind != standingChangesRequested) {
				t.Errorf("history %d head %s: failure status without a standing change request", i, head)
			}
		}
	}
}

// The additive CommentAtHead field must not move any pre-existing output.
func TestReviewerStandings_CommentAtHeadIsAdditive(t *testing.T) {
	bot := "pruefer[bot]"
	h := []gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitA, 0), rv(2, bot, "COMMENTED", commitB, 5)}
	for _, declared := range [][]string{nil, {"pruefer"}} {
		for _, s := range reviewerStandings(h, commitB, declared) {
			if !s.CommentAtHead {
				t.Errorf("declared=%v: CommentAtHead = false, want true for a COMMENTED at head", declared)
			}
		}
	}
	for _, s := range reviewerStandings(h, commitA, nil) {
		if s.CommentAtHead {
			t.Errorf("CommentAtHead = true with the only COMMENTED on another commit")
		}
	}
}

func rvBody(id int, author, state, commit string, minutes int, body string) gh.PRReview {
	r := rv(id, author, state, commit, minutes)
	r.Body = body
	return r
}

func markerBody(inline, body int) string {
	return fmt.Sprintf("summary\n\n<!-- pruefer:findings inline=%d body=%d coverage=complete -->", inline, body)
}

// A comment-only reviewer's findings review must name the open findings, and the
// verdict must still be pending while another reviewer is outstanding.
func TestReviewStatusVerdict_FindingsDescription(t *testing.T) {
	declared := []string{"pruefer"}
	h := []gh.PRReview{rvBody(1, "pruefer[bot]", "COMMENTED", commitA, 0, markerBody(1, 0))}
	got, ok := reviewStatusVerdict(reviewerStandings(h, commitA, declared), declared, commitA)
	if !ok || got.State != reviewStatusPending || !strings.Contains(got.Description, "open findings") {
		t.Fatalf("got %+v ok=%v, want pending naming open findings", got, ok)
	}
}

// The review-wait pause comment names Outstanding, so it must list only the
// declared reviewers that keep the status pending — never one that already
// cleared the head, and nothing at all for a non-pending verdict.
func TestReviewStatusVerdict_OutstandingNamesOnlyPendingReviewers(t *testing.T) {
	standings := []reviewerStanding{{Author: "gemini", Kind: standingClean}}
	got, ok := reviewStatusVerdict(standings, []string{"gemini", "pruefer"}, commitB)
	if !ok || got.State != reviewStatusPending {
		t.Fatalf("got %+v ok=%v, want pending", got, ok)
	}
	if len(got.Outstanding) != 1 || got.Outstanding[0] != "pruefer" {
		t.Errorf("Outstanding = %v, want [pruefer]", got.Outstanding)
	}
	done, _ := reviewStatusVerdict(standings, []string{"gemini"}, commitB)
	if done.State != reviewStatusSuccess || len(done.Outstanding) != 0 {
		t.Errorf("success verdict = %+v, want no Outstanding", done)
	}
}
