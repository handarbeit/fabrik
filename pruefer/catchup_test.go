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
	cuBot    = "pruefer-bot[bot]"
	cuEngine = "fabrik-dev[bot]"
)

// 40-hex SHAs: the previous (reviewed) head, the catch-up head and the base
// commit that was merged in.
var (
	cuPrev = strings.Repeat("1", 40)
	cuHead = strings.Repeat("2", 40)
	cuBase = strings.Repeat("3", 40)
)

func cuMarker(head, base string, pure bool) string {
	return fmt.Sprintf("<!-- fabrik:train-catch-up head=%s base=%s pure=%t -->", head, base, pure)
}

type cuFixture struct {
	t      *testing.T
	client *fakeReviewer
	cfg    Config
	pr     gh.PRDetails
}

// newCUFixture returns the happy path: a pure catch-up with a valid engine
// marker for the live head, whose first parent Pruefer reviewed.
func newCUFixture(t *testing.T) *cuFixture {
	t.Helper()
	oldDelays, oldSleep := catchUpRecheckDelays, catchUpSleep
	catchUpRecheckDelays = []time.Duration{0, 0, 0}
	catchUpSleep = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }
	t.Cleanup(func() { catchUpRecheckDelays, catchUpSleep = oldDelays, oldSleep })

	c := newFakeReviewer()
	c.botLogin = cuBot
	c.reviews = []gh.PRReview{{Author: cuBot, CommitID: cuPrev, State: "COMMENTED"}}
	c.commitParents = map[string][]string{cuHead: {cuPrev, cuBase}}
	c.comments = []gh.Comment{{Author: cuEngine, Body: cuMarker(cuHead, cuBase, true)}}
	return &cuFixture{
		t:      t,
		client: c,
		cfg:    Config{CatchUpMarkerAuthors: []string{cuEngine}},
		pr:     gh.PRDetails{Number: 7, Author: "alice", HeadSHA: cuHead, BaseRef: "main", Title: "t"},
	}
}

func (f *cuFixture) review() ReviewOutcome {
	clone, _ := fakeClone(f.t, nil)
	return ReviewPR(context.Background(), f.client, okClaudeInvoker(), clone, f.cfg, cuBot, "o", "r", f.pr, nil)
}

func okClaudeInvoker() *mockClaudeInvoker {
	return &mockClaudeInvoker{fn: func(ReviewRequest) (ReviewResult, error) { return ReviewResult{Text: "ok"}, nil }}
}

func TestReviewPR_PureCatchUp_Skipped(t *testing.T) {
	f := newCUFixture(t)
	out := f.review()
	if !out.Skipped || out.Reason != SkipCatchUp {
		t.Fatalf("outcome = %+v, want Skipped with SkipCatchUp", out)
	}
	if !out.Conclusive {
		t.Errorf("a clean skip must be conclusive (memoisable)")
	}
	if f.client.diffCallCount() != 0 || f.client.submitCallCount() != 0 {
		t.Errorf("a skip must do no diff fetch (%d) and submit nothing (%d)", f.client.diffCallCount(), f.client.submitCallCount())
	}
	if len(f.client.addedBodies) != 0 {
		t.Errorf("a skip must post no comment, posted %v", f.client.addedBodies)
	}
}

// Neutralisation: the identical scenario with the skip switched off (the
// setting unset) reviews, so TestReviewPR_PureCatchUp_Skipped depends on the
// skip branch and not on some incidental ineligibility.
func TestReviewPR_PureCatchUp_SettingUnset_ReviewsAndMakesNoExtraCalls(t *testing.T) {
	f := newCUFixture(t)
	f.cfg.CatchUpMarkerAuthors = nil
	out := f.review()
	if !out.Reviewed {
		t.Fatalf("outcome = %+v, want a review with the setting unset", out)
	}
	if f.client.parentCalls != 0 || f.client.behindCalls != 0 {
		t.Errorf("unset setting must make no catch-up API calls, got parents=%d behind=%d", f.client.parentCalls, f.client.behindCalls)
	}
}

func TestReviewPR_CatchUpForceReviewOverridesSkip(t *testing.T) {
	f := newCUFixture(t)
	f.client.comments = append(f.client.comments, gh.Comment{Author: "alice", Body: "/pruefer review", CreatedAt: time.Now()})
	out := f.review()
	if !out.Reviewed {
		t.Fatalf("outcome = %+v, want /pruefer review to force a review", out)
	}
}

func TestCatchUpSkip_ReviewsWhenUnverified(t *testing.T) {
	otherSHA := strings.Repeat("4", 40)
	cases := []struct {
		name         string
		mutate       func(f *cuFixture)
		wantDegraded bool
	}{
		{"conflict-resolution catch-up (pure=false)", func(f *cuFixture) {
			f.client.comments = []gh.Comment{{Author: cuEngine, Body: cuMarker(cuHead, cuBase, false)}}
		}, false},
		{"trailer but no marker", func(f *cuFixture) {
			f.client.comments = []gh.Comment{{Author: cuEngine, Body: "Fabrik-Train-Catch-Up: " + cuBase}}
		}, false},
		{"marker for a different head", func(f *cuFixture) {
			f.client.comments = []gh.Comment{{Author: cuEngine, Body: cuMarker(otherSHA, cuBase, true)}}
		}, false},
		{"marker authored by another login", func(f *cuFixture) {
			f.client.comments = []gh.Comment{{Author: "mallory", Body: cuMarker(cuHead, cuBase, true)}}
		}, false},
		{"pure=false marker beats a pure=true one", func(f *cuFixture) {
			f.client.comments = append(f.client.comments, gh.Comment{Author: cuEngine, Body: cuMarker(cuHead, cuBase, false)})
		}, false},
		{"markers disagree on base", func(f *cuFixture) {
			f.client.comments = append(f.client.comments, gh.Comment{Author: cuEngine, Body: cuMarker(cuHead, otherSHA, true)})
		}, false},
		{"abbreviated SHAs in the marker", func(f *cuFixture) {
			f.client.comments = []gh.Comment{{Author: cuEngine, Body: cuMarker(cuHead[:10], cuBase[:10], true)}}
		}, false},
		{"marker base is not the second parent", func(f *cuFixture) {
			f.client.comments = []gh.Comment{{Author: cuEngine, Body: cuMarker(cuHead, otherSHA, true)}}
		}, false},
		{"parents swapped", func(f *cuFixture) { f.client.commitParents[cuHead] = []string{cuBase, cuPrev} }, false},
		{"single parent (ordinary push)", func(f *cuFixture) { f.client.commitParents[cuHead] = []string{cuPrev} }, false},
		{"octopus merge (three parents)", func(f *cuFixture) { f.client.commitParents[cuHead] = []string{cuPrev, cuBase, otherSHA} }, false},
		{"second parent not on the base branch", func(f *cuFixture) { f.client.behindBy = map[string]int{cuBase: 3} }, false},
		{"previous head never reviewed", func(f *cuFixture) {
			f.client.reviews = []gh.PRReview{{Author: cuBot, CommitID: otherSHA, State: "COMMENTED"}}
		}, false},
		{"PR never reviewed at all", func(f *cuFixture) { f.client.reviews = nil }, false},
		{"previous head reviewed by someone else", func(f *cuFixture) {
			f.client.reviews = []gh.PRReview{{Author: "human", CommitID: cuPrev, State: "APPROVED"}, {Author: cuBot, CommitID: otherSHA}}
		}, false},
		{"comment read error", func(f *cuFixture) { f.client.fetchErr = errors.New("boom") }, true},
		{"parents read error", func(f *cuFixture) { f.client.parentsErr = errors.New("boom") }, true},
		{"base-ancestry read error", func(f *cuFixture) { f.client.behindErr = errors.New("boom") }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCUFixture(t)
			tc.mutate(f)
			out := f.review()
			if !out.Reviewed {
				t.Fatalf("outcome = %+v, want a review (fail toward reviewing)", out)
			}
			if tc.wantDegraded && out.Conclusive {
				t.Errorf("a review caused by a failed read must not be conclusive (memoisable)")
			}
		})
	}
}

func TestCatchUpSkip_ChainOfConsecutiveCatchUps(t *testing.T) {
	// reviewed <- m1 <- m2 <- m3 (each a clean catch-up of a distinct base)
	m1, m2, m3 := strings.Repeat("a", 40), strings.Repeat("b", 40), cuHead
	b1, b2, b3 := strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 40)
	f := newCUFixture(t)
	f.client.commitParents = map[string][]string{
		m1: {cuPrev, b1},
		m2: {m1, b2},
		m3: {m2, b3},
	}
	f.client.comments = []gh.Comment{
		{Author: cuEngine, Body: cuMarker(m1, b1, true)},
		{Author: cuEngine, Body: cuMarker(m2, b2, true)},
		{Author: cuEngine, Body: cuMarker(m3, b3, true)},
	}
	if out := f.review(); !out.Skipped || out.Reason != SkipCatchUp {
		t.Fatalf("three consecutive pure catch-ups over a reviewed head: outcome = %+v, want skip", out)
	}

	// An unmarked link in the chain breaks it.
	f = newCUFixture(t)
	f.client.commitParents = map[string][]string{m1: {cuPrev, b1}, m2: {m1, b2}, m3: {m2, b3}}
	f.client.comments = []gh.Comment{
		{Author: cuEngine, Body: cuMarker(m1, b1, true)},
		{Author: cuEngine, Body: cuMarker(m3, b3, true)}, // m2 has no marker
	}
	if out := f.review(); !out.Reviewed {
		t.Fatalf("chain with an unmarked link: outcome = %+v, want review", out)
	}

	// A chain longer than maxCatchUpChain is reviewed.
	f = newCUFixture(t)
	f.client.commitParents = map[string][]string{}
	f.client.comments = nil
	prev := cuPrev
	var heads []string
	for i := 0; i < maxCatchUpChain+1; i++ {
		h := strings.Repeat(string(rune('a'+i)), 40)
		b := strings.Repeat(string(rune('k'+i)), 40)
		f.client.commitParents[h] = []string{prev, b}
		f.client.comments = append(f.client.comments, gh.Comment{Author: cuEngine, Body: cuMarker(h, b, true)})
		prev = h
		heads = append(heads, h)
	}
	f.pr.HeadSHA = heads[len(heads)-1]
	if out := f.review(); !out.Reviewed {
		t.Fatalf("chain deeper than the cap: outcome = %+v, want review", out)
	}
}

func TestCatchUpSkip_MarkerArrivesDuringBoundedRecheck(t *testing.T) {
	f := newCUFixture(t)
	marker := f.client.comments
	f.client.comments = nil
	sleeps := 0
	catchUpSleep = func(ctx context.Context, d time.Duration) bool {
		sleeps++
		if sleeps == 2 { // the marker lands before the second re-read
			f.client.fakeCommenter.mu.Lock()
			f.client.comments = marker
			f.client.fakeCommenter.mu.Unlock()
		}
		return true
	}
	if out := f.review(); !out.Skipped || out.Reason != SkipCatchUp {
		t.Fatalf("marker appearing on re-check: outcome = %+v, want skip", out)
	}
	if sleeps != 2 {
		t.Errorf("slept %d times, want 2 (stop re-checking once the marker is found)", sleeps)
	}
}

func TestCatchUpSkip_MarkerNeverArrives_ReviewsWithinBound(t *testing.T) {
	f := newCUFixture(t)
	f.client.comments = nil
	sleeps := 0
	catchUpSleep = func(ctx context.Context, d time.Duration) bool { sleeps++; return true }
	if out := f.review(); !out.Reviewed {
		t.Fatalf("outcome = %+v, want review when the marker never arrives", out)
	}
	if sleeps != len(catchUpRecheckDelays) {
		t.Errorf("slept %d times, want exactly the bound %d", sleeps, len(catchUpRecheckDelays))
	}
}

func TestCatchUpSkip_RecheckHonoursContextCancel(t *testing.T) {
	f := newCUFixture(t)
	f.client.comments = nil
	ctx, cancel := context.WithCancel(context.Background())
	catchUpSleep = func(ctx context.Context, d time.Duration) bool { cancel(); return false }
	v := catchUpSkip(ctx, f.client, f.cfg, cuBot, "o", "r", f.pr, f.client.reviews)
	if v.Skip || !v.Degraded {
		t.Fatalf("verdict = %+v, want no skip and degraded after cancellation", v)
	}
}

func TestCatchUpSkip_NoRecheckForOrdinaryPushOrOffBaseMerge(t *testing.T) {
	f := newCUFixture(t)
	f.client.comments = nil
	f.client.commitParents[cuHead] = []string{cuPrev} // ordinary push
	sleeps := 0
	catchUpSleep = func(ctx context.Context, d time.Duration) bool { sleeps++; return true }
	if out := f.review(); !out.Reviewed || sleeps != 0 {
		t.Fatalf("ordinary push: outcome=%+v sleeps=%d, want review with no wait", out, sleeps)
	}

	f = newCUFixture(t)
	f.client.comments = nil
	f.client.behindBy = map[string]int{cuBase: 2} // merge of a non-base commit
	catchUpSleep = func(ctx context.Context, d time.Duration) bool { sleeps++; return true }
	if out := f.review(); !out.Reviewed || sleeps != 0 {
		t.Fatalf("off-base merge: outcome=%+v sleeps=%d, want review with no wait", out, sleeps)
	}
}

func TestParseCatchUpMarker(t *testing.T) {
	got := ParseCatchUpMarker("x " + cuMarker(cuHead, cuBase, true) + " y")
	if len(got) != 1 || got[0] != (CatchUpMarker{Head: cuHead, Base: cuBase, Pure: true}) {
		t.Fatalf("got %+v", got)
	}
	if got := ParseCatchUpMarker("<!-- fabrik:train-catch-up head=zz base=yy pure=true -->"); len(got) != 0 {
		t.Errorf("malformed marker parsed: %+v", got)
	}
}
