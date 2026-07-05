//go:build e2e

package e2e

import (
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

	clean1Issue, clean1PR := QueueMember(t, env, env.RepoAlpha, base, "clean1", "e2e/train/entries/clean1.txt", "clean entry 1\n")
	clean2Issue, clean2PR := QueueMember(t, env, env.RepoAlpha, base, "clean2", "e2e/train/entries/clean2.txt", "clean entry 2\n")
	poisonIssue, _ := QueueMember(t, env, env.RepoAlpha, base, "poison", "e2e/train/entries/poison.txt", "POISON — this member fails the combined check\n")
	t.Logf("queued 2 clean (#%d,#%d) + 1 poison (#%d); awaiting bisection", clean1Issue, clean2Issue, poisonIssue)

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
		WaitForProjectStatus(t, env, env.RepoAlpha, m.issue, "Done", 25*time.Minute)
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

	assertNoStaleTrainArtifacts(t, env, env.RepoAlpha)
	t.Logf("bisection contract verified: red batch → O(log N) bisect → poisoner ejected → survivors landed")
}
