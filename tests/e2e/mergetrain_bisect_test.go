//go:build e2e

package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMergeTrainBisectionEjectsPoisoner is the e2e proof of ADR-059 D4: when a
// batch validates RED, the train halving-bisects to isolate the single poisoning
// member, ejects it, and lands the survivors — instead of O(N) per-member retests.
//
// Construction: three members with DISTINCT file paths (so the combined batch
// merges cleanly — this is a *semantic* cross-PR failure, not a textual conflict).
// Two are clean; one writes a file containing the sentinel "POISON". The test
// repo's required "train-poison-guard" check fails iff any file under
// e2e/train/entries/ contains "POISON", so:
//   - each clean member's own PR is green,
//   - the combined trial branch is RED (the poison file is present),
//   - bisection isolates the poison member, ejects it (back to Queued / paused),
//     and the two survivors re-form and land Queued → Done.
//
// Prerequisites (Phase-B bed setup): the Queued column + train-capable binary AND
// the `train-poison-guard` required check on fabrik-test-alpha (workflow committed
// from tests/e2e/testdata/train-poison-guard.yml — see README "Merge-train
// scenarios"). Skips cleanly if the Queued column is absent.
//
// Wall-clock: ~20–40 min (combined validate + O(log N) bisection rounds, each a
// full trial CI). Cost: low-moderate (no conflict resolution; bisection is git +
// CI only).
func TestMergeTrainBisectionEjectsPoisoner(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	requireTrainBed(t, env)

	const base = "main"
	logStart := LogOffset(t, env)

	// Queue all three PAUSED, wait until the engine has seen every one, then
	// release them together (removePausedConcurrently). Neither "queue each as it
	// is created" (0.0.83 gate run 9: the two clean members batched and landed
	// before the poisoner was Queued) nor "queue back to back" (run 11: the board
	// probe surfaced the poisoner a full poll before the clean members, so it
	// batched alone as a red singleton) guarantees one batch. A paused Queued
	// member is excluded from batching, so the engine can discover all three
	// without forming anything; once all three are in its cache, the release is
	// seen in one poll. QueueMemberPaused keeps paths unique per run under
	// e2e/train/entries/, which the poison guard scans.
	ensurePausedLabelExists(t, env, env.RepoAlpha)
	discoverOffset := LogOffset(t, env)
	clean1Issue, clean1PR := QueueMemberPaused(t, env, env.RepoAlpha, base, "clean1", "e2e/train/entries/clean1.txt", "clean entry 1\n")
	repauseOnFailure(t, env, env.RepoAlpha, clean1Issue)
	clean2Issue, clean2PR := QueueMemberPaused(t, env, env.RepoAlpha, base, "clean2", "e2e/train/entries/clean2.txt", "clean entry 2\n")
	repauseOnFailure(t, env, env.RepoAlpha, clean2Issue)
	poisonIssue, _ := QueueMemberPaused(t, env, env.RepoAlpha, base, "poison", "e2e/train/entries/poison.txt", "POISON — this member fails the combined check\n")
	repauseOnFailure(t, env, env.RepoAlpha, poisonIssue)
	members := []int{clean1Issue, clean2Issue, poisonIssue}

	for _, n := range members {
		seen := fmt.Sprintf("[#%d cache] probe: new item discovered", n)
		waitForLogMatch(t, env, discoverOffset, 10*time.Minute, fmt.Sprintf("the engine discovering paused member #%d", n),
			func(l string) bool { return strings.Contains(l, seen) })
	}
	t.Logf("engine has discovered all three paused members %v; releasing them together", members)

	placeOffset := LogOffset(t, env)
	if errs := removePausedConcurrently(env, env.RepoAlpha, members); len(errs) > 0 {
		t.Fatalf("could not release the members (they stay paused; nothing has formed): %v", errs)
	}

	// The first batch must contain all three. Other parallel RepoAlpha scenarios may
	// add members of their own, so this is a containment check, not an exact one.
	waitForLogLineOrFail(t, env, logBatchSnapshot+env.RepoAlpha+": ", nil, placeOffset, 10*time.Minute)
	snapshot, ok := firstSnapshotForRepo(readLogLinesFrom(t, env, placeOffset), env.RepoAlpha)
	if !ok {
		t.Fatalf("batch snapshot line for %s vanished after being observed", env.RepoAlpha)
	}
	for _, n := range members {
		if !slices.Contains(snapshot, n) {
			// A precondition guard (#1973): fires before any bisection assertion; the
			// poll saw the release mid-flight, so there is nothing to bisect.
			Inconclusive(t, "fixture: first batch snapshot for %s listed %v, missing #%d — the release straddled a poll boundary, "+
				"so there may be nothing to bisect. Not an engine regression", env.RepoAlpha, snapshot, n)
		}
	}
	t.Logf("first batch %v contains all three members; awaiting bisection", snapshot)

	// Bisection must run (combined batch is red) — the strongest internal signal.
	WaitForLogLine(t, env, "bisecting to isolate the poisoner", logStart, 25*time.Minute)
	t.Logf("bisection engaged on the red batch")

	// The poison member is ejected — asserted via the ejection comment the engine
	// posts on the member issue (posted on every ejection path: halving-isolated,
	// isolation-fail, and one-at-a-time fallback), so this is path-independent.
	WaitForIssueComment(t, env, env.RepoAlpha, poisonIssue, "merge-train — ejected", 25*time.Minute)
	t.Logf("poison member #%d ejected", poisonIssue)

	// The two survivors re-form and land Queued → Done, PRs closed.
	for _, m := range []struct {
		issue, pr int
	}{{clean1Issue, clean1PR}, {clean2Issue, clean2PR}} {
		WaitForMemberLanded(t, env, env.RepoAlpha, m.issue, 25*time.Minute)
		waitForPRClosed(t, env, env.RepoAlpha, m.pr, 5*time.Minute)
		t.Logf("survivor #%d landed (Done, PR #%d closed)", m.issue, m.pr)
	}

	// The poison member must NOT have landed: it is back in Queued (retry) or
	// paused after repeated ejection — never Done.
	if st := projectStatus(t, env, env.RepoAlpha, poisonIssue); st == "Done" {
		t.Fatalf("poison member #%d reached Done — bisection failed to eject it", poisonIssue)
	} else {
		t.Logf("poison member #%d correctly not landed (status=%q)", poisonIssue, st)
	}

	WaitForNoStaleTrainArtifacts(t, env, env.RepoAlpha, 2*time.Minute)
	t.Logf("bisection contract verified: red batch → O(log N) bisect → poisoner ejected → survivors landed")
}
