package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// ── checkCIGate ──────────────────────────────────────────────────────────────

func TestCheckCIGate_WaitForCIFalse_ClearsImmediately(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate"} // WaitForCI is nil

	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, PRSettleResult{})
	if blocked || ciFailure || timedOut {
		t.Errorf("expected all false when wait_for_ci not set, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
}

func TestCheckCIGate_NoPR_ClearsGate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{Status: PRMergeNoPR}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected clear when no PR, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
}

func TestCheckCIGate_NoCheckRuns_ClearsGate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{Status: PRMergeReady, Reason: "no CI configured", PR: &gh.PRDetails{Number: 5, HeadSHA: "sha1"}}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected clear for no check runs (R5), got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
}

func TestCheckCIGate_PostPushDelay_BlocksGate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)

	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	// hadChecks=true: settle returns "post-push registration delay (hadChecks)"
	settle := PRSettleResult{
		Status: PRMergeUnsettled,
		Reason: "post-push registration delay (hadChecks)",
		PR:     &gh.PRDetails{Number: 5, HeadSHA: "sha-new"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Error("expected blocked=true when zero check runs after previously seeing checks (post-push delay)")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false for post-push delay, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("fabrik:awaiting-ci must NOT be removed during post-push registration delay")
		}
	}
}

func TestCheckCIGate_AllGreen_ClearsGate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeReady,
		Reason: "all CI checks passed",
		CheckRuns: []gh.CheckRun{
			{Name: "build", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "completed", Conclusion: "success"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha2"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected clear for all-green CI, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
}

func TestCheckCIGate_Pending_BlocksNoLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeUnsettled,
		Reason: "CI checks pending",
		CheckRuns: []gh.CheckRun{
			{Name: "ci", Status: "in_progress"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha3"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Error("expected blocked=true for pending CI")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false for pending, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	// checkCIGate must not add fabrik:awaiting-ci when CI is only pending;
	// it was already applied by handleStageComplete when the stage completed.
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("checkCIGate must NOT add fabrik:awaiting-ci when CI is only pending")
		}
	}
	// stage:X:complete must not be added while CI is pending
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must NOT be added when CI is pending")
		}
	}
}

// TestCheckCIGate_Pending_ProgressStalled_TimesOut verifies the new
// liveness-stall dwell (ADR-1410, R2): check runs pending with no observable
// progress (no check-run content change recorded in the store) for
// CIWaitTimeout still escalates — the liveness path must be shown to
// actually fire, not merely to never fire. Progress is anchored on
// LinkedPRState.LastCIProgressAt, not the fabrik:awaiting-ci label's own
// applied-at time (mockGitHubClient's fetchLabelAppliedAtFn is deliberately
// left unset — this path must not consult it at all).
func TestCheckCIGate_Pending_ProgressStalled_TimesOut(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	eng.cfg.CIWaitTimeout = 1 * time.Nanosecond // tiny dwell

	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	// Link the SHA and record one check-run observation, stamping
	// LastCIProgressAt — then let the tiny dwell elapse with no further
	// observation, simulating CI that stopped reporting.
	eng.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, SHA: "sha_stalled"})
	eng.store.Apply(itemstate.CheckRunCompleted{Repo: "owner/repo", SHA: "sha_stalled", Run: gh.CheckRun{ID: 1, Name: "slow-ci", Status: "in_progress"}})
	time.Sleep(20 * time.Millisecond) // let even a low-resolution clock see the dwell elapsed

	settle := PRSettleResult{
		Status: PRMergeUnsettled,
		Reason: "CI checks pending",
		CheckRuns: []gh.CheckRun{
			{ID: 1, Name: "slow-ci", Status: "in_progress"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha_stalled"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !timedOut {
		t.Error("expected timedOut=true when CI progress has stalled past CIWaitTimeout")
	}
	if blocked || ciFailure {
		t.Errorf("expected blocked=false ciFailure=false on stall timeout, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed on stall timeout")
	}
}

// TestCheckCIGate_Pending_Progressing_NeverTimesOut is the #342 repro (R1):
// CI that keeps showing progress (a fresh check-run observation just
// recorded) must never time out, no matter how long the fabrik:awaiting-ci
// label itself has been applied. fetchLabelAppliedAtFn is set to a value far
// past CIWaitTimeout specifically to prove the pending path no longer
// consults it at all — under the pre-ADR-1410 engine this exact setup times
// out (see TestCheckCIGate_Pending_ProgressStalled_TimesOut's predecessor,
// the old TestCheckCIGate_Pending_TimedOut, which asserted timedOut=true from
// an old labelAppliedAt with no progress signal involved), which is precisely
// the #342 defect: a suite slower than the constant got paused while green.
func TestCheckCIGate_Pending_Progressing_NeverTimesOut(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			// The awaiting-ci label itself was applied long ago — well past
			// CIWaitTimeout — but this must not matter while CI progresses.
			return time.Now().Add(-2 * time.Hour), nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.CIWaitTimeout = 10 * time.Millisecond

	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	eng.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, SHA: "sha_progressing"})
	// Fresh progress observed just now — well within the dwell.
	eng.store.Apply(itemstate.CheckRunCompleted{Repo: "owner/repo", SHA: "sha_progressing", Run: gh.CheckRun{ID: 1, Name: "slow-ci", Status: "in_progress"}})

	settle := PRSettleResult{
		Status: PRMergeUnsettled,
		Reason: "CI checks pending",
		CheckRuns: []gh.CheckRun{
			{ID: 1, Name: "slow-ci", Status: "in_progress"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha_progressing"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if timedOut {
		t.Error("expected timedOut=false — CI is observably progressing, elapsed label age must not matter (R1, #342)")
	}
	if !blocked || ciFailure {
		t.Errorf("expected blocked=true ciFailure=false while progressing, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
}

// TestCheckCIGate_Pending_ColdStart_NeverEscalates verifies the Open
// Questions cold-start default (ADR-1410): with no LastCIProgressAt ever
// recorded in this process's store (e.g. immediately after an engine
// restart), the pending path must never escalate blind — it re-observes
// instead, even when the awaiting-ci label itself looks old.
func TestCheckCIGate_Pending_ColdStart_NeverEscalates(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return time.Now().Add(-2 * time.Hour), nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.CIWaitTimeout = 1 * time.Nanosecond

	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	// No store seeding at all — simulates a cold restart with no progress
	// observed yet in this process's lifetime.

	settle := PRSettleResult{
		Status:    PRMergeUnsettled,
		Reason:    "CI checks pending",
		CheckRuns: []gh.CheckRun{{ID: 1, Name: "ci", Status: "in_progress"}},
		PR:        &gh.PRDetails{Number: 5, HeadSHA: "sha_cold"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if timedOut {
		t.Error("expected timedOut=false on cold start (no LastCIProgressAt observed yet) — must re-observe, never escalate blind")
	}
	if !blocked || ciFailure {
		t.Errorf("expected blocked=true ciFailure=false on cold start, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
}

// TestCheckCIGate_PendingSiblingBeatsFailed_BlocksNoLabel is the #958
// regression: a failed check coexisting with a pending check on the same
// head must classify as WAIT (blocked, no ciFailure), never FAIL — the
// engine must not dispatch a CI-fix reinvoke or add fabrik:awaiting-ci
// while the current head's CI is still running.
func TestCheckCIGate_PendingSiblingBeatsFailed_BlocksNoLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeUnsettled,
		Reason: "CI checks pending",
		CheckRuns: []gh.CheckRun{
			{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"},
			{ID: 2, Name: "test", Status: "in_progress"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha3"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Error("expected blocked=true when a pending check coexists with a failed sibling")
	}
	if ciFailure {
		t.Error("expected ciFailure=false when a pending check coexists with a failed sibling — must not dispatch CI-fix reinvoke")
	}
	if timedOut {
		t.Error("expected timedOut=false")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("checkCIGate must NOT add fabrik:awaiting-ci while a sibling check is still pending")
		}
	}
}

// TestCheckCIGate_StaleFailedSupersededByPendingRerun_BlocksNoLabel covers
// the same-name case: a stale failed run superseded by a fresh (higher-ID)
// pending rerun of the same check name must not be classified as failed.
func TestCheckCIGate_StaleFailedSupersededByPendingRerun_BlocksNoLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeUnsettled,
		Reason: "CI checks pending",
		CheckRuns: []gh.CheckRun{
			{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"},
			{ID: 2, Name: "build", Status: "in_progress"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha3"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked || ciFailure || timedOut {
		t.Errorf("expected blocked=true ciFailure=false timedOut=false for stale-failed-superseded-by-pending, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
}

func TestCheckCIGate_Failed_BlocksAndAddsLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeBlocked,
		Reason: "CI checks failed",
		CheckRuns: []gh.CheckRun{
			{Name: "lint", Status: "completed", Conclusion: "failure"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha4"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked || !ciFailure {
		t.Errorf("expected blocked=true ciFailure=true for failed CI, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
	if timedOut {
		t.Error("expected timedOut=false for failed CI without timeout")
	}
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			found = true
		}
	}
	if !found {
		t.Error("expected fabrik:awaiting-ci to be added on CI failure")
	}
}

// TestCheckCIGate_Failed_NeverTimesOut_RegardlessOfElapsedTime replaces the
// pre-ADR-1410 TestCheckCIGate_Failed_AlreadyLabeledWithTimeout_TimesOut,
// which pinned the R3 bug this issue fixes: a confirmed CI failure occurring
// after CIWaitTimeout used to be misreported as a timeout, bypassing
// MaxCiFixCycles entirely. A CI failure is now a verdict, not a wait — it
// always routes to the CI-fix path regardless of how long fabrik:awaiting-ci
// has been applied.
func TestCheckCIGate_Failed_NeverTimesOut_RegardlessOfElapsedTime(t *testing.T) {
	appliedAt := time.Now().Add(-2 * time.Hour) // well past any timeout
	client := &mockGitHubClient{
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return appliedAt, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.CIWaitTimeout = 1 * time.Millisecond // tiny timeout — must not matter

	tr := true
	// Item already has fabrik:awaiting-ci
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeBlocked,
		Reason: "CI checks failed",
		CheckRuns: []gh.CheckRun{
			{Name: "lint", Status: "completed", Conclusion: "failure"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha5"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if timedOut {
		t.Error("expected timedOut=false — a CI failure is a verdict, never a timeout (R3)")
	}
	if !blocked || !ciFailure {
		t.Errorf("expected blocked=true ciFailure=true regardless of elapsed time, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
	// fabrik:awaiting-ci must stay applied (idempotent add), not be removed —
	// this is the CI-fix path, not a timeout pause.
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("fabrik:awaiting-ci must NOT be removed on a CI failure — that only happens on a timeout pause")
		}
	}
}

func TestCheckCIGate_Failed_AlreadyLabeledNotYetTimedOut_Blocked(t *testing.T) {
	appliedAt := time.Now().Add(-1 * time.Minute) // within a 30-min window
	client := &mockGitHubClient{
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return appliedAt, nil
		},
	}
	eng := testEngineForMerge(t, client) // CIWaitTimeout = 0 → defaults to 30 min

	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeBlocked,
		Reason: "CI checks failed",
		CheckRuns: []gh.CheckRun{
			{Name: "lint", Status: "completed", Conclusion: "failure"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha6"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked || !ciFailure {
		t.Errorf("expected blocked=true ciFailure=true when not yet timed out, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
	if timedOut {
		t.Error("expected timedOut=false when timeout has not elapsed")
	}
}

// ── checkCIGate adds stage:X:complete on gate clear ──────────────────────────

// TestCheckCIGate_AllGreen_AddsCompleteLabel verifies that checkCIGate adds
// stage:X:complete when all CI checks pass (R5 — gate cleared).
func TestCheckCIGate_AllGreen_AddsCompleteLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeReady,
		Reason: "all CI checks passed",
		CheckRuns: []gh.CheckRun{
			{Name: "build", Status: "completed", Conclusion: "success"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha10"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected gate cleared, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
	foundComplete := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Error("expected stage:Validate:complete to be added when all CI checks pass")
	}
	// fabrik:awaiting-ci should also be removed
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed when gate clears")
	}
}

// TestCheckCIGate_NoCheckRuns_AddsCompleteLabel verifies that checkCIGate adds
// stage:X:complete when no check runs exist (no CI configured).
func TestCheckCIGate_NoCheckRuns_AddsCompleteLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeReady,
		Reason: "no CI configured",
		PR:     &gh.PRDetails{Number: 5, HeadSHA: "sha11"},
	}
	blocked, _, _, _ := eng.checkCIGate(nil, item, stage, settle)
	if blocked {
		t.Error("expected gate cleared for no check runs (R5)")
	}
	foundComplete := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Error("expected stage:Validate:complete to be added when no CI is configured (R5)")
	}
}

// TestCheckCIGate_NoPR_AddsCompleteLabel verifies that checkCIGate adds
// stage:X:complete when there is no linked PR (gate clears — no PR, no CI).
// Regression test: before the fix, fabrik:awaiting-ci was never removed and
// stage:X:complete was never added when FetchLinkedPR returns nil.
func TestCheckCIGate_NoPR_AddsCompleteLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{Status: PRMergeNoPR}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected gate cleared for no PR, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
	foundComplete := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Error("expected stage:Validate:complete to be added when no linked PR (R5 equivalent)")
	}
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed when gate clears (no linked PR)")
	}
}

// TestCheckCIGate_Failed_DoesNotAddCompleteLabel verifies that checkCIGate does
// NOT add stage:X:complete when CI checks have failed.
func TestCheckCIGate_Failed_DoesNotAddCompleteLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeBlocked,
		Reason: "CI checks failed",
		CheckRuns: []gh.CheckRun{
			{Name: "lint", Status: "completed", Conclusion: "failure"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha12"},
	}
	_, ciFailure, _, _ := eng.checkCIGate(nil, item, stage, settle)
	if !ciFailure {
		t.Error("expected ciFailure=true for failed CI")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must NOT be added when CI failed")
		}
	}
}

// TestCheckCIGate_NonValidateStage_AddsCorrectCompleteLabel verifies that
// checkCIGate uses the correct stage name when adding the completion label
// (not hard-coded to "Validate").
func TestCheckCIGate_NonValidateStage_AddsCorrectCompleteLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	// Use a non-Validate stage name
	stage := &stages.Stage{Name: "Review", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeReady,
		Reason: "all CI checks passed",
		CheckRuns: []gh.CheckRun{
			{Name: "build", Status: "completed", Conclusion: "success"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha13"},
	}
	blocked, _, _, _ := eng.checkCIGate(nil, item, stage, settle)
	if blocked {
		t.Error("expected gate cleared")
	}
	foundComplete := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Review:complete" {
			foundComplete = true
		}
		if c.labelName == "stage:Validate:complete" {
			t.Error("wrong completion label added — should be stage:Review:complete")
		}
	}
	if !foundComplete {
		t.Errorf("expected stage:Review:complete, got add calls: %v", func() []string {
			var names []string
			for _, c := range client.addLabelCalls {
				names = append(names, c.labelName)
			}
			return names
		}())
	}
}

// ── addCompleteLabelAndRemoveCI atomic-ish behavior ──────────────────────────

// TestAddCompleteLabelAndRemoveCI_AddLabelFails_PreservesAwaitingCI verifies
// that fabrik:awaiting-ci is NOT removed when AddLabelToIssue fails.
// This preserves R3 — the in-flight marker must stay while CI is still pending,
// so the dispatcher continues to suppress re-invocation on the next poll.
func TestAddCompleteLabelAndRemoveCI_AddLabelFails_PreservesAwaitingCI(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			// Simulate a transient GitHub API failure.
			if labelName == fmt.Sprintf("stage:%s:complete", "Validate") {
				return fmt.Errorf("GitHub API 503")
			}
			return nil
		},
	}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	eng.addCompleteLabelAndRemoveCI("owner", "repo", item, stage)

	// fabrik:awaiting-ci must NOT be removed — AddLabelToIssue failed.
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("fabrik:awaiting-ci must NOT be removed when AddLabelToIssue fails (R3 preservation)")
		}
	}
}

// ── buildCIFixComment ─────────────────────────────────────────────────────────

func TestBuildCIFixComment_IncludesFailedChecks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1}
	tr := true
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeBlocked,
		Reason: "CI checks failed",
		CheckRuns: []gh.CheckRun{
			{Name: "build", Status: "completed", Conclusion: "failure"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha7"},
	}
	comment := eng.buildCIFixComment(item, stage, "/tmp", settle)
	if comment.DatabaseID != 0 {
		t.Error("synthetic comment should have DatabaseID=0")
	}
	if !strings.Contains(comment.Body, "build") {
		t.Error("expected failed check name 'build' in comment body")
	}
	if !strings.Contains(comment.Body, "CI Fix Required") {
		t.Error("expected CI Fix Required header in comment body")
	}
}

// TestBuildCIFixComment_RequiredContextFailure_NamesTheContext covers a
// required-context failure whose only producer is a classic commit status
// (the local-CI-takeover case #933 was filed for) — there are no failed
// check runs at all, so the comment must name the failed required context
// instead of falling back to the generic "check GitHub Actions" message,
// which would point the reinvoked stage at a signal that was never the
// actual failure.
func TestBuildCIFixComment_RequiredContextFailure_NamesTheContext(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1}
	tr := true
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status:                 PRMergeBlocked,
		Reason:                 "required status context(s) failed: [fantasy/local-test]",
		RequiredContextsStatus: gh.RequiredContextsFailed,
		RequiredFailed:         []string{"fantasy/local-test"},
		PR:                     &gh.PRDetails{Number: 5, HeadSHA: "sha7"},
	}
	comment := eng.buildCIFixComment(item, stage, "/tmp", settle)
	if !strings.Contains(comment.Body, "fantasy/local-test") {
		t.Errorf("expected failed required context name 'fantasy/local-test' in comment body, got: %s", comment.Body)
	}
	if strings.Contains(comment.Body, "Could not determine specific failed checks") {
		t.Error("must not fall back to the generic 'could not determine' message when a required-context failure is known")
	}
}

// TestCheckCIGate_FetchLinkedPRError_BlocksGate verifies that a transient
// FetchLinkedPR API error returns blocked=true rather than clearing the gate,
// preventing auto-advance when CI status is unknown.
func TestCheckCIGate_FetchLinkedPRError_BlocksGate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeUnsettled,
		Reason: "FetchLinkedPR error: transient network error",
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Error("expected blocked=true when FetchLinkedPR returns an error")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false on API error, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
}

// ── R1/R2/R3 — merged/closed PR and required-never-running check ──────────────

// TestCheckCIGate_MergedPR_ClearsGate verifies R1: when the linked PR is
// merged, checkCIGate clears the CI gate and adds stage:X:complete without
// requiring check runs.
func TestCheckCIGate_MergedPR_ClearsGate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeTerminal,
		PR:     &gh.PRDetails{Number: 5, HeadSHA: "sha-merged", Merged: true, State: "closed"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected (false,false,false) for merged PR, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
	foundComplete := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Error("expected stage:Validate:complete to be added when PR is merged")
	}
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed when PR is merged")
	}
}

// TestCheckCIGate_PRMergeQueued_BlocksNoChurn verifies the FR-1 gate hand-off:
// an in-queue PR (PRMergeQueued) blocks the CI gate exactly like PRMergeUnsettled
// — no fabrik:awaiting-ci churn, no completion label, no pause — so the queue
// owns the merge decision while it waits.
func TestCheckCIGate_PRMergeQueued_BlocksNoChurn(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{Status: PRMergeQueued, Reason: "PR in merge queue", PR: &gh.PRDetails{Number: 5}}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked || ciFailure || timedOut {
		t.Errorf("expected (true,false,false) for PRMergeQueued, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("PRMergeQueued must not add any label, got %d add(s)", len(client.addLabelCalls))
	}
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("PRMergeQueued must not remove any label (no churn), got %d remove(s)", len(client.removeLabelCalls))
	}
}

// TestCheckCIGate_ClosedNotMergedPR_Pauses verifies R2: when the linked PR is
// closed without merging, checkCIGate pauses the issue with fabrik:paused +
// fabrik:awaiting-input and removes fabrik:awaiting-ci. stage:X:complete must
// NOT be added.
func TestCheckCIGate_ClosedNotMergedPR_Pauses(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeTerminal,
		PR:     &gh.PRDetails{Number: 5, HeadSHA: "sha-closed", Merged: false, State: "closed"},
	}
	blocked, ciFailure, timedOut, terminated := eng.checkCIGate(nil, item, stage, settle)
	if blocked || ciFailure || timedOut || !terminated {
		t.Errorf("expected (false,false,false,true) for closed-not-merged PR, got blocked=%v ciFailure=%v timedOut=%v terminated=%v", blocked, ciFailure, timedOut, terminated)
	}
	foundPaused := false
	foundAwaitingInput := false
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:paused":
			foundPaused = true
		case "fabrik:awaiting-input":
			foundAwaitingInput = true
		case "stage:Validate:complete":
			t.Error("stage:Validate:complete must NOT be added for closed-not-merged PR")
		}
	}
	if !foundPaused {
		t.Error("expected fabrik:paused to be added for closed-not-merged PR")
	}
	if !foundAwaitingInput {
		t.Error("expected fabrik:awaiting-input to be added for closed-not-merged PR")
	}
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed for closed-not-merged PR")
	}
}

// TestCheckCIGate_OpenBlockedNoChecks_DwellNotElapsed_StaysBlocked verifies
// the false-positive guard for R3: when the PR is OPEN+BLOCKED with no check
// runs ever observed but fabrik:awaiting-ci was applied recently (< CIWaitTimeout),
// checkCIGate must return (true, false, false) without pausing. This prevents
// spurious R3 pauses on first push before checks have registered.
func TestCheckCIGate_OpenBlockedNoChecks_DwellNotElapsed_StaysBlocked(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return time.Now().Add(-1 * time.Minute), nil // well within the 30-min default timeout
		},
	}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	// mergeable_state="blocked" + no check runs: settle returns Unsettled with MergeableState="blocked"
	settle := PRSettleResult{
		Status:         PRMergeUnsettled,
		MergeableState: "blocked",
		PR:             &gh.PRDetails{Number: 5, HeadSHA: "sha-blocked", Merged: false, State: "open"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Error("expected blocked=true when OPEN+BLOCKED with no check runs and dwell not elapsed (R3 false-positive guard)")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused must NOT be added when dwell has not elapsed (R3 false-positive guard)")
		}
	}
}

// TestCheckCIGate_OpenBlockedNoChecks_DwellElapsed_Pauses verifies R3: when
// the PR is OPEN+BLOCKED with no check runs ever observed and fabrik:awaiting-ci
// has been present for ≥ CIWaitTimeout, checkCIGate pauses with a distinct
// "required check never runs on PR" message.
func TestCheckCIGate_OpenBlockedNoChecks_DwellElapsed_Pauses(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return time.Now().Add(-2 * time.Hour), nil // well past the 30-min default timeout
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.CIWaitTimeout = 30 * time.Minute
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status:         PRMergeUnsettled,
		MergeableState: "blocked",
		PR:             &gh.PRDetails{Number: 5, HeadSHA: "sha-blocked-old", Merged: false, State: "open"},
	}
	blocked, ciFailure, timedOut, terminated := eng.checkCIGate(nil, item, stage, settle)
	if blocked || ciFailure || timedOut || !terminated {
		t.Errorf("expected (false,false,false,true) for R3 dwell-elapsed pause, got blocked=%v ciFailure=%v timedOut=%v terminated=%v", blocked, ciFailure, timedOut, terminated)
	}
	foundPaused := false
	foundAwaitingInput := false
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:paused":
			foundPaused = true
		case "fabrik:awaiting-input":
			foundAwaitingInput = true
		}
	}
	if !foundPaused {
		t.Error("expected fabrik:paused to be added for R3 required-never-running pause")
	}
	if !foundAwaitingInput {
		t.Error("expected fabrik:awaiting-input to be added for R3 required-never-running pause")
	}
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed for R3 required-never-running pause")
	}
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected a comment to be posted for R3 required-never-running pause")
	}
	if !strings.Contains(client.addCommentCalls[0].body, "PR #5") {
		t.Errorf("expected R3 comment to mention PR #5, got: %q", client.addCommentCalls[0].body[:min(200, len(client.addCommentCalls[0].body))])
	}
	if !strings.Contains(client.addCommentCalls[0].body, "required check") {
		t.Errorf("expected R3 comment to mention 'required check', got: %q", client.addCommentCalls[0].body[:min(200, len(client.addCommentCalls[0].body))])
	}
}

// TestCheckCIGate_OpenBlockedNoChecks_HadChecks_Waits verifies that R5 is
// preserved when mergeableState is "blocked" but hadChecks is true: the engine
// must treat this as a post-push registration delay and return (true, false, false)
// without triggering R3's "required check never runs" pause.
func TestCheckCIGate_OpenBlockedNoChecks_HadChecks_Waits(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	// hadChecks=true: settle returns "post-push registration delay (hadChecks)"
	// with empty MergeableState so R3 path does not fire.
	settle := PRSettleResult{
		Status: PRMergeUnsettled,
		Reason: "post-push registration delay (hadChecks)",
		PR:     &gh.PRDetails{Number: 5, HeadSHA: "sha-blocked-hadchecks", Merged: false, State: "open"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Error("expected blocked=true when OPEN+BLOCKED with no check runs but hadChecks=true (R5 preserved)")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	// R3 must not fire when hadChecks=true
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused must NOT be added when hadChecks=true (R5 preserved — post-push registration delay, not R3)")
		}
	}
}

// TestCheckCIGate_FetchCheckRunsError_BlocksGate verifies that a transient
// FetchCheckRuns API error returns blocked=true rather than clearing the gate.
func TestCheckCIGate_FetchCheckRunsError_BlocksGate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeUnsettled,
		Reason: "FetchCheckRuns error: GitHub API 503",
		PR:     &gh.PRDetails{Number: 5, HeadSHA: "sha1"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Error("expected blocked=true when FetchCheckRuns returns an error")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false on API error, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
}

func TestBuildCIFixComment_SyntheticHasDatabaseIDZero(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 42}
	stage := &stages.Stage{Name: "Validate"}

	comment := eng.buildCIFixComment(item, stage, "/tmp", PRSettleResult{})
	if comment.DatabaseID != 0 {
		t.Errorf("DatabaseID = %d, want 0 (synthetic)", comment.DatabaseID)
	}
	if comment.Author != "fabrik" {
		t.Errorf("Author = %q, want %q", comment.Author, "fabrik")
	}
}

// TestCheckCIGate_MergeableStateClean_ClearsGate verifies that when GitHub
// reports mergeable_state=clean, the gate clears regardless of raw check_runs
// state. The raw check_runs gate was over-aggressive (any run with
// conclusion=failure blocked, even non-required workflow jobs). When GitHub
// itself says the PR is ready to merge, trust that.
func TestCheckCIGate_MergeableStateClean_ClearsGate(t *testing.T) {
	addCalls := []string{}
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			addCalls = append(addCalls, labelName)
			return nil
		},
	}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	// mergeable_state=clean → PRMergeReady shortcut (no FetchCheckRuns needed)
	settle := PRSettleResult{
		Status:         PRMergeReady,
		MergeableState: "clean",
		PR:             &gh.PRDetails{Number: 5, HeadSHA: "shaA"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected gate clear, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
	// addCompleteLabelAndRemoveCI should have applied stage:Validate:complete.
	foundComplete := false
	for _, l := range addCalls {
		if l == "stage:Validate:complete" {
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Errorf("expected stage:Validate:complete to be added, got addLabelToIssue calls: %v", addCalls)
	}
}

// TestCheckCIGate_MergeableStateUnstable_ClearsGate verifies checkCIGate
// still clears the gate whenever it receives PRMergeReady, regardless of the
// MergeableState value carried alongside it — checkCIGate's case PRMergeReady
// dispatches purely on settle.Status, never re-inspecting MergeableState (see
// checkCIGate's doc comment). Post-ADR-1441, settlePRMergeState no longer
// produces this combination via the old shortcut — "unstable" now only
// reaches PRMergeReady after the full per-check classification below finds
// nothing blocking (e.g. all observed checks passed, or R5's
// skipped/neutral/cancelled-only case). This test pins checkCIGate's own
// contract at the unit level, independent of how settlePRMergeState arrived
// at it.
func TestCheckCIGate_MergeableStateUnstable_ClearsGate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	// PRMergeReady with MergeableState=unstable, e.g. because every observed
	// check run passed for this unstable-but-not-failing PR.
	settle := PRSettleResult{
		Status:         PRMergeReady,
		MergeableState: "unstable",
		PR:             &gh.PRDetails{Number: 5, HeadSHA: "shaB"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if blocked || ciFailure || timedOut {
		t.Errorf("expected gate clear for unstable, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
}

// TestCheckCIGate_MergeableStateUnstable_FailedCheckRun_DoesNotClearGate is
// the direct AC1 regression test: checkCIGate's tuple for the settle-result
// shape settlePRMergeState now actually produces for an unstable PR carrying
// a confirmed check-run failure (PRMergeBlocked, MergeableState="unstable",
// a failed CheckRun) — the gate must not clear, ciFailure must be true, and
// fabrik:awaiting-ci must be applied. This must fail against pre-ADR-1441
// main's settlePRMergeState (which never produced PRMergeBlocked for an
// accepted mergeable_state at all — see pr_settle_test.go's sibling
// regression test for the settle-layer half of this).
func TestCheckCIGate_MergeableStateUnstable_FailedCheckRun_DoesNotClearGate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: nil}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status:         PRMergeBlocked,
		Reason:         "CI checks failed",
		MergeableState: "unstable",
		CheckRuns: []gh.CheckRun{
			{Name: "Test and vet", Status: "completed", Conclusion: "failure"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "shaB"},
	}
	blocked, ciFailure, timedOut, terminated := eng.checkCIGate(nil, item, stage, settle)
	if !blocked || !ciFailure || timedOut || terminated {
		t.Errorf("expected blocked=true ciFailure=true timedOut=false terminated=false for unstable+failed check, got blocked=%v ciFailure=%v timedOut=%v terminated=%v",
			blocked, ciFailure, timedOut, terminated)
	}
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			found = true
		}
	}
	if !found {
		t.Error("expected fabrik:awaiting-ci to be applied on confirmed CI failure")
	}
}

// TestCheckCIGate_MergeableStateBlocked_FallsThroughToCheckRuns verifies that
// mergeable_state=blocked does NOT shortcut — instead the existing per-check
// classification runs to distinguish failure vs pending and apply the right
// label/dispatch.
func TestCheckCIGate_MergeableStateBlocked_FallsThroughToCheckRuns(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	// mergeable_state=blocked + pending check runs → PRMergeUnsettled with CheckRuns
	settle := PRSettleResult{
		Status:         PRMergeUnsettled,
		MergeableState: "blocked",
		CheckRuns:      []gh.CheckRun{{Name: "ci", Status: "in_progress"}},
		PR:             &gh.PRDetails{Number: 5, HeadSHA: "shaC"},
	}
	blocked, ciFailure, _, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked || ciFailure {
		t.Errorf("expected blocked-pending for mergeable_state=blocked + in_progress checks, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
}

// TestCheckCIGate_EmptyHeadSHA_StaysBlocked is a regression test for the
// original symptom of issue #779: when the boardcache layer returned a non-nil
// PRDetails with HeadSHA=="" (due to Bugs 1/2/3 in the cache layer), checkCIGate
// was clearing the CI gate as if no PR existed, silently disarming the safety
// mechanism. After the fix, a non-nil PR with an empty HeadSHA is treated as
// "data incomplete — block until SHA is populated" rather than "no PR — gate clears."
func TestCheckCIGate_EmptyHeadSHA_StaysBlocked(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeUnsettled,
		Reason: "HeadSHA empty",
		PR:     &gh.PRDetails{Number: 5, HeadSHA: ""},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Error("expected blocked=true when PR exists but HeadSHA is empty")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false for incomplete HeadSHA, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	// addCompleteLabelAndRemoveCI must NOT have been called.
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must NOT be added when HeadSHA is empty (CI gate must stay armed)")
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("fabrik:awaiting-ci must NOT be removed when HeadSHA is empty")
		}
	}
}

// TestRemoveAwaitingCILabel_ErrNotFound verifies that a 404 from
// RemoveLabelFromIssue is treated as success (label already absent) — exactly
// one call, no warning logged, and cache write-through applied.
func TestRemoveAwaitingCILabel_ErrNotFound(t *testing.T) {
	var calls int
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:awaiting-ci" {
				calls++
				return fmt.Errorf("GitHub API returned 404: label not found: %w", gh.ErrNotFound)
			}
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 1), "fabrik:awaiting-ci")

	eventsCh := make(chan tui.Event, 16)
	eng.events = eventsCh

	item := gh.ProjectItem{
		Number: 1,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-ci"},
	}

	eng.removeAwaitingCILabel("owner", "repo", item)

	if calls != 1 {
		t.Errorf("expected exactly 1 RemoveLabelFromIssue call for ErrNotFound, got %d", calls)
	}

	// No warn log should be emitted when ErrNotFound is returned.
	close(eventsCh)
	for ev := range eventsCh {
		if le, ok := ev.(tui.LogEvent); ok && le.Tag == "warn" {
			t.Errorf("unexpected warn log: %q", le.Message)
		}
	}

	// Cache write-through applied: fabrik:awaiting-ci must be absent from cache.
	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	for _, l := range labels {
		if l == "fabrik:awaiting-ci" {
			t.Error("expected fabrik:awaiting-ci to be removed from cache after ErrNotFound")
		}
	}
}

// TestCheckCIGate_BehindNoChecks_Blocks verifies SC-2: when mergeable_state="behind"
// (branch is behind the base) and check_runs=[] and hadChecks=false, the new guard
// must return (true, false, false) without clearing the gate or adding
// stage:Validate:complete. The "behind" state signals that branch protection is
// blocking via a signal Fabrik cannot see via check_runs (e.g. up-to-date policy).
func TestCheckCIGate_BehindNoChecks_Blocks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status:         PRMergeUnsettled,
		MergeableState: "behind",
		PR:             &gh.PRDetails{Number: 5, HeadSHA: "sha-behind", Merged: false, State: "open"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Error("expected blocked=true when mergeable_state=behind with no check_runs and hadChecks=false (new guard)")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must NOT be added when new guard blocks (mergeable_state=behind, no check_runs)")
		}
	}
}

// TestCheckCIGate_DirtyNoChecks_Blocks verifies SC-2: when mergeable_state="dirty"
// (merge conflict) and check_runs=[] and hadChecks=false, the new guard must
// return (true, false, false) without clearing the gate or adding
// stage:Validate:complete. The "dirty" state signals that branch protection is
// blocking due to a merge conflict — a signal Fabrik cannot see via check_runs.
func TestCheckCIGate_DirtyNoChecks_Blocks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status:         PRMergeUnsettled,
		MergeableState: "dirty",
		PR:             &gh.PRDetails{Number: 5, HeadSHA: "sha-dirty", Merged: false, State: "open"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Error("expected blocked=true when mergeable_state=dirty with no check_runs and hadChecks=false (new guard)")
	}
	if ciFailure || timedOut {
		t.Errorf("expected ciFailure=false timedOut=false, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must NOT be added when new guard blocks (mergeable_state=dirty, no check_runs)")
		}
	}
}

// TestCheckCIGate_BehindNoChecks_TimeoutElapsed_TimesOut verifies that the new guard
// returns (false, false, true) and removes fabrik:awaiting-ci when mergeable_state
// is "behind", check_runs=[] and fabrik:awaiting-ci has been present for >= CIWaitTimeout.
// This guards against indefinite blocking when branch protection signals "behind" via a
// signal Fabrik cannot see via check_runs.
func TestCheckCIGate_BehindNoChecks_TimeoutElapsed_TimesOut(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return time.Now().Add(-2 * time.Hour), nil // well past the 30-min default timeout
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.CIWaitTimeout = 30 * time.Minute
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status:         PRMergeUnsettled,
		MergeableState: "behind",
		PR:             &gh.PRDetails{Number: 5, HeadSHA: "sha-behind-timeout", Merged: false, State: "open"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if blocked || ciFailure {
		t.Errorf("expected blocked=false ciFailure=false for timed-out new guard, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
	if !timedOut {
		t.Error("expected timedOut=true when fabrik:awaiting-ci elapsed >= CIWaitTimeout and mergeable_state=behind with no check_runs")
	}
	foundRemove := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundRemove = true
		}
	}
	if !foundRemove {
		t.Error("expected fabrik:awaiting-ci to be removed when new guard times out")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must NOT be added when new guard times out")
		}
	}
}

// ── Post-push dwell guard (SC-1 through SC-4) ────────────────────────────────

// TestCheckCIGate_PostPushDwell_WithinDwell_Blocks covers SC-1:
// mergeable_state="" + check_runs=[] + hadChecks=false + LastHeadSHAUpdate
// within dwell → gate must NOT clear (returns true, false, false).
func TestCheckCIGate_PostPushDwell_WithinDwell_Blocks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)

	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	// Post-push dwell active: settlePRMergeState returns Unsettled with "post-push dwell active"
	settle := PRSettleResult{
		Status: PRMergeUnsettled,
		Reason: "post-push dwell active",
		PR:     &gh.PRDetails{Number: 5, HeadSHA: "sha-fresh", State: "open"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Error("SC-1: expected blocked=true when check_runs=[] and LastHeadSHAUpdate is within PostPushDwell")
	}
	if ciFailure || timedOut {
		t.Errorf("SC-1: expected ciFailure=false timedOut=false, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("SC-1: stage:Validate:complete must NOT be added during post-push dwell window")
		}
	}
}

// TestClassifyCIFromMergeableState_GenericUnsettled_LogsClaim is a #1303
// regression: classifyCIFromMergeableState's generic "Unsettled" fallback
// (hadChecks/dwell/HeadSHA-empty/mergeable=nil/unknown, no check_runs) used
// to claim the item (blocked=true) with no log line — the one branch in this
// function that didn't name itself, unlike every sibling branch. Reuses the
// exact settle shape from TestCheckCIGate_PostPushDwell_WithinDwell_Blocks
// (empty MergeableState, no check runs) to confirm the fallback now logs
// under the "ci-gate" tag.
func TestClassifyCIFromMergeableState_GenericUnsettled_LogsClaim(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	eventsCh := make(chan tui.Event, 16)
	eng.events = eventsCh

	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{
		Status: PRMergeUnsettled,
		Reason: "post-push dwell active",
		PR:     &gh.PRDetails{Number: 5, HeadSHA: "sha-fresh", State: "open"},
	}

	blocked, _, _, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Fatal("expected blocked=true for the generic Unsettled fallback")
	}

	close(eventsCh)
	var found bool
	for ev := range eventsCh {
		if le, ok := ev.(tui.LogEvent); ok && le.Tag == "ci-gate" && strings.Contains(le.Message, "CI state unsettled") {
			found = true
		}
	}
	if !found {
		t.Error("expected a ci-gate log line naming the generic Unsettled claim")
	}
}

// TestCheckCIGate_PostPushDwell_DwellElapsed_Clears covers SC-2:
// mergeable_state="unknown" + check_runs=[] + hadChecks=false + dwell elapsed
// → gate clears as "no CI configured" (existing fallthrough preserved).
func TestCheckCIGate_PostPushDwell_DwellElapsed_Clears(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)

	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	// Dwell elapsed: settlePRMergeState returns Ready with "no CI configured"
	settle := PRSettleResult{
		Status: PRMergeReady,
		Reason: "no CI configured",
		PR:     &gh.PRDetails{Number: 5, HeadSHA: "sha-old", State: "open"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if blocked || ciFailure || timedOut {
		t.Errorf("SC-2: expected gate to clear (false,false,false) when dwell elapsed, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
	foundComplete := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Error("SC-2: expected stage:Validate:complete to be added when dwell has elapsed (no CI configured)")
	}
}

// TestCheckCIGate_PostPushDwell_ZeroTimestamp_Clears covers SC-3:
// mergeable_state="" + LastHeadSHAUpdate zero (PRHeadSHAUpdated never fired)
// → gate clears (cold-start / post-restart behavior preserved).
func TestCheckCIGate_PostPushDwell_ZeroTimestamp_Clears(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)

	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	// Zero timestamp: settlePRMergeState returns Ready with "no CI configured"
	settle := PRSettleResult{
		Status: PRMergeReady,
		Reason: "no CI configured",
		PR:     &gh.PRDetails{Number: 5, HeadSHA: "sha-cold", State: "open"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if blocked || ciFailure || timedOut {
		t.Errorf("SC-3: expected gate to clear (false,false,false) when LastHeadSHAUpdate is zero, got blocked=%v ciFailure=%v timedOut=%v", blocked, ciFailure, timedOut)
	}
}

// TestCheckCIGate_PostPushDwell_Integration covers SC-4:
// simulate a force-push (PRHeadSHAUpdated applied to the store) followed
// immediately by checkCIGate — gate must not clear within the dwell window.
func TestCheckCIGate_PostPushDwell_Integration(t *testing.T) {
	const newSHA = "sha-force-pushed"
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	eng.cfg.PostPushDwell = 30 * time.Second

	// Seed the prior SHA first so the push transition stamps LastHeadSHAUpdate.
	eng.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, SHA: "sha-previous"})
	// The push event fires PRHeadSHAUpdated; the store stamps LastHeadSHAUpdate.
	eng.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, SHA: newSHA})

	// checkCIGate runs immediately (catch-up loop cadence), well within the dwell.
	// The settle result reflects what settlePRMergeState would return: dwell active.
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeUnsettled,
		Reason: "post-push dwell active",
		PR:     &gh.PRDetails{Number: 7, HeadSHA: newSHA, State: "open"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Error("SC-4: gate must not clear immediately after PRHeadSHAUpdated while within PostPushDwell")
	}
	if ciFailure || timedOut {
		t.Errorf("SC-4: expected ciFailure=false timedOut=false, got ciFailure=%v timedOut=%v", ciFailure, timedOut)
	}
	// Verify the store did record the timestamp (i.e., PRHeadSHAUpdated caused the stamp).
	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("SC-4: store.Get failed: %v", err)
	}
	lpr := snap.LinkedPR()
	if lpr == nil {
		t.Fatal("SC-4: LinkedPR is nil after PRHeadSHAUpdated")
	}
	if lpr.LastHeadSHAUpdate.IsZero() {
		t.Error("SC-4: LastHeadSHAUpdate must be non-zero after PRHeadSHAUpdated with a new SHA")
	}
}

// ── classifyCIFromRequiredContexts (ADR-933 / #933) ───────────────────────────

// TestCheckCIGate_RequiredContextFailed_BlocksAndAddsLabel covers the #933
// regression: a confirmed required-context failure (e.g. a classic commit
// status the checkRuns-only classification never sees) must block and drive
// the same fabrik:awaiting-ci escalation path as a failed check run — even
// though settle.CheckRuns itself may be empty or all-skipped/neutral.
func TestCheckCIGate_RequiredContextFailed_BlocksAndAddsLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status:                 PRMergeBlocked,
		Reason:                 "required status context(s) failed: [fantasy/local-test]",
		RequiredContextsStatus: gh.RequiredContextsFailed,
		RequiredFailed:         []string{"fantasy/local-test"},
		PR:                     &gh.PRDetails{Number: 5, HeadSHA: "sha-rc-failed"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked || !ciFailure {
		t.Errorf("expected blocked=true ciFailure=true for a failed required context, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
	if timedOut {
		t.Error("expected timedOut=false for a freshly-failed required context")
	}
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			found = true
		}
	}
	if !found {
		t.Error("expected fabrik:awaiting-ci to be added on a required-context failure")
	}
}

// TestCheckCIGate_RequiredContextFailed_NeverTimesOut_RegardlessOfElapsedTime
// replaces the pre-ADR-1410
// TestCheckCIGate_RequiredContextFailed_AlreadyLabeledWithTimeout_TimesOut,
// mirroring TestCheckCIGate_Failed_NeverTimesOut_RegardlessOfElapsedTime for
// the required-context failure path (R3): a confirmed required-context
// failure is a verdict, never a timeout, no matter how long
// fabrik:awaiting-ci has been applied.
func TestCheckCIGate_RequiredContextFailed_NeverTimesOut_RegardlessOfElapsedTime(t *testing.T) {
	appliedAt := time.Now().Add(-2 * time.Hour)
	client := &mockGitHubClient{
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return appliedAt, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.CIWaitTimeout = 1 * time.Millisecond

	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status:                 PRMergeBlocked,
		Reason:                 "required status context(s) failed: [fantasy/local-test]",
		RequiredContextsStatus: gh.RequiredContextsFailed,
		RequiredFailed:         []string{"fantasy/local-test"},
		PR:                     &gh.PRDetails{Number: 5, HeadSHA: "sha-rc-failed-timeout"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if timedOut {
		t.Error("expected timedOut=false — a required-context failure is a verdict, never a timeout (R3)")
	}
	if !blocked || !ciFailure {
		t.Errorf("expected blocked=true ciFailure=true regardless of elapsed time, got blocked=%v ciFailure=%v", blocked, ciFailure)
	}
}

// TestCheckCIGate_RequiredContextPending_FallsThroughToNormalHandling ensures
// a merely-pending (missing/skipped/neutral, not confirmed-failed) required
// context does not take the classifyCIFromRequiredContexts early-return path
// — it must defer to the normal Unsettled handling below, matching Plan's
// "nothing has regressed" decision (no CI-fix reinvoke for a context that
// simply hasn't reported yet).
func TestCheckCIGate_RequiredContextPending_FallsThroughToNormalHandling(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status:                 PRMergeUnsettled,
		Reason:                 "required status context(s) not yet confirmed (missing:[fantasy/local-test] pending:[])",
		RequiredContextsStatus: gh.RequiredContextsPending,
		RequiredMissing:        []string{"fantasy/local-test"},
		PR:                     &gh.PRDetails{Number: 5, HeadSHA: "sha-rc-pending"},
	}
	blocked, ciFailure, timedOut, _ := eng.checkCIGate(nil, item, stage, settle)
	if !blocked {
		t.Error("expected blocked=true for a pending required context")
	}
	if ciFailure {
		t.Error("expected ciFailure=false for a merely-pending (not failed) required context — nothing has regressed")
	}
	if timedOut {
		t.Error("expected timedOut=false on the first pending observation")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("fabrik:awaiting-ci must NOT be added for a merely-pending required context")
		}
	}
}

// ── R4: degenerate CI-gate coverage warning (ADR-1441) ───────────────────────
//
// warnIfCIGateCoverageDegenerate has no externally observable side effect
// besides the log line itself and the ciGateCoverageWarnedSet dedup entry —
// these tests are in-package so they can inspect that unexported sync.Map
// directly as the proxy for "fired".

func TestWarnIfCIGateCoverageDegenerate_FiresWhenUnconfigured(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client) // RequiredStatusContexts left unconfigured (nil)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{
		CheckRuns: []gh.CheckRun{
			{Name: "Test and vet", Status: "completed", Conclusion: "success"},
		},
	}

	eng.warnIfCIGateCoverageDegenerate("owner", "repo", item, stage, settle)

	if _, warned := eng.ciGateCoverageWarnedSet.Load("owner/repo|Validate"); !warned {
		t.Error("expected warning to fire (and be recorded) when no required_status_contexts are configured")
	}
}

func TestWarnIfCIGateCoverageDegenerate_FiresWhenNoIntersection(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	eng.cfg.RequiredStatusContexts = map[string][]string{"owner/repo": {"some-other-check"}}
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{
		CheckRuns: []gh.CheckRun{
			{Name: "Test and vet", Status: "completed", Conclusion: "success"},
		},
	}

	eng.warnIfCIGateCoverageDegenerate("owner", "repo", item, stage, settle)

	if _, warned := eng.ciGateCoverageWarnedSet.Load("owner/repo|Validate"); !warned {
		t.Error("expected warning to fire when configured required_status_contexts don't match any observed check")
	}
}

func TestWarnIfCIGateCoverageDegenerate_DoesNotFireWhenCovered(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	eng.cfg.RequiredStatusContexts = map[string][]string{"owner/repo": {"Test and vet"}}
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{
		CheckRuns: []gh.CheckRun{
			{Name: "Test and vet", Status: "completed", Conclusion: "success"},
		},
	}

	eng.warnIfCIGateCoverageDegenerate("owner", "repo", item, stage, settle)

	if _, warned := eng.ciGateCoverageWarnedSet.Load("owner/repo|Validate"); warned {
		t.Error("expected no warning when a configured required_status_context matches an observed check")
	}
}

func TestWarnIfCIGateCoverageDegenerate_DoesNotFireOnEmptyCheckRuns(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{} // no CheckRuns observed this pass

	eng.warnIfCIGateCoverageDegenerate("owner", "repo", item, stage, settle)

	if _, warned := eng.ciGateCoverageWarnedSet.Load("owner/repo|Validate"); warned {
		t.Error("expected no warning when settle.CheckRuns is empty (gated on data already fetched)")
	}
}

func TestWarnIfCIGateCoverageDegenerate_DedupesAcrossCalls(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{
		CheckRuns: []gh.CheckRun{
			{Name: "Test and vet", Status: "completed", Conclusion: "success"},
		},
	}

	eng.warnIfCIGateCoverageDegenerate("owner", "repo", item, stage, settle)
	eng.warnIfCIGateCoverageDegenerate("owner", "repo", item, stage, settle)
	eng.warnIfCIGateCoverageDegenerate("owner", "repo", item, stage, settle)

	// LoadOrStore-based dedup: the key exists and further calls are no-ops.
	// Not directly assertable beyond "still exactly one entry for this key"
	// since sync.Map has no count; the load succeeding is the contract.
	if _, warned := eng.ciGateCoverageWarnedSet.Load("owner/repo|Validate"); !warned {
		t.Error("expected warned-set entry to persist across repeated calls")
	}
}

// TestCheckCIGate_WiresCoverageWarning confirms checkCIGate itself calls
// warnIfCIGateCoverageDegenerate on the fall-through path (not just that the
// helper works in isolation) — a wiring-level check per the codebase's
// "neutralize check" convention (#1422).
func TestCheckCIGate_WiresCoverageWarning(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client) // RequiredStatusContexts unconfigured
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	settle := PRSettleResult{
		Status: PRMergeBlocked,
		Reason: "CI checks failed",
		CheckRuns: []gh.CheckRun{
			{Name: "Test and vet", Status: "completed", Conclusion: "failure"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha-cov"},
	}
	eng.checkCIGate(nil, item, stage, settle)

	if _, warned := eng.ciGateCoverageWarnedSet.Load("owner/repo|Validate"); !warned {
		t.Error("expected checkCIGate to invoke warnIfCIGateCoverageDegenerate on the fall-through path")
	}
}
