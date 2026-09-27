//go:build e2e

package e2e

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// TestPauseLiftedOnlyByPostPauseHumanComment verifies the ADR-1813 resume rule
// against real GitHub event timestamps (#1876; rule from #1813, incident #1752):
// a paused item is lifted only by a HUMAN comment created at or after the latest
// fabrik:paused `labeled` event. A human comment that predates the pause must not
// lift it — before #1813 it did, on every poll, cycling the pause label.
//
// Flow:
//  1. File an issue in alpha but do NOT add it to the project board. The engine
//     discovers work only through the board (FetchProjectBoard), so an off-board
//     issue cannot be deep-fetched or dispatched: it is impossible for the engine
//     to consume the comment before the pause anchor exists.
//  2. Post the "old" human comment, wait, apply fabrik:paused + fabrik:awaiting-input
//     as an operator would, wait again. The labeled event is now strictly later than
//     the comment, with real gaps so one-second timestamp resolution cannot make
//     them equal (equality resolves toward resuming).
//  3. Add the issue to the board and move it to Specify. A Projects v2 Status change
//     writes no issue labeled/unlabeled event and no engine path strips the pause
//     labels on a move, which the events assertions below confirm rather than assume.
//  4. A1: the engine emits the refusal line for THIS issue, the labels hold across
//     >= 4 min (>= 3 polls at the bed's 60s cadence), and the old comment gets no
//     👀/🚀. The refusal line is not emitted per poll — a quiescent paused item is not
//     re-evaluated (poll.go skips its deep-fetch) — so two `🏭 **Fabrik — e2e nudge**`
//     comments are posted, which change the item's comments (re-evaluating it) yet are
//     never resume-eligible (findNewComments skips the 🏭 **Fabrik prefix).
//  5. A2: the events log shows exactly one `labeled` and zero `unlabeled` events for
//     fabrik:paused over the whole window — no lift/re-add cycles, no re-stamp on move.
//  6. A3: a new human comment posted after the pause resumes it: 👀 then 🚀 on the new
//     comment and an `unlabeled` fabrik:paused event. The events log rather than the
//     current label set is asserted, since the resumed worker may block again. Nothing
//     is asserted about the OLD comment after the resume: an authorised resume hands
//     the full raw findNewComments set to processComments (ADR-1813 R5).
//
// Non-vacuity (pre-#1813 engine: any human comment resumed a pause):
//   - A1 labels + no-reaction: the pause would be lifted at the move and the old
//     comment would get 👀.
//   - A1 refusal line: "[#N skip] awaiting-input: 1 human comment(s) predate the pause
//     — still waiting" (engine/item.go) does not exist pre-fix.
//   - A2: the events log would show an `unlabeled` event (and re-add cycles).
//   - A3 is true both before and after the fix; it guards the other direction (an
//     over-strict fix that never resumes fails it).
//
// R2 / auth legs: the scenario posts only as the harness account (arbeithand,
// FABRIK_TOKEN), which matches none of gh.IsBotLogin's patterns, so filterHuman reads
// it as human. In the PAT leg it is also Fabrik's own identity, but every engine
// comment carries the 🏭 **Fabrik prefix and is skipped by findNewComments. In the App
// leg Fabrik posts as <slug>[bot] and arbeithand is the separate human. Nothing here
// reads the engine's identity, so the scenario is identical under E2E_AUTH_MODE=pat
// and app, and needs no reviewer-token fallback. A4 (the item's own bot comments never
// resume) is observable only for the 🏭-prefix path through the nudges; a real [bot]
// login classification is left to the App-auth self-recognition scenario.
//
// Filed in alpha only: the engine log carries the issue number but not the repo, and
// the needle is unique to this scenario.
//
// Wall-clock: ~10-15 min. Cost: ~$0.15-0.40 (one Specify comment-review invocation
// after the resume).
func TestPauseLiftedOnlyByPostPauseHumanComment(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	const gap = 6 * time.Second // > 1s timestamp resolution, with margin
	stamp := time.Now().UTC()
	nonce := fmt.Sprintf("pra%s", stamp.Format("150405"))
	oldNonce := nonce + "old"
	newNonce := nonce + "new"

	ensureEngineLabelExists(t, env, env.RepoAlpha, pausedLabel)
	ensureEngineLabelExists(t, env, env.RepoAlpha, pausedAwaitingInputLabel)

	// 1. File the issue, off-board.
	num := FileIssue(t, env, env.RepoAlpha,
		fmt.Sprintf("e2e pause resume anchor (%s)", stamp.Format("15:04:05")),
		`## Goal

Verify that a pause is lifted only by a human comment made after it (ADR-1813).

This issue is filed by the e2e harness. There is nothing to specify or build: any
comment on it asking for work can be answered with "no change needed".`)
	t.Logf("filed %s#%d off-board (invisible to the engine)", env.RepoAlpha, num)

	// 2. Old human comment, then the pause.
	CommentOnIssue(t, env, env.RepoAlpha, num,
		fmt.Sprintf("Old human comment %s: posted before the pause. It must not lift it.", oldNonce))
	time.Sleep(gap)
	AddLabel(t, env, env.RepoAlpha, num, pausedLabel)
	AddLabel(t, env, env.RepoAlpha, num, pausedAwaitingInputLabel)
	time.Sleep(gap)

	// 3. Move onto the board. Capture the log offset first so the scan covers
	// every line the engine emits about this item from the move onwards.
	offset := LogOffset(t, env)
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	movedAt := time.Now()
	t.Logf("moved %s#%d to Specify while paused, with a pre-pause human comment", env.RepoAlpha, num)

	// 4. A1: the refusal line for this issue, then the held window.
	needle := pauseRefusalLogNeedle(num)
	line := WaitForLogLine(t, env, needle, offset, 8*time.Minute)
	t.Logf("refusal line observed: %s", line)

	for i := 1; i <= 2; i++ {
		CommentOnIssue(t, env, env.RepoAlpha, num,
			fmt.Sprintf("%s\n\nRe-evaluation nudge %d (%s). Never resume-eligible.", nudgeCommentPrefix, i, nonce))
		pollSleep(pollBase())
	}
	if remaining := time.Until(movedAt.Add(4 * time.Minute)); remaining > 0 {
		time.Sleep(remaining)
	}

	labels := IssueLabels(t, env, env.RepoAlpha, num)
	for _, want := range []string{pausedLabel, pausedAwaitingInputLabel} {
		if !slices.Contains(labels, want) {
			t.Fatalf("pause was lifted by a comment that predates it: %s#%d lost %q (labels: %v)",
				env.RepoAlpha, num, want, labels)
		}
	}
	old := requireCommentReactions(t, env, env.RepoAlpha, num, oldNonce)
	if old.Matches != 1 {
		t.Fatalf("expected exactly one old comment containing %q, found %d", oldNonce, old.Matches)
	}
	if old.Eyes != 0 || old.Rocket != 0 {
		t.Fatalf("old pre-pause comment was processed while the pause held: 👀=%d 🚀=%d", old.Eyes, old.Rocket)
	}
	refusals := CountLogLines(t, env, needle, offset)
	t.Logf("A1: pause held for %s after the move; old comment has no 👀/🚀; %d refusal line(s) for #%d (1 at the move + up to 2 from nudges)",
		time.Since(movedAt).Round(time.Second), refusals, num)

	// 5. A2: no lift cycles, no re-stamp.
	events, err := tryIssueLabelEvents(env, env.RepoAlpha, num)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if l, u := countLabelEvents(events, pausedLabel); l != 1 || u != 0 {
		t.Fatalf("A2: %s events over the held window: labeled=%d unlabeled=%d, want 1/0 (a lift/re-add cycle or a re-stamp on the move)",
			pausedLabel, l, u)
	}
	t.Logf("A2: exactly one labeled and no unlabeled %s event across the window", pausedLabel)

	// 6. A3: a post-pause human comment resumes it.
	CommentOnIssue(t, env, env.RepoAlpha, num,
		fmt.Sprintf("New human comment %s: acknowledged, no change is needed. Please continue.", newNonce))
	waitForCommentReaction(t, env, env.RepoAlpha, num, newNonce, "eyes", 15*time.Minute)
	waitForCommentReaction(t, env, env.RepoAlpha, num, newNonce, "rocket", 20*time.Minute)

	events, err = tryIssueLabelEvents(env, env.RepoAlpha, num)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if _, u := countLabelEvents(events, pausedLabel); u < 1 {
		t.Fatalf("A3: new comment was processed (👀 then 🚀) but no %s unlabeled event exists — the pause was never lifted", pausedLabel)
	}
	t.Logf("A3: post-pause human comment resumed %s#%d: processed 👀→🚀 and %s unlabeled", env.RepoAlpha, num, pausedLabel)
	// Deliberately stop here: FileIssue's cleanup closes the issue, and waiting on
	// Specify completion would add cost with nothing further to prove.
}
