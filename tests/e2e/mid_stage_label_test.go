//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Mid-stage label-state scenarios (#1878): two 0.0.83 fixes concern label state
// changing in the MIDDLE of a stage, and both are operator-trust properties —
// removing yolo is how an operator says "stop, don't merge", and the board is how
// they see what is happening.
//
// Neither fix logs anything (refreshAutonomyLabels has no success log; the rework
// swap has none), so both scenarios assert on GitHub state and on the ORDER of
// events in the issue events log (label_events.go), never on bed-log lines and
// never on sleeps.
//
// Auth legs (PAT and GitHub App, #1861): neither scenario skips in either mode.
//   - S1 only removes a label; label writes are not identity-gated and the engine
//     reads label state, not the actor. The member PR is authored by FABRIK_TOKEN's
//     account (Fabrik's own identity in PAT mode, a human in App mode); nothing
//     here depends on which.
//   - S2's comment is admitted by findNewComments, which has no author filter —
//     only a 🏭-prefixed body, a 🚀 reaction or a bot service notice is skipped. In
//     PAT mode arbeithand is Fabrik's own identity but the body carries no 🏭
//     prefix (asserted below); in App mode arbeithand is a plain human. The
//     filterHuman gate applies only on the paused / awaiting-input resume paths,
//     which S2 does not use, so the reviewer token is not needed.

// waitForValidateInProgressAndRemoveYolo polls the issue's labels over REST (not
// the shared GraphQL budget) and removes fabrik:yolo the moment
// stage:Validate:in_progress appears. It ends the test INCONCLUSIVE (uncovered,
// retried by the gate — never green) if Validate is seen to have progressed past
// the in-progress window first.
func waitForValidateInProgressAndRemoveYolo(t *testing.T, env *Env, repo string, num int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		labels, err := restIssueLabels(env, repo, num)
		if err != nil {
			t.Logf("transient error reading labels on %s#%d: %v (will retry)", repo, num, err)
			pollSleep(5 * time.Second)
			continue
		}
		has := map[string]bool{}
		for _, l := range labels {
			has[l] = true
		}
		if has["stage:Validate:in_progress"] {
			RemoveLabel(t, env, repo, num, "fabrik:yolo")
			t.Logf("observed stage:Validate:in_progress on %s#%d — removed fabrik:yolo", repo, num)
			return
		}
		if has["fabrik:awaiting-ci"] || has["stage:Validate:complete"] {
			Inconclusive(t, "%s#%d reached fabrik:awaiting-ci / stage:Validate:complete (labels %v) without stage:Validate:in_progress ever being observed — the removal window was missed", repo, num, labels)
		}
		pollSleep(5 * time.Second)
	}
	t.Fatalf("timed out after %s waiting for stage:Validate:in_progress on %s#%d", timeout, repo, num)
}

// TestYoloRemovedMidValidateBlocksMerge is the live e2e proof of #1769 (ADR-1769):
// removing fabrik:yolo while Validate is in progress must prevent the merge. The
// advance/merge decision re-reads autonomy labels live (refreshAutonomyLabels,
// engine/stages.go, called from handleStageComplete and runCatchUpPhase2 ahead of
// attemptMergeOnValidate) instead of trusting the snapshot taken at dispatch.
//
// Construction: one yolo issue is taken to Validate directly (member PR on
// fabrik/issue-N via CreateMemberPR, stage:Review:complete seeded, Status set to
// Validate — the same seeding as TestLateCheckRunSuiteGate), so exactly one real
// Validate invocation runs. The harness polls labels over REST every ~5s and
// removes fabrik:yolo as soon as stage:Validate:in_progress is present.
//
// Window proof (checkYoloRemovedMidValidate, on the durable events log, by event
// id): labeled stage:Validate:in_progress < unlabeled fabrik:yolo < labeled
// fabrik:awaiting-ci (applied by handleStageComplete the instant Validate
// finishes). A missed window ends the test INCONCLUSIVE (#1973): the gate retries
// it, and what stays inconclusive is recorded as uncovered — so the release gate
// never goes quietly green.
//
// Assertions once stage:Validate:complete is applied (the CI gate has cleared —
// the moment the merge decision runs) and a settle window of three polls has
// passed:
//
//	A1 fabrik:auto-merge-enabled was never applied; PR still OPEN; issue still OPEN
//	   (the merge, if wrongly triggered, would close both)
//	A1 board Status is still Validate (not Queued or Done) and
//	   stage:Validate:complete is present — parked at Validate-complete.
//
// The check is mode-invariant for merge_train: with no autonomy label
// runCatchUpPhase2 returns before the advanceToQueued fork. It skips only when
// train mode is "on" and the board has no Queued column, as
// TestLateCheckRunSuiteGate does.
//
// Non-vacuity (A2) — argued, not run, and stated with its limit. Pre-#1769 the
// merge decision read item.Labels from the dispatch-time snapshot / board cache.
// The bed's Validate has wait_for_ci: true (as does the shipped default), so the
// decision happens at runCatchUpPhase2 (via settleAwaitingCIScan) rather than at
// handleStageComplete's !waitForCI path; a pre-fix runCatchUpPhase2 read whatever
// the board cache then held. A yolo removed early in a multi-minute Validate is
// often already visible there, so a pre-fix engine can also pass this scenario on
// the bed. What it does guard is the operator-trust property end to end (mid-stage
// removal => no merge, no Queued, no Done) and the live re-read in the window
// where the cache lags the removal. Forcing a wait_for_ci: false Validate to
// exercise handleStageComplete's snapshot path would need a bed stage-file change
// and restart, which is out of scope.
//
// Wall-clock: ~15–30 min (Validate's Claude run, CI, then a 3-poll settle window).
// Cost: one Validate Claude invocation (~$0.10–0.50).
func TestYoloRemovedMidValidateBlocksMerge(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	if resolveTrainMode(t, env) == "on" {
		// requireTrainBed skips unless a Queued column exists (mode is "on" here).
		requireTrainBed(t, env)
	}

	stamp := time.Now().UTC().Format("150405.000")
	title := fmt.Sprintf("e2e yolo removed mid-Validate (%s)", stamp)
	num := FileIssue(t, env, env.RepoAlpha, title,
		"e2e scenario for the live re-read of autonomy labels. The only requirement is that the file "+
			"under e2e/mid-stage-yolo/entries/ exists on the branch; there is nothing to implement. "+
			"Validate should confirm that and complete.",
		"fabrik:yolo")
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)

	branch := fmt.Sprintf("fabrik/issue-%d", num)
	path := uniqueMemberPath("e2e/mid-stage-yolo/entries/mid-stage-yolo.txt", num)
	prNum := CreateMemberPR(t, env, env.RepoAlpha, "main", branch, path,
		"yolo-removed-mid-Validate scenario member\n", title, num)
	AwaitPRForBranchVisible(t, env, env.RepoAlpha, num, awaitSeedTimeout)

	AddLabel(t, env, env.RepoAlpha, num, "stage:Review:complete")
	SetIssueStatus(t, env, itemID, "Validate")
	AwaitBoardItemVisible(t, env, env.RepoAlpha, num, awaitSeedTimeout)
	t.Logf("seeded %s#%d (PR #%d, path %s) at Status=Validate with fabrik:yolo", env.RepoAlpha, num, prNum, path)

	waitForValidateInProgressAndRemoveYolo(t, env, env.RepoAlpha, num, 15*time.Minute)

	// The CI gate clears, and the merge decision runs, once stage:Validate:complete
	// is applied.
	completeAt := waitForLabelFirstApplied(t, env, env.RepoAlpha, num, "stage:Validate:complete", 30*time.Minute)
	t.Logf("stage:Validate:complete first applied at %s", completeAt.Format(time.RFC3339))

	if err := checkYoloRemovedMidValidate(mustFetchIssueEvents(t, env, env.RepoAlpha, num)); err != nil {
		failOrInconclusive(t, err)
	}

	// Give a wrongly-triggered merge time to show up: the decision runs in the same
	// poll pass that applies the complete label, but auto-merge / landing is
	// asynchronous.
	pollSleep(3 * pollBase())

	AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:auto-merge-enabled")

	prState, err := restPRState(env, env.RepoAlpha, prNum)
	if err != nil {
		t.Fatalf("read PR #%d state: %v", prNum, err)
	}
	if prState != "OPEN" {
		t.Errorf("PR #%d state = %q, want OPEN — yolo was removed mid-Validate, so the PR must not merge", prNum, prState)
	}
	issueState, err := restIssueState(env, env.RepoAlpha, num)
	if err != nil {
		t.Fatalf("read issue state: %v", err)
	}
	if issueState != "OPEN" {
		t.Errorf("issue %s#%d state = %q, want OPEN", env.RepoAlpha, num, issueState)
	}
	items, err := fetchBoardItems(env)
	if err != nil {
		t.Fatalf("read board: %v", err)
	}
	item, ok := findBoardItem(items, env.RepoAlpha, num)
	if !ok {
		t.Fatalf("%s#%d not found on the board", env.RepoAlpha, num)
	}
	if item.Status != "Validate" {
		t.Errorf("board Status = %q, want Validate (not advanced to Queued or Done)", item.Status)
	}
	labels := IssueLabels(t, env, env.RepoAlpha, num)
	if !containsString(labels, "stage:Validate:complete") {
		t.Errorf("stage:Validate:complete missing from %v — the issue should stay at Validate-complete", labels)
	}
	if containsString(labels, "fabrik:yolo") {
		t.Errorf("fabrik:yolo present in %v — the engine must not re-add an operator-removed label", labels)
	}
	t.Logf("verified: yolo removed mid-Validate — PR #%d OPEN, issue OPEN, Status=Validate, stage:Validate:complete present, no auto-merge", prNum)
}

// TestCommentReentryShowsReworking is the live e2e proof of #1802 (ADR-1802):
// during comment re-entry on an already-completed stage, stage:<Stage>:complete is
// swapped for the fabrik:reworking:<Stage> marker (beginStageRework) and restored
// afterwards (handleStageComplete on a completing re-entry, endStageRework
// otherwise), so the board never reports a reworking item as done.
//
// Construction: a no-autonomy-label issue is taken through a real Research run
// (Research has no auto_advance on the bed, so with no yolo/cruise it parks at
// Research). A real run — not a harness-seeded label — so the engine itself
// applied stage:Research:complete and its snapshot is guaranteed to carry it;
// beginStageRework reads the dispatch-time snapshot and silently no-ops when the
// complete label is absent. Then one human comment asking for real re-verification
// work (to lengthen the re-entry) is posted.
//
// Why the events log and not a live poll: the transient state lives between two
// label writes with sub-second gaps, which a poll cannot reliably sample. The
// deterministic proof is checkReworkSequence: after sinceID (the highest event id
// captured just before the comment, so Research's genuine earlier
// `labeled stage:Research:complete` is ignored) the events occur in order
//
//	labeled fabrik:reworking:Research   (marker first — ADR-1802)
//	unlabeled stage:Research:complete   (A3: complete absent while marker present)
//	labeled stage:Research:complete     (A4: restored)
//	unlabeled fabrik:reworking:Research (A4: marker gone)
//
// The reworking label name is reworkingLabelPrefix + stage, the exact prefix of
// engine/comments.go, pinned by TestReworkingLabelPrefixLiteral. A live poll for
// "marker present, complete absent" runs alongside and is logged for information
// only.
//
// Final-state assertions: stage:Research:complete present, marker absent, item
// still at Research.
//
// Non-vacuity (A5): pre-#1802 the engine left stage:Research:complete on for the
// whole re-entry, so no `unlabeled stage:Research:complete` event exists after
// sinceID (and no marker is ever applied); checkReworkSequence fails at step 1.
//
// Wall-clock: ~10–20 min (Research run, then one comment-review run).
// Cost: two Claude invocations (~$0.20–0.60).
func TestCommentReentryShowsReworking(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	const stage = "Research"
	completeLabel := "stage:" + stage + ":complete"
	markerLabel := reworkingLabelPrefix + stage

	logStart := LogOffset(t, env)
	stamp := time.Now().UTC().Format("150405.000")
	num := FileIssue(t, env, env.RepoAlpha,
		fmt.Sprintf("e2e comment re-entry shows reworking (%s)", stamp),
		"e2e scenario for the comment re-entry rework marker. Goal: add a short "+
			"\"Files\" section to README.md that names README.md and go.mod and states in "+
			"one sentence each what they are for. Research should read both files and "+
			"record what the new section needs to say; later stages would make the edit.")
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	AddLabel(t, env, env.RepoAlpha, num, "stage:Specify:complete")
	SetIssueStatus(t, env, itemID, stage)
	t.Logf("filed %s#%d at Status=%s (no autonomy label — the item parks after Research)", env.RepoAlpha, num, stage)

	waitForLabelFirstApplied(t, env, env.RepoAlpha, num, completeLabel, 30*time.Minute)
	// Research may still judge the issue to need no work (FABRIK_NO_WORK_NEEDED),
	// which sends it to Done and closes it — there is then no parked stage for the
	// comment to re-enter. That is a fixture outcome, not an engine regression, so
	// report it as such instead of timing out 20 minutes later on a missing marker.
	if _, found, err := tryLabelFirstAppliedAt(env, env.RepoAlpha, num, "fabrik:awaiting-done"); err != nil {
		t.Logf("could not check %s#%d for fabrik:awaiting-done (%v) — skipping the no-work-needed fast-fail", env.RepoAlpha, num, err)
	} else if found {
		// A precondition guard (#1973): fires before any assertion about the rework
		// marker. Claude's judgement is non-deterministic, so a retry can clear it.
		Inconclusive(t, "fixture: Research judged %s#%d to need no work (fabrik:awaiting-done applied), so the item went to Done "+
			"instead of parking after Research — the re-entry scenario cannot run; if it recurs make the issue body "+
			"describe more concrete work", env.RepoAlpha, num)
	}
	t.Logf("%s applied — Research complete, item parked", completeLabel)

	sinceID := maxEventID(mustFetchIssueEvents(t, env, env.RepoAlpha, num))

	// Let the engine's snapshot catch up with the label before the comment lands.
	pollSleep(2 * pollBase())

	body := "Please re-verify your Research findings before we move on: re-read README.md and " +
		"go.mod in full, then write a fresh summary that cites at least three specific lines " +
		"from each file, and state whether anything in your earlier findings changed."
	if strings.HasPrefix(strings.TrimSpace(body), "🏭") {
		t.Fatalf("test bug: comment body must not carry the Fabrik 🏭 prefix (findNewComments skips it)")
	}
	CommentOnIssue(t, env, env.RepoAlpha, num, body)
	t.Logf("posted re-entry comment on %s#%d (events bound sinceID=%d)", env.RepoAlpha, num, sinceID)

	// Watch until the marker has been removed again. Each iteration also samples
	// live labels; that observation is informational only.
	sawLive := false
	var events []issueEvent
	deadline := time.Now().Add(20 * time.Minute)
	done := false
	for time.Now().Before(deadline) {
		evs, err := fetchIssueEvents(env, env.RepoAlpha, num)
		if err != nil {
			t.Logf("transient error reading events: %v (will retry)", err)
		} else {
			events = evs
			if _, ok := firstEvent(events, eventUnlabeled, markerLabel, sinceID); ok {
				done = true
				break
			}
		}
		if labels, lerr := restIssueLabels(env, env.RepoAlpha, num); lerr == nil {
			if containsString(labels, markerLabel) && !containsString(labels, completeLabel) {
				sawLive = true
			}
		}
		pollSleep(5 * time.Second)
	}
	if !done {
		err := checkReworkSequence(events, sinceID, stage)
		t.Fatalf("timed out waiting for %s to be removed after the re-entry comment; sequence check: %v", markerLabel, err)
	}
	t.Logf("informational: live poll observed %q present with %q absent: %v", markerLabel, completeLabel, sawLive)
	t.Logf("informational: %d `[#%d comments] processing` log line(s) since test start",
		CountLogLines(t, env, fmt.Sprintf("[#%d comments] processing", num), logStart), num)

	if err := checkReworkSequence(events, sinceID, stage); err != nil {
		t.Fatalf("%v", err)
	}

	labels := IssueLabels(t, env, env.RepoAlpha, num)
	if !containsString(labels, completeLabel) {
		t.Errorf("%s missing from %v — completion was not restored after the re-entry", completeLabel, labels)
	}
	if containsString(labels, markerLabel) {
		t.Errorf("%s still present in %v after the re-entry finished", markerLabel, labels)
	}
	items, err := fetchBoardItems(env)
	if err != nil {
		t.Fatalf("read board: %v", err)
	}
	item, ok := findBoardItem(items, env.RepoAlpha, num)
	if !ok {
		t.Fatalf("%s#%d not found on the board", env.RepoAlpha, num)
	}
	if item.Status != stage {
		t.Errorf("board Status = %q, want %q (the re-entry must not advance the item)", item.Status, stage)
	}
	t.Logf("verified: re-entry swapped %s for %s and restored it; item parked at %s", completeLabel, markerLabel, stage)
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
