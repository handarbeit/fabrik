//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// App self-recognition scenarios (#1877, guarding #1754; ADR-1877).
//
// Under GitHub App auth the engine's identity is "<slug>[bot]", and every "is
// this comment/review mine?" comparison goes through engine.selfLogin(). These
// scenarios run only in the App auth leg (requireAppLeg skips otherwise) and
// assert, from GitHub state and the bed log, that Fabrik treats its own [bot]
// comments as its own and never as human input (the #1083-class runaway).
//
// Wire-shape caveat (Research, unverified live): REST reports the bot author as
// "fabrik-bed[bot]" but the GraphQL comment fragment (`author { login }`, no
// __typename) is believed to yield the bare "fabrik-bed". Under App auth the
// engine reads issue comments through GraphQL, so A1 and A2 may be red on a
// live App leg until the engine normalises Bot-typed comment authors at
// ingestion. A3 reads REST and is not exposed to that. Every failure message
// below points at the logged author shapes so the cause is attributable. The
// scenarios are NOT weakened or skipped for this: they assert the correct
// behaviour.
//
// Isolation: each scenario uses its own issues on RepoAlpha, so all three are
// t.Parallel(). Every log scan is scoped to this scenario's own issue numbers
// (log lines carry no repo) and starts at a LogOffset taken before the first
// action.

// TestAppSelfRecognitionBotCommentNeverResumes (A1): a plain, marker-free
// comment authored by the App bot never resumes a paused item — and a human
// comment does (positive control).
//
// Non-vacuity, honestly: pre-#1754 filterHuman contained no cfg.User comparison,
// so this does NOT discriminate the #1754 delta. The pre-fix defect was the
// cache write-through stamping Author: cfg.User on Fabrik's own posts, a
// transient human classification that a refetch corrects and that is not
// live-observable. A1 guards filterHuman/gh.IsBotLogin on the real wire shape
// and the ADR-1813 resume path against regression. Its failing evidence is a
// resume line ("[#N unpause]"/"[#N unblock]", engine/item.go), a
// missing "none human-authored" skip line, a dropped pause label, or a 👀/🚀 on
// the bot's comment.
//
// Admission of a paused item is event-driven (a new comment bumps updatedAt), so
// only the first evaluation is guaranteed to log. The scenario therefore waits
// for that evaluation line FIRST — proof the engine looked at the comment — and
// only then holds for three poll intervals asserting nothing changed. A bare
// "nothing happened for 3 polls" would be vacuous.
//
// Cost: one comment-processing Claude invocation (the positive control).
// Wall-clock: ~6-10 min.
func TestAppSelfRecognitionBotCommentNeverResumes(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	leg := requireAppLeg(t, env)
	humanLogin := assertHarnessAccountIsHuman(t, env)
	repo := env.RepoAlpha

	ensurePausedLabelExists(t, env, repo)
	ensureEngineLabelExists(t, env, repo, awaitingInputLabel)

	off := LogOffset(t, env)
	stamp := time.Now().UTC().Format("150405.000")
	// Filed already paused: the engine never sees the item unpaused, so no stage
	// (and no Claude) ever runs for it before the control.
	num := FileIssue(t, env, repo, fmt.Sprintf("e2e app self-recognition: bot comment never resumes (%s)", stamp),
		"e2e scenario for #1877 (A1). Filed paused; a plain bot comment must not resume it.", pausedLabel, awaitingInputLabel)
	itemID := AddIssueToProject(t, env, repo, num)
	SetIssueStatus(t, env, itemID, "Specify")

	bot := postBotComment(t, leg, repo, num,
		"e2e self-recognition probe (#1877): a plain comment authored by the App identity. No Fabrik markers.")
	t.Logf("posted bot comment %d on %s#%d as %s", bot.ID, repo, num, bot.Login)
	logCommentAuthorShapes(t, env, repo, num, bot.ID)

	// Positive first: the engine must have evaluated the comment and kept the pause.
	// A resume line is watched in the same loop: if the bot comment is read as
	// human the engine unpauses and never logs the skip line, and waiting only
	// for the skip line would sit out the whole budget hiding that regression.
	evalLine := waitForLogLineWhere(t, env, fmt.Sprintf("[#%d ", num),
		func(l string) bool { return noHumanSkipLine(l, num) || resumeLine(l, num) }, off, 15*time.Minute,
		fmt.Sprintf("the engine to evaluate the bot comment on #%d and report it non-human", num))
	if resumeLine(evalLine, num) {
		t.Fatalf("engine resumed #%d on a bot-authored comment: %s — the bot comment was treated as human input; see the logged author shapes", num, evalLine)
	}
	t.Logf("engine evaluated the bot comment and kept the pause: %s", evalLine)

	// Then hold: at least three poll intervals in which nothing may change.
	hold := 3*bedPollInterval() + 30*time.Second
	t.Logf("holding %s (3 polls) asserting the pause persists", hold)
	holdFor(hold, bedPollInterval()/2, func() {
		labels, err := restIssueLabels(env, repo, num)
		if err != nil {
			t.Logf("transient label read error on %s#%d: %v", repo, num, err)
		} else if !containsString(labels, pausedLabel) || !containsString(labels, awaitingInputLabel) {
			t.Fatalf("%s#%d lost its pause labels after a bot-authored comment (labels: %v) — the bot comment was treated as human input; see the logged author shapes",
				repo, num, labels)
		}
		if l := anyLogLineWhere(t, env, fmt.Sprintf("[#%d ", num), func(l string) bool { return resumeLine(l, num) }, off); l != "" {
			t.Fatalf("engine resumed #%d on a bot-authored comment: %s — see the logged author shapes", num, l)
		}
		comments, err := listComments(env, repo, num)
		if err != nil {
			t.Logf("transient comment read error on %s#%d: %v", repo, num, err)
			return
		}
		for _, c := range comments {
			if c.ID == bot.ID && (c.Eyes > 0 || c.Rocket > 0) {
				t.Fatalf("bot comment %d on %s#%d has reactions (👀=%d 🚀=%d): the engine processed it as input", c.ID, repo, num, c.Eyes, c.Rocket)
			}
		}
	})
	t.Logf("A1 negative arm held: %s#%d stayed paused, no resume line, no reactions on the bot comment", repo, num)

	// Positive control: a human comment resumes the same item.
	CommentOnIssue(t, env, repo, num, "e2e self-recognition control (#1877): a human comment from "+humanLogin+" must resume the pause.")
	WaitForLabelAbsent(t, env, repo, num, pausedLabel, 15*time.Minute)
	resume := waitForLogLineWhere(t, env, fmt.Sprintf("[#%d ", num),
		func(l string) bool { return resumeLine(l, num) }, off, 2*time.Minute,
		fmt.Sprintf("the resume line for #%d after the human comment", num))
	t.Logf("A1 positive control: human comment from %s resumed %s#%d: %s", humanLogin, repo, num, resume)
}

// TestAppSelfRecognitionBlockedCommentUpdatedInPlace (A2): when an item's
// dependency set changes, Fabrik edits its own blocked comment in place — one
// comment, body reflecting the new set.
//
// Non-vacuity, honestly: pre-#1754 findBlockedComment(item.Comments,
// e.cfg.User) compared the operator login to the bot author, matched nothing
// and skipped the update — leaving a STALE body, not a duplicate. So the
// dependency-set assertion is the one that catches the regression; the count
// check alone would not. On current main the assertion may ALSO fail if the
// GraphQL comment author is the bare "<slug>" (see the file comment): then it
// is red both before and after #1754 and does not separate them until the
// engine normalises comment authors at ingestion. Failure evidence: the
// "[#N blocked] waiting for #B1, #B2" line (engine/dependencies.go) present
// while the comment body still lists only the old set.
//
// The re-check is throttled by the "dep-blocked" cooldown
// (githubRecheckInterval = PollSeconds*10, ~10 min at the bed's 60s poll), so
// the edit only lands after that cooldown; the wait budget covers it.
//
// No Claude cost: checkDependencies runs before any worktree or Claude work.
// Wall-clock: ~12-25 min.
func TestAppSelfRecognitionBlockedCommentUpdatedInPlace(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	blockedCommentUpdatedInPlace(t, env, appSelfRecognitionLeg(t, env))
}

// blockedCommentUpdatedInPlace is A2's body, shared with its PAT counterpart
// (TestPATSelfRecognitionBlockedCommentUpdatedInPlace, #1975): the scenario is
// identical, only who the engine is differs.
func blockedCommentUpdatedInPlace(t *testing.T, env *Env, leg selfRecognitionLeg) {
	t.Helper()
	repo := env.RepoAlpha

	off := LogOffset(t, env)
	stamp := time.Now().UTC().Format("150405.000")
	// Blockers are plain open issues kept OFF the board, so Fabrik never works
	// them and they read as open (no store entry → dep.State != CLOSED).
	b1 := FileIssue(t, env, repo, fmt.Sprintf("e2e %s self-recognition: blocker 1 (%s)", leg.Mode, stamp), "e2e blocker for #1877 (A2). Never worked; closed at teardown.")
	b2 := FileIssue(t, env, repo, fmt.Sprintf("e2e %s self-recognition: blocker 2 (%s)", leg.Mode, stamp), "e2e blocker for #1877 (A2). Never worked; closed at teardown.")
	num := FileIssue(t, env, repo, fmt.Sprintf("e2e %s self-recognition: blocked comment edited in place (%s)", leg.Mode, stamp),
		"e2e scenario for #1877 (A2). Blocked on a changing dependency set.")

	// Edge first, board placement second: the engine must never see this item
	// unblocked at Specify, or it would start a real Specify run.
	addBlockedByEdge(t, env, repo, num, b1)
	AssertBlockedBy(t, env, repo, num, repo, b1)
	itemID := AddIssueToProject(t, env, repo, num)
	SetIssueStatus(t, env, itemID, "Specify")

	// First block: Fabrik posts its blocked comment (as itself).
	first := waitForBlockedComment(t, env, repo, num, 15*time.Minute, func(c restComment, deps []string) bool { return true })
	t.Logf("blocked comment %d posted by %s: deps %v", first.ID, first.Login, mustDeps(t, first.Body))
	logCommentAuthorShapes(t, env, repo, num, first.ID)
	if first.Login != leg.Login {
		t.Fatalf("blocked comment %d on %s#%d was authored by %q, want the engine's own login %q (%s leg) — that is not the identity the engine posts as",
			first.ID, repo, num, first.Login, leg.Login, leg.Mode)
	}
	if deps := mustDeps(t, first.Body); !sameDepSet(deps, b1) {
		t.Fatalf("first blocked comment lists %v, want exactly #%d", deps, b1)
	}

	// Change the dependency set; the edit lands after the dep-blocked cooldown.
	addBlockedByEdge(t, env, repo, num, b2)
	AssertBlockedBy(t, env, repo, num, repo, b2)
	t.Logf("added blocker #%d to %s#%d; waiting out the dep-blocked cooldown (~%s)", b2, repo, num, 10*bedPollInterval())

	deadline := time.Now().Add(25 * time.Minute)
	for {
		comments, err := blockedComments(env, repo, num)
		if err == nil && len(comments) == 1 {
			if deps, ok := blockedCommentDeps(comments[0].Body); ok && sameDepSet(deps, b1, b2) {
				break
			}
		}
		if err == nil && len(comments) > 1 {
			t.Fatalf("%d blocked comments on %s#%d, want exactly 1 (edited in place, never duplicated)", len(comments), repo, num)
		}
		if time.Now().After(deadline) {
			evaluated := anyLogLineWhere(t, env, blockedLogPrefix(num), func(l string) bool { return strings.Contains(l, fmt.Sprintf("#%d", b2)) }, off)
			if evaluated != "" {
				t.Fatalf("engine evaluated the new dependency set (%s) but the blocked comment on %s#%d was not edited in place — "+
					"findBlockedComment did not recognise the comment as Fabrik's own (see the logged author shapes)", evaluated, repo, num)
			}
			t.Fatalf("timed out after 25m: the engine never re-evaluated %s#%d with blockers {#%d, #%d} (no matching %q line) — dep-blocked cooldown not elapsed or item not re-admitted",
				repo, num, b1, b2, blockedLogPrefix(num))
		}
		time.Sleep(30 * time.Second)
	}

	// Final state: exactly one, still the bot's, body reflects {B1, B2}.
	comments, err := blockedComments(env, repo, num)
	if err != nil {
		t.Fatalf("re-reading blocked comments: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("%d blocked comments on %s#%d, want exactly 1", len(comments), repo, num)
	}
	if comments[0].ID != first.ID {
		t.Fatalf("blocked comment id changed %d -> %d: it was replaced, not edited in place", first.ID, comments[0].ID)
	}
	if comments[0].Login != leg.Login {
		t.Fatalf("edited blocked comment is authored by %q, want %q", comments[0].Login, leg.Login)
	}
	t.Logf("A2 (%s) verified: one blocked comment (%d) on %s#%d edited in place to {#%d, #%d}", leg.Mode, first.ID, repo, num, b1, b2)
}

// TestAppSelfRecognitionDurableReviewSuppression (A3): a review-ids-addressed
// marker comment authored by the engine's own identity suppresses redelivery of
// that review (durablyAddressedReviewIDs, engine/reviews.go — the #1754 site
// that reads comments over REST, so it is not exposed to the GraphQL author
// shape). The same marker authored by anyone else is ignored.
//
// Non-vacuity: pre-#1754 durablyAddressedReviewIDs compared c.Author !=
// e.cfg.User ("arbeithand") against the REST author "fabrik-bed[bot]", so the
// bot's marker was ignored and review-body:R was dispatched — a clean pre-fix
// failure. The control arm proves the author scoping still rejects a non-self
// marker (the Pruefer #1555 finding): the harness account's marker for R2 must
// NOT suppress it, so R2 is dispatched.
//
// Topology: GraphQL latestReviews keeps one review per reviewer, so two reviews
// cannot share a PR. Each arm therefore has its own item and PR and one review
// from the reviewer token. Each review is created PENDING (invisible to the
// engine), its id is known, the marker is posted, and only then is the review
// submitted — so the marker exists before the engine can possibly see the
// review, with no race against the poll.
//
// Positive-first: the control arm's dispatch proves the engine evaluated review
// feedback on the bed after both reviews were submitted (the suppressed arm's
// review was submitted first); only then is the suppressed arm held for three
// poll intervals asserting review-body:R is never dispatched. The suppressed
// arm logs nothing of its own on suppression, so this is the strongest
// available observable without engine logging changes (out of scope).
//
// Cost: one review-reinvoke Claude invocation (the control arm).
// Wall-clock: ~8-15 min.
func TestAppSelfRecognitionDurableReviewSuppression(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	durableReviewSuppression(t, env, appSelfRecognitionLeg(t, env))
}

// durableReviewSuppression is A3's body, shared with its PAT counterpart
// (TestPATSelfRecognitionDurableReviewSuppression, #1975). The "self" marker is
// authored by the engine's own identity on the running leg; the control marker
// by an account the engine must not recognise as itself (the harness account on
// the App leg, the reviewer account on the PAT leg, where the harness account IS
// the engine).
func durableReviewSuppression(t *testing.T, env *Env, leg selfRecognitionLeg) {
	t.Helper()
	repo := env.RepoAlpha

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — required to submit a review from a non-author identity")
	}
	reviewerLogin := TokenLogin(t, reviewerToken)
	if reviewerLogin == TokenLogin(t, env.GHToken) {
		t.Fatalf("FABRIK_REVIEWER_TOKEN resolves to %q, the same identity as the engine/PR author — set it to a distinct account's PAT", reviewerLogin)
	}

	off := LogOffset(t, env)
	supNum, supPR, _ := seedReviewGateItem(t, env, repo, "main", "Review", "self-recog-suppressed", "review-authority:authoritative")
	ctlNum, ctlPR, _ := seedReviewGateItem(t, env, repo, "main", "Review", "self-recog-control", "review-authority:authoritative")
	AssertPRAuthorIsExpectedIdentity(t, env, repo, supPR)
	AssertPRAuthorIsExpectedIdentity(t, env, repo, ctlPR)

	// Engage the gate deterministically on both (same technique as
	// review_authority_test.go, #1312) before any review exists.
	RequestPRReviewer(t, env, repo, supPR, reviewerLogin)
	RequestPRReviewer(t, env, repo, ctlPR, reviewerLogin)
	WaitForIssueLabel(t, env, repo, supNum, "fabrik:awaiting-review", 10*time.Minute)
	WaitForIssueLabel(t, env, repo, ctlNum, "fabrik:awaiting-review", 10*time.Minute)

	// Suppressed arm: bot marker for R, posted BEFORE R is visible.
	const body = "e2e self-recognition review (#1877): please change something."
	rSup := createPendingReview(t, reviewerToken, repo, supPR, body)
	botMarker := leg.PostSelf(t, repo, supPR, reviewAddressedMarkerComment(rSup))
	logCommentAuthorShapes(t, env, repo, supPR, botMarker.ID)
	submitPendingReview(t, reviewerToken, repo, supPR, rSup, "REQUEST_CHANGES", body)
	t.Logf("suppressed arm: review %d on %s PR #%d submitted after self marker comment %d (author %s)", rSup, repo, supPR, botMarker.ID, botMarker.Login)

	// Control arm: the same marker, authored by a non-self account, for R2.
	rCtl := createPendingReview(t, reviewerToken, repo, ctlPR, body)
	spoof := leg.PostOther(t, env, reviewerToken, repo, ctlPR, reviewAddressedMarkerComment(rCtl))
	submitPendingReview(t, reviewerToken, repo, ctlPR, rCtl, "REQUEST_CHANGES", body)
	t.Logf("control arm: review %d on %s PR #%d submitted after non-self marker comment %d (author %s)", rCtl, repo, ctlPR, spoof.ID, spoof.Login)

	// Positive first: the control's review must be dispatched despite the
	// non-self marker.
	line := waitForLogLineWhere(t, env, reviewReinvokePrefix(ctlNum),
		func(l string) bool { return dispatchesReview(l, rCtl) }, off, 20*time.Minute,
		fmt.Sprintf("a review-reinvoke on #%d naming review-body:%d", ctlNum, rCtl))
	t.Logf("control verified — a marker authored by a non-self account did not suppress review %d: %s", rCtl, line)

	// Then hold: the suppressed review must never be dispatched.
	hold := 3*bedPollInterval() + 30*time.Second
	t.Logf("holding %s (3 polls) asserting review %d is never dispatched on #%d", hold, rSup, supNum)
	holdFor(hold, bedPollInterval()/2, func() {
		lines, err := allLogLinesContaining(env, reviewReinvokePrefix(supNum), off)
		if err != nil {
			t.Fatalf("scanning reinvoke lines for #%d: %v", supNum, err)
		}
		for _, l := range lines {
			if dispatchesReview(l, rSup) {
				t.Fatalf("review %d on #%d was dispatched despite the self-authored review-ids-addressed marker: %s — "+
					"durablyAddressedReviewIDs did not recognise the marker comment as the engine's own (selfLogin() vs REST author %q)",
					rSup, supNum, strings.TrimSpace(l), leg.Login)
			}
		}
	})
	t.Logf("A3 (%s) verified: review %d on %s#%d suppressed by the engine's own marker; review %d on #%d (non-self marker) was delivered", leg.Mode, rSup, repo, supNum, rCtl, ctlNum)
}

// awaitingInputLabel is the engine's awaiting-input half of the pause pair.
const awaitingInputLabel = "fabrik:awaiting-input"

// mustDeps parses a blocked comment body's dependency list or fails.
func mustDeps(t *testing.T, body string) []string {
	t.Helper()
	deps, ok := blockedCommentDeps(body)
	if !ok {
		t.Fatalf("not a parseable blocked comment: %q", body)
	}
	return deps
}

// waitForBlockedComment polls until exactly one blocked comment exists and keep
// accepts it, and returns it.
func waitForBlockedComment(t *testing.T, env *Env, repo string, number int, timeout time.Duration, keep func(restComment, []string) bool) restComment {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		comments, err := blockedComments(env, repo, number)
		if err == nil && len(comments) == 1 {
			if deps, ok := blockedCommentDeps(comments[0].Body); ok && keep(comments[0], deps) {
				return comments[0]
			}
		}
		if err == nil && len(comments) > 1 {
			t.Fatalf("%d blocked comments on %s#%d, want exactly 1", len(comments), repo, number)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for the blocked comment on %s#%d (last read error: %v)", timeout, repo, number, err)
		}
		time.Sleep(30 * time.Second)
	}
}

// addBlockedByEdge records "number is blocked by blocker" via the Issue
// Dependencies REST API, which takes the blocker's numeric issue id (.id), not
// its number.
func addBlockedByEdge(t *testing.T, env *Env, repo string, number, blocker int) {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	idOut, err := ghOutput(env, "api", fmt.Sprintf("repos/%s/%s/issues/%d", owner, name, blocker), "--jq", ".id")
	if err != nil {
		t.Fatalf("resolving the numeric id of %s#%d: %v\n%s", repo, blocker, err, idOut)
	}
	out, err := ghOutput(env, "api", "-X", "POST",
		fmt.Sprintf("repos/%s/%s/issues/%d/dependencies/blocked_by", owner, name, number),
		"-F", "issue_id="+strings.TrimSpace(idOut))
	if err != nil {
		t.Fatalf("adding %s#%d as a blocker of #%d: %v\n%s", repo, blocker, number, err, out)
	}
}
