//go:build e2e

package e2e

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Helpers for the unprocessed-comment landing scenarios (issue #1873):
// TestCommentLandingGateHolds, TestQueuedMemberCommentEjection and
// TestPostMergeCommentNotApplied. See adrs/1873-e2e-comment-landing-coverage.md.
//
// This half of the file is pure (no GitHub, no bed): string/time parsing that
// comment_landing_helpers_test.go pins against the engine's own format strings.
// The strings have no compile-time link to the engine, so an engine-side
// rewording makes these return "not found" and the live scenario fails loudly
// instead of passing vacuously. Every string below is copied verbatim from:
//
//	engine/comment_landing_gate.go   "holding landing decision: %d unprocessed comment(s) pending — will re-evaluate once processed"  (tag comment-gate)
//	engine/merge_train.go            "#%d ejected for an unprocessed comment: rerouted to %s (not paused, no ejection counted)"       (tag merge-train)
//	engine/merge_train.go            "🏭 **Fabrik merge-train — ejected (unprocessed comment)**"  (ejectQueuedMemberForComments)
//	engine/merge_train.go            "🏭 **Fabrik merge-train — ejected**"                         (ejectMember, the COUNTED path)
//	engine/merge_train.go            "#%d ejected %d time(s) — pausing"                            (ejectMember at the cap)
//	engine/post_merge_comments.go    "🏭 **Fabrik — comment not applied**" and "work already landed — not applying %d comment(s); replying instead"  (tag post-merge)
//	engine/comments.go               "processing %d new comment(s) — stage: %s"                    (tag comments)
//
// A real log line is "<RFC3339> [#N tag] msg" (engine.logEvent), so every log
// pattern below is scoped by the "[#N tag]" prefix — one scenario's issue
// numbers never match another's.

const (
	// commentEjectCommentPrefix opens the comment ejectQueuedMemberForComments posts.
	commentEjectCommentPrefix = "🏭 **Fabrik merge-train — ejected (unprocessed comment)**"
	// countedEjectCommentPrefix opens the comment ejectMember posts. It is
	// deliberately NOT a prefix of commentEjectCommentPrefix: after "ejected"
	// this one has "**" where the comment-eject has " (unprocessed comment)**".
	countedEjectCommentPrefix = "🏭 **Fabrik merge-train — ejected**"
	// ejectionCapCommentPrefix opens the comment ejectMember posts at MaxMergeTrainEjections.
	ejectionCapCommentPrefix = "🏭 **Fabrik merge-train — pausing after"
	// postMergeReplyPrefix opens the reply postMergeCommentGuard posts.
	postMergeReplyPrefix = "🏭 **Fabrik — comment not applied**"
	// engineCommentPrefix is what findNewComments treats as Fabrik's own. In PAT
	// mode the engine posts as the same account as the harness, so a harness
	// comment beginning with this would never be seen as a human comment.
	engineCommentPrefix = "🏭 **Fabrik"
	// noCodeChangePhrase must appear in every harness comment: the comment
	// worker is told not to push, so the SHA-invalidation scan never re-runs
	// Validate and shifts the landing timeline.
	noCodeChangePhrase = "no code change"
)

var (
	commentGateHoldRE = regexp.MustCompile(`\[#(\d+) comment-gate\] holding landing decision: (\d+) unprocessed comment\(s\) pending — will re-evaluate once processed`)
	commentEjectRE    = regexp.MustCompile(`\[#(\d+) merge-train\] #(\d+) ejected for an unprocessed comment: rerouted to (.+) \(not paused, no ejection counted\)\s*$`)
	cappedEjectRE     = regexp.MustCompile(`\[#(\d+) merge-train\] #(\d+) ejected (\d+) time\(s\) — pausing`)
	postMergeLogRE    = regexp.MustCompile(`\[#(\d+) post-merge\] work already landed — not applying (\d+) comment\(s\); replying instead`)
	commentsWorkRE    = regexp.MustCompile(`\[#(\d+) comments\] processing (\d+) new comment\(s\) — stage: `)
)

// countCommentGateHolds counts the landing-gate hold lines logged for issue.
func countCommentGateHolds(lines []string, issue int) int {
	n := 0
	for _, l := range lines {
		if m := commentGateHoldRE.FindStringSubmatch(l); m != nil && m[1] == strconv.Itoa(issue) {
			n++
		}
	}
	return n
}

// parseCommentEjectLine parses the comment-eject log line, returning the issue
// number and the stage it was rerouted to. Both "#N" occurrences must agree.
func parseCommentEjectLine(line string) (issue int, target string, ok bool) {
	m := commentEjectRE.FindStringSubmatch(line)
	if m == nil || m[1] != m[2] {
		return 0, "", false
	}
	issue, _ = strconv.Atoi(m[1])
	return issue, m[3], true
}

// countCommentEjectLines counts comment-eject log lines for issue.
func countCommentEjectLines(lines []string, issue int) int {
	n := 0
	for _, l := range lines {
		if got, _, ok := parseCommentEjectLine(l); ok && got == issue {
			n++
		}
	}
	return n
}

// countCappedEjectLines counts "ejected N time(s) — pausing" lines for issue,
// the ejectMember cap path. A comment eject never logs one.
func countCappedEjectLines(lines []string, issue int) int {
	n := 0
	for _, l := range lines {
		if m := cappedEjectRE.FindStringSubmatch(l); m != nil && m[1] == strconv.Itoa(issue) && m[2] == m[1] {
			n++
		}
	}
	return n
}

// isCommentEjectComment reports whether body is the #1863 comment-eject comment.
func isCommentEjectComment(body string) bool {
	return strings.HasPrefix(body, commentEjectCommentPrefix)
}

// isCountedEjectComment reports whether body is the ejectMember (counted)
// ejection comment. It must not match the comment-eject comment.
func isCountedEjectComment(body string) bool {
	return strings.HasPrefix(body, countedEjectCommentPrefix)
}

// isEjectionCapComment reports whether body is the "pausing after N ejections" comment.
func isEjectionCapComment(body string) bool {
	return strings.HasPrefix(body, ejectionCapCommentPrefix)
}

// isPostMergeReply reports whether body is the post-merge "comment not applied" reply.
func isPostMergeReply(body string) bool {
	return strings.HasPrefix(body, postMergeReplyPrefix)
}

// countMatching counts bodies satisfying pred.
func countMatching(bodies []string, pred func(string) bool) int {
	n := 0
	for _, b := range bodies {
		if pred(b) {
			n++
		}
	}
	return n
}

// countPostMergeGuardReplies sums the comment counts of the post-merge guard's
// "not applying N comment(s)" log lines for issue.
func countPostMergeGuardReplies(lines []string, issue int) int {
	n := 0
	for _, l := range lines {
		if m := postMergeLogRE.FindStringSubmatch(l); m != nil && m[1] == strconv.Itoa(issue) {
			c, _ := strconv.Atoi(m[2])
			n += c
		}
	}
	return n
}

// countCommentWorkerLines counts "processing N new comment(s)" lines for issue —
// the log line every dispatched comment-review worker emits.
func countCommentWorkerLines(lines []string, issue int) int {
	n := 0
	for _, l := range lines {
		if m := commentsWorkRE.FindStringSubmatch(l); m != nil && m[1] == strconv.Itoa(issue) {
			n++
		}
	}
	return n
}

// reactionsOrdered reports whether a 👀 and a 🚀 both exist (non-zero) and the
// 👀 is not later than the 🚀. GitHub timestamps have 1-second resolution and
// the engine can react in the same second, so equality passes.
func reactionsOrdered(eyes, rocket time.Time) bool {
	if eyes.IsZero() || rocket.IsZero() {
		return false
	}
	return !eyes.After(rocket)
}

// landedAtOrAfter reports whether the issue closed no earlier than the 🚀 — i.e.
// the comment was processed before the item landed. Both must be set; equality
// passes (1-second resolution).
func landedAtOrAfter(rocket, closedAt time.Time) bool {
	if rocket.IsZero() || closedAt.IsZero() {
		return false
	}
	return !closedAt.Before(rocket)
}

// humanCommentBodyOK validates a harness comment body: it must not begin with
// the engine's own "🏭 **Fabrik" prefix (PAT mode: the engine posts as the same
// account, so the comment would be filtered out as Fabrik's own and never gate
// anything), and must carry "no code change" so the comment worker never pushes.
func humanCommentBodyOK(body string) error {
	if strings.HasPrefix(strings.TrimSpace(body), engineCommentPrefix) {
		return fmt.Errorf("comment body begins with %q — the engine would treat it as its own comment", engineCommentPrefix)
	}
	if !strings.Contains(strings.ToLower(body), noCodeChangePhrase) {
		return fmt.Errorf("comment body lacks %q — the comment worker might push and re-run Validate", noCodeChangePhrase)
	}
	return nil
}
