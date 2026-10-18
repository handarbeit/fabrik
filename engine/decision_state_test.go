package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// restartedEngine models a daemon restart: a fresh engine over the same decision-state dir.
func restartedEngine(t *testing.T, client *mockGitHubClient, dir string, clk ...Clock) *Engine {
	t.Helper()
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	if len(clk) > 0 {
		eng.SetClock(clk[0])
	}
	eng.EnableDecisionStateForTest(dir)
	return eng
}

func ejectionCount(e *Engine, key string) int {
	e.mergeTrainEjectionsMu.Lock()
	defer e.mergeTrainEjectionsMu.Unlock()
	return e.mergeTrainEjectionCounts[key]
}

func TestDecisionStore_MissingCorruptAndUnknownVersionAreAbsent(t *testing.T) {
	dir := t.TempDir()
	eng := restartedEngine(t, &mockGitHubClient{}, dir) // nothing on disk: fine
	if n := ejectionCount(eng, "owner/repo#1"); n != 0 {
		t.Fatalf("fresh state: count = %d", n)
	}

	if err := os.WriteFile(filepath.Join(dir, trainCapsFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, usageLimitFileName), []byte(`{"version":99,"until":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	eng = restartedEngine(t, &mockGitHubClient{}, dir) // must not panic or fail
	if _, held := eng.claudeUsageLimitedUntilTime(eng.now()); held {
		t.Fatal("an unknown-version usage-limit file must be treated as absent")
	}
	for _, name := range []string{trainCapsFileName, usageLimitFileName} {
		if _, err := os.Stat(filepath.Join(dir, name+".corrupt")); err != nil {
			t.Errorf("%s not quarantined: %v", name, err)
		}
	}
}

func TestEjectionCount_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	client := &mockGitHubClient{}
	eng := restartedEngine(t, client, dir)
	member := makeTrainItem(5, "Problem Issue")

	eng.ejectMemberAtHead("owner", "repo", member, "aaa", "conflict", nil, nil, true)
	eng.ejectMemberAtHead("owner", "repo", member, "aaa", "conflict", nil, nil, true)

	eng2 := restartedEngine(t, client, dir)
	if n := ejectionCount(eng2, "owner/repo#5"); n != 2 {
		t.Fatalf("count after restart = %d, want 2", n)
	}
	eng2.ejectMemberAtHead("owner", "repo", member, "aaa", "conflict", nil, nil, true)
	client.mu.Lock()
	defer client.mu.Unlock()
	paused := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			paused = true
		}
	}
	if !paused {
		t.Fatal("the third ejection, across a restart, must reach the cap (MaxMergeTrainEjections = 3) and pause the member")
	}
}

func TestEjectionCount_RestartsOnHeadChange(t *testing.T) {
	eng := restartedEngine(t, &mockGitHubClient{}, t.TempDir())
	member := makeTrainItem(5, "Problem Issue")

	eng.ejectMemberAtHead("owner", "repo", member, "aaa", "conflict", nil, nil, true)
	eng.ejectMemberAtHead("owner", "repo", member, "aaa", "conflict", nil, nil, true)
	eng.ejectMemberAtHead("owner", "repo", member, "bbb", "conflict", nil, nil, true) // new push
	if n := ejectionCount(eng, "owner/repo#5"); n != 1 {
		t.Fatalf("count after a head change = %d, want 1 (only ejections against the current head count)", n)
	}
	// An unknown head never resets: it errs toward counting.
	eng.ejectMemberAtHead("owner", "repo", member, "", "conflict", nil, nil, true)
	if n := ejectionCount(eng, "owner/repo#5"); n != 2 {
		t.Fatalf("count after an unknown-head ejection = %d, want 2", n)
	}
}

func TestEjectionCount_GreenTrialAndLandingBreakTheRun(t *testing.T) {
	eng := restartedEngine(t, &mockGitHubClient{}, t.TempDir())
	member := makeTrainItem(5, "Problem Issue")
	m := trainMember{item: member, headSHA: "aaa"}
	p := trialParams{owner: "owner", repo: "repo"}

	eng.ejectMemberAtHead("owner", "repo", member, "aaa", "conflict", nil, nil, true)
	eng.recordTrialIfCounts(t.Context(), p, []trainMember{m}, TrainCIRed, nil)
	if n := ejectionCount(eng, "owner/repo#5"); n != 1 {
		t.Fatalf("a red trial must not reset the count, got %d", n)
	}
	eng.recordTrialIfCounts(t.Context(), p, []trainMember{m}, TrainCIGreen, nil)
	if n := ejectionCount(eng, "owner/repo#5"); n != 0 {
		t.Fatalf("a green trial verdict must break the run, got %d", n)
	}

	eng.ejectMemberAtHead("owner", "repo", member, "aaa", "conflict", nil, nil, true)
	eng.resetEjectionCount("owner", "repo", 5)
	if n := ejectionCount(eng, "owner/repo#5"); n != 0 {
		t.Fatalf("a landing must reset the count, got %d", n)
	}
}

// The count is persisted before the ejection comment is posted, so a crash between the two
// cannot grant a free ejection.
func TestEjectMember_CountsAndPersistsBeforeComment(t *testing.T) {
	dir := t.TempDir()
	client := &mockGitHubClient{}
	var onDisk string
	client.addCommentFn = func(owner, repo string, n int, body string) (int, error) {
		if strings.Contains(body, "ejected") {
			b, _ := os.ReadFile(filepath.Join(dir, trainCapsFileName))
			onDisk = string(b)
		}
		return 0, nil
	}
	eng := restartedEngine(t, client, dir)
	eng.ejectMemberAtHead("owner", "repo", makeTrainItem(5, "x"), "aaa", "conflict", nil, nil, true)
	if !strings.Contains(onDisk, `"owner/repo#5"`) {
		t.Fatalf("the count was not on disk when the comment was posted: %q", onDisk)
	}
}

func TestTrialWindow_SurvivesRestartAndUsesEngineClock(t *testing.T) {
	dir := t.TempDir()
	client := &mockGitHubClient{}
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	eng := restartedEngine(t, client, dir, clk)

	eng.recordTrial("owner/repo:")
	clk.t = clk.t.Add(10 * time.Minute)
	if n := eng.recordTrial("owner/repo:"); n != 2 {
		t.Fatalf("trial count = %d, want 2 (recordTrial must read the engine clock)", n)
	}

	eng2 := restartedEngine(t, client, dir, clk)
	if n, _ := eng2.isRunawayTripped("owner/repo:"); n != 2 {
		t.Fatalf("trials after restart = %d, want 2", n)
	}
	clk.t = clk.t.Add(2 * time.Hour) // past the 60-minute window
	if n, _ := eng2.isRunawayTripped("owner/repo:"); n != 0 {
		t.Fatalf("trials past the window = %d, want 0", n)
	}
}

func TestAutoRepairAttempts_SurviveRestart(t *testing.T) {
	dir := t.TempDir()
	client := &mockGitHubClient{}
	eng := restartedEngine(t, client, dir)
	key := autoRepairKey("owner", "repo", 7)
	eng.recordAutoRepairAttempt(key, repairAttempt{BaseSHA: "base1", MemberHeadSHA: "h", At: time.Now()})
	if !eng.takeRequeue(key, "base1") {
		t.Fatal("first requeue should be granted")
	}

	eng2 := restartedEngine(t, client, dir)
	if n := eng2.autoRepairAttemptsAt(key, "base1"); n != 1 {
		t.Fatalf("attempts after restart = %d, want 1", n)
	}
	if eng2.takeRequeue(key, "base1") {
		t.Fatal("the spent requeue must stay spent across a restart")
	}
}

func TestPruneDecisionState_DropsClosedAndAbsentItems(t *testing.T) {
	dir := t.TempDir()
	eng := restartedEngine(t, &mockGitHubClient{}, dir)
	eng.ejectMemberAtHead("owner", "repo", makeTrainItem(5, "keep"), "a", "c", nil, nil, true)
	eng.ejectMemberAtHead("owner", "repo", makeTrainItem(6, "closed"), "a", "c", nil, nil, true)
	eng.ejectMemberAtHead("owner", "repo", makeTrainItem(7, "gone"), "a", "c", nil, nil, true)

	keep := makeTrainItem(5, "keep")
	closed := makeTrainItem(6, "closed")
	closed.IsClosed = true
	eng.pruneDecisionState(&gh.ProjectBoard{Items: []gh.ProjectItem{keep, closed}})

	if ejectionCount(eng, "owner/repo#5") != 1 || ejectionCount(eng, "owner/repo#6") != 0 || ejectionCount(eng, "owner/repo#7") != 0 {
		t.Fatal("prune must keep live items and drop closed/absent ones")
	}
	if n := ejectionCount(restartedEngine(t, &mockGitHubClient{}, dir), "owner/repo#6"); n != 0 {
		t.Fatalf("pruned count came back after restart: %d", n)
	}
	// An empty board is unreadable, not "everything left".
	eng.pruneDecisionState(&gh.ProjectBoard{})
	if ejectionCount(eng, "owner/repo#5") != 1 {
		t.Fatal("an empty board must prune nothing")
	}
}

func TestUsageLimitHold_PersistRestoreClear(t *testing.T) {
	dir := t.TempDir()
	client := &mockGitHubClient{}
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	eng := restartedEngine(t, client, dir, clk)

	eng.activateClaudeUsageLimit(1, nil, eng.now())

	eng2 := restartedEngine(t, client, dir, clk)
	until, held := eng2.claudeUsageLimitedUntilTime(clk.t)
	if !held || !until.Equal(clk.t.Add(claudeUsageLimitFallbackBackoff)) {
		t.Fatalf("restored hold = %v held=%v, want until %v", until, held, clk.t.Add(claudeUsageLimitFallbackBackoff))
	}

	eng2.clearClaudeUsageLimit("test")
	if _, err := os.Stat(filepath.Join(dir, usageLimitFileName)); !os.IsNotExist(err) {
		t.Fatalf("clearing the hold must delete the file, stat err = %v", err)
	}
}

func TestUsageLimitHold_ExpiredAndFarFutureAtLoad(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	write := func(until time.Time) {
		store := newDecisionStore(dir)
		if err := store.save(usageLimitFileName, &usageLimitFile{Version: decisionStateVersion, Until: until}); err != nil {
			t.Fatal(err)
		}
	}
	load := func() *Engine {
		eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
		eng.SetClock(clk)
		eng.EnableDecisionStateForTest(dir)
		return eng
	}

	write(clk.t.Add(-time.Minute))
	eng := load()
	if _, held := eng.claudeUsageLimitedUntilTime(clk.t); held {
		t.Fatal("an expired persisted hold must be ignored")
	}
	if _, err := os.Stat(filepath.Join(dir, usageLimitFileName)); !os.IsNotExist(err) {
		t.Fatal("an expired hold file must be removed")
	}

	write(clk.t.Add(100 * 24 * time.Hour))
	eng = load()
	until, held := eng.claudeUsageLimitedUntilTime(clk.t)
	if !held || until.After(clk.t.Add(usageLimitMaxResetHorizon)) {
		t.Fatalf("a far-future deadline must be clamped to the 8-day ceiling, got %v held=%v", until, held)
	}
}
