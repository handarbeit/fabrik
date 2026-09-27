//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestMergeTrainSingletonFastPathLandsExactlyOnce is the live regression check for
// #1871 / ADR-1871 on the singleton fast path (#1644): a member landed alone by the
// fast path must land exactly ONCE — one "Landed via singleton fast path" comment
// and one issue close — even after the poll(s) that follow the landing, when a
// stale Queued snapshot could previously re-run the whole landing.
//
// No existing scenario reaches the fast path reliably (TestYoloAutoMergeLabel accepts
// either path and is parallel, so main moves under it; every other train scenario
// queues 2+ members). This one makes the fast path deterministic:
//
//   - NOT t.Parallel(): go runs non-parallel tests to completion before resuming
//     parallel ones, so main cannot move (the fast path requires the pinned base to be
//     an ancestor of the member head) and no sibling can join the batch (the train
//     batches every Queued item on the repo/base partition). Same rationale as
//     TestMergeTrainRedSingletonReroutesOffQueued.
//   - The member is PREPARED (PrepareMemberExactPath) but not queued until its own CI
//     is complete and green and its mergeable_state is clean/unstable. Queuing first
//     would let the first train poll see pending CI and take the trial path, which
//     lands via "Landed via batch PR" and proves nothing about the fast path.
//   - The bed log must show "singleton fast path taken for #N"; "singleton fast path
//     not taken for #N" is a named failure carrying the engine's own reason. A bed that
//     cannot support the fast path (e.g. branch protection needing an approval) fails
//     loudly rather than skipping, so the fast-path half of the coverage cannot
//     silently vanish.
//
// The required check run comes from the train-poison-guard workflow on Alpha
// (README prerequisite #18), so the scenario skips cleanly where it is not enrolled.
//
// Prerequisites: see tests/e2e/README.md → "Merge-train scenarios".
//
// Wall-clock: ~10–15 min (member CI ~1–2 min, one poll, landing, 3-poll settle wait).
// Cost: low (no Claude invocations).
func TestMergeTrainSingletonFastPathLandsExactlyOnce(t *testing.T) {
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	requireTrainBed(t, env)
	assertTrainPoisonGuardRequired(t, env, env.RepoAlpha)

	const base = "main"
	logStart := LogOffset(t, env)

	// A unique path under e2e/train/entries/ (the poison-guard's scanned directory):
	// landed files persist on main, so a fixed path would collide on the next run.
	path := fmt.Sprintf("e2e/train/entries/singleton-fastpath-%d.txt", time.Now().UnixNano())
	issue, pr, itemID := PrepareMemberExactPath(t, env, env.RepoAlpha, base, "singleton-fastpath", path,
		"singleton fast-path member (clean) — #1874 exactly-once landing\n")

	// Wait until the member's own CI is complete and green and the PR is mergeable,
	// BEFORE queuing, so the train's first poll is eligible for the fast path.
	deadline := time.Now().Add(10 * time.Minute)
	var conclusions []string
	var mergeState string
	for {
		cs, cerr := tryPRCheckRunConclusions(env, env.RepoAlpha, pr)
		ms, merr := tryPRMergeableState(env, env.RepoAlpha, pr)
		switch {
		case cerr != nil:
			t.Logf("transient error reading check runs of PR #%d: %v (will retry)", pr, cerr)
		case merr != nil:
			t.Logf("transient error reading mergeable_state of PR #%d: %v (will retry)", pr, merr)
		default:
			conclusions, mergeState = cs, ms
		}
		if cerr == nil && merr == nil && len(cs) > 0 && allSuccess(cs) && (ms == "clean" || ms == "unstable") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("member PR #%d never became fast-path eligible within 10m (check runs %v, mergeable_state %q) — "+
				"the fast path needs at least one completed green check run and a clean/unstable PR; "+
				"check the bed's branch protection and the train-poison-guard workflow", pr, conclusions, mergeState)
		}
		time.Sleep(10 * time.Second)
	}
	t.Logf("member PR #%d is fast-path eligible (check runs %v, mergeable_state %q); queuing issue #%d", pr, conclusions, mergeState, issue)
	SetIssueStatus(t, env, itemID, "Queued")

	// The fast path, not the trial path, must land it.
	taken := waitForLogLineOrFail(t, env,
		fmt.Sprintf("singleton fast path taken for #%d:", issue),
		map[string]string{
			fmt.Sprintf("singleton fast path not taken for #%d:", issue): "the member fell through to the trial path, so this run cannot exercise the fast path's exactly-once guarantee",
		}, logStart, 25*time.Minute)
	t.Logf("fast path taken: %s", strings.TrimSpace(taken))

	WaitForMemberLanded(t, env, env.RepoAlpha, issue, 10*time.Minute)
	WaitForIssueClosed(t, env, env.RepoAlpha, issue, 10*time.Minute)

	// The landing comment must be the fast path's own, and cite the member's own PR.
	landingPR, viaFastPath := waitForLandingPRDetail(t, env, env.RepoAlpha, pr, 5*time.Minute)
	if !viaFastPath || landingPR != pr {
		t.Fatalf("member PR #%d landing comment cites PR #%d (viaFastPath=%v); want the singleton fast path citing its own PR", pr, landingPR, viaFastPath)
	}
	t.Logf("member #%d landed via the singleton fast path (PR #%d)", issue, pr)

	AssertMembersLandedExactlyOnce(t, env, env.RepoAlpha,
		[]landedMember{{Name: "singleton-fastpath", Issue: issue, PR: pr}}, logStart)

	WaitForNoStaleTrainArtifacts(t, env, env.RepoAlpha, 2*time.Minute)
	t.Logf("singleton fast path verified: landed once, one landing comment, one close, no stale train artifacts")
}

// allSuccess reports whether every check-run conclusion is "success". In-progress
// or queued runs surface as "pending" (see tryPRCheckRunConclusions) and so fail it.
func allSuccess(conclusions []string) bool {
	for _, c := range conclusions {
		if c != "success" {
			return false
		}
	}
	return true
}
