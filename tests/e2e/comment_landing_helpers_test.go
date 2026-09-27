//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// Pure-function tests for the unprocessed-comment landing scenarios' parsers.
// Fixtures are the engine's own format strings with the "<RFC3339> [#N tag]"
// prefix a real log line carries (engine.logEvent), so an engine-side
// rewording is caught here as well as by the live scenarios.

func TestCommentLandingGateHoldCount(t *testing.T) {
	lines := []string{
		`2026-09-26T21:50:00Z [#42 comment-gate] holding landing decision: 1 unprocessed comment(s) pending — will re-evaluate once processed`,
		`2026-09-26T21:51:00Z [#42 comment-gate] holding landing decision: 1 unprocessed comment(s) pending — will re-evaluate once processed`,
		`2026-09-26T21:51:00Z [#43 comment-gate] holding landing decision: 2 unprocessed comment(s) pending — will re-evaluate once processed`,
		`2026-09-26T21:51:00Z [#42 advance] skipping stage "Review" — 1 unprocessed comment(s) pending`,
		`2026-09-26T21:51:00Z [#142 comment-gate] holding landing decision: 1 unprocessed comment(s) pending — will re-evaluate once processed`,
	}
	if got := countCommentGateHolds(lines, 42); got != 2 {
		t.Errorf("issue 42: got %d holds, want 2", got)
	}
	if got := countCommentGateHolds(lines, 43); got != 1 {
		t.Errorf("issue 43: got %d holds, want 1", got)
	}
	if got := countCommentGateHolds(lines, 4); got != 0 {
		t.Errorf("issue 4 must not match #42/#142: got %d", got)
	}
}

func TestCommentLandingParseEjectLine(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		issue  int
		target string
		ok     bool
	}{
		{
			name:   "real line",
			line:   `2026-09-26T21:50:00Z [#77 merge-train] #77 ejected for an unprocessed comment: rerouted to Validate (not paused, no ejection counted)`,
			issue:  77,
			target: "Validate",
			ok:     true,
		},
		{
			name:   "multi-word stage name",
			line:   `2026-09-26T21:50:00Z [#77 merge-train] #77 ejected for an unprocessed comment: rerouted to Code Review (not paused, no ejection counted)`,
			issue:  77,
			target: "Code Review",
			ok:     true,
		},
		{
			name: "mismatched issue numbers",
			line: `2026-09-26T21:50:00Z [#77 merge-train] #78 ejected for an unprocessed comment: rerouted to Validate (not paused, no ejection counted)`,
		},
		{
			name: "counted ejection cap line",
			line: `2026-09-26T21:50:00Z [#77 merge-train] #77 ejected 3 time(s) — pausing`,
		},
		{
			name: "reroute line preceding it",
			line: `2026-09-26T21:50:00Z [#77 merge-train] rerouted off Queued to Validate`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			issue, target, ok := parseCommentEjectLine(tc.line)
			if ok != tc.ok || issue != tc.issue || target != tc.target {
				t.Errorf("got (%d, %q, %v), want (%d, %q, %v)", issue, target, ok, tc.issue, tc.target, tc.ok)
			}
		})
	}
	lines := []string{
		`2026-09-26T21:50:00Z [#77 merge-train] #77 ejected for an unprocessed comment: rerouted to Validate (not paused, no ejection counted)`,
		`2026-09-26T21:50:00Z [#78 merge-train] #78 ejected for an unprocessed comment: rerouted to Validate (not paused, no ejection counted)`,
	}
	if got := countCommentEjectLines(lines, 77); got != 1 {
		t.Errorf("countCommentEjectLines(77) = %d, want 1", got)
	}
}

func TestCommentLandingCappedEjectLines(t *testing.T) {
	lines := []string{
		`2026-09-26T21:50:00Z [#77 merge-train] #77 ejected 3 time(s) — pausing`,
		`2026-09-26T21:50:00Z [#78 merge-train] #78 ejected 3 time(s) — pausing`,
		`2026-09-26T21:50:00Z [#77 merge-train] #77 ejected for an unprocessed comment: rerouted to Validate (not paused, no ejection counted)`,
	}
	if got := countCappedEjectLines(lines, 77); got != 1 {
		t.Errorf("got %d, want 1 (the comment-eject line must not count)", got)
	}
}

func TestCommentLandingEjectCommentClassification(t *testing.T) {
	commentEject := "🏭 **Fabrik merge-train — ejected (unprocessed comment)**\n\n#5 left the merge-train queue because an unprocessed comment arrived while it was Queued."
	counted := "🏭 **Fabrik merge-train — ejected**\n\n#5: something conflicted"
	cap := "🏭 **Fabrik merge-train — pausing after 3 ejections**\n\nexplanation"

	if !isCommentEjectComment(commentEject) || isCommentEjectComment(counted) || isCommentEjectComment(cap) {
		t.Error("isCommentEjectComment misclassified")
	}
	// The negative case that matters: the counted-ejection detector must NOT
	// match the comment-eject comment, or A4 would fail on the very comment A3
	// requires.
	if isCountedEjectComment(commentEject) {
		t.Error("isCountedEjectComment matched the comment-eject comment")
	}
	if !isCountedEjectComment(counted) {
		t.Error("isCountedEjectComment did not match the counted ejection comment")
	}
	if isCountedEjectComment(cap) || !isEjectionCapComment(cap) || isEjectionCapComment(counted) {
		t.Error("cap comment misclassified")
	}
}

func TestCommentLandingPostMergeClassification(t *testing.T) {
	reply := "🏭 **Fabrik — comment not applied**\n\nPR #9 already landed, so your comment arrived after the work was merged.\n\n<!-- fabrik:post-merge-comment:IC_x -->"
	if !isPostMergeReply(reply) {
		t.Error("reply not recognised")
	}
	if isPostMergeReply("🏭 **Fabrik — stage: Implement**\n\nx") || isPostMergeReply("plain human comment") {
		t.Error("non-reply matched")
	}
	if got := countMatching([]string{reply, "x", reply}, isPostMergeReply); got != 2 {
		t.Errorf("countMatching = %d, want 2", got)
	}

	lines := []string{
		`2026-09-26T21:50:00Z [#9 post-merge] work already landed — not applying 1 comment(s); replying instead`,
		`2026-09-26T21:50:30Z [#9 post-merge] work already landed — comment processing skipped, nothing new to answer`,
		`2026-09-26T21:50:00Z [#19 post-merge] work already landed — not applying 4 comment(s); replying instead`,
	}
	if got := countPostMergeGuardReplies(lines, 9); got != 1 {
		t.Errorf("countPostMergeGuardReplies(9) = %d, want 1", got)
	}

	work := []string{
		`2026-09-26T21:50:00Z [#9 comments] processing 1 new comment(s) — stage: Implement`,
		`2026-09-26T21:50:00Z [#90 comments] processing 1 new comment(s) — stage: Implement`,
		`2026-09-26T21:50:00Z [#9 comments] all candidate comments were bot service notices; skipping`,
	}
	if got := countCommentWorkerLines(work, 9); got != 1 {
		t.Errorf("countCommentWorkerLines(9) = %d, want 1", got)
	}
}

func TestCommentLandingReactionOrdering(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 21, 50, 0, 0, time.UTC)
	if !reactionsOrdered(t0, t0.Add(30*time.Second)) {
		t.Error("eyes before rocket should pass")
	}
	if !reactionsOrdered(t0, t0) {
		t.Error("same-second reactions must pass (1s resolution)")
	}
	if reactionsOrdered(t0.Add(time.Second), t0) {
		t.Error("rocket before eyes must fail")
	}
	if reactionsOrdered(time.Time{}, t0) || reactionsOrdered(t0, time.Time{}) {
		t.Error("a missing reaction must fail")
	}
}

func TestCommentLandingLandedAtOrAfter(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 21, 50, 0, 0, time.UTC)
	if !landedAtOrAfter(t0, t0.Add(time.Minute)) || !landedAtOrAfter(t0, t0) {
		t.Error("closing at/after the rocket should pass")
	}
	if landedAtOrAfter(t0, t0.Add(-time.Second)) {
		t.Error("closing before the rocket must fail")
	}
	if landedAtOrAfter(time.Time{}, t0) || landedAtOrAfter(t0, time.Time{}) {
		t.Error("a missing timestamp must fail")
	}
}

func TestCommentLandingHumanBodyOK(t *testing.T) {
	if err := humanCommentBodyOK("Please double-check the wording. No code change is needed — acknowledge only."); err != nil {
		t.Errorf("valid body rejected: %v", err)
	}
	if err := humanCommentBodyOK("🏭 **Fabrik — stage: X** no code change"); err == nil {
		t.Error("engine-prefixed body accepted")
	}
	if err := humanCommentBodyOK("  🏭 **Fabrik merge-train — ejected** no code change"); err == nil ||
		!strings.Contains(err.Error(), "engine") {
		t.Errorf("engine-prefixed body (leading space) accepted: %v", err)
	}
	if err := humanCommentBodyOK("please change the function"); err == nil {
		t.Error("body without the no-code-change phrase accepted")
	}
}
