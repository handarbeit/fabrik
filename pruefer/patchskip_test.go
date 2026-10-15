package pruefer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

const (
	psBot  = "pruefer-bot[bot]"
	psPrev = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // the reviewed head
	psHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" // the new head
)

// psDiff renders a one-file diff; idx and line numbers are what a rebase moves.
func psDiff(idx string, startLine int, added string) string {
	return fmt.Sprintf("diff --git a/a.go b/a.go\nindex %s..%s 100644\n--- a/a.go\n+++ b/a.go\n@@ -%d,3 +%d,4 @@ func f() {\n ctx1\n+%s\n ctx2\n ctx3\n",
		idx[:7], idx[7:14], startLine, startLine, added)
}

func completeReview(sha string) gh.PRReview {
	return gh.PRReview{Author: psBot, CommitID: sha, State: "COMMENTED", Body: "looks fine\n\nPRUEFER_COVERAGE: complete", SubmittedAt: time.Now()}
}

type psFixture struct {
	t       *testing.T
	client  *fakeReviewer
	cfg     Config
	pr      gh.PRDetails
	tracker *ReviewTracker
}

// newPSFixture is the happy path: Pruefer completed a review of psPrev, and
// psHead is the same patch rebased onto a moved base.
func newPSFixture(t *testing.T) *psFixture {
	t.Helper()
	c := newFakeReviewer()
	c.botLogin = psBot
	c.reviews = []gh.PRReview{completeReview(psPrev)}
	c.compareDiffs = map[string]string{psPrev: psDiff("1111111aaaaaaa", 10, "added")}
	c.diff = psDiff("2222222bbbbbbb", 250, "added")
	return &psFixture{
		t: t, client: c, tracker: NewReviewTracker(),
		cfg: Config{PatchSkip: true},
		pr:  gh.PRDetails{Number: 7, Author: "alice", HeadSHA: psHead, BaseRef: "main", Title: "t"},
	}
}

func (f *psFixture) review() ReviewOutcome {
	clone, _ := fakeClone(f.t, nil)
	return ReviewPR(context.Background(), f.client, okClaudeInvoker(), clone, f.cfg, psBot, "o", "r", f.pr, f.tracker)
}

func TestReviewPR_PatchUnchanged_RebaseIsSkipped(t *testing.T) {
	f := newPSFixture(t)
	out := f.review()
	if !out.Skipped || out.Reason != SkipPatchUnchanged {
		t.Fatalf("outcome = %+v, want Skipped with SkipPatchUnchanged", out)
	}
	if !out.Conclusive {
		t.Errorf("a clean skip must be conclusive")
	}
	if f.client.submitCallCount() != 0 || len(f.client.addedBodies) != 0 {
		t.Errorf("a skip must submit and post nothing (submits=%d comments=%v)", f.client.submitCallCount(), f.client.addedBodies)
	}
}

// Non-vacuity: with the kill switch off the same scenario reviews, never
// touches the new client methods, and so matches the pre-#2230 behaviour.
func TestReviewPR_PatchUnchanged_KillSwitchRestoresReview(t *testing.T) {
	f := newPSFixture(t)
	f.cfg.PatchSkip = false
	out := f.review()
	if !out.Reviewed {
		t.Fatalf("outcome = %+v, want a review with patch_skip off", out)
	}
	if f.client.compareCallCount() != 0 || f.client.findCalls != 0 {
		t.Errorf("patch_skip off must not call the new client methods (compare=%d find=%d)", f.client.compareCallCount(), f.client.findCalls)
	}
}

func TestReviewPR_PatchChanged_IsReviewed(t *testing.T) {
	cases := map[string]string{
		// A conflict-resolving rebase: same lines touched, different content.
		"conflict resolved differently": psDiff("2222222bbbbbbb", 250, "added differently"),
		// A new commit on top: an additional hunk.
		"new commit": psDiff("2222222bbbbbbb", 250, "added") +
			"@@ -90,2 +91,3 @@\n x\n+y\n z\n",
		"whitespace only": psDiff("2222222bbbbbbb", 250, "added "),
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPSFixture(t)
			f.client.diff = d
			out := f.review()
			if !out.Reviewed {
				t.Fatalf("outcome = %+v, want a review", out)
			}
			if f.client.compareCallCount() != 1 {
				t.Errorf("compare calls = %d, want the check to have run", f.client.compareCallCount())
			}
		})
	}
}

func TestReviewPR_PatchUnchanged_ForceReviewBypasses(t *testing.T) {
	f := newPSFixture(t)
	f.client.comments = append(f.client.comments, gh.Comment{Author: "alice", Body: "/pruefer review", CreatedAt: time.Now()})
	out := f.review()
	if !out.Reviewed {
		t.Fatalf("outcome = %+v, want /pruefer review to force a review", out)
	}
	if f.client.compareCallCount() != 0 {
		t.Errorf("a forced review must not even run the check")
	}
}

func TestReviewPR_PatchUnchanged_ChainBound(t *testing.T) {
	f := newPSFixture(t)
	for i := 1; i <= maxPatchSkipChain; i++ {
		f.pr.HeadSHA = fmt.Sprintf("%040d", i)
		out := f.review()
		if !out.Skipped || out.Reason != SkipPatchUnchanged {
			t.Fatalf("skip %d: outcome = %+v", i, out)
		}
	}
	f.pr.HeadSHA = fmt.Sprintf("%040d", 99)
	if out := f.review(); !out.Reviewed {
		t.Fatalf("outcome after %d skips = %+v, want a real review", maxPatchSkipChain, out)
	}
	// The real review reset the run: the review it posted is now the anchor, and
	// the tracker holds no stale run.
	if n := f.tracker.PatchSkips("o", "r", 7, psPrev); n != 0 {
		t.Errorf("chain after a real review = %d, want 0", n)
	}
}

func TestReviewPR_PatchUnchanged_PriorReviewNotComplete_Reviews(t *testing.T) {
	bodies := map[string]string{
		"partial":   "x\n\nPRUEFER_COVERAGE: partial — 2 of 5 diff entries not opened\n- `a.go`",
		"absent":    "a review from before the coverage contract",
		"malformed": "x\n\nPRUEFER_COVERAGE: ok",
		"quoted":    "> PRUEFER_COVERAGE: complete\n\nPRUEFER_COVERAGE: partial — reviewer did not report coverage",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			f := newPSFixture(t)
			r := completeReview(psPrev)
			r.Body = body
			f.client.reviews = []gh.PRReview{r}
			out := f.review()
			if !out.Reviewed {
				t.Fatalf("outcome = %+v, want a review", out)
			}
			if f.client.compareCallCount() != 0 {
				t.Errorf("an incomplete prior review must short-circuit before any compare call")
			}
		})
	}
}

func TestReviewPR_PatchUnchanged_UnreachablePriorHead_ReviewsConclusively(t *testing.T) {
	for _, e := range []error{gh.ErrNotFound, gh.ErrUnprocessableEntity, gh.ErrDiffTooLarge} {
		f := newPSFixture(t)
		f.client.compareDiffs = nil
		f.client.compareDefaultErr = fmt.Errorf("compare: %w", e)
		out := f.review()
		if !out.Reviewed {
			t.Fatalf("%v: outcome = %+v, want a review", e, out)
		}
		if !out.Conclusive {
			t.Errorf("%v: a deterministic cannot-compare is a settled verdict, not degraded", e)
		}
	}
}

func TestReviewPR_PatchUnchanged_TransientFailure_ReviewsAndIsNotConclusive(t *testing.T) {
	f := newPSFixture(t)
	f.client.compareDiffs = nil
	f.client.compareDefaultErr = errors.New("502 bad gateway")
	out := f.review()
	if !out.Reviewed {
		t.Fatalf("outcome = %+v, want a review", out)
	}
	if out.Conclusive {
		t.Errorf("a tolerated transient read failure must not be memoised as conclusive")
	}
}

func TestReviewPR_PatchUnchanged_NotComparableDiffs_Review(t *testing.T) {
	bin := "diff --git a/x.png b/x.png\nindex 1..2 100644\nBinary files a/x.png and b/x.png differ\n"
	f := newPSFixture(t)
	f.client.compareDiffs = map[string]string{psPrev: bin}
	f.client.diff = bin
	if out := f.review(); !out.Reviewed {
		t.Fatalf("binary: outcome = %+v, want a review (identical binary diff text proves nothing)", out)
	}
	f = newPSFixture(t)
	f.client.compareDiffs = map[string]string{psPrev: ""}
	if out := f.review(); !out.Reviewed {
		t.Fatalf("empty prior diff: outcome = %+v, want a review", out)
	}
}

func TestReviewPR_PatchUnchanged_ChangeInExcludedPathStillCounts(t *testing.T) {
	vendor := func(idx, added string) string {
		return strings.NewReplacer("a/a.go", "a/vendor/x.go", "b/a.go", "b/vendor/x.go").Replace(psDiff(idx, 5, added))
	}
	f := newPSFixture(t)
	f.cfg.ExcludedPaths = []string{"vendor/**"}
	good := psDiff("3333333ccccccc", 10, "added")
	f.client.compareDiffs = map[string]string{psPrev: good + vendor("1111111aaaaaaa", "v1")}
	f.client.diff = good + vendor("2222222bbbbbbb", "v2")
	if out := f.review(); !out.Reviewed {
		t.Fatalf("outcome = %+v: a change confined to an excluded path is still a change", out)
	}
}

func TestReviewTracker_PatchChain(t *testing.T) {
	var nilT *ReviewTracker
	if nilT.PatchSkips("o", "r", 1, "x") != 0 || nilT.NotePatchSkip("o", "r", 1, "x") != 1 {
		t.Fatal("nil tracker must be inert")
	}
	tr := NewReviewTracker()
	if got := tr.NotePatchSkip("O", "R", 1, "x"); got != 1 {
		t.Fatalf("first skip = %d", got)
	}
	if got := tr.NotePatchSkip("o", "r", 1, "x"); got != 2 {
		t.Fatalf("second skip (case-insensitive repo) = %d", got)
	}
	if tr.PatchSkips("o", "r", 1, "other") != 0 {
		t.Error("a run over a different reviewed head must read as 0")
	}
	if got := tr.NotePatchSkip("o", "r", 1, "other"); got != 1 {
		t.Errorf("a new reviewed head restarts the run, got %d", got)
	}
	tr.Record("o", "r", 1, "newhead")
	if tr.PatchSkips("o", "r", 1, "other") != 0 {
		t.Error("Record must reset the run")
	}
}

func TestReviewBodyComplete(t *testing.T) {
	cases := map[string]bool{
		"x\n\nPRUEFER_COVERAGE: complete":                           true,
		"x\r\n\r\nPRUEFER_COVERAGE: complete\r\n":                   true,
		"PRUEFER_COVERAGE: partial — z":                             false,
		"PRUEFER_COVERAGE: complete\n\nPRUEFER_COVERAGE: partial —": false,
		"> PRUEFER_COVERAGE: complete":                              false,
		"":                                                          false,
		"PRUEFER_COVERAGE: completely":                              false,
	}
	for body, want := range cases {
		if got := reviewBodyComplete(body); got != want {
			t.Errorf("reviewBodyComplete(%q) = %t, want %t", body, got, want)
		}
	}
}
