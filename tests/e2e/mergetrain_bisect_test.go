//go:build e2e

package e2e

import (
	"fmt"
	"slices"
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

	// Prepare all three, then queue them back to back: queuing each as it is
	// created (QueueMember) let the train form a batch from the two clean members
	// before the poisoner was Queued. That batch was green and landed, the
	// poisoner went through alone, and nothing was ever bisected (0.0.83 gate run
	// 9). Paths are unique per run (a landed batch merges its files into main) and
	// stay under e2e/train/entries/, which the poison guard scans.
	stamp := time.Now().UTC().Format("20060102-150405")
	clean1Issue, clean1PR, clean1Item := PrepareMemberExactPath(t, env, env.RepoAlpha, base, "clean1",
		fmt.Sprintf("e2e/train/entries/clean1-%s.txt", stamp), "clean entry 1\n")
	clean2Issue, clean2PR, clean2Item := PrepareMemberExactPath(t, env, env.RepoAlpha, base, "clean2",
		fmt.Sprintf("e2e/train/entries/clean2-%s.txt", stamp), "clean entry 2\n")
	poisonIssue, _, poisonItem := PrepareMemberExactPath(t, env, env.RepoAlpha, base, "poison",
		fmt.Sprintf("e2e/train/entries/poison-%s.txt", stamp), "POISON — this member fails the combined check\n")

	placeOffset := LogOffset(t, env)
	for _, item := range []string{clean1Item, clean2Item, poisonItem} {
		SetIssueStatus(t, env, item, "Queued")
	}
	t.Logf("queued 2 clean (#%d,#%d) + 1 poison (#%d) together; verifying batch composition", clean1Issue, clean2Issue, poisonIssue)

	// The first batch must contain all three. Other parallel RepoAlpha scenarios may
	// add members of their own, so this is a containment check, not an exact one.
	waitForLogLineOrFail(t, env, logBatchSnapshot+env.RepoAlpha+": ", nil, placeOffset, 10*time.Minute)
	snapshot, ok := firstSnapshotForRepo(readLogLinesFrom(t, env, placeOffset), env.RepoAlpha)
	if !ok {
		t.Fatalf("batch snapshot line for %s vanished after being observed", env.RepoAlpha)
	}
	for _, n := range []int{clean1Issue, clean2Issue, poisonIssue} {
		if !slices.Contains(snapshot, n) {
			t.Fatalf("fixture: first batch snapshot for %s listed %v, missing #%d — a partial batch formed before all three "+
				"members were Queued, so there may be nothing to bisect. Not an engine regression; re-run", env.RepoAlpha, snapshot, n)
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
