package engine

import (
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// R3 / FR-005 / FR-006 (#2471): while the item's fabrik/review status is pending
// the CI wait timers do not count the time and the backstop comment names the
// review wait. Each test also runs with the status absent (the pre-feature
// behaviour must still escalate) and with the attribution seam on (the
// non-vacuity neutraliser).

// setReviewRecord injects the engine's last-written fabrik/review record for item.
func setReviewRecord(eng *Engine, item gh.ProjectItem, rec reviewStatusRecord) {
	eng.reviewStatus.mu.Lock()
	defer eng.reviewStatus.mu.Unlock()
	if eng.reviewStatus.records == nil {
		eng.reviewStatus.records = map[string]*reviewStatusRecord{}
	}
	eng.reviewStatus.records[issueKey(item, eng.defaultRepo())] = &rec
}

func attributionDwell(t *testing.T, state string, rec *reviewStatusRecord, seam bool) (blocked, timedOut, terminated bool) {
	t.Helper()
	client := &mockGitHubClient{
		fetchLabelAppliedAtFn: func(_, _ string, _ int, _ string) (time.Time, error) { return time.Now().Add(-2 * time.Hour), nil },
		fetchLabelRemovedAtFn: removedAtFn(time.Time{}, nil),
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.CIWaitTimeout = 30 * time.Minute
	eng.SetReviewWaitAttributionDisabledForTest(seam)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}, LinkedPRNumber: 5}
	if rec != nil {
		setReviewRecord(eng, item, *rec)
	}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{Status: PRMergeUnsettled, MergeableState: state, PR: &gh.PRDetails{Number: 5, HeadSHA: "sha", State: "open"}}
	blocked, _, timedOut, terminated = eng.checkCIGate(nil, item, stage, settle)
	return
}

func TestCIWaitDwells_ReviewWaitIsNotCITime(t *testing.T) {
	pending := &reviewStatusRecord{pr: 5, head: "sha", state: reviewStatusPending}
	ended := func(ago time.Duration) *reviewStatusRecord {
		return &reviewStatusRecord{pr: 5, head: "sha", state: reviewStatusSuccess, endedAt: time.Now().Add(-ago)}
	}
	for _, tc := range []struct {
		name      string
		state     string
		escalated func(blocked, timedOut, terminated bool) bool
	}{
		{"never-checked dwell", "blocked", func(_, _, terminated bool) bool { return terminated }},
		{"mergeable-state dwell", "behind", func(_, timedOut, _ bool) bool { return timedOut }},
	} {
		t.Run(tc.name+"/pending status: not escalated", func(t *testing.T) {
			b, to, term := attributionDwell(t, tc.state, pending, false)
			if tc.escalated(b, to, term) || !b {
				t.Errorf("blocked=%v timedOut=%v terminated=%v, want blocked and not escalated", b, to, term)
			}
		})
		t.Run(tc.name+"/no status record: escalates as before", func(t *testing.T) {
			b, to, term := attributionDwell(t, tc.state, nil, false)
			if !tc.escalated(b, to, term) {
				t.Errorf("blocked=%v timedOut=%v terminated=%v, want escalated", b, to, term)
			}
		})
		t.Run(tc.name+"/attribution seam neutralises the fix", func(t *testing.T) {
			b, to, term := attributionDwell(t, tc.state, pending, true)
			if !tc.escalated(b, to, term) {
				t.Errorf("neutralised attribution must escalate again: blocked=%v timedOut=%v terminated=%v", b, to, term)
			}
		})
		t.Run(tc.name+"/review wait ended recently: dwell restarts from its end", func(t *testing.T) {
			b, to, term := attributionDwell(t, tc.state, ended(2*time.Minute), false)
			if tc.escalated(b, to, term) {
				t.Errorf("a wait that ended 2m ago must not count as 2h of CI time: blocked=%v timedOut=%v terminated=%v", b, to, term)
			}
		})
		t.Run(tc.name+"/review wait ended long ago: CI timeout applies unchanged", func(t *testing.T) {
			b, to, term := attributionDwell(t, tc.state, ended(time.Hour), false)
			if !tc.escalated(b, to, term) {
				t.Errorf("success/failure status must leave the CI timeout unchanged: blocked=%v timedOut=%v terminated=%v", b, to, term)
			}
		})
	}
}

// The liveness-stall dwell (a pending check run that stopped reporting) is also
// measured from the end of a review wait.
func TestCIWaitLivenessDwell_ReviewWaitIsNotCITime(t *testing.T) {
	run := func(t *testing.T, rec *reviewStatusRecord, seam bool) (timedOut bool) {
		eng := testEngineForMerge(t, &mockGitHubClient{})
		eng.cfg.CIWaitTimeout = 50 * time.Millisecond
		eng.SetReviewWaitAttributionDisabledForTest(seam)
		tr := true
		item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}, LinkedPRNumber: 5}
		stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
		eng.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, SHA: "sha"})
		eng.store.Apply(itemstate.CheckRunCompleted{Repo: "owner/repo", SHA: "sha", Run: gh.CheckRun{ID: 1, Name: "slow-ci", Status: "in_progress"}})
		time.Sleep(120 * time.Millisecond) // CI silent for longer than the dwell
		if rec != nil {
			setReviewRecord(eng, item, *rec)
		}
		settle := PRSettleResult{
			Status:    PRMergeUnsettled,
			Reason:    "CI checks pending",
			CheckRuns: []gh.CheckRun{{ID: 1, Name: "slow-ci", Status: "in_progress"}},
			PR:        &gh.PRDetails{Number: 5, HeadSHA: "sha"},
		}
		_, _, timedOut, _ = eng.checkCIGate(nil, item, stage, settle)
		return timedOut
	}
	pending := &reviewStatusRecord{pr: 5, head: "sha", state: reviewStatusPending}
	if run(t, pending, false) {
		t.Error("CI silent only because the review is pending must not time out as a CI stall")
	}
	if !run(t, nil, false) {
		t.Error("with no status record the stalled dwell still fires (pre-feature behaviour)")
	}
	if !run(t, pending, true) {
		t.Error("neutralised attribution must time out again")
	}
}

func TestPauseForCIBackstop_ReviewWaitWording(t *testing.T) {
	eng, client, item, stage := backstopMsgFixture(t, nGreenRuns(3))
	item.LinkedPRNumber = 9
	// Two declared reviewers; only pruefer is still outstanding, so only it is named.
	stage.ExpectedReviewers = &[]string{"pruefer", "gemini"}
	setReviewRecord(eng, item, reviewStatusRecord{pr: 9, head: "abcdef0123456789", state: reviewStatusPending, outstanding: []string{"pruefer"}})

	if !eng.pauseForCIBackstop(&gh.ProjectBoard{}, item, stage, time.Now().Add(-4*time.Hour)) {
		t.Fatal("want a fresh pause (the CIBackstopTimeout bound is unchanged)")
	}
	body := lastComment(client)
	for _, want := range []string{
		"Fabrik — review wait timeout",
		"The review wait for stage **Validate** timed out",
		"not for CI",
		"abcdef0",
		"`fabrik/review`",
		"pruefer",
		"`fabrik:awaiting-ci` has been set for 4h0m0s",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "awaiting-CI wait timeout") || strings.Contains(body, "CI gate itself was never reached") {
		t.Errorf("a review wait must not be worded as a CI stall:\n%s", body)
	}
	if strings.Contains(body, "gemini") {
		t.Errorf("a reviewer that has already cleared the head must not be named as outstanding:\n%s", body)
	}

	// The same episode is recognised on the next poll, so it is reapplied, not reposted.
	item.Comments = []gh.Comment{{Body: body, Author: "testuser"}}
	if !hasCIGatePauseComment(item, stage) {
		t.Error("the review-wait comment must be recognised by hasCIGatePauseComment (#1408 dedup)")
	}
}

func TestPauseForCIBackstop_ReviewWaitSeamAndNoStatusKeepCIWording(t *testing.T) {
	for name, setup := range map[string]func(*Engine, gh.ProjectItem){
		"no record": func(*Engine, gh.ProjectItem) {},
		"seam on": func(eng *Engine, item gh.ProjectItem) {
			setReviewRecord(eng, item, reviewStatusRecord{pr: 9, head: "abc", state: reviewStatusPending})
			eng.SetReviewWaitAttributionDisabledForTest(true)
		},
		"status not pending": func(eng *Engine, item gh.ProjectItem) {
			setReviewRecord(eng, item, reviewStatusRecord{pr: 9, head: "abc", state: reviewStatusFailure})
		},
	} {
		t.Run(name, func(t *testing.T) {
			eng, client, item, stage := backstopMsgFixture(t, nGreenRuns(3))
			item.LinkedPRNumber = 9
			setup(eng, item)
			eng.pauseForCIBackstop(&gh.ProjectBoard{}, item, stage, time.Time{})
			body := lastComment(client)
			if !strings.Contains(body, "The awaiting-CI wait for stage **Validate** timed out") || strings.Contains(body, "review wait") {
				t.Errorf("comment must keep the CI wording:\n%s", body)
			}
		})
	}
}
