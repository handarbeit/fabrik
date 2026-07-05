//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestMergeTrainRestartSafety is the e2e proof of ADR-059 D5 restart-safety: the
// train reconstructs durable state after a process restart (empty in-memory
// in-flight map) instead of stalling or duplicating work. It targets the severe
// reconstruction bug fixed in PR #960: after a batch lands, its MERGED integration
// PR persists (ListPRs state=all, newest-first) and still carries the batch
// marker — a naive reconstruct would pick it, complete-deferred would find no
// still-Queued members, and the worker would exit, PERMANENTLY stalling the train
// after the first landing. This asserts that a restart with exactly that
// historical artifact present does NOT stall the next batch.
//
// Flow:
//  1. Land batch 1 (clean) → a merged integration PR now exists as history.
//  2. Restart the bed (clears the in-memory in-flight map — the definition of a
//     restart) using the harness lifecycle helpers.
//  3. Queue batch 2 (clean).
//  4. Assert batch 2 lands Queued → Done — proving reconstruct skips the historical
//     merged PR and proceeds fresh.
//
// NOT t.Parallel(): it restarts the shared bed, so it must not run concurrently
// with the other train scenarios. Go runs non-parallel tests to completion before
// resuming parallel ones, so this runs in isolation and leaves the bed up.
//
// Wall-clock: ~25–50 min (two full batch landings + a restart). Cost: low.
func TestMergeTrainRestartSafety(t *testing.T) {
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	requireTrainBed(t, env)

	const base = "main"

	// --- Batch 1: land it fully so a merged integration PR becomes history. ---
	logStart1 := LogOffset(t, env)
	b1a, b1aPR := QueueMember(t, env, env.RepoAlpha, base, "r1a", "e2e/train/restart/r1a.txt", "restart batch1 a\n")
	b1b, b1bPR := QueueMember(t, env, env.RepoAlpha, base, "r1b", "e2e/train/restart/r1b.txt", "restart batch1 b\n")
	WaitForLogLine(t, env, "landing complete", logStart1, 30*time.Minute)
	for _, m := range [][2]int{{b1a, b1aPR}, {b1b, b1bPR}} {
		WaitForProjectStatus(t, env, env.RepoAlpha, m[0], "Done", 10*time.Minute)
		waitForPRClosed(t, env, env.RepoAlpha, m[1], 10*time.Minute)
	}
	t.Logf("batch 1 landed — a merged integration PR is now history on the repo")

	// --- Restart: clears the in-memory in-flight map; historical merged PR remains. ---
	RestartFabrikTestBed(t, env)
	t.Logf("bed restarted — in-memory train state cleared, historical merged integration PR still on the repo")

	// --- Batch 2: must land despite the historical merged PR (no permanent stall). ---
	// logStart2 is taken AFTER the restart, so the "landing complete" wait matches
	// batch 2's landing, not batch 1's — proving reconstruct skipped the historical
	// merged PR and formed a fresh batch (the severe #960 bug this scenario targets).
	logStart2 := LogOffset(t, env)
	b2a, b2aPR := QueueMember(t, env, env.RepoAlpha, base, "r2a", "e2e/train/restart/r2a.txt", "restart batch2 a\n")
	b2b, b2bPR := QueueMember(t, env, env.RepoAlpha, base, "r2b", "e2e/train/restart/r2b.txt", "restart batch2 b\n")
	WaitForLogLine(t, env, "landing complete", logStart2, 30*time.Minute)
	for _, m := range [][2]int{{b2a, b2aPR}, {b2b, b2bPR}} {
		WaitForProjectStatus(t, env, env.RepoAlpha, m[0], "Done", 10*time.Minute)
		waitForPRClosed(t, env, env.RepoAlpha, m[1], 10*time.Minute)
	}
	assertNoStaleTrainArtifacts(t, env, env.RepoAlpha)
	t.Logf("restart-safety verified: batch 2 landed after restart — no stall from the historical merged PR")
}
