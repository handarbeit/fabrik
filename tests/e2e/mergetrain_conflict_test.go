//go:build e2e

package e2e

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestMergeTrainConflictBisectPrefixRerere is the live counterpart of the sim bed's
// scripted-Claude conflict/bisect/prefix/rerere coverage (ADR-1454): it drives a
// four-member batch through a REAL-Claude conflict resolution, a red trial,
// bisection, trial-prefix reuse and rerere, and asserts what the engine did through
// GitHub state and the bed log. The property only a live run can check is how real
// Claude behaves under the real conflict-resolution prompt — the defect class of
// #1841, where a Go build instruction on a JS repo burned 51 turns and went unnoticed
// because nothing under the release gate ever created a textual conflict.
//
// It exercises three 0.0.83 fixes:
//   - #1841: the resolution prompt no longer orders build/test commands, and a
//     turn-limited exit that did resolve the conflict is kept, not ejected.
//   - #1835: re-assembly reuses the still-valid prefix of an abandoned trial's chain
//     ("reusing a recorded prefix (K/N member(s) already merged) for trial …").
//   - #1834: git rerere replays a recorded resolution when the same conflict recurs
//     ("conflict for #N fully resolved by git rerere replay — no Claude invocation").
//
// Construction. Four members are queued in the order A, B, C, P:
//   - A and B write the SAME path (e2e/train/conflict/shared-<stamp>.txt, outside
//     e2e/train/entries/ so train-poison-guard cannot see it) with divergent content,
//     so B add/add-conflicts with A in the trial.
//   - C is a clean member.
//   - P is the semantic poisoner (e2e/train/entries/, content "POISON…"), the same
//     train-poison-guard mechanism TestMergeTrainBisectionEjectsPoisoner uses.
//
// Queued order is ascending (StatusEnteredAt, Number) (#1833) and both keys agree
// here, so the trial is [A,B,C,P]: A merges, B conflicts and Claude resolves it
// (chain seed→A→B), C and P merge, the trial is red because of P. bisect splits at
// mid=2 and tests [A,B] (2/2 of the recorded chain → "reusing a recorded prefix
// (2/2 …)", green, no Claude), [C,P] (red), [C] (green), [P] (red, isolated). P is
// ejected, forgetPoisonerResolutions replays the trial-so-far state, [A,B,C]
// re-forms on the recorded prefix ("(3/3 …)") and lands. P stays in Queued after the
// ejection, re-batches as a singleton, and is rerouted off Queued (by the admission
// gate or by ejectRedSingleton).
//
// What is asserted (details and failure messages in conflictTrainReport.problems):
//   - A1: the initial assembly resolves B's conflict with Claude ("conflict for #B
//     resolved"), B is never ejected, and no conflict markers reach main.
//   - A2: that one Claude invocation uses at most maxConflictResolutionTurns turns
//     (far below the holding stage's 50-turn comment cap).
//   - A3: P is isolated as the poisoner and ejected.
//   - A4: after the bisect line, the [A,B] trial logs "reusing a recorded prefix
//     (2/2 member(s) already merged)" and no Claude is invoked for any member
//     until the survivors re-form.
//   - A5, the GUARANTEED part: B has exactly one Claude invocation over the whole
//     run, and forgetPoisonerResolutions logs no "did not fully replay via rerere"
//     warning. A B-onto-A re-assembly that logs the rerere replay line is NOT
//     guaranteed by this shape: prefix reuse covers every re-form, so B is never
//     re-merged by a logged assembly, and the only unconditional re-merge is inside
//     forgetPoisonerResolutions, which logs nothing on success. The replay line is
//     therefore asserted only CONDITIONALLY — if main moved mid-run and the train
//     rebuilt on the new base ("(main moved) — rebasing off the new base"), the
//     changed base SHA misses the prefix cache, B is re-merged, and the replay line
//     for B must follow.
//   - A6: A, B and C land (Done/closed, member PRs closed) and P ends off Queued and
//     not Done.
//
// NOT t.Parallel(): the train forms from every item in Queued on a repo, so any
// concurrently queued sibling member would join this batch and break its shape. As
// with TestMergeTrainRedSingletonReroutesOffQueued, a non-parallel test runs to
// completion before any parallel Alpha merge-train scenario resumes.
//
// Batch-composition race. A worker dispatches on the first poll that sees ANY Queued
// member, and each member's creation is many seconds of gh calls. So all four members
// are fully prepared first (P's PR last, giving its own CI the least time to finish),
// and only then moved to Queued back to back. The first "batch snapshot" line is then
// checked to list exactly A, B, C, P in order — proving the merge order at runtime and
// failing loudly on a premature partial batch or on stale Queued leftovers.
//
// Known race (shared with TestMergeTrainBisectionEjectsPoisoner): if P's own PR CI
// completes red before the batch forms, the admission gate (#1821, active when the
// stage before Queued has wait_for_ci: true) defers P before bisection. The scenario
// fails fast with that named cause rather than timing out; a re-run resolves it.
//
// Prerequisites: see tests/e2e/README.md prerequisite #22 (Queued column, merge_train
// on, train-poison-guard required on fabrik-test-alpha, real Claude usable from the
// Queued holding stage). Skips cleanly if the bed is not train-capable.
//
// Wall-clock: ~45–80 min (unmeasured estimate: 4 member CI runs, ~6 trial CI cycles,
// ~1 follow-up cycle for P). Cost: ONE real Claude invocation (conflict resolution)
// plus the bed reviewer's reviews of four member PRs; ~6 trial CI cycles.
func TestMergeTrainConflictBisectPrefixRerere(t *testing.T) {
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	requireTrainBed(t, env)
	assertTrainPoisonGuardRequired(t, env, env.RepoAlpha)

	const base = "main"
	repo := env.RepoAlpha
	logStart := LogOffset(t, env)

	// Unique per run: a landed batch writes the conflict file to main.
	stamp := time.Now().UTC().Format("20060102-150405")
	sharedPath := fmt.Sprintf("e2e/train/conflict/shared-%s.txt", stamp)
	cleanPath := fmt.Sprintf("e2e/train/conflict/clean-%s.txt", stamp)
	poisonPath := fmt.Sprintf("e2e/train/entries/conflict-poison-%s.txt", stamp)

	aIssue, aPR, aItem := PrepareMemberExactPath(t, env, repo, base, "conflictA", sharedPath, "alpha line\n")
	bIssue, bPR, bItem := PrepareMemberExactPath(t, env, repo, base, "conflictB", sharedPath, "bravo line\n")
	cIssue, cPR, cItem := PrepareMemberExactPath(t, env, repo, base, "conflictC", cleanPath, "clean entry\n")
	pIssue, _, pItem := PrepareMemberExactPath(t, env, repo, base, "conflictP", poisonPath, "POISON — this member fails the combined check\n")

	// Back to back, in merge order, so no train worker can form a partial batch.
	placeOffset := LogOffset(t, env)
	for _, m := range []struct {
		issue int
		item  string
	}{{aIssue, aItem}, {bIssue, bItem}, {cIssue, cItem}, {pIssue, pItem}} {
		SetIssueStatus(t, env, m.item, "Queued")
	}
	t.Logf("queued A=#%d B=#%d C=#%d P=#%d (shared path %s); verifying batch composition", aIssue, bIssue, cIssue, pIssue, sharedPath)

	// The batch must be exactly [A,B,C,P] in that order.
	waitForLogLineOrFail(t, env, logBatchSnapshot+repo+": ", nil, placeOffset, 10*time.Minute)
	snapshot, ok := firstSnapshotForRepo(readLogLinesFrom(t, env, placeOffset), repo)
	if !ok {
		t.Fatalf("batch snapshot line for %s vanished after being observed", repo)
	}
	if want := []int{aIssue, bIssue, cIssue, pIssue}; !intsEqual(snapshot, want) {
		t.Fatalf("first batch snapshot for %s listed %v, want exactly %v in that order — a partial batch formed before all four were Queued, "+
			"or stale Queued items from an earlier run joined it; clear the Queued column and re-run", repo, snapshot, want)
	}

	// Bisection must engage. Fail fast, naming the cause, on the known early exits.
	waitForLogLineOrFail(t, env, logBisecting, map[string]string{
		fmt.Sprintf(logDeferringFmt, pIssue): "the admission gate (#1821) deferred P because its own PR CI went red before the batch formed, " +
			"so no bisection will happen (known race — see this test's doc comment); re-run",
		fmt.Sprintf(logCannotResolveFmt, bIssue): "the engine could not resolve B's conflict and ejected it (the #1841 failure mode)",
	}, logStart, 30*time.Minute)
	t.Logf("bisection engaged on the red batch")

	// A3 (GitHub side): P is ejected.
	WaitForIssueComment(t, env, repo, pIssue, "merge-train — ejected", 25*time.Minute)
	t.Logf("poisoner #%d ejected", pIssue)

	// A6: A, B, C land, member PRs closed.
	for _, m := range []struct {
		name      string
		issue, pr int
	}{{"A", aIssue, aPR}, {"B", bIssue, bPR}, {"C", cIssue, cPR}} {
		WaitForMemberLanded(t, env, repo, m.issue, 25*time.Minute)
		waitForPRClosed(t, env, repo, m.pr, 5*time.Minute)
		t.Logf("member %s #%d landed (PR #%d closed)", m.name, m.issue, m.pr)
	}

	// A1 (GitHub side): B was never ejected, and no conflict markers reached main.
	bodies, err := tryPRComments(env, repo, bIssue)
	if err != nil {
		t.Fatalf("read comments on B #%d: %v", bIssue, err)
	}
	for _, b := range bodies {
		if strings.Contains(b, "merge-train — ejected") {
			t.Fatalf("B #%d carries a merge-train ejection comment although it landed — its conflict resolution was ejected at some point:\n%s", bIssue, b)
		}
	}
	shared := readRepoFile(t, env, repo, base, sharedPath)
	for _, marker := range []string{"<<<<<<<", "=======", ">>>>>>>"} {
		if strings.Contains(shared, marker) {
			t.Fatalf("%s on %s contains conflict marker %q after landing:\n%s", sharedPath, base, marker, shared)
		}
	}
	if strings.TrimSpace(shared) == "" {
		t.Fatalf("%s on %s is empty after landing — the resolution discarded both sides", sharedPath, base)
	}

	// A1–A5 (log side): the log window is complete now that the survivors have landed.
	report := analyzeConflictTrainLog(readLogLinesFrom(t, env, logStart), aIssue, bIssue, cIssue, pIssue)
	if problems := report.problems(maxConflictResolutionTurns); len(problems) > 0 {
		t.Fatalf("merge-train conflict/bisect/prefix/rerere contract violated:\n  - %s", strings.Join(problems, "\n  - "))
	}
	t.Logf("log contract verified: B resolved by Claude in %d turn(s) (bound %d), first half reused prefix with no Claude, "+
		"B invoked exactly once, main moved=%v (rerere replay after move=%v)",
		report.BTurns, maxConflictResolutionTurns, report.MainMovedIdx >= 0, report.BReplayAfterMove)

	// A6: P ends off Queued and not Done. After the ejection it stays in Queued, re-batches
	// as a singleton and is rerouted off Queued by the admission gate or ejectRedSingleton
	// (~one more CI cycle).
	deadline := time.Now().Add(25 * time.Minute)
	var pStatus string
	for time.Now().Before(deadline) {
		pStatus = projectStatus(t, env, repo, pIssue)
		if pStatus == "Done" {
			t.Fatalf("poisoner #%d reached Done — bisection failed to keep it out", pIssue)
		}
		if pStatus != "" && pStatus != "Queued" {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if pStatus == "" || pStatus == "Queued" {
		t.Fatalf("poisoner #%d still %q after 25m — expected it rerouted off Queued", pIssue, pStatus)
	}
	t.Logf("poisoner #%d ends off Queued (status=%q), not Done", pIssue, pStatus)

	WaitForNoStaleTrainArtifacts(t, env, repo, 5*time.Minute)
}

// readRepoFile returns the decoded contents of path at ref on repo, failing the test
// on any error.
func readRepoFile(t *testing.T, env *Env, repo, ref, path string) string {
	t.Helper()
	out, err := ghOutput(env, "api", fmt.Sprintf("repos/%s/contents/%s?ref=%s", repo, path, ref), "--jq", ".content")
	if err != nil {
		t.Fatalf("read %s@%s on %s: %v\n%s", path, ref, repo, err, out)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(out), ""))
	if err != nil {
		t.Fatalf("decode %s@%s on %s: %v", path, ref, repo, err)
	}
	return string(raw)
}
