//go:build e2e

package e2e

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tests/e2e/seedspec"
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
	// The landing hold for a pending comment. Since #1953 the feedback gate
	// (engine/feedback_gate.go, tag feedback-gate) runs ahead of the #1862
	// comment gate in attemptMergeOnValidate and holds first, logging
	// "holding landing decision — N unprocessed comment(s) (<ids>)"; the legacy
	// comment-gate line is still accepted.
	commentGateHoldRE = regexp.MustCompile(`\[#(\d+) (?:comment-gate\] holding landing decision: (\d+) unprocessed comment\(s\) pending — will re-evaluate once processed|feedback-gate\] holding landing decision — (\d+) unprocessed comment\(s\) )`)
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

// ---------------------------------------------------------------------------
// Harness-touching helpers (GitHub REST via gh; no engine knowledge).
// ---------------------------------------------------------------------------

// postHumanIssueComment posts body on the issue (or PR) thread as FABRIK_TOKEN's
// account and returns the new comment's database ID, which CommentOnIssue does
// not surface. arbeithand classifies as human to Fabrik in BOTH auth legs:
// IsBotLogin("arbeithand") is false, and findNewComments excludes only
// "🏭 **Fabrik"-prefixed bodies — which humanCommentBodyOK refuses to post.
// (PAT leg: Fabrik posts as the same account, so that prefix is the only thing
// distinguishing its comments. App leg: Fabrik posts as <slug>[bot].)
func postHumanIssueComment(t *testing.T, env *Env, repo string, number int, body string) int64 {
	t.Helper()
	if err := humanCommentBodyOK(body); err != nil {
		t.Fatalf("refusing to post harness comment on %s#%d: %v", repo, number, err)
	}
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api", "--method", "POST",
		fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, name, number),
		"-f", "body="+body, "--jq", ".id")
	if err != nil {
		t.Fatalf("post comment on %s#%d: %v\n%s", repo, number, err, out)
	}
	id, perr := strconv.ParseInt(lastNonEmpty(out), 10, 64)
	if perr != nil || id == 0 {
		t.Fatalf("could not parse comment id from %q: %v", out, perr)
	}
	return id
}

// commentExistsViaREST reports whether the comment is readable through the REST
// API. Verifying this before the item is exposed to the engine closes the
// read-after-write gap: the engine's first evaluation must see the comment.
func commentExistsViaREST(env *Env, repo string, commentID int64) bool {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return false
	}
	out, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/issues/comments/%d", owner, name, commentID), "--jq", ".id")
	return err == nil && strings.TrimSpace(out) == strconv.FormatInt(commentID, 10)
}

// commentReactionTimes returns the earliest 👀 and 🚀 created_at on the comment
// (zero time when absent). Existence and timestamps only: the reacting login
// differs by auth leg (arbeithand under PAT, <slug>[bot] under App), so no
// assertion may key on it.
func commentReactionTimes(env *Env, repo string, commentID int64) (eyes, rocket time.Time, err error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return eyes, rocket, fmt.Errorf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api", "--paginate",
		fmt.Sprintf("repos/%s/%s/issues/comments/%d/reactions", owner, name, commentID),
		"--jq", `.[] | "\(.content) \(.created_at)"`)
	if err != nil {
		return eyes, rocket, fmt.Errorf("read reactions: %w\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		ts, perr := time.Parse(time.RFC3339, f[1])
		if perr != nil {
			return eyes, rocket, fmt.Errorf("parse reaction time %q: %w", f[1], perr)
		}
		switch f[0] {
		case "eyes":
			if eyes.IsZero() || ts.Before(eyes) {
				eyes = ts
			}
		case "rocket":
			if rocket.IsZero() || ts.Before(rocket) {
				rocket = ts
			}
		}
	}
	return eyes, rocket, nil
}

// restTime reads a jq-selected RFC3339 field from a REST path; zero when null/empty.
func restTime(env *Env, path, jq string) (time.Time, error) {
	out, err := ghOutput(env, "api", path, "--jq", jq)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %w\n%s", path, err, out)
	}
	s := strings.TrimSpace(out)
	if s == "" || s == "null" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

// issueClosedAt returns the issue's closed_at (zero while open).
func issueClosedAt(env *Env, repo string, number int) (time.Time, error) {
	return restTime(env, fmt.Sprintf("repos/%s/issues/%d", repo, number), ".closed_at")
}

// prMergedAt returns the PR's merged_at (zero while unmerged).
func prMergedAt(env *Env, repo string, prNumber int) (time.Time, error) {
	return restTime(env, fmt.Sprintf("repos/%s/pulls/%d", repo, prNumber), ".merged_at")
}

// seedLandingCandidate files an issue carrying extraLabels, adds it to the
// project WITHOUT a Status, opens a non-draft member PR on fabrik/issue-<N>,
// confirms the engine can resolve it, and applies stage:Validate:complete (a thin
// wrapper over seedAtStage, #1992, with the minimal labelling it always had). The
// caller decides when to expose the item to the engine (SetIssueStatus): with no
// Status the engine cannot act on it, so a comment posted first is deterministically
// present at the engine's first landing evaluation — no timing luck.
func seedLandingCandidate(t *testing.T, env *Env, repo, baseBranch, marker, path string, extraLabels ...string) (issueNum, prNum int, itemID string) {
	t.Helper()
	stamp := time.Now().UTC().Format("150405.000")
	return seedAtStage(t, env, repo, seedspec.Spec{
		Column:      "Validate",
		BaseBranch:  baseBranch,
		Title:       fmt.Sprintf("e2e comment-landing %s (%s)", marker, stamp),
		IssueBody:   fmt.Sprintf("e2e unprocessed-comment landing scenario. marker=%s", marker),
		Path:        path,
		PathMode:    seedspec.PathUnique,
		Content:     fmt.Sprintf("# e2e comment-landing marker\n\nmarker=%s\n", marker),
		ExtraLabels: extraLabels,
		Minimal:     true,
		DeferStatus: true,
	})
}

// waitForCommentRocket polls until the comment carries a 🚀, returning both
// reaction times. held is called once per poll while the rocket is still absent
// (its argument is the eyes time, zero if none yet) so a scenario can assert the
// item is still held the whole time the comment is unprocessed.
func waitForCommentRocket(t *testing.T, env *Env, repo string, commentID int64, timeout time.Duration, held func()) (eyes, rocket time.Time) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		e, r, err := commentReactionTimes(env, repo, commentID)
		if err != nil {
			t.Logf("waitForCommentRocket: transient error (will retry): %v", err)
		} else if !r.IsZero() {
			return e, r
		} else if held != nil {
			held()
		}
		time.Sleep(20 * time.Second)
	}
	t.Fatalf("timed out after %s waiting for a 🚀 on comment %d (%s)", timeout, commentID, repo)
	return
}
