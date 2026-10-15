package engine

import (
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func backstopMsgFixture(t *testing.T, runs []gh.CheckRun) (*Engine, *mockGitHubClient, gh.ProjectItem, *stages.Stage) {
	t.Helper()
	client := &mockGitHubClient{
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) { return runs, nil },
	}
	stgs := []*stages.Stage{{Name: "Validate", Order: 1, Prompt: "v"}}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.PollSeconds = 60
	item := gh.ProjectItem{Number: 31, Repo: "owner/repo", LinkedPRHeadSHA: "abcdef0123456789", Labels: []string{"fabrik:awaiting-ci"}}
	return eng, client, item, stgs[0]
}

func lastComment(client *mockGitHubClient) string {
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) == 0 {
		return ""
	}
	return client.addCommentCalls[len(client.addCommentCalls)-1].body
}

func TestPauseForCIBackstop_NamesMergeGateAndReason(t *testing.T) {
	eng, client, item, stage := backstopMsgFixture(t, nGreenRuns(20))
	eng.noteGateHold(item, gateMerge, "CI checks pending", nil)

	if !eng.pauseForCIBackstop(&gh.ProjectBoard{}, item, stage, time.Now().Add(-4*time.Hour)) {
		t.Fatal("want a fresh pause")
	}
	body := lastComment(client)
	for _, want := range []string{
		"The awaiting-CI wait for stage **Validate** timed out while the PR was held by the merge gate: CI checks pending.",
		"observed holding this item for at least",
		"`fabrik:awaiting-ci` has been set for 4h0m0s",
		"20 of 20 checks passed",
		"The CI gate itself was never reached",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"waiting for checks to pass", "address any failures"} {
		if strings.Contains(body, bad) {
			t.Errorf("comment must not contain %q:\n%s", bad, body)
		}
	}
}

func TestPauseForCIBackstop_NoRecordedHold(t *testing.T) {
	eng, client, item, stage := backstopMsgFixture(t, nGreenRuns(3))
	eng.pauseForCIBackstop(&gh.ProjectBoard{}, item, stage, time.Time{})
	body := lastComment(client)
	if !strings.Contains(body, "no recorded gate holding the PR") || strings.Contains(body, "address any failures") {
		t.Errorf("comment = %s", body)
	}
	if !strings.Contains(body, "3 of 3 checks passed") {
		t.Errorf("comment = %s", body)
	}
}

func TestPauseForCIBackstop_LapsedHoldIsTreatedAsAbsent(t *testing.T) {
	eng, client, item, stage := backstopMsgFixture(t, nGreenRuns(3))
	start := time.Now()
	eng.SetClock(stubClock{t: start})
	eng.noteGateHold(item, gateMerge, "CI checks pending", nil)
	eng.SetClock(stubClock{t: start.Add(24 * time.Hour)})
	eng.pauseForCIBackstop(&gh.ProjectBoard{}, item, stage, time.Time{})
	if body := lastComment(client); !strings.Contains(body, "no recorded gate holding the PR") {
		t.Errorf("a hold the gate stopped confirming must not be named:\n%s", body)
	}
}

func TestPauseForCIBackstop_CIGateHoldKeepsTodaysText(t *testing.T) {
	eng, client, item, stage := backstopMsgFixture(t, []gh.CheckRun{{ID: 1, Name: "ci", Status: "in_progress"}})
	eng.noteGateHold(item, gateCI, "waiting for CI checks", nil)
	eng.pauseForCIBackstop(&gh.ProjectBoard{}, item, stage, time.Now().Add(-5*time.Hour))
	body := lastComment(client)
	if !strings.Contains(body, "The CI gate for stage **Validate** timed out waiting for checks to pass.") ||
		!strings.Contains(body, "address any failures") {
		t.Errorf("a CI-gate hold must keep today's text:\n%s", body)
	}
}

func TestPauseForCIBackstop_FailedCheckKeepsCITextAndNamesHold(t *testing.T) {
	runs := append(nGreenRuns(2), gh.CheckRun{ID: 50, Name: "bad", Status: "completed", Conclusion: "failure"})
	eng, client, item, stage := backstopMsgFixture(t, runs)
	eng.noteGateHold(item, gateMerge, "CI checks pending", nil)
	eng.pauseForCIBackstop(&gh.ProjectBoard{}, item, stage, time.Now().Add(-5*time.Hour))
	body := lastComment(client)
	if !strings.Contains(body, "timed out waiting for checks to pass") || !strings.Contains(body, "The merge gate was holding the item (CI checks pending), and 1 check(s) failed.") {
		t.Errorf("comment = %s", body)
	}
}

func TestPauseForCIBackstop_DedupAndReapply(t *testing.T) {
	eng, client, item, stage := backstopMsgFixture(t, nGreenRuns(2))
	eng.noteGateHold(item, gateMerge, "CI checks pending", nil)
	eng.pauseForCIBackstop(&gh.ProjectBoard{}, item, stage, time.Time{})
	first := lastComment(client)
	item.Comments = []gh.Comment{{Body: first}}
	if !hasCIGatePauseComment(item, stage) {
		t.Fatal("hasCIGatePauseComment must recognise the new backstop comment")
	}
	if eng.pauseForCIBackstop(&gh.ProjectBoard{}, item, stage, time.Time{}) {
		t.Fatal("same episode must reapply, not escalate")
	}
	client.mu.Lock()
	n := len(client.addCommentCalls)
	client.mu.Unlock()
	if n != 1 {
		t.Fatalf("comments = %d, want 1 (no repost, #1408)", n)
	}
}
