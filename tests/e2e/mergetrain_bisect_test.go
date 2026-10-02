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
// Own partition (#1977, ADR-1977). The scenario runs on its own throwaway base:<branch>
// — its own (repo, base) train partition (ADR-1648) — so it is safe to run alongside
// every other shared test, including the yolo pipeline tests that enqueue on
// RepoAlpha/main under train "on". Before #1977 it ran t.Parallel() on main and its
// "first batch contains all three" check was a containment check precisely because
// siblings could add members. Every log read is scoped to this partition's trainKey.
// The train-poison-guard workflow fires on any pull_request (tests/e2e/testdata) and
// is only REQUIRED on main, so on the throwaway base the trial is still red but the
// survivors' landing needs no branch protection.
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

	repo := env.RepoAlpha
	base := fmt.Sprintf("e2e-bisect-%s", time.Now().UTC().Format("20060102-150405"))
	trainKey := repo + ":" + base // a non-default partition's key (mergeTrainKey)
	CreateThrowawayBaseBranch(t, env, repo, base)
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
	ensurePausedLabelExists(t, env, repo)
	discoverOffset := LogOffset(t, env)
	clean1Issue, clean1PR := QueueMemberPausedOnBase(t, env, repo, base, "clean1", "e2e/train/entries/clean1.txt", "clean entry 1\n")
	repauseOnFailure(t, env, repo, clean1Issue)
	clean2Issue, clean2PR := QueueMemberPausedOnBase(t, env, repo, base, "clean2", "e2e/train/entries/clean2.txt", "clean entry 2\n")
	repauseOnFailure(t, env, repo, clean2Issue)
	poisonIssue, _ := QueueMemberPausedOnBase(t, env, repo, base, "poison", "e2e/train/entries/poison.txt", "POISON — this member fails the combined check\n")
	repauseOnFailure(t, env, repo, poisonIssue)
	members := []int{clean1Issue, clean2Issue, poisonIssue}

	for _, n := range members {
		seen := fmt.Sprintf("[#%d cache] probe: new item discovered", n)
		waitForLogMatch(t, env, discoverOffset, 10*time.Minute, fmt.Sprintf("the engine discovering paused member #%d", n),
			func(l string) bool { return strings.Contains(l, seen) })
	}
	t.Logf("engine has discovered all three paused members %v; releasing them together", members)

	placeOffset := LogOffset(t, env)
	if errs := removePausedConcurrently(env, repo, members); len(errs) > 0 {
		t.Fatalf("could not release the members (they stay paused; nothing has formed): %v", errs)
	}

	// The first batch must contain all three. The partition is this test's own, but
	// the check stays a containment check: it is the cheaper guarantee and not
	// worth re-proving what the exact-composition tests assert.
	waitForLogLineOrFail(t, env, logBatchSnapshot+trainKey+": ", nil, placeOffset, 10*time.Minute)
	snapshot, ok := firstSnapshotForRepo(readLogLinesFrom(t, env, placeOffset), trainKey)
	if !ok {
		t.Fatalf("batch snapshot line for %s vanished after being observed", trainKey)
	}
	for _, n := range members {
		if !slices.Contains(snapshot, n) {
			// A precondition guard (#1973): fires before any bisection assertion; the
			// poll saw the release mid-flight, so there is nothing to bisect.
			Inconclusive(t, "fixture: first batch snapshot for %s listed %v, missing #%d — the release straddled a poll boundary, "+
				"so there may be nothing to bisect. Not an engine regression", trainKey, snapshot, n)
		}
	}
	t.Logf("first batch %v contains all three members; awaiting bisection", snapshot)

	// Bisection must run (combined batch is red) — the strongest internal signal.
	waitForLogMatch(t, env, logStart, 25*time.Minute, "bisection of "+trainKey, func(l string) bool {
		return strings.Contains(l, "combined Validate RED for "+trainKey+" (") && strings.Contains(l, logBisecting)
	})
	t.Logf("bisection engaged on the red batch")

	// The poison member is ejected — asserted via the ejection comment the engine
	// posts on the member issue (posted on every ejection path: halving-isolated,
	// isolation-fail, and one-at-a-time fallback), so this is path-independent.
	WaitForIssueComment(t, env, repo, poisonIssue, "merge-train — ejected", 25*time.Minute)
	t.Logf("poison member #%d ejected", poisonIssue)

	// The two survivors re-form and land Queued → Done, PRs closed.
	for _, m := range []struct {
		issue, pr int
	}{{clean1Issue, clean1PR}, {clean2Issue, clean2PR}} {
		WaitForMemberLanded(t, env, repo, m.issue, 25*time.Minute)
		waitForPRClosed(t, env, repo, m.pr, 5*time.Minute)
		t.Logf("survivor #%d landed (Done, PR #%d closed)", m.issue, m.pr)
	}

	// The poison member must NOT have landed: it is back in Queued (retry) or
	// paused after repeated ejection — never Done.
	if st := projectStatus(t, env, repo, poisonIssue); st == "Done" {
		t.Fatalf("poison member #%d reached Done — bisection failed to eject it", poisonIssue)
	} else {
		t.Logf("poison member #%d correctly not landed (status=%q)", poisonIssue, st)
	}

	WaitForNoStaleTrainArtifactsOnBase(t, env, repo, base, 2*time.Minute)
	t.Logf("bisection contract verified: red batch → O(log N) bisect → poisoner ejected → survivors landed")
}
