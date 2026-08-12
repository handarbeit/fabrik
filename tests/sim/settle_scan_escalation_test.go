package sim

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// This file is R1: one scenario pair — retry-then-clear, and
// retry-past-MaxRetries-then-escalate — per settle scan matching the
// property "owns a fabrik:awaiting-* label and escalates to fabrik:paused
// after MaxRetries sustained failures of the underlying call". Seven scans
// match on current main (see the inventory table in README.md and the PR
// description):
//
//	scan                          | label                          | ADR      | covered here
//	------------------------------|----------------------------------|----------|-------------
//	settleNoWorkNeededScan        | fabrik:awaiting-done            | ADR-060  | TestSettleScan_AwaitingDone (+ ordering: no_work_needed_test.go)
//	settleMergeTrainMemberCloses  | fabrik:awaiting-member-close    | ADR-061  | TestSettleScan_AwaitingMemberClose
//	settleNonDefaultBaseCloses    | fabrik:awaiting-close           | ADR-1097 | TestSettleScan_AwaitingClose
//	settleChildPlacements         | fabrik:awaiting-placement       | ADR-062  | TestSettleScan_AwaitingPlacement
//	settleAwaitingAdvanceScan     | fabrik:awaiting-advance         | ADR-1422 | TestSettleScan_AwaitingAdvance
//	settleRunawayGuardAlertScan   | fabrik:awaiting-runaway-alert   | ADR-1533 | TestSettleScan_AwaitingRunawayAlert
//	settleAwaitingCIScan          | fabrik:awaiting-ci              | ADR-1270 | TestSettleScan_AwaitingCI
//
// Six of the seven are direct-seed + fault injection: seed a board item
// carrying the scan's own marker label plus whatever minimal state the scan
// needs (never a full pipeline dispatch — see each scan's own doc comment
// for why board.Items admission bypasses itemMayNeedWork entirely), then
// fault the one GitHub call the scan retries, narrowed to that item so a
// scenario with more than one seeded item never cross-contaminates (see
// simgh/fault.go's own doc comment on why FailWhen's narrowing is what makes
// these scans testable at all). settleAwaitingCIScan is the exception: it
// runs the full catchUpPhase1Handlers chain, so its two scenarios use two
// different techniques — see its own doc comment below.
//
// Non-vacuity (AC8): every escalation scenario asserts the item is NOT yet
// fabrik:paused after fewer than MaxRetries failing polls, and IS paused
// (with the scan's own literal comment prefix, matched by HasPrefix per
// #1320's discriminator convention) after exactly MaxRetries — and that the
// comment is posted exactly once even after further polls. A scan whose
// MaxRetries cutover was neutralised (escalating on the first failure, or
// never escalating, or re-posting the comment every poll) fails one of
// these.

// settleScanMaxRetries is the MaxRetries budget used by every scenario in
// this file — small (2), per this issue's own Plan-stage runtime-budget
// constraint, mirroring TestCIFixReinvokeCycleLimit's maxCycles=2 precedent.
const settleScanMaxRetries = 2

// errInjectedSettleFault is a generic, non-transient injected error for
// scans that don't care about the specific error shape (everything in this
// file except TestSettleScan_AwaitingCI's FetchItemDetails fault, which
// must specifically avoid the rate-limit/5xx/timeout phrases
// isTransientAPIError recognizes — see that scan's own doc comment).
var errInjectedSettleFault = errors.New("simgh: injected settle-scan fault")

// settleScanEnv returns a fresh Env wired for direct-seed settle-scan
// scenarios: smokeStages' 7-stage pipeline (so every column a scenario
// needs — Review, Validate, Done — already exists) plus one extra
// "Backlog" column for TestSettleScan_AwaitingPlacement's stranded-child
// seed, and settleScanMaxRetries as the escalation budget.
func settleScanEnv(t *testing.T) *Env {
	t.Helper()
	stgs := smokeStages()
	cols := append(stageColumns(stgs), "Backlog")
	return NewEnv(t, EnvOptions{
		Stages:  stgs,
		Columns: cols,
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxRetries = settleScanMaxRetries
		},
	})
}

// assertNotPausedYet fails the test if num already carries fabrik:paused —
// the "not one poll early" half of AC8's non-vacuity requirement.
func assertNotPausedYet(t *testing.T, env *Env, num int, afterPolls int) {
	t.Helper()
	if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
		t.Fatalf("#%d already fabrik:paused after only %d failing poll(s) — MaxRetries=%d, so escalation must not fire this early (non-vacuity: an unconditional/premature escalation would pass the eventual assertion too)",
			num, afterPolls, settleScanMaxRetries)
	}
}

// countCommentsWithPrefix counts how many of comments start with prefix —
// AC8's "no duplicate escalation comment on a later poll" half.
func countCommentsWithPrefix(comments []gh.Comment, prefix string) int {
	n := 0
	for _, c := range comments {
		if strings.HasPrefix(c.Body, prefix) {
			n++
		}
	}
	return n
}

// assertEscalatedExactlyOnce is the common escalation-episode assertion
// shared by every scan in this file except TestSettleScan_AwaitingCI (whose
// escalation reaches the item via a different code path but shares this
// same shape once reached): after extraPolls further poll cycles, the
// scan's own comment (matched by HasPrefix, never Contains — #1320) appears
// exactly once, fabrik:paused is present, markerLabel is absent (all seven
// scans remove their own marker on escalation — see each escalateX's doc
// comment), and the issue stays open for operator triage.
func assertEscalatedExactlyOnce(t *testing.T, env *Env, num int, markerLabel, commentPrefix string, extraPolls int) {
	t.Helper()
	WaitForIssueLabel(t, env, num, "fabrik:paused", 5)
	WaitForLabelAbsent(t, env, num, markerLabel, 5)
	if got := countCommentsWithPrefix(projectItem(t, env, num).Comments, commentPrefix); got != 1 {
		t.Fatalf("expected exactly 1 escalation comment (prefix %q) right after escalation, got %d", commentPrefix, got)
	}
	if item := projectItem(t, env, num); item.IsClosed {
		t.Error("escalated issue must remain open for operator triage")
	}
	RunPolls(t, env, extraPolls)
	if got := countCommentsWithPrefix(projectItem(t, env, num).Comments, commentPrefix); got != 1 {
		t.Errorf("expected exactly 1 escalation comment (prefix %q) after %d further poll(s), got %d — a paused item's marker-removed state must not re-trigger escalation", commentPrefix, extraPolls, got)
	}
}

// --- fabrik:awaiting-done (ADR-060, settleNoWorkNeededScan) ---------------

// TestSettleScan_AwaitingDone covers R1 for settleNoWorkNeededScan
// (ADR-060). Direct-seeds an item already at the "Done" column (not
// closed) — the sub-case where settleNoWorkNeeded's label/comment/board-move
// steps are all already-satisfied (item.Status == "Done" skips them
// entirely, per that function's own doc comment) and only the final
// CloseIssue call is outstanding, so this scenario exercises exactly the
// call this scan retries, with nothing else in the way.
func TestSettleScan_AwaitingDone(t *testing.T) {
	t.Parallel()

	t.Run("retry-then-clear", func(t *testing.T) {
		t.Parallel()
		env := settleScanEnv(t)
		num := FileIssue(t, env, "awaiting-done retry-then-clear", "body", "Done", "fabrik:awaiting-done")
		env.Sim.Faults().FailNTimes("CloseIssue", 1, errInjectedSettleFault)

		RunPolls(t, env, 2)
		WaitForLabelAbsent(t, env, num, "fabrik:awaiting-done", 10)
		WaitForIssueClosed(t, env, num, 5)
		if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
			t.Error("a settle scan that recovered on its own must never leave fabrik:paused behind")
		}
	})

	t.Run("escalates after MaxRetries", func(t *testing.T) {
		t.Parallel()
		env := settleScanEnv(t)
		num := FileIssue(t, env, "awaiting-done escalation", "body", "Done", "fabrik:awaiting-done")
		env.Sim.Faults().FailAlways("CloseIssue", errInjectedSettleFault)

		RunPoll(t, env)
		assertNotPausedYet(t, env, num, 1)

		RunPoll(t, env)
		assertEscalatedExactlyOnce(t, env, num, "fabrik:awaiting-done", "🏭 **Fabrik — no-work-needed completion failed**", 3)
	})
}

// --- fabrik:awaiting-member-close (ADR-061) -------------------------------

// TestSettleScan_AwaitingMemberClose covers R1 for
// settleMergeTrainMemberCloses (ADR-061). This scan has no item.Status
// precondition at all — any open, non-paused item carrying the marker
// qualifies — so this seeds at "Review" purely for variety.
func TestSettleScan_AwaitingMemberClose(t *testing.T) {
	t.Parallel()

	t.Run("retry-then-clear", func(t *testing.T) {
		t.Parallel()
		env := settleScanEnv(t)
		num := FileIssue(t, env, "awaiting-member-close retry-then-clear", "body", "Review",
			"stage:Review:complete", "fabrik:awaiting-member-close")
		env.Sim.Faults().FailNTimes("CloseIssue", 1, errInjectedSettleFault)

		RunPolls(t, env, 2)
		WaitForLabelAbsent(t, env, num, "fabrik:awaiting-member-close", 10)
		WaitForIssueClosed(t, env, num, 5)
	})

	t.Run("escalates after MaxRetries", func(t *testing.T) {
		t.Parallel()
		env := settleScanEnv(t)
		num := FileIssue(t, env, "awaiting-member-close escalation", "body", "Review",
			"stage:Review:complete", "fabrik:awaiting-member-close")
		env.Sim.Faults().FailAlways("CloseIssue", errInjectedSettleFault)

		RunPoll(t, env)
		assertNotPausedYet(t, env, num, 1)

		RunPoll(t, env)
		assertEscalatedExactlyOnce(t, env, num, "fabrik:awaiting-member-close", "🏭 **Fabrik — merge-train member-issue close failed**", 3)
	})
}

// --- fabrik:awaiting-close (ADR-1097) -------------------------------------

// TestSettleScan_AwaitingClose covers R1 for settleNonDefaultBaseCloses
// (ADR-1097) — structurally identical to settleMergeTrainMemberCloses
// (same retried call, same shared escalateSettle shape), so mirrors it
// exactly aside from the label/comment identifiers.
func TestSettleScan_AwaitingClose(t *testing.T) {
	t.Parallel()

	t.Run("retry-then-clear", func(t *testing.T) {
		t.Parallel()
		env := settleScanEnv(t)
		num := FileIssue(t, env, "awaiting-close retry-then-clear", "body", "Validate",
			"stage:Validate:complete", "fabrik:awaiting-close")
		env.Sim.Faults().FailNTimes("CloseIssue", 1, errInjectedSettleFault)

		RunPolls(t, env, 2)
		WaitForLabelAbsent(t, env, num, "fabrik:awaiting-close", 10)
		WaitForIssueClosed(t, env, num, 5)
	})

	t.Run("escalates after MaxRetries", func(t *testing.T) {
		t.Parallel()
		env := settleScanEnv(t)
		num := FileIssue(t, env, "awaiting-close escalation", "body", "Validate",
			"stage:Validate:complete", "fabrik:awaiting-close")
		env.Sim.Faults().FailAlways("CloseIssue", errInjectedSettleFault)

		RunPoll(t, env)
		assertNotPausedYet(t, env, num, 1)

		RunPoll(t, env)
		assertEscalatedExactlyOnce(t, env, num, "fabrik:awaiting-close", "🏭 **Fabrik — non-default-base explicit close failed**", 3)
	})
}

// --- fabrik:awaiting-placement (ADR-062) ----------------------------------

// TestSettleScan_AwaitingPlacement covers R1 for settleChildPlacements
// (ADR-062). Seeds the child at "Backlog" — an extra column
// settleScanEnv adds specifically for this scenario, standing in for the
// stranded state a spawned child ends up in when its initial placement
// fails (settleChildPlacement always targets "Specify", per
// resolveSpecifyOptionID's exact-match branch — smokeStages' first column).
// The retried call is UpdateProjectItemStatus, which — unlike CloseIssue —
// carries no issue Number in its Args (see mutationlog.go's Args doc
// comment: project-item calls key off itemID instead), so the fault must
// narrow on Args.Values[0] (itemID) rather than Args.Number.
func TestSettleScan_AwaitingPlacement(t *testing.T) {
	t.Parallel()

	t.Run("retry-then-clear", func(t *testing.T) {
		t.Parallel()
		env := settleScanEnv(t)
		num := FileIssue(t, env, "awaiting-placement retry-then-clear", "body", "Backlog", "fabrik:awaiting-placement")
		itemID := projectItem(t, env, num).ItemID
		env.Sim.Faults().FailWhen("UpdateProjectItemStatus",
			func(a simgh.Args) bool { return len(a.Values) > 0 && a.Values[0] == itemID },
			1, errInjectedSettleFault)

		RunPolls(t, env, 2)
		WaitForLabelAbsent(t, env, num, "fabrik:awaiting-placement", 10)
		WaitForProjectStatus(t, env, num, "Specify", 5)
	})

	t.Run("escalates after MaxRetries", func(t *testing.T) {
		t.Parallel()
		env := settleScanEnv(t)
		num := FileIssue(t, env, "awaiting-placement escalation", "body", "Backlog", "fabrik:awaiting-placement")
		itemID := projectItem(t, env, num).ItemID
		env.Sim.Faults().FailWhen("UpdateProjectItemStatus",
			func(a simgh.Args) bool { return len(a.Values) > 0 && a.Values[0] == itemID },
			simgh.Always, errInjectedSettleFault)

		RunPoll(t, env)
		assertNotPausedYet(t, env, num, 1)

		RunPoll(t, env)
		assertEscalatedExactlyOnce(t, env, num, "fabrik:awaiting-placement", "🏭 **Fabrik — spawned child board placement failed**", 3)
	})
}

// --- fabrik:awaiting-advance (ADR-1422) -----------------------------------

// TestSettleScan_AwaitingAdvance covers R1 for settleAwaitingAdvanceScan
// (ADR-1422). Seeds the item at "Review" (stage:Review:complete already
// present, mirroring the genuine precondition — this label is only ever
// applied after a real terminal-advance call already failed following
// genuine stage completion) so the scan's recordAdvanceOutcome computes
// next=Validate and retries the resulting UpdateProjectItemStatus call,
// narrowed on the item's own itemID exactly as
// TestSettleScan_AwaitingPlacement's fault is.
func TestSettleScan_AwaitingAdvance(t *testing.T) {
	t.Parallel()

	t.Run("retry-then-clear", func(t *testing.T) {
		t.Parallel()
		env := settleScanEnv(t)
		num := FileIssue(t, env, "awaiting-advance retry-then-clear", "body", "Review",
			"stage:Review:complete", "fabrik:awaiting-advance")
		itemID := projectItem(t, env, num).ItemID
		env.Sim.Faults().FailWhen("UpdateProjectItemStatus",
			func(a simgh.Args) bool { return len(a.Values) > 0 && a.Values[0] == itemID },
			1, errInjectedSettleFault)

		RunPolls(t, env, 2)
		WaitForLabelAbsent(t, env, num, "fabrik:awaiting-advance", 10)
		WaitForProjectStatus(t, env, num, "Validate", 5)
	})

	t.Run("escalates after MaxRetries", func(t *testing.T) {
		t.Parallel()
		env := settleScanEnv(t)
		num := FileIssue(t, env, "awaiting-advance escalation", "body", "Review",
			"stage:Review:complete", "fabrik:awaiting-advance")
		itemID := projectItem(t, env, num).ItemID
		env.Sim.Faults().FailWhen("UpdateProjectItemStatus",
			func(a simgh.Args) bool { return len(a.Values) > 0 && a.Values[0] == itemID },
			simgh.Always, errInjectedSettleFault)

		RunPoll(t, env)
		assertNotPausedYet(t, env, num, 1)

		RunPoll(t, env)
		// escalateAwaitingAdvanceFailure deliberately does NOT remove its own
		// marker label (see that function's doc comment — unlike the other
		// six scans, keeping it in place is what lets settleAwaitingAdvanceScan
		// pick the item back up with a fresh budget once an operator unpauses
		// it), so this assertion sequence can't reuse assertEscalatedExactlyOnce
		// verbatim — it inlines the same shape minus the marker-removal check.
		WaitForIssueLabel(t, env, num, "fabrik:paused", 5)
		if got := countCommentsWithPrefix(projectItem(t, env, num).Comments, "🏭 **Fabrik — terminal advance failed repeatedly**"); got != 1 {
			t.Fatalf("expected exactly 1 escalation comment right after escalation, got %d", got)
		}
		if !hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-advance") {
			t.Error("fabrik:awaiting-advance must remain in place after escalation (deliberate — see escalateAwaitingAdvanceFailure's doc comment), so a later unpause can be picked back up")
		}
		RunPolls(t, env, 3)
		if got := countCommentsWithPrefix(projectItem(t, env, num).Comments, "🏭 **Fabrik — terminal advance failed repeatedly**"); got != 1 {
			t.Errorf("expected exactly 1 escalation comment after further polls, got %d", got)
		}
	})
}

// --- fabrik:awaiting-runaway-alert (ADR-1533) -----------------------------

// TestSettleScan_AwaitingRunawayAlert covers R1 for
// settleRunawayGuardAlertScan (ADR-1533). Unlike the other six scans,
// fabrik:paused co-occurs with the marker from the very first application —
// fireRunawayGuard's pause never depends on the alert comment succeeding
// (see the scan's own doc comment) — so, per Research's own flagged risk,
// the retry-then-clear sub-case here must NOT gate on fabrik:paused's
// absence the way the other six's do; it is seeded present-and-paused
// throughout, and the only thing under test is whether the alert comment
// itself eventually lands (or, on the escalation path, whether the
// fallback comment does once MaxRetries is exhausted).
func TestSettleScan_AwaitingRunawayAlert(t *testing.T) {
	t.Parallel()

	t.Run("retry-then-clear", func(t *testing.T) {
		t.Parallel()
		env := settleScanEnv(t)
		num := FileIssue(t, env, "awaiting-runaway-alert retry-then-clear", "body", "Review",
			"stage:Review:complete", "fabrik:paused", "fabrik:awaiting-input", "fabrik:awaiting-runaway-alert")
		env.Sim.Faults().FailNTimes("AddComment", 1, errInjectedSettleFault)

		RunPolls(t, env, 2)
		WaitForLabelAbsent(t, env, num, "fabrik:awaiting-runaway-alert", 10)
		// fabrik:paused is the runaway guard's own, independent pause — it must
		// stay in place; only the alert-delivery marker is this scan's to clear.
		if !hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
			t.Error("fabrik:paused must remain — the runaway guard's pause is independent of alert-comment delivery")
		}
	})

	t.Run("escalates after MaxRetries (fallback comment)", func(t *testing.T) {
		t.Parallel()
		env := settleScanEnv(t)
		num := FileIssue(t, env, "awaiting-runaway-alert escalation", "body", "Review",
			"stage:Review:complete", "fabrik:paused", "fabrik:awaiting-input", "fabrik:awaiting-runaway-alert")
		// FailNTimes(2), not FailAlways: the primary alert must fail exactly
		// MaxRetries (2) times to trigger escalateRunawayAlertFailure — whose
		// own fallback AddComment call is then the 3rd call to this method,
		// landing after the fault is exhausted. A FailAlways fault here would
		// also fail the fallback forever, which is a real (and deliberately
		// different, see escalateRunawayAlertFailure's doc comment) scenario
		// this test does not cover.
		env.Sim.Faults().FailNTimes("AddComment", 2, errInjectedSettleFault)

		RunPoll(t, env)
		if !hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-runaway-alert") {
			t.Fatal("marker must still be present after only 1 failing poll — MaxRetries=2, so escalation must not fire this early")
		}

		RunPoll(t, env)
		WaitForLabelAbsent(t, env, num, "fabrik:awaiting-runaway-alert", 5)
		if got := countCommentsWithPrefix(projectItem(t, env, num).Comments, "🏭 **Fabrik merge-train — runaway guard alert delivery failed**"); got != 1 {
			t.Fatalf("expected exactly 1 fallback comment right after escalation, got %d", got)
		}
		RunPolls(t, env, 3)
		if got := countCommentsWithPrefix(projectItem(t, env, num).Comments, "🏭 **Fabrik merge-train — runaway guard alert delivery failed**"); got != 1 {
			t.Errorf("expected exactly 1 fallback comment after further polls, got %d", got)
		}
	})
}

// --- fabrik:awaiting-ci (ADR-1270) -----------------------------------------

// TestSettleScan_AwaitingCI covers R1 for settleAwaitingCIScan (ADR-1270),
// the one scan of the seven that runs the full catchUpPhase1Handlers chain
// rather than retrying a single isolated call — see its own doc comment
// (engine/ci_settle.go) for why. Its two sub-cases deliberately use two
// different techniques rather than one fault narrowed to a single method:
//
//   - "orphan column" (escalation): smokeStages declares no wait_for_ci
//     stage at all, so ANY status column is, from this scan's perspective,
//     "a fabrik:awaiting-ci item stranded on a column with no wait_for_ci
//     stage" — recordAwaitingCIOrphanRetry fires deterministically every
//     poll with no fault injection needed at all. This is the simplest,
//     most robust way to reach escalation for this scan.
//   - "retry-then-clear": needs a real wait_for_ci stage (ciFixStages(),
//     shared with ci_fix_reinvoke_test.go) and a real dispatched Validate
//     invocation, so a genuine fabrik:awaiting-ci item (with a real linked
//     PR) exists for the scan to evaluate. FetchItemDetails is faulted with
//     a *non-transient* error (errInjectedSettleFault — critically not one
//     of ghfault's rate-limit/5xx/timeout shapes, which isTransientAPIError
//     defers indefinitely without ever counting toward escalation, per
//     #1313) so the retry counter genuinely advances; clearing the fault
//     lets FetchItemDetails succeed, and with no required contexts
//     registered the CI gate evaluates vacuously clean (mirroring
//     TestCIFixReinvoke's own SeedRequiredContexts comment on this exact
//     vacuous-clean shortcut, deliberately exploited here instead of
//     avoided) and fabrik:awaiting-ci clears.
func TestSettleScan_AwaitingCI(t *testing.T) {
	t.Parallel()

	t.Run("retry-then-clear", func(t *testing.T) {
		t.Parallel()
		env := NewEnv(t, EnvOptions{
			Stages: ciFixStages(),
			// StartTime near real time.Now(), not the package's far-past
			// default — see TestCIFixReinvoke's identical StartTime comment:
			// with the default, the CIBackstopTimeout/ciWaitTimeout elapsed-
			// since-applied checks would instantly exceed their thresholds and
			// pause the item through a path unrelated to this scenario's own
			// MaxRetries-counted fault.
			StartTime: time.Now(),
			ConfigureCfg: func(cfg *engine.Config) {
				cfg.MaxCiFixCycles = 5
				cfg.MaxRetries = settleScanMaxRetries
			},
		})
		env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})

		num := FileIssue(t, env, "awaiting-ci retry-then-clear", "body", "Implement")
		WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)

		env.Sim.Faults().FailWhen("FetchItemDetails",
			func(a simgh.Args) bool { return a.Number == num },
			1, errInjectedSettleFault)

		RunPolls(t, env, 2)
		WaitForLabelAbsent(t, env, num, "fabrik:awaiting-ci", 20)
		// Must clear via a genuine gate evaluation, not via escalation (which
		// also removes fabrik:awaiting-ci — see escalateAwaitingCIOrphanFailure
		// — so WaitForLabelAbsent alone cannot tell the two apart).
		if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
			t.Error("fabrik:awaiting-ci cleared via escalation, not a genuine CI-gate evaluation — the fault must be exhausted (1 < MaxRetries) for this to be the retry-then-clear case")
		}
	})

	t.Run("escalates after MaxRetries (orphan column)", func(t *testing.T) {
		t.Parallel()
		env := settleScanEnv(t)
		num := FileIssue(t, env, "awaiting-ci orphan escalation", "body", "Review",
			"stage:Review:complete", "fabrik:awaiting-ci")

		RunPoll(t, env)
		assertNotPausedYet(t, env, num, 1)

		RunPoll(t, env)
		assertEscalatedExactlyOnce(t, env, num, "fabrik:awaiting-ci", "🏭 **Fabrik — awaiting-ci settle failed**", 3)
	})
}
