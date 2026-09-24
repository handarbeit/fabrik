package sim

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// This file covers ADR-1834's Acceptance Criteria 1-4: git rerere replays a
// merge-train conflict resolution across independent trials without paying for
// a second Claude invocation, a partial replay still reaches Claude for the
// unresolved remainder, a conflict with no recorded resolution behaves exactly
// as today, and AC1 is shown non-vacuous by disabling rerere and observing the
// second invocation return.
//
// Key fact underpinning every scenario here (verified by direct git
// experimentation, not assumed): git rerere's cache key is derived purely from
// the conflict's three-way content (the preimage), never the file's path or
// name. Two textually identical conflicts on two differently-named files, in
// two entirely independent merge-train batches, replay against the same
// rr-cache entry. This lets every scenario below use two ordinary, independent
// QueueMember batches rather than contriving a single batch's trial to be
// abandoned and re-formed (bisection, eviction, etc.) — simpler to construct
// and exactly as faithful to the "which trial" independence ADR-1834 requires
// (the rr-cache is scoped to the repo's shared bare clone, not to any one
// trial or batch).

// claudeResolveTwoPathConflicts returns a CommentScript that resolves a
// conflict spanning exactly two paths by overwriting both with the given
// content, staging everything, and committing — used by
// TestMergeTrainConflict_RerereReplayIsPartialAcrossTwoFiles (AC2) to resolve
// the one path git rerere could not replay, while incidentally re-writing the
// other (harmless: same content rerere already staged).
func claudeResolveTwoPathConflicts(pathA, contentA, pathB, contentB string) simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		for path, content := range map[string]string{pathA: contentA, pathB: contentB} {
			if err := os.WriteFile(filepath.Join(workDir, path), []byte(content), 0644); err != nil {
				return "", false, engine.TokenUsage{}, fmt.Errorf("write resolved %s: %w", path, err)
			}
		}
		addCmd := exec.Command("git", "add", "-A")
		addCmd.Dir = workDir
		if out, err := addCmd.CombinedOutput(); err != nil {
			return string(out), false, engine.TokenUsage{}, nil
		}
		commitCmd := exec.Command("git", "commit", "--no-edit", "-m",
			fmt.Sprintf("chore(merge-train): resolve conflict for #%d", issue.Number))
		commitCmd.Dir = workDir
		if out, err := commitCmd.CombinedOutput(); err != nil {
			return string(out), false, engine.TokenUsage{}, nil
		}
		return "resolved successfully", true, engine.TokenUsage{}, nil
	}
}

// hasConflictMarkers reports whether path (relative to workDir) currently
// contains live git conflict markers — used to directly observe, at the
// moment a scripted Claude invocation runs, which paths git rerere has
// already resolved-and-staged versus which are still genuinely conflicted.
func hasConflictMarkers(t *testing.T, workDir, path string) bool {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(workDir, path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.Contains(string(content), "<<<<<<<")
}

// TestMergeTrainConflict_RerereReplaysAcrossIndependentTrials is ADR-1834's
// Acceptance Criterion 1: a conflict Claude resolves once in one merge-train
// trial is resolved by git rerere alone — no second Claude invocation — the
// next time the identical conflict recurs in a later, independent trial
// (here: a later batch's own first trial), after the first trial's worktree
// and branch have already been deleted (cleanupTrialArtifacts runs
// unconditionally once a batch lands — see landGreenBatch/landMergeTrainBatch
// — so by the time the second batch is queued, nothing survives from the
// first trial except the shared bare clone's rr-cache).
func TestMergeTrainConflict_RerereReplaysAcrossIndependentTrials(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	startTrialVerdictSeeder(t, env, allGreenVerdict)
	env.Claude.ForStageComments("Queued", claudeResolveConflict("shared1.txt", "resolved-1\n"))

	numA, _ := QueueMember(t, env, "rerere-a", map[string]string{"shared1.txt": "content-from-a\n"})
	numB, _ := QueueMember(t, env, "rerere-b", map[string]string{"shared1.txt": "content-from-b\n"})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 20)
	WaitForIssueClosed(t, env, numA, 5)
	WaitForIssueClosed(t, env, numB, 5)

	if got := env.Claude.CommentCallCount("Queued"); got != 1 {
		t.Fatalf("expected exactly 1 Claude conflict-resolution call for the first batch's conflict, got %d", got)
	}

	// The merge-train's own draft/integration PRs auto-assign numbers from
	// the sim's internal per-repo counter, independent of this Env's own
	// issueSeqNext reservation sequence QueueMember draws from — resync
	// before queuing a second batch in the same repo or its numbers can
	// collide with one the first batch's landing already claimed.
	resyncIssueSeqAfterEngineActivity(t, env)

	// A second, independent batch reproduces the identical conflict content
	// (same "content-from-a\n" vs "content-from-b\n" three-way diff) on a
	// different file — deliberately different, so nothing about main's own
	// history (shared1.txt now holds the first batch's resolved content) can
	// interfere. The registered Claude script is still only scripted to write
	// "shared1.txt" — if it were invoked against this batch it would write to
	// the wrong path and the trial would never leave conflict, so a second
	// Claude call would surface as a stuck/timed-out batch, not a silent pass.
	numC, _ := QueueMember(t, env, "rerere-c", map[string]string{"shared2.txt": "content-from-a\n"})
	numD, _ := QueueMember(t, env, "rerere-d", map[string]string{"shared2.txt": "content-from-b\n"})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numC, "Done", 20)
	WaitForProjectStatus(t, env, numD, "Done", 20)
	WaitForIssueClosed(t, env, numC, 5)
	WaitForIssueClosed(t, env, numD, 5)

	if got := env.Claude.CommentCallCount("Queued"); got != 1 {
		t.Errorf("expected the second, textually-identical conflict to be resolved by git rerere replay with zero additional Claude invocations; CommentCallCount = %d, want 1", got)
	}
}

// TestMergeTrainConflict_RerereReplayIsPartialAcrossTwoFiles is ADR-1834's
// Acceptance Criterion 2: when a conflict spans two files and git rerere has
// a recorded resolution for only one of them, resolveConflictWithClaude is
// still invoked for the remainder — proven directly by inspecting, at the
// moment the scripted Claude invocation runs, that the rerere-replayable path
// already carries no conflict markers while the genuinely novel path still
// does.
func TestMergeTrainConflict_RerereReplayIsPartialAcrossTwoFiles(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	startTrialVerdictSeeder(t, env, allGreenVerdict)
	env.Claude.ForStageComments("Queued", claudeResolveConflict("seed-known.txt", "resolved-seed\n"))

	// Seed the recorded resolution: an ordinary single-file conflict, resolved
	// and landed like any other.
	numE, _ := QueueMember(t, env, "rerere-seed-e", map[string]string{"seed-known.txt": "known-a\n"})
	numF, _ := QueueMember(t, env, "rerere-seed-f", map[string]string{"seed-known.txt": "known-b\n"})
	RunPoll(t, env)
	WaitForProjectStatus(t, env, numE, "Done", 20)
	WaitForProjectStatus(t, env, numF, "Done", 20)
	WaitForIssueClosed(t, env, numE, 5)
	WaitForIssueClosed(t, env, numF, 5)
	if got := env.Claude.CommentCallCount("Queued"); got != 1 {
		t.Fatalf("expected exactly 1 Claude call to seed the recorded resolution, got %d", got)
	}
	resyncIssueSeqAfterEngineActivity(t, env)

	// A mixed conflict: "known2.txt" reproduces the seeded conflict's exact
	// content pattern (replayable), "fresh.txt" is a conflict git rerere has
	// never seen before (not replayable). The registered script asserts the
	// replay boundary directly before doing the part only Claude can do.
	var observedKnownHadMarkers, observedFreshHadMarkers bool
	env.Claude.ForStageComments("Queued",
		claudeResolveConflict("seed-known.txt", "resolved-seed\n"), // call #1 (seed, above)
		func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
			observedKnownHadMarkers = hasConflictMarkers(t, workDir, "known2.txt")
			observedFreshHadMarkers = hasConflictMarkers(t, workDir, "fresh.txt")
			return claudeResolveTwoPathConflicts("known2.txt", "resolved-seed\n", "fresh.txt", "resolved-fresh\n")(ctx, stage, issue, comments, workDir, opts)
		},
	)

	numG, _ := QueueMember(t, env, "rerere-mixed-g", map[string]string{
		"known2.txt": "known-a\n",
		"fresh.txt":  "fresh-a\n",
	})
	numH, _ := QueueMember(t, env, "rerere-mixed-h", map[string]string{
		"known2.txt": "known-b\n",
		"fresh.txt":  "fresh-b\n",
	})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numG, "Done", 20)
	WaitForProjectStatus(t, env, numH, "Done", 20)
	WaitForIssueClosed(t, env, numG, 5)
	WaitForIssueClosed(t, env, numH, 5)

	if got := env.Claude.CommentCallCount("Queued"); got != 2 {
		t.Errorf("expected the mixed conflict's novel path to still require a Claude invocation (2 total calls), got %d", got)
	}
	if observedKnownHadMarkers {
		t.Error("known2.txt still had live conflict markers when Claude ran — git rerere should have already replayed its recorded resolution")
	}
	if !observedFreshHadMarkers {
		t.Error("fresh.txt had no conflict markers when Claude ran — expected it to still be genuinely conflicted (no recorded resolution exists for it)")
	}
}

// TestMergeTrainConflict_NoRecordedResolutionUnchanged is ADR-1834's
// Acceptance Criterion 3: a conflict with no recorded resolution anywhere
// resolves exactly as today — a single Claude invocation, no rerere
// involvement at all (the negative-control counterpart to AC1/AC2 above).
func TestMergeTrainConflict_NoRecordedResolutionUnchanged(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	startTrialVerdictSeeder(t, env, allGreenVerdict)
	env.Claude.ForStageComments("Queued", claudeResolveConflict("novel.txt", "resolved-novel\n"))

	numA, _ := QueueMember(t, env, "no-history-a", map[string]string{"novel.txt": "unique-content-a\n"})
	numB, _ := QueueMember(t, env, "no-history-b", map[string]string{"novel.txt": "unique-content-b\n"})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 20)
	WaitForIssueClosed(t, env, numA, 5)
	WaitForIssueClosed(t, env, numB, 5)

	if got := env.Claude.CommentCallCount("Queued"); got != 1 {
		t.Errorf("expected exactly 1 Claude call for a conflict with no prior recorded resolution, got %d", got)
	}
}

// TestMergeTrainConflict_RerereReplayNonVacuous is ADR-1834's Acceptance
// Criterion 4: AC1's "no second Claude invocation" result is shown
// non-vacuous by disabling git rerere on the shared bare clone between the
// two batches and observing the second, textually-identical conflict DOES
// reach a second Claude invocation — proving AC1 passes because rerere is
// genuinely active, not by accident (e.g. a scenario bug that never re-forms
// the conflict at all).
func TestMergeTrainConflict_RerereReplayNonVacuous(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	startTrialVerdictSeeder(t, env, allGreenVerdict)
	env.Claude.ForStageComments("Queued",
		claudeResolveConflict("shared1.txt", "resolved-1\n"),
		claudeResolveConflict("shared2.txt", "resolved-2\n"),
	)

	numA, _ := QueueMember(t, env, "novacuity-a", map[string]string{"shared1.txt": "content-from-a\n"})
	numB, _ := QueueMember(t, env, "novacuity-b", map[string]string{"shared1.txt": "content-from-b\n"})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 20)
	WaitForIssueClosed(t, env, numA, 5)
	WaitForIssueClosed(t, env, numB, 5)

	if got := env.Claude.CommentCallCount("Queued"); got != 1 {
		t.Fatalf("expected exactly 1 Claude call for the first batch's conflict, got %d", got)
	}
	resyncIssueSeqAfterEngineActivity(t, env)

	// Force-disable rerere on the shared bare clone before the second batch —
	// a test-only override of the same repo-wide enablement ensureBareClone
	// (production) / buildWorktreeManager (this harness) applies by default.
	disableCmd := exec.Command("git", "config", "rerere.enabled", "false")
	disableCmd.Dir = env.WM.BaseDir()
	if out, err := disableCmd.CombinedOutput(); err != nil {
		t.Fatalf("disabling rerere on the shared bare clone: %s: %v", out, err)
	}

	numC, _ := QueueMember(t, env, "novacuity-c", map[string]string{"shared2.txt": "content-from-a\n"})
	numD, _ := QueueMember(t, env, "novacuity-d", map[string]string{"shared2.txt": "content-from-b\n"})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numC, "Done", 20)
	WaitForProjectStatus(t, env, numD, "Done", 20)
	WaitForIssueClosed(t, env, numC, 5)
	WaitForIssueClosed(t, env, numD, 5)

	if got := env.Claude.CommentCallCount("Queued"); got != 2 {
		t.Errorf("with rerere disabled, expected the identical conflict to require a second Claude invocation (2 total calls) — proving AC1's zero-Claude result is not vacuous; got %d", got)
	}
}
