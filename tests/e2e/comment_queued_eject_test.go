//go:build e2e

package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestQueuedMemberCommentEjection is the live-e2e proof of #1863 (ADR-1863,
// settleQueuedCommentCause / ejectQueuedMemberForComments): a human comment that
// arrives on a member sitting in Queued ejects it off Queued so the comment can be
// processed. The eject is not a train failure — no fabrik:paused, no
// MaxMergeTrainEjections count. Skips cleanly under merge_train: off
// (requireTrainBed).
//
//   - A3: the `🏭 **Fabrik merge-train — ejected (unprocessed comment)**` comment is
//     posted on the member and the log carries
//     `#N ejected for an unprocessed comment: rerouted to <stage> (not paused, no ejection counted)`.
//   - A4: fabrik:paused is never applied to the member; exactly one comment-eject
//     comment and NO counted-ejection artifact exists (no
//     `🏭 **Fabrik merge-train — ejected**` comment, no `pausing after N ejections`
//     comment, no `ejected N time(s) — pausing` log line). The
//     mergeTrainEjectionCounts counter itself is in memory and unobservable, so
//     "not counted" is asserted INDIRECTLY, as the absence of everything a counted
//     ejection (ejectMember) leaves behind.
//   - A5: the comment is processed (👀 then 🚀), the member re-queues and lands
//     (issue closed_at >= the 🚀).
//
// # Deterministic placement (an occupant member)
//
// Queuing a member and commenting cannot deterministically produce a DIRECT eject:
// the batch worker dispatches before the settle scan in the same poll, so a member
// in the just-formed batch is ejected via the pending-signal route, which a worker on
// the singleton fast path can miss. So an occupant M1 is queued first and made to
// build a trial, holding a worker in flight for the (repo, base) partition for the
// bed's ~10 min slow-gate. A lone, green, up-to-date member takes the singleton fast
// path and never opens a trial (member-PR CI on the bed is green within about a
// minute; only trial branches are slow), so M1's PR is prepared first, main is then
// advanced by one unrelated commit (AdvanceBaseBranch), and only then is M1 queued:
// its head is no longer a fast-forward of the pinned base, so the engine must build
// a trial (engine/merge_train.go: "pinned base is N commit(s) ahead of the member's
// head"). The first live run of this scenario failed exactly here, waiting 25 min
// for a trial the fast path had skipped. Once the log shows
// `opened draft CI PR … (1 survivor(s))`, M2 is queued and commented on. M2 is never
// in the fixed batch, dispatch skips, and the settle scan ejects it directly. The
// scenario asserts the settle scan's direct-route line for M2 ("not owned by any
// live batch … ejecting directly") and fails loudly ("occupant window closed") if it
// took the pending-signal route instead, rather than reporting an engine regression.
//
// M2 is prepared first (issue, PR, fabrik:yolo, stage:Validate:complete, reviewer
// APPROVE) so that after the eject it returns to Validate, clears the comment gate
// once the comment is processed, and re-queues and lands.
//
// # Identity
//
// The comment is posted by FABRIK_TOKEN's account — human to Fabrik in both auth
// legs (see postHumanIssueComment). FABRIK_REVIEWER_TOKEN supplies the APPROVE only.
//
// # Isolation
//
// NOT t.Parallel(), on RepoAlpha/main — like the batch-cap, conflict and restart
// scenarios: a foreign Queued member on the partition would join M1's batch and
// change its composition. A pre-flight fails loudly on stale Queued items.
//
// Wall-clock: ~35–60 min (M1's slow-gate trial, then M2's comment processing and
// re-queue trial). Cost: one comment-review Claude invocation.
func TestQueuedMemberCommentEjection(t *testing.T) {
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	requireTrainBed(t, env)
	repo := env.RepoAlpha
	assertSlowGateRequired(t, env, repo)

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — the scenario needs a deterministic non-author APPROVE for the ejected " +
			"member's re-queue (a review-wait timeout would apply fabrik:paused and corrupt A4)")
	}

	const base = "main"
	trainKey := repo // default-base partition (mergeTrainKey)

	stale, err := staleQueuedMembers(env, repo)
	if err != nil {
		t.Fatalf("pre-flight: could not check %s for stale Queued items: %v", repo, err)
	}
	if len(stale) > 0 {
		// Deliberately a Fatalf, not Inconclusive (#1973): bed STATE an operator must
		// clear — an automatic retry would hit the same stale items.
		t.Fatalf("pre-flight: %s already has open, non-paused Queued item(s) %v — they would join the occupant's batch and change "+
			"its composition; move them out of Queued (or close them) and re-run", repo, stale)
	}

	stamp := time.Now().UTC().Format("20060102-150405.000")

	// --- Prepare M2 (the member that will be commented on) before the occupant, so its CI runs meanwhile. ---
	m2, m2PR, m2Item := PrepareMemberExactPath(t, env, repo, base, "ce-m2",
		fmt.Sprintf("e2e/train/entries/comment-eject-m2-%s.txt", stamp), "comment-eject member M2\n")
	AddLabel(t, env, repo, m2, "fabrik:yolo")
	AddLabel(t, env, repo, m2, "stage:Validate:complete")
	SubmitPRReview(t, env, reviewerToken, repo, m2PR, "APPROVE")

	// --- Queue the occupant M1 and wait for its trial to be in flight. ---
	m1, m1PR, m1Item := PrepareMemberExactPath(t, env, repo, base, "ce-m1",
		fmt.Sprintf("e2e/train/entries/comment-eject-m1-%s.txt", stamp), "comment-eject occupant M1\n")
	// Move main past M1's head so M1 cannot take the singleton fast path.
	AdvanceBaseBranch(t, env, repo, base,
		fmt.Sprintf("e2e/train/entries/comment-eject-basebump-%s.txt", stamp), "base bump for the comment-eject occupant\n")
	offset := LogOffset(t, env)
	SetIssueStatus(t, env, m1Item, "Queued")
	AwaitStatusVisible(t, env, repo, m1, "Queued", awaitSeedTimeout)
	t.Logf("occupant M1 = #%d (PR #%d), member under test M2 = #%d (PR #%d)", m1, m1PR, m2, m2PR)

	fastPathTaken := fmt.Sprintf("singleton fast path taken for #%d:", m1)
	isM1Trial := func(l string) bool {
		if strings.Contains(l, fastPathTaken) {
			t.Fatalf("occupant M1 #%d took the singleton fast path despite the base bump, so no trial will open: %s", m1, strings.TrimSpace(l))
		}
		_, survivors, ok := parseOpenedDraftCI(l, repo)
		return ok && survivors == 1
	}
	waitForLogMatch(t, env, offset, 25*time.Minute,
		"\"opened draft CI PR … (1 survivor(s))\" for "+repo+" — the occupant's trial never started", isM1Trial)
	t.Logf("occupant trial in flight; queuing M2 and commenting")

	// --- Queue M2 and post the unprocessed human comment. ---
	SetIssueStatus(t, env, m2Item, "Queued")
	AwaitStatusVisible(t, env, repo, m2, "Queued", awaitSeedTimeout)
	commentID := postHumanIssueComment(t, env, repo, m2,
		"Reviewer note: please double-check the wording of this change. No code change is required — acknowledge only, do not modify or push anything.")
	t.Logf("M2 #%d queued and comment %d posted", m2, commentID)

	// --- A3: ejected for the unprocessed comment. ---
	WaitForIssueComment(t, env, repo, m2, commentEjectCommentPrefix, 15*time.Minute)
	isM2Eject := func(l string) bool {
		got, _, ok := parseCommentEjectLine(l)
		return ok && got == m2
	}
	ejectLine := waitForLogMatch(t, env, offset, 5*time.Minute,
		"the comment-eject log line for #"+strconv.Itoa(m2), isM2Eject)
	_, target, _ := parseCommentEjectLine(ejectLine)
	t.Logf("A3: ejected — rerouted to %q: %s", target, strings.TrimSpace(ejectLine))

	// Determinism check: the eject must have taken the DIRECT route (M2 not owned by
	// any live batch), not the pending-signal route. Assert it from the settle scan's
	// own route line (engine/queued_review_settle.go). A "batch snapshot" line listing
	// M2 is NOT evidence of the pending route: that line reports what is in Queued
	// whenever it changes, even while the worker holds a fixed batch and logs
	// "train worker already assembling … — skipping" (the first live run of this
	// scenario tripped over exactly that).
	directLine := fmt.Sprintf("unprocessed human comment(s) on Queued member #%d — not owned by any live batch for %s, ejecting directly", m2, trainKey)
	pendingLine := fmt.Sprintf("unprocessed human comment(s) on Queued member #%d — owned by the live batch for %s, flagging pending eject", m2, trainKey)
	sawDirect := false
	for _, l := range logLinesSince(t, env, offset) {
		if strings.Contains(l, pendingLine) {
			// A precondition guard (#1973): it fires before the direct-route assertion
			// below, and means the occupant's trial finished before M2 was queued, so
			// the scenario's precondition (M2 not owned by a live batch) never arose.
			Inconclusive(t, "occupant window closed: M2 #%d was owned by the live batch and took the pending-signal eject route (%s) — "+
				"the occupant's trial finished before M2 was queued (or the slow-gate is not holding it). This is a harness/bed "+
				"problem, not an engine regression.", m2, strings.TrimSpace(l))
		}
		if strings.Contains(l, directLine) {
			sawDirect = true
		}
	}
	if !sawDirect {
		t.Fatalf("A3: M2 #%d was ejected, but the settle scan never logged the direct route (%q)", m2, directLine)
	}
	t.Logf("A3: direct eject route confirmed (M2 not owned by any live batch)")

	// --- A5: processed (👀 then 🚀), then re-queued and landed. ---
	eyes, rocket := waitForCommentRocket(t, env, repo, commentID, 40*time.Minute, nil)
	if !reactionsOrdered(eyes, rocket) {
		t.Fatalf("A5: comment %d reactions out of order or 👀 missing: eyes=%v rocket=%v", commentID, eyes, rocket)
	}
	t.Logf("A5: comment processed: 👀 %s, 🚀 %s", eyes.Format(time.RFC3339), rocket.Format(time.RFC3339))

	WaitForMemberLanded(t, env, repo, m2, 90*time.Minute)
	closedAt, err := issueClosedAt(env, repo, m2)
	if err != nil {
		t.Fatalf("read closed_at of %s#%d: %v", repo, m2, err)
	}
	if !landedAtOrAfter(rocket, closedAt) {
		t.Fatalf("A5: M2 #%d closed at %v, before the comment's 🚀 at %v — it landed before the comment was processed", m2, closedAt, rocket)
	}
	WaitForMemberLanded(t, env, repo, m1, 60*time.Minute)

	// --- A4: not paused, and no counted-ejection artifact. ---
	AssertLabelWasNeverApplied(t, env, repo, m2, pausedLabel)
	bodies, err := tryPRComments(env, repo, m2)
	if err != nil {
		t.Fatalf("read comments on %s#%d: %v", repo, m2, err)
	}
	if n := countMatching(bodies, isCommentEjectComment); n != 1 {
		t.Fatalf("A3/A4: %d comment-eject comment(s) on M2 #%d, want exactly 1", n, m2)
	}
	if n := countMatching(bodies, isCountedEjectComment); n != 0 {
		t.Fatalf("A4: %d counted-ejection comment(s) (`🏭 **Fabrik merge-train — ejected**`) on M2 #%d — the eject went through ejectMember and was counted", n, m2)
	}
	if n := countMatching(bodies, isEjectionCapComment); n != 0 {
		t.Fatalf("A4: %d `pausing after N ejections` comment(s) on M2 #%d", n, m2)
	}
	lines := logLinesSince(t, env, offset)
	if n := countCappedEjectLines(lines, m2); n != 0 {
		t.Fatalf("A4: %d `ejected N time(s) — pausing` log line(s) for M2 #%d", n, m2)
	}
	if n := countCommentEjectLines(lines, m2); n != 1 {
		t.Fatalf("A3: %d comment-eject log line(s) for M2 #%d, want exactly 1", n, m2)
	}
	t.Logf("TestQueuedMemberCommentEjection passed: M2 #%d ejected once (not paused, no counted-ejection artifact), processed, and landed", m2)
}
