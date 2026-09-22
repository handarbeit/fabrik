package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

var suiteNow = time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)

func runningSuite() gh.CheckSuite {
	return gh.CheckSuite{AppSlug: "github-actions", Status: "in_progress", LatestCheckRunsCount: 5, CreatedAt: suiteNow.Add(-time.Hour)}
}

func inertSuite() gh.CheckSuite {
	return gh.CheckSuite{AppSlug: "cursor", Status: "queued", CreatedAt: suiteNow.Add(-4 * time.Hour)}
}

// settleSuiteClient builds a client whose PR is open at the given
// mergeable_state with the given check runs and suites.
func settleSuiteClient(state string, runs []gh.CheckRun, suites []gh.CheckSuite, suitesErr error) *mockGitHubClient {
	return &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, n int) (*bool, string, error) {
			return boolPtr(true), state, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) { return runs, nil },
		fetchCheckSuitesFn: func(owner, repo, sha string) ([]gh.CheckSuite, error) {
			return suites, suitesErr
		},
	}
}

func suiteEngine(t *testing.T, c *mockGitHubClient) *Engine {
	t.Helper()
	eng := testEngineForMerge(t, c)
	eng.SetClock(stubClock{t: suiteNow})
	return eng
}

func greenRuns() []gh.CheckRun {
	return []gh.CheckRun{
		{Name: "CI / Lint", Status: "completed", Conclusion: "success"},
		{Name: "Build (app)", Status: "completed", Conclusion: "success"},
	}
}

func TestCISuiteHold(t *testing.T) {
	tests := []struct {
		name     string
		suites   []gh.CheckSuite
		err      error
		wantHold bool
		wantSub  string
	}{
		{"no suites", nil, nil, false, ""},
		{"running suite with runs", []gh.CheckSuite{runningSuite()}, nil, true, "github-actions (in_progress, 5 runs)"},
		{"inert old suite does not hold", []gh.CheckSuite{inertSuite()}, nil, false, ""},
		{"completed suite does not hold", []gh.CheckSuite{{Status: "completed", LatestCheckRunsCount: 3, CreatedAt: suiteNow.Add(-time.Hour)}}, nil, false, ""},
		{"young zero-run suite holds (post-push window)", []gh.CheckSuite{{AppSlug: "github-actions", Status: "queued", CreatedAt: suiteNow.Add(-10 * time.Second)}}, nil, true, "github-actions"},
		{"read error fails closed", nil, errors.New("boom"), true, "check-suite read failed: boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := settleSuiteClient("clean", nil, tc.suites, tc.err)
			eng := suiteEngine(t, c)
			hold, detail := eng.ciSuiteHold(c, "owner", "repo", "sha1")
			if hold != tc.wantHold {
				t.Fatalf("hold = %v (%q), want %v", hold, detail, tc.wantHold)
			}
			if !strings.Contains(detail, tc.wantSub) {
				t.Errorf("detail %q does not contain %q", detail, tc.wantSub)
			}
		})
	}
}

func TestPostPushDwell_DefaultAndConfigured(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{})
	eng.cfg.PostPushDwell = 0
	if got := eng.postPushDwell(); got != 90*time.Second {
		t.Errorf("default dwell = %v, want 90s", got)
	}
	eng.cfg.PostPushDwell = 5 * time.Second
	if got := eng.postPushDwell(); got != 5*time.Second {
		t.Errorf("configured dwell = %v, want 5s", got)
	}
}

// ── rule 9 (mergeable_state == clean) ────────────────────────────────────────

// TestSettle_Clean_OutstandingSuite_HoldsAndOmitsMergeableState is the incident
// shape seen through the shortcut: GitHub says clean, every existing check run is
// green (the shortcut never even reads them), but the suite is in_progress.
func TestSettle_Clean_OutstandingSuite_HoldsAndOmitsMergeableState(t *testing.T) {
	eng := suiteEngine(t, settleSuiteClient("clean", greenRuns(), []gh.CheckSuite{runningSuite()}, nil))
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Fatalf("status = %v (%s), want Unsettled", r.Status, r.Reason)
	}
	if r.MergeableState != "" {
		t.Errorf("MergeableState = %q, must be omitted so checkCIGate's R3 pause cannot misfire", r.MergeableState)
	}
	if !strings.Contains(r.Reason, "github-actions") {
		t.Errorf("reason %q should name the suite's app", r.Reason)
	}
}

func TestSettle_Clean_InertSuiteStillClears(t *testing.T) {
	eng := suiteEngine(t, settleSuiteClient("clean", nil, []gh.CheckSuite{inertSuite()}, nil))
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeReady {
		t.Errorf("status = %v (%s), want Ready — an inert App suite must never deadlock the gate", r.Status, r.Reason)
	}
}

func TestSettle_Clean_SuiteReadErrorHolds(t *testing.T) {
	eng := suiteEngine(t, settleSuiteClient("clean", nil, nil, errors.New("api down")))
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Errorf("status = %v (%s), want Unsettled (fail closed)", r.Status, r.Reason)
	}
}

func TestSettle_Clean_EmptyHeadSHA_KeepsItsOwnUnsettled(t *testing.T) {
	suiteCalls := 0
	c := settleSuiteClient("clean", nil, nil, nil)
	c.fetchLinkedPRFn = func(owner, repo string, n int) (*gh.PRDetails, error) {
		return &gh.PRDetails{Number: 5, State: "open", HeadSHA: ""}, nil
	}
	c.fetchCheckSuitesFn = func(owner, repo, sha string) ([]gh.CheckSuite, error) {
		suiteCalls++
		return nil, nil
	}
	eng := suiteEngine(t, c)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled || r.Reason != "HeadSHA empty" {
		t.Errorf("got %v (%s), want the existing 'HeadSHA empty' Unsettled", r.Status, r.Reason)
	}
	if suiteCalls != 0 {
		t.Errorf("suites read %d times for an empty SHA, want 0", suiteCalls)
	}
}

// ── rule 19 (all check runs green) ───────────────────────────────────────────

func TestSettle_AllGreen_OutstandingSuite_Holds(t *testing.T) {
	eng := suiteEngine(t, settleSuiteClient("unstable", greenRuns(), []gh.CheckSuite{runningSuite()}, nil))
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Fatalf("status = %v (%s), want Unsettled — an all-green prefix is not a complete pass", r.Status, r.Reason)
	}
	if r.MergeableState != "" {
		t.Errorf("MergeableState = %q, must be omitted", r.MergeableState)
	}
	if len(r.CheckRuns) != 2 {
		t.Errorf("CheckRuns not carried on the held result: %+v", r.CheckRuns)
	}
}

func TestSettle_AllGreen_NoOutstandingSuite_Ready(t *testing.T) {
	eng := suiteEngine(t, settleSuiteClient("unstable", greenRuns(), []gh.CheckSuite{inertSuite(), {Status: "completed", LatestCheckRunsCount: 5, CreatedAt: suiteNow.Add(-time.Hour)}}, nil))
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeReady {
		t.Errorf("status = %v (%s), want Ready", r.Status, r.Reason)
	}
}

func TestSettle_AllGreen_SuiteReadErrorHolds(t *testing.T) {
	eng := suiteEngine(t, settleSuiteClient("unstable", greenRuns(), nil, errors.New("api down")))
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Errorf("status = %v (%s), want Unsettled (fail closed)", r.Status, r.Reason)
	}
}

// TestSettle_FailedRun_WithOutstandingSuite_StillBlocked pins Req 7's "classify
// the SHA red once the failure lands": the suite is still open (it stays open
// until the last job finishes) but the confirmed failure must decide the verdict.
func TestSettle_FailedRun_WithOutstandingSuite_StillBlocked(t *testing.T) {
	runs := append(greenRuns(), gh.CheckRun{Name: "E2E (Playwright)", Status: "completed", Conclusion: "failure"})
	eng := suiteEngine(t, settleSuiteClient("unstable", runs, []gh.CheckSuite{runningSuite()}, nil))
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeBlocked {
		t.Errorf("status = %v (%s), want Blocked", r.Status, r.Reason)
	}
}

func TestSettle_PendingRun_ReasonUnchangedBySuites(t *testing.T) {
	runs := append(greenRuns(), gh.CheckRun{Name: "E2E", Status: "in_progress"})
	eng := suiteEngine(t, settleSuiteClient("unstable", runs, []gh.CheckSuite{runningSuite()}, nil))
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled || r.Reason != "CI checks pending" {
		t.Errorf("got %v (%s), want the existing 'CI checks pending'", r.Status, r.Reason)
	}
	if r.MergeableState != "unstable" {
		t.Errorf("MergeableState = %q; the pre-existing pending return keeps it", r.MergeableState)
	}
}

// TestSettle_AllGreen_RequiredContextFailure_NotMaskedBySuiteHold: the required-
// context classification decides first, so a confirmed required failure still
// blocks instead of being downgraded to a suite wait.
func TestSettle_AllGreen_RequiredContextFailure_NotMaskedBySuiteHold(t *testing.T) {
	c := settleSuiteClient("blocked", greenRuns(), []gh.CheckSuite{runningSuite()}, nil)
	c.fetchCombinedStatusFn = func(owner, repo, ref string) ([]gh.CommitStatus, error) {
		return []gh.CommitStatus{{Context: "fantasy/local-test", State: "failure"}}, nil
	}
	eng := suiteEngine(t, c)
	eng.cfg.RequiredStatusContexts = map[string][]string{"owner/repo": {"fantasy/local-test"}}
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeBlocked {
		t.Errorf("status = %v (%s), want Blocked", r.Status, r.Reason)
	}
}

// ── rule 18 (no check runs → "no CI configured") ─────────────────────────────

func TestSettle_NoRuns_NoSuites_StillNoCIConfigured(t *testing.T) {
	eng := suiteEngine(t, settleSuiteClient("", nil, nil, nil))
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeReady || r.Reason != "no CI configured" {
		t.Errorf("got %v (%s), want Ready/no CI configured — a repo with no CI must still clear", r.Status, r.Reason)
	}
}

func TestSettle_NoRuns_SuiteReportsRuns_Holds(t *testing.T) {
	eng := suiteEngine(t, settleSuiteClient("", nil, []gh.CheckSuite{runningSuite()}, nil))
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Errorf("status = %v (%s), want Unsettled — zero runs contradicted by a suite with runs is not 'no CI'", r.Status, r.Reason)
	}
}

// TestSettle_NoRuns_YoungSuiteHoldsThenClearsAfterDwell covers the post-push
// window, anchored on the suite's own created_at (so it survives a restart).
func TestSettle_NoRuns_YoungSuiteHoldsThenClearsAfterDwell(t *testing.T) {
	young := gh.CheckSuite{AppSlug: "github-actions", Status: "queued", CreatedAt: suiteNow.Add(-10 * time.Second)}
	eng := suiteEngine(t, settleSuiteClient("", nil, []gh.CheckSuite{young}, nil))
	if r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"}); r.Status != PRMergeUnsettled {
		t.Fatalf("young suite: status = %v (%s), want Unsettled", r.Status, r.Reason)
	}
	eng.SetClock(stubClock{t: suiteNow.Add(2 * time.Minute)})
	if r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"}); r.Status != PRMergeReady {
		t.Errorf("after dwell: status = %v (%s), want Ready (a run-less suite that never registers is inert)", r.Status, r.Reason)
	}
}

// ── pause note ────────────────────────────────────────────────────────────────

func TestSuiteTimeoutNote(t *testing.T) {
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo", LinkedPRHeadSHA: "sha1"}
	eng := suiteEngine(t, settleSuiteClient("clean", nil, []gh.CheckSuite{runningSuite()}, nil))
	if note := eng.suiteTimeoutNote(item); !strings.Contains(note, "github-actions (in_progress, 5 runs)") {
		t.Errorf("note %q should name the stuck suite", note)
	}
	eng = suiteEngine(t, settleSuiteClient("clean", nil, nil, nil))
	if note := eng.suiteTimeoutNote(item); note != "" {
		t.Errorf("note %q, want empty when no suite is outstanding", note)
	}
	eng = suiteEngine(t, settleSuiteClient("clean", nil, nil, errors.New("boom")))
	if note := eng.suiteTimeoutNote(item); note != "" {
		t.Errorf("note %q, want empty on a read error (best-effort)", note)
	}
	if note := eng.suiteTimeoutNote(gh.ProjectItem{Number: 1}); note != "" {
		t.Errorf("note %q, want empty without a head SHA", note)
	}
}
