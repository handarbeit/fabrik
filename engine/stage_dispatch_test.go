package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// dispatchTestEngine is an engine with a temp session root, a mock client (for
// the pause comment and labels) and the given cap.
func dispatchTestEngine(t *testing.T, limit int) (*Engine, *mockGitHubClient) {
	t.Helper()
	client := &mockGitHubClient{}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxConcurrent: 5, MaxStageDispatches: limit, Stages: testStages()},
		client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()),
	)
	eng.SetSessionDirForTest(t.TempDir())
	return eng, client
}

var dispatchTestItem = gh.ProjectItem{Number: 31, Title: "t", Status: "Research"}

func readCount(e *Engine, item gh.ProjectItem, stage string) int {
	rec, _ := readStageDispatch(e.stageDispatchPath(item, stage))
	return rec.Count
}

func TestStageDispatch_RecordCountsAndKeepsLastOutcomes(t *testing.T) {
	e, _ := dispatchTestEngine(t, 25)
	for i := 0; i < 7; i++ {
		e.recordStageDispatch(dispatchTestItem, "Research", dispatchOutcomeIncomplete, stageInputs{HeadSHA: "h"})
	}
	e.recordStageDispatch(dispatchTestItem, "Research", dispatchOutcomeComplete, stageInputs{HeadSHA: "h"})
	rec, found := readStageDispatch(e.stageDispatchPath(dispatchTestItem, "Research"))
	if !found || rec.Count != 8 {
		t.Fatalf("count = %d (found %v), want 8", rec.Count, found)
	}
	if len(rec.Outcomes) != stageDispatchOutcomeKeep || rec.Outcomes[len(rec.Outcomes)-1] != dispatchOutcomeComplete {
		t.Errorf("outcomes = %v, want the last %d ending in complete", rec.Outcomes, stageDispatchOutcomeKeep)
	}
	if rec.Inputs == nil || rec.Inputs.HeadSHA != "h" {
		t.Errorf("inputs not recorded: %+v", rec.Inputs)
	}
	// The record is a sidecar next to the stage's session pointer, mode 0600.
	info, err := os.Stat(e.stageDispatchPath(dispatchTestItem, "Research"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Errorf("record file: err=%v mode=%v, want 0600", err, info)
	}
}

func TestStageDispatch_CorruptRecordIsAbsent(t *testing.T) {
	e, _ := dispatchTestEngine(t, 3)
	path := e.stageDispatchPath(dispatchTestItem, "Research")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("{not json"), 0600)
	if _, found := readStageDispatch(path); found {
		t.Fatal("corrupt record must read as absent")
	}
	if e.dispatchLimitReached(dispatchTestItem, testStageByName(t, "Research")) {
		t.Fatal("a corrupt record must not pause")
	}
	// The next dispatch starts the count over from a clean file.
	e.recordStageDispatch(dispatchTestItem, "Research", dispatchOutcomeIncomplete, stageInputs{})
	if got := readCount(e, dispatchTestItem, "Research"); got != 1 {
		t.Fatalf("count after corrupt record = %d, want 1", got)
	}
}

func TestStageDispatch_ResetKeepsInputsAndSkipsMissingRecord(t *testing.T) {
	e, _ := dispatchTestEngine(t, 3)
	e.resetStageDispatches(dispatchTestItem, "Research") // no record: no file created
	if _, err := os.Stat(e.stageDispatchPath(dispatchTestItem, "Research")); err == nil {
		t.Fatal("reset must not create a record")
	}
	e.recordStageDispatch(dispatchTestItem, "Research", dispatchOutcomeComplete, stageInputs{HeadSHA: "h"})
	e.resetStageDispatches(dispatchTestItem, "Research")
	rec, _ := readStageDispatch(e.stageDispatchPath(dispatchTestItem, "Research"))
	if rec.Count != 0 || len(rec.Outcomes) != 0 || rec.Paused {
		t.Errorf("episode not reset: %+v", rec)
	}
	if rec.Inputs == nil || rec.Inputs.HeadSHA != "h" {
		t.Errorf("reset must keep the previous run's inputs: %+v", rec.Inputs)
	}
}

func TestStageDispatch_ClearOtherStagesOnDispatch(t *testing.T) {
	e, _ := dispatchTestEngine(t, 3)
	e.recordStageDispatch(dispatchTestItem, "Research", dispatchOutcomeComplete, stageInputs{})
	e.recordStageDispatch(dispatchTestItem, "Plan", dispatchOutcomeComplete, stageInputs{})
	e.clearOtherStageDispatches(dispatchTestItem, "Plan")
	if got := readCount(e, dispatchTestItem, "Research"); got != 0 {
		t.Errorf("Research record survived a Plan dispatch: count %d", got)
	}
	if got := readCount(e, dispatchTestItem, "Plan"); got != 1 {
		t.Errorf("Plan record lost: count %d", got)
	}
}

func TestStageDispatch_CapPausesOnceAndIsStrict(t *testing.T) {
	e, client := dispatchTestEngine(t, 3)
	stage := testStageByName(t, "Research")
	for i := 0; i < 2; i++ {
		e.recordStageDispatch(dispatchTestItem, "Research", dispatchOutcomeComplete, stageInputs{})
		if e.dispatchLimitReached(dispatchTestItem, stage) {
			t.Fatalf("paused after %d dispatches, limit is 3", i+1)
		}
	}
	e.recordStageDispatch(dispatchTestItem, "Research", dispatchOutcomeComplete, stageInputs{})
	if !e.dispatchLimitReached(dispatchTestItem, stage) {
		t.Fatal("expected a pause once the count reached the limit")
	}
	client.mu.Lock()
	comments := append([]addCommentCall(nil), client.addCommentCalls...)
	labels := append([]addLabelCall(nil), client.addLabelCalls...)
	client.mu.Unlock()
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want exactly 1", len(comments))
	}
	body := comments[0].body
	for _, want := range []string{"🏭 **Fabrik — stage dispatch limit reached**", "Research", "3 time(s)", "limit of 3", "complete (completion not recorded)", "--max-stage-dispatches"} {
		if !strings.Contains(body, want) {
			t.Errorf("pause comment missing %q:\n%s", want, body)
		}
	}
	var paused, awaiting, failed bool
	for _, l := range labels {
		switch l.labelName {
		case "fabrik:paused":
			paused = true
		case "fabrik:awaiting-input":
			awaiting = true
		case "stage:Research:failed":
			failed = true
		}
	}
	if !paused || !awaiting || failed {
		t.Errorf("labels paused=%v awaiting-input=%v failed=%v, want true true false", paused, awaiting, failed)
	}
	snap, err := e.store.Get("owner/repo", dispatchTestItem.Number)
	if err != nil || !snap.PausedByEngine("Research") {
		t.Errorf("EnginePaused not recorded (err=%v)", err)
	}
}

// A failed fabrik:paused write must not be recorded as a pause: the next poll
// would read EnginePaused-without-label as a manual unpause and reset the count,
// giving the looping stage a fresh allowance. The pause is retried instead.
func TestStageDispatch_FailedPauseLabelIsNotRecordedAndIsRetried(t *testing.T) {
	e, client := dispatchTestEngine(t, 2)
	stage := testStageByName(t, "Research")
	fail := true
	client.addLabelToIssueFn = func(_, _ string, _ int, label string) error {
		if fail && label == "fabrik:paused" {
			return os.ErrPermission
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		e.recordStageDispatch(dispatchTestItem, "Research", dispatchOutcomeComplete, stageInputs{})
	}
	if !e.dispatchLimitReached(dispatchTestItem, stage) {
		t.Fatal("expected the dispatch to be refused at the cap")
	}
	rec, _ := readStageDispatch(e.stageDispatchPath(dispatchTestItem, "Research"))
	if rec.Paused || rec.Count != 2 {
		t.Errorf("record paused=%v count=%d, want paused=false count=2", rec.Paused, rec.Count)
	}
	if snap, err := e.store.Get("owner/repo", dispatchTestItem.Number); err == nil && snap.PausedByEngine("Research") {
		t.Error("EnginePaused recorded for a pause label that was never written")
	}
	client.mu.Lock()
	n := len(client.addCommentCalls)
	client.mu.Unlock()
	if n != 0 {
		t.Errorf("comments = %d, want 0 until the pause lands", n)
	}
	// The next poll still refuses (the count was not reset) and now the pause lands.
	fail = false
	if !e.dispatchLimitReached(dispatchTestItem, stage) {
		t.Fatal("expected the retry to pause at the cap")
	}
	rec, _ = readStageDispatch(e.stageDispatchPath(dispatchTestItem, "Research"))
	if !rec.Paused || rec.Count != 2 {
		t.Errorf("after retry paused=%v count=%d, want true 2", rec.Paused, rec.Count)
	}
}

func TestStageDispatch_ZeroDisablesAndSeamNeutralises(t *testing.T) {
	stage := testStageByName(t, "Research")
	e, client := dispatchTestEngine(t, 0)
	for i := 0; i < 50; i++ {
		e.recordStageDispatch(dispatchTestItem, "Research", dispatchOutcomeComplete, stageInputs{})
	}
	if e.dispatchLimitReached(dispatchTestItem, stage) {
		t.Fatal("cap 0 must never pause")
	}
	e2, client2 := dispatchTestEngine(t, 2)
	e2.SetStageDispatchCapDisabledForTest(true)
	for i := 0; i < 5; i++ {
		e2.recordStageDispatch(dispatchTestItem, "Research", dispatchOutcomeComplete, stageInputs{})
	}
	if e2.dispatchLimitReached(dispatchTestItem, stage) {
		t.Fatal("neutralised cap must not pause")
	}
	if len(client.addCommentCalls)+len(client2.addCommentCalls) != 0 {
		t.Error("no comment expected")
	}
}

// A record the cap paused, seen again with the item no longer paused, was resumed
// by a human — possibly across a restart that lost the in-memory pause record.
func TestStageDispatch_ResumedAfterCapStartsNewEpisode(t *testing.T) {
	e, _ := dispatchTestEngine(t, 2)
	stage := testStageByName(t, "Research")
	e.recordStageDispatch(dispatchTestItem, "Research", dispatchOutcomeComplete, stageInputs{})
	e.recordStageDispatch(dispatchTestItem, "Research", dispatchOutcomeComplete, stageInputs{})
	if !e.dispatchLimitReached(dispatchTestItem, stage) {
		t.Fatal("expected the cap pause")
	}
	// processItem's paused gates return before the check while the item is paused,
	// so reaching it again means the pause was lifted.
	if e.dispatchLimitReached(dispatchTestItem, stage) {
		t.Fatal("a resumed item must not be re-paused immediately")
	}
	if got := readCount(e, dispatchTestItem, "Research"); got != 0 {
		t.Fatalf("count after resume = %d, want 0", got)
	}
	// Raising the cap past a stale Paused flag must not disarm the next cap.
	e.recordStageDispatch(dispatchTestItem, "Research", dispatchOutcomeComplete, stageInputs{})
	rec, _ := readStageDispatch(e.stageDispatchPath(dispatchTestItem, "Research"))
	if rec.Paused {
		t.Error("a dispatch must clear the Paused flag")
	}
}

func TestStageDispatchOutcome(t *testing.T) {
	cases := []struct {
		completed, blocked, slice bool
		err                       error
		want                      string
	}{
		{true, false, false, nil, dispatchOutcomeComplete},
		{false, true, false, nil, dispatchOutcomeBlocked},
		{false, false, true, os.ErrClosed, dispatchOutcomeSlice},
		{false, false, false, os.ErrClosed, dispatchOutcomeError},
		{false, false, false, nil, dispatchOutcomeIncomplete},
	}
	for _, c := range cases {
		if got := stageDispatchOutcome(c.completed, c.blocked, c.slice, c.err); got != c.want {
			t.Errorf("stageDispatchOutcome(%+v) = %q, want %q", c, got, c.want)
		}
	}
}

func TestComputeStageInputs_ExcludesFabrikCommentsAndTracksChanges(t *testing.T) {
	base := gh.ProjectItem{Body: "spec", Comments: []gh.Comment{{ID: "c1", Body: "human"}}}
	dir := t.TempDir() // not a git repo: head unknown
	a := computeStageInputs(base, dir)

	withOwn := base
	withOwn.Comments = append(append([]gh.Comment(nil), base.Comments...), gh.Comment{ID: "c2", Body: "🏭 **Fabrik — stage: Research**\nout", CreatedAt: time.Now()})
	if b := computeStageInputs(withOwn, dir); b.CommentsHash != a.CommentsHash {
		t.Error("the engine's own stage comment must not change the comment set")
	}
	withHuman := base
	withHuman.Comments = append(append([]gh.Comment(nil), base.Comments...), gh.Comment{ID: "c3", Body: "new input"})
	if b := computeStageInputs(withHuman, dir); b.CommentsHash == a.CommentsHash {
		t.Error("a new human comment must change the comment set")
	}
	edited := base
	edited.Comments = []gh.Comment{{ID: "c1", Body: "human edited"}}
	if b := computeStageInputs(edited, dir); b.CommentsHash == a.CommentsHash {
		t.Error("an edited comment must change the comment set")
	}
	body := base
	body.Body = "spec v2"
	if b := computeStageInputs(body, dir); b.BodyHash == a.BodyHash {
		t.Error("a new body must change the body hash")
	}
}

func TestStageInputs_UnchangedSince(t *testing.T) {
	cur := stageInputs{HeadSHA: "h", BodyHash: "b", CommentsHash: "c"}
	same := cur
	if !cur.unchangedSince(&same) {
		t.Error("identical inputs are unchanged")
	}
	for name, prev := range map[string]*stageInputs{
		"nil":      nil,
		"head":     {HeadSHA: "x", BodyHash: "b", CommentsHash: "c"},
		"body":     {HeadSHA: "h", BodyHash: "x", CommentsHash: "c"},
		"comments": {HeadSHA: "h", BodyHash: "b", CommentsHash: "x"},
		"no head":  {BodyHash: "b", CommentsHash: "c"},
	} {
		if cur.unchangedSince(prev) {
			t.Errorf("%s: must read as changed", name)
		}
	}
	if (stageInputs{BodyHash: "b", CommentsHash: "c"}).unchangedSince(&stageInputs{BodyHash: "b", CommentsHash: "c"}) {
		t.Error("an unknown head on both sides must read as changed")
	}
}

func TestPruneSessions_AgesOutDispatchRecords(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	issueDir := filepath.Join(root, "issue-1")
	oldRec := seedSessionFile(t, issueDir, "Research.dispatches", now.Add(-15*24*time.Hour))
	newRec := seedSessionFile(t, issueDir, "Plan.dispatches", now.Add(-24*time.Hour))
	noResolve := func(string) (string, bool) { return "", false }
	pruneSessions(root, 14, now, "owner/repo", noResolve, func(string, int) bool { return false })
	if _, err := os.Stat(oldRec); !os.IsNotExist(err) {
		t.Error("old dispatch record should be removed")
	}
	if _, err := os.Stat(newRec); err != nil {
		t.Errorf("recent dispatch record should be kept: %v", err)
	}
	// An in-flight item keeps even an old record.
	issueDir2 := filepath.Join(root, "issue-2")
	inFlight := seedSessionFile(t, issueDir2, "Research.dispatches", now.Add(-15*24*time.Hour))
	pruneSessions(root, 14, now, "owner/repo", noResolve, func(string, int) bool { return true })
	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("in-flight item's record must be kept: %v", err)
	}
}

func testStageByName(t *testing.T, name string) *stages.Stage {
	t.Helper()
	for _, s := range testStages() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no test stage %q", name)
	return nil
}

// The reset follows a durable completion write, never the `completed` branch: a
// failed complete-label write leaves the count in place (the #2388 failure), a
// successful one ends the episode.
func TestHandleStageComplete_ResetsDispatchCountOnlyAfterDurableWrite(t *testing.T) {
	item := gh.ProjectItem{Number: 41, ItemID: "PVTI_41", Status: "Research"}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research"}

	for _, tc := range []struct {
		name      string
		failWrite bool
		wantCount int
	}{
		{"label write fails", true, 2},
		{"label write succeeds", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{}
			if tc.failWrite {
				client.addLabelToIssueFn = func(_, _ string, _ int, label string) error {
					if label == "stage:Research:complete" {
						return os.ErrPermission
					}
					return nil
				}
			}
			eng := testEngineWithStages(t, client, testStagesWithValidate())
			eng.SetSessionDirForTest(t.TempDir())
			eng.recordStageDispatch(item, "Research", dispatchOutcomeComplete, stageInputs{})
			eng.recordStageDispatch(item, "Research", dispatchOutcomeComplete, stageInputs{})
			eng.handleStageComplete(context.Background(), board, item, stage)
			if got := readCount(eng, item, "Research"); got != tc.wantCount {
				t.Errorf("count = %d, want %d", got, tc.wantCount)
			}
		})
	}
}

// Arming fabrik:awaiting-ci is the durable completion of a wait_for_ci stage.
func TestHandleStageComplete_AwaitingCIArmResetsDispatchCount(t *testing.T) {
	item := gh.ProjectItem{Number: 42, ItemID: "PVTI_42", Status: "Research"}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	waitCI := true
	stage := &stages.Stage{Name: "Research", WaitForCI: &waitCI}

	for _, tc := range []struct {
		name      string
		failWrite bool
		wantCount int
	}{
		{"arm fails", true, 2},
		{"arm succeeds", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{}
			if tc.failWrite {
				client.addLabelToIssueFn = func(_, _ string, _ int, label string) error {
					if label == "fabrik:awaiting-ci" {
						return os.ErrPermission
					}
					return nil
				}
			}
			eng := testEngineWithStages(t, client, testStagesWithValidate())
			eng.SetSessionDirForTest(t.TempDir())
			eng.recordStageDispatch(item, "Research", dispatchOutcomeComplete, stageInputs{})
			eng.recordStageDispatch(item, "Research", dispatchOutcomeComplete, stageInputs{})
			eng.handleStageComplete(context.Background(), board, item, stage)
			if got := readCount(eng, item, "Research"); got != tc.wantCount {
				t.Errorf("count = %d, want %d", got, tc.wantCount)
			}
		})
	}
}
