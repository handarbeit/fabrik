package engine

import (
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// #2296: a cancelled / stale / unrecognised latest check run is not a pass.

func cancelledCheck(id int64) gh.CheckRun {
	return gh.CheckRun{ID: id, Name: "test", Status: "completed", Conclusion: "cancelled",
		DetailsURL: "https://github.com/owner/repo/actions/runs/900/job/1"}
}

func TestSettle_CancelledCheck_ReturnsBlockedNamingConclusion(t *testing.T) {
	for _, conclusion := range []string{"cancelled", "stale", "", "future_value"} {
		t.Run("conclusion="+conclusion, func(t *testing.T) {
			client := &mockGitHubClient{
				fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
					return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
				},
				fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
					return boolPtr(true), "blocked", nil
				},
				fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
					return []gh.CheckRun{
						{ID: 1, Name: "build", Status: "completed", Conclusion: "success"},
						{ID: 2, Name: "test", Status: "completed", Conclusion: conclusion},
					}, nil
				},
			}
			eng := testEngineForMerge(t, client)
			r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
			if r.Status != PRMergeBlocked {
				t.Fatalf("status = %v, want PRMergeBlocked (a non-passing run is a verdict, never ready/unsettled)", r.Status)
			}
			if !strings.Contains(r.Reason, "did not pass") || strings.Contains(r.Reason, "failed") {
				t.Errorf("reason = %q, want it to say the checks did not pass and never 'failed'", r.Reason)
			}
		})
	}
}

// The "clean" shortcut must not clear a wait_for_ci gate on a cancelled run.
func TestSettle_CleanShortcut_CancelledCheckDoesNotClear(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "clean", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{cancelledCheck(2)}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	tr := true
	r := eng.settlePRMergeStateWith(settleItem(1), &stages.Stage{Name: "Validate", WaitForCI: &tr}, true)
	if r.Status != PRMergeBlocked {
		t.Fatalf("status = %v, want PRMergeBlocked", r.Status)
	}
}

func TestClassifyCIFromCheckRuns_CancelledIsAVerdictNotAWait(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{})
	blocked, ciFailure, timedOut := eng.classifyCIFromCheckRuns("owner", "repo", gh.ProjectItem{Number: 1},
		[]gh.CheckRun{cancelledCheck(2)})
	if !blocked || !ciFailure || timedOut {
		t.Fatalf("(blocked, ciFailure, timedOut) = (%v, %v, %v), want (true, true, false)", blocked, ciFailure, timedOut)
	}
}

// R1: a cancelled latest run gets the one flake re-run, with no CI-fix worker.
func TestFlakeRerun_Cancelled_R1RerunsWithoutDispatch(t *testing.T) {
	f := newFlakeFixture(t)
	f.setChecks(cancelledCheck(10))
	if f.poll(t) {
		t.Fatal("cancelled run dispatched a CI-fix worker on first sight; want a re-run")
	}
	if got := f.rerunCount(); got != 1 {
		t.Fatalf("RerunFailedJobs calls = %d, want 1", got)
	}
}

// Still cancelled after the re-run (a new check-run ID): the CI-fix path runs,
// and the budget is not re-spent.
func TestFlakeRerun_Cancelled_AfterR1Dispatches(t *testing.T) {
	f := newFlakeFixture(t)
	f.setChecks(cancelledCheck(10))
	f.poll(t)
	f.setChecks(cancelledCheck(11))
	if !f.poll(t) {
		t.Fatal("still cancelled after the re-run: want a CI-fix dispatch")
	}
	if got := f.rerunCount(); got != 1 {
		t.Fatalf("RerunFailedJobs calls = %d, want 1", got)
	}
}

// A spent or unavailable re-run dispatches CI-fix immediately — never a silent wait.
func TestFlakeRerun_Cancelled_ForbiddenDispatchesImmediately(t *testing.T) {
	f := newFlakeFixture(t)
	f.setChecks(cancelledCheck(10))
	f.mu.Lock()
	f.rerunErr = gh.ErrForbidden
	f.mu.Unlock()
	if !f.poll(t) {
		t.Fatal("ErrForbidden on a cancelled run: want an immediate CI-fix dispatch")
	}
}

// Neutralised twin: with the re-run off, the same cancelled head dispatches at once.
func TestFlakeRerun_Cancelled_Neutralised(t *testing.T) {
	f := newFlakeFixture(t)
	f.setChecks(cancelledCheck(10))
	f.eng.SetCIFlakeRerunDisabledForTest(true)
	if !f.poll(t) {
		t.Fatal("re-run disabled: want a CI-fix dispatch")
	}
	if got := f.rerunCount(); got != 0 {
		t.Fatalf("RerunFailedJobs calls = %d, want 0", got)
	}
}

func TestBuildCIFixComment_Cancelled_NamesCancellationNotFailure(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{})
	tr := true
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{
		Status: PRMergeBlocked,
		CheckRuns: []gh.CheckRun{
			{ID: 1, Name: "build", Status: "completed", Conclusion: "success"},
			cancelledCheck(2),
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha7"},
	}
	body := eng.buildCIFixComment(gh.ProjectItem{Number: 1}, stage, "/tmp", settle).Body
	for _, want := range []string{"**test**: cancelled", "not a test failure", "fresh run is needed"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"NEW REGRESSION", "Failed checks on PR branch", "check runs failed", "Could not determine"} {
		if strings.Contains(body, bad) {
			t.Errorf("body must not contain %q for a cancelled-only set:\n%s", bad, body)
		}
	}
}

// A mixed set shows both groups; the failure keeps its regression framing.
func TestBuildCIFixComment_MixedCancelledAndFailed_ShowsBoth(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{})
	tr := true
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{
		Status: PRMergeBlocked,
		CheckRuns: []gh.CheckRun{
			{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"},
			cancelledCheck(2),
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha7"},
	}
	body := eng.buildCIFixComment(gh.ProjectItem{Number: 1}, stage, "/tmp", settle).Body
	for _, want := range []string{"**build**: failure [NEW REGRESSION]", "**test**: cancelled", "Failed checks on PR branch"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// A cancelled run superseded by a successful rerun of the same check is not
// the head's state and must not reach the CI-fix context.
func TestBuildCIFixComment_SupersededCancelledRunIsIgnored(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{})
	tr := true
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{
		Status: PRMergeBlocked,
		CheckRuns: []gh.CheckRun{
			cancelledCheck(1),
			{ID: 2, Name: "test", Status: "completed", Conclusion: "success"},
			{ID: 3, Name: "build", Status: "completed", Conclusion: "failure"},
		},
		PR: &gh.PRDetails{Number: 5, HeadSHA: "sha7"},
	}
	body := eng.buildCIFixComment(gh.ProjectItem{Number: 1}, stage, "/tmp", settle).Body
	if strings.Contains(body, "cancelled") || strings.Contains(body, "no result") {
		t.Errorf("superseded cancelled run leaked into the CI-fix context:\n%s", body)
	}
	if !strings.Contains(body, "**build**: failure [NEW REGRESSION]") {
		t.Errorf("body missing the real failure:\n%s", body)
	}
}

// A required-context failure must still be named beside a cancelled run, and the
// worker must not be told there is no failure to fix.
func TestBuildCIFixComment_RequiredContextFailureNotMaskedByCancelled(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{})
	tr := true
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{
		Status:                 PRMergeBlocked,
		CheckRuns:              []gh.CheckRun{cancelledCheck(1)},
		RequiredContextsStatus: gh.RequiredContextsFailed,
		RequiredFailed:         []string{"local-ci"},
		PR:                     &gh.PRDetails{Number: 5, HeadSHA: "sha7"},
	}
	body := eng.buildCIFixComment(gh.ProjectItem{Number: 1}, stage, "/tmp", settle).Body
	if !strings.Contains(body, "**local-ci**: failed") {
		t.Errorf("required-context failure missing:\n%s", body)
	}
	if strings.Contains(body, "there is no failure to fix") || strings.Contains(body, "which is not a test failure") {
		t.Errorf("cancelled run must not mask a required-context failure:\n%s", body)
	}
}

// The cancelled-only instructions are numbered consecutively.
func TestBuildCIFixComment_CancelledOnly_InstructionsNumberedConsecutively(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{})
	tr := true
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{Status: PRMergeBlocked, CheckRuns: []gh.CheckRun{cancelledCheck(1)}, PR: &gh.PRDetails{Number: 5, HeadSHA: "sha7"}}
	body := eng.buildCIFixComment(gh.ProjectItem{Number: 1}, stage, "/tmp", settle).Body
	for _, want := range []string{"\n1. ", "\n2. ", "\n3. "} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "\n4. ") || strings.Contains(body, "\n5. ") {
		t.Errorf("unexpected trailing items:\n%s", body)
	}
}
