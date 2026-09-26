//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// Unit tests for the pure half of the exactly-once landing assertion (#1874).
// They run under -tags e2e like the other *_test.go pure-helper tests and need no
// bed: go test -tags e2e -run 'LandedExactlyOnce|LandingComment|ParseIssueComments|CountLifecycle|CollapseRetry' ./tests/e2e/

// The three engine landing comments, copied from their format strings in
// engine/merge_train.go (:4511 landMergeTrainBatch, :2201
// finishSingletonFastPathLanding, :1994 landSingleton). If the engine wording
// drifts these fixtures must be re-copied — that is the point: the counting
// pattern is pinned to real engine text, not to a paraphrase.
const (
	landedBatchBody    = "🏭 **Fabrik merge-train** — Landed via batch PR #77."
	landedFastPathBody = "🏭 **Fabrik merge-train** — Landed via singleton fast path PR #42. That is this PR: the pinned base was already an ancestor of this head, this PR was mergeable, and its own CI was green and complete, so it was landed directly — no trial branch was assembled and no separate integration PR exists. See ADR-1644."
	landedOneByOneBody = "🏭 **Fabrik merge-train** — Landed one-at-a-time via singleton PR #55."
	postMergeReplyBody = "🏭 comment not applied: this item's work has already landed."
	ejectionNoticeBody = "🏭 **Fabrik merge-train** — ejected from the batch."
)

var landedT0 = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func cmt(body string, offset time.Duration) issueComment {
	return issueComment{Body: body, URL: "https://github.com/o/r/pull/1#issuecomment-x", CreatedAt: landedT0.Add(offset)}
}

func TestLandingCommentPattern_MatchesAllThreeEngineForms(t *testing.T) {
	for name, body := range map[string]string{
		"batch":      landedBatchBody,
		"fast path":  landedFastPathBody,
		"one-by-one": landedOneByOneBody,
	} {
		if !landingCommentPattern.MatchString(body) {
			t.Errorf("%s: counting pattern does not match engine comment %q", name, body)
		}
		// The extraction pattern must agree that these are landing comments, so
		// counting can never be narrower than what waitForLandingPRDetail sees.
		if !landedPRPattern.MatchString(body) {
			t.Errorf("%s: landedPRPattern no longer matches engine comment %q", name, body)
		}
	}
}

func TestLandingCommentPattern_IgnoresOtherComments(t *testing.T) {
	for _, body := range []string{postMergeReplyBody, ejectionNoticeBody, "", "Landed", "looks good, landed it"} {
		if landingCommentPattern.MatchString(body) {
			t.Errorf("counting pattern wrongly matched %q", body)
		}
	}
}

func TestLandedExactlyOnce_OneLandingPerForm(t *testing.T) {
	for name, body := range map[string]string{
		"batch":      landedBatchBody,
		"fast path":  landedFastPathBody,
		"one-by-one": landedOneByOneBody,
	} {
		notes, err := checkLandedExactlyOnce("m", []issueComment{cmt(ejectionNoticeBody, 0), cmt(body, time.Minute), cmt(postMergeReplyBody, 2*time.Minute)}, 1, 0)
		if err != nil {
			t.Errorf("%s: unexpected failure: %v", name, err)
		}
		if len(notes) != 0 {
			t.Errorf("%s: unexpected notes: %v", name, notes)
		}
	}
}

func TestLandedExactlyOnce_ZeroLandingsFails(t *testing.T) {
	_, err := checkLandedExactlyOnce("member alpha", []issueComment{cmt(ejectionNoticeBody, 0)}, 1, 0)
	if err == nil {
		t.Fatal("zero landing comments must fail")
	}
	for _, want := range []string{"member alpha", "found 0 landing comments", "could not post landed comment"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestLandedExactlyOnce_DoubleLandingFails(t *testing.T) {
	// The #1871 shape: a second landing comment one poll (60s) later, possibly via a
	// different path (integration PR number differs).
	second := cmt("🏭 **Fabrik merge-train** — Landed via batch PR #88.", 61*time.Second)
	second.URL = "https://github.com/o/r/pull/1#issuecomment-second"
	_, err := checkLandedExactlyOnce("member bravo", []issueComment{cmt(landedBatchBody, 0), second}, 1, 0)
	if err == nil {
		t.Fatal("two landing comments a poll apart must fail")
	}
	for _, want := range []string{"member bravo", "found 2 landing comments", "PR #77.", "PR #88.", "issuecomment-second"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestLandedExactlyOnce_MixedPathDoubleLandingFails(t *testing.T) {
	_, err := checkLandedExactlyOnce("m", []issueComment{cmt(landedFastPathBody, 0), cmt(landedOneByOneBody, 2*time.Minute)}, 1, 0)
	if err == nil {
		t.Fatal("a member landed once via fast path and again one-at-a-time must fail")
	}
}

func TestLandedExactlyOnce_RetryDuplicateIsOneLanding(t *testing.T) {
	notes, err := checkLandedExactlyOnce("m", []issueComment{cmt(landedBatchBody, 0), cmt(landedBatchBody, 700*time.Millisecond)}, 1, 0)
	if err != nil {
		t.Fatalf("a retried post within the window must not fail: %v", err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "treated as one landing") {
		t.Errorf("expected one collapse note, got %v", notes)
	}
}

func TestCollapseRetryDuplicates_Boundary(t *testing.T) {
	at := func(d time.Duration) issueComment { return cmt(landedBatchBody, d) }
	// Exactly at the window: collapsed. One nanosecond over: separate.
	if got := collapseRetryDuplicates([]issueComment{at(0), at(landingRetryWindow)}, landingRetryWindow); len(got) != 1 {
		t.Errorf("gap == window: got %d clusters, want 1", len(got))
	}
	if got := collapseRetryDuplicates([]issueComment{at(0), at(landingRetryWindow + time.Nanosecond)}, landingRetryWindow); len(got) != 2 {
		t.Errorf("gap just over window: got %d clusters, want 2", len(got))
	}
	// Input order must not matter.
	if got := collapseRetryDuplicates([]issueComment{at(time.Minute), at(0)}, landingRetryWindow); len(got) != 2 {
		t.Errorf("unsorted input: got %d clusters, want 2", len(got))
	}
	// Chained: each within the window of the previous → one cluster.
	if got := collapseRetryDuplicates([]issueComment{at(0), at(8 * time.Second), at(16 * time.Second)}, landingRetryWindow); len(got) != 1 {
		t.Errorf("chained gaps: got %d clusters, want 1", len(got))
	}
	if got := collapseRetryDuplicates(nil, landingRetryWindow); len(got) != 0 {
		t.Errorf("nil input: got %d clusters, want 0", len(got))
	}
}

func TestLandedExactlyOnce_LifecycleEvents(t *testing.T) {
	one := []issueComment{cmt(landedBatchBody, 0)}
	for _, tc := range []struct {
		name             string
		closed, reopened int
		want             string
	}{
		{"never closed", 0, 0, "0 closed events"},
		{"closed twice", 2, 0, "2 closed events"},
		{"reopened", 2, 1, "1 reopened events"},
		{"reopened once, closed once", 1, 1, "1 reopened events"},
	} {
		_, err := checkLandedExactlyOnce("m", one, tc.closed, tc.reopened)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
	}
}

func TestParseIssueComments_BothPaginateShapes(t *testing.T) {
	objects := `{"body":"a","url":"u1","created_at":"2026-09-26T12:00:00Z"}
{"body":"b","url":"u2","created_at":"2026-09-26T12:01:00Z"}
`
	arrays := `[{"body":"a","url":"u1","created_at":"2026-09-26T12:00:00Z"}]
[{"body":"b","url":"u2","created_at":"2026-09-26T12:01:00Z"}]
`
	for name, raw := range map[string]string{"objects per line": objects, "concatenated arrays": arrays} {
		got, err := parseIssueComments([]byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 2 || got[0].Body != "a" || got[1].URL != "u2" || !got[1].CreatedAt.Equal(landedT0.Add(time.Minute)) {
			t.Errorf("%s: parsed %+v", name, got)
		}
	}
	if got, err := parseIssueComments([]byte("  \n")); err != nil || len(got) != 0 {
		t.Errorf("empty output: got %v, %v", got, err)
	}
	if _, err := parseIssueComments([]byte(`{"body":`)); err == nil {
		t.Error("truncated JSON must error")
	}
}

func TestCountLifecycleEvents(t *testing.T) {
	objects := `{"event":"closed"}
{"event":"reopened"}
{"event":"closed"}
`
	arrays := `[{"event":"closed"}]
[{"event":"closed"},{"event":"reopened"}]
`
	for name, tc := range map[string]struct {
		raw              string
		closed, reopened int
	}{
		"objects": {objects, 2, 1},
		"arrays":  {arrays, 2, 1},
		"empty":   {"", 0, 0},
		"other":   {`{"event":"labeled"}`, 0, 0},
	} {
		c, r, err := countLifecycleEvents([]byte(tc.raw))
		if err != nil || c != tc.closed || r != tc.reopened {
			t.Errorf("%s: got closed=%d reopened=%d err=%v, want %d/%d", name, c, r, err, tc.closed, tc.reopened)
		}
	}
	if _, _, err := countLifecycleEvents([]byte(`[{"event"`)); err == nil {
		t.Error("truncated JSON must error")
	}
}

func TestLandingSettleWait_ScalesWithBedPollInterval(t *testing.T) {
	t.Setenv("E2E_BED_POLL_SECONDS", "")
	if got := landingSettleWait(); got != 180*time.Second {
		t.Errorf("default: got %s, want 3m", got)
	}
	t.Setenv("E2E_BED_POLL_SECONDS", "30")
	if got := landingSettleWait(); got != 90*time.Second {
		t.Errorf("30s poll: got %s, want 90s", got)
	}
}
