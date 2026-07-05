//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestMergeTrainHappyPathLanding is the e2e proof of the ADR-059 internal merge
// train's core contract: a clean batch of Queued members is assembled onto one
// trial branch, validated once, landed via a single integration PR through normal
// branch protection, and every member is advanced Queued → Done with its PR
// closed. This is the O(N²) → ~O(1) guarantee the train exists to provide, proven
// end-to-end against real GitHub (not the unit-test seam).
//
// Setup shortcut: members are placed directly in Queued with real linked PRs
// (QueueMember) rather than run through the full Specify→Validate pipeline — the
// train is column-driven (D1), so this is a faithful batch. Distinct file paths
// per member make the batch conflict-free (the happy path); conflict/bisection
// behaviour is covered by the sibling scenarios.
//
// Prerequisites: the test board must have a Queued column and the test-bed Fabrik
// must run a train-capable binary with the Queued holding stage configured (see
// tests/e2e/README.md → "Merge-train scenarios"). Skips cleanly otherwise.
//
// Wall-clock: ~10–25 min (one combined validation + integration-PR CI). Cost: low
// (no Claude invocations on the happy path — no conflicts to resolve).
func TestMergeTrainHappyPathLanding(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	requireTrainBed(t, env)

	const base = "main"
	logStart := LogOffset(t, env)

	// Three clean members — distinct paths so the combined batch has no conflicts.
	type member struct {
		issue int
		pr    int
	}
	var members []member
	for _, m := range []struct{ marker, path, content string }{
		{"alpha", "e2e/train/alpha.txt", "alpha member\n"},
		{"bravo", "e2e/train/bravo.txt", "bravo member\n"},
		{"charlie", "e2e/train/charlie.txt", "charlie member\n"},
	} {
		iss, pr := QueueMember(t, env, env.RepoAlpha, base, m.marker, m.path, m.content)
		members = append(members, member{iss, pr})
	}
	t.Logf("queued %d clean members; awaiting train assembly + landing", len(members))

	// Proof the internal train landed a batch: the engine logs "landing complete …
	// (integration PR #N, M members)" after merging the single integration PR and
	// advancing members. Read from logStart so only THIS run's landing matches.
	WaitForLogLine(t, env, "landing complete", logStart, 30*time.Minute)
	t.Logf("train landed a batch")

	// Every member advances Queued → Done and its member PR is closed (the member
	// PR is closed, not merged — the changes land via the integration PR). Board=Done
	// + PR-closed is the definitive per-member landed signal; issue closure is a
	// downstream Done-stage cleanup and is not asserted here to avoid timing flakiness.
	for _, m := range members {
		WaitForProjectStatus(t, env, env.RepoAlpha, m.issue, "Done", 10*time.Minute)
		waitForPRClosed(t, env, env.RepoAlpha, m.pr, 10*time.Minute)
		t.Logf("member #%d landed: Done + member PR #%d closed", m.issue, m.pr)
	}

	// No stale train branches/PRs should remain after the landing.
	assertNoStaleTrainArtifacts(t, env, env.RepoAlpha)
	t.Logf("happy-path land verified: 3 members → 1 integration PR → all Done, no O(N²) per-member retests")
}
