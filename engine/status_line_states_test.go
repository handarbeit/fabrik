package engine

import (
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// statusLineStatesEngine builds a status-line-enabled engine over depTestStages
// (Specify → Research → Implement), the shape testEngineWithStages gives a
// statusField for every stage.
func statusLineStatesEngine(t *testing.T) (*Engine, *mockGitHubClient) {
	t.Helper()
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, depTestStages())
	enableStatusLine(t, eng, client)
	return eng, client
}

func stateItem(n int) gh.ProjectItem {
	return gh.ProjectItem{Number: n, Repo: "owner/repo", ItemID: "PVTI_state"}
}

func TestStatusLine_Paused_ReasonFromComment(t *testing.T) {
	eng, client := statusLineStatesEngine(t)
	eng.pauseIssue(stateItem(7), "🏭 **Fabrik — CI timeout**\n\nCI did not finish in time.", pauseOpts{})
	if got := statusLineSeq(client, "PVTI_state"); !equalSeq(got, []string{"paused: CI timeout"}) {
		t.Errorf("lines = %q", got)
	}
}

func TestPauseReasonFromComment(t *testing.T) {
	cases := map[string]string{
		"🏭 **Fabrik — cycle detected**\n\nbody":                "cycle detected",
		"🏭 **Fabrik merge-train — pausing after 3 ejections**": "pausing after 3 ejections",
		"no header at all": "",
		"":                 "",
	}
	for in, want := range cases {
		if got := pauseReasonFromComment(in); got != want {
			t.Errorf("pauseReasonFromComment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusLine_Blocked_ThenUnblockedClears(t *testing.T) {
	eng, client := statusLineStatesEngine(t)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := stateItem(10)
	item.BlockedBy = []gh.Dependency{{Number: 1568, State: "OPEN", Repo: "owner/repo"}}
	stage := &stages.Stage{Name: "Research"}

	if !eng.checkDependencies(board, item, stage) {
		t.Fatal("expected blocked")
	}
	if got := statusLineSeq(client, "PVTI_state"); !equalSeq(got, []string{"blocked by #1568"}) {
		t.Fatalf("lines = %q", got)
	}

	// Blocker resolved while the label is still present: the line is cleared.
	item.Labels = []string{"fabrik:blocked"}
	item.BlockedBy = []gh.Dependency{{Number: 1568, State: "CLOSED", Repo: "owner/repo"}}
	if eng.checkDependencies(board, item, stage) {
		t.Fatal("expected unblocked")
	}
	if got := statusLineSeq(client, "PVTI_state"); !equalSeq(got, []string{"blocked by #1568", "<cleared>"}) {
		t.Errorf("lines = %q", got)
	}
}

func TestStatusLine_AdvanceClearsLine(t *testing.T) {
	eng, client := statusLineStatesEngine(t)
	item := stateItem(11)
	eng.setStatusLine(item, statusLineStageRunning("Specify"))

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	if err := eng.advanceToNextStage(board, item, eng.stageByName("Specify")); err != nil {
		t.Fatalf("advanceToNextStage: %v", err)
	}
	if got := statusLineSeq(client, "PVTI_state"); !equalSeq(got, []string{"Specify · running", "<cleared>"}) {
		t.Errorf("lines = %q", got)
	}
}

// The Done move of every path funnels through advanceToNextStage; the cleanup
// stage is the next stage for the last pipeline stage.
func TestStatusLine_DoneMoveClears(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithCleanup())
	enableStatusLine(t, eng, client)
	item := stateItem(12)
	eng.setStatusLine(item, "landing")

	last := eng.cfg.Stages[len(eng.cfg.Stages)-2]
	if err := eng.advanceToNextStage(&gh.ProjectBoard{ProjectID: "PVT_1"}, item, last); err != nil {
		t.Fatalf("advanceToNextStage: %v", err)
	}
	if got := statusLineSeq(client, "PVTI_state"); !equalSeq(got, []string{"landing", "<cleared>"}) {
		t.Errorf("lines = %q", got)
	}
}

func TestStatusLine_FailedAdvanceKeepsLine(t *testing.T) {
	eng, client := statusLineStatesEngine(t)
	client.updateProjectItemStatusFn = func(_, _, _, _ string) error { return errors.New("api down") }
	item := stateItem(13)
	eng.setStatusLine(item, "landing")
	_ = eng.advanceToNextStage(&gh.ProjectBoard{ProjectID: "PVT_1"}, item, eng.stageByName("Specify"))
	if got := statusLineSeq(client, "PVTI_state"); !equalSeq(got, []string{"landing"}) {
		t.Errorf("a failed advance must not clear the line: %q", got)
	}
}

func TestStatusLine_WithStatusLine_RestoresPrevious(t *testing.T) {
	eng, client := statusLineStatesEngine(t)
	item := stateItem(14)
	eng.setStatusLine(item, "awaiting CI on PR #5")
	done := eng.withStatusLine(item, statusLineCommentReview("Validate"))
	done()
	want := []string{"awaiting CI on PR #5", "Validate · comment review", "awaiting CI on PR #5"}
	if got := statusLineSeq(client, "PVTI_state"); !equalSeq(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}

func TestStatusLine_WithStatusLine_LeavesNewerLineAlone(t *testing.T) {
	eng, client := statusLineStatesEngine(t)
	item := stateItem(15)
	done := eng.withStatusLine(item, statusLineCommentReview("Validate"))
	eng.setStatusLine(item, statusLinePaused("cycle limit")) // the review paused the item
	done()
	want := []string{"Validate · comment review", "paused: cycle limit"}
	if got := statusLineSeq(client, "PVTI_state"); !equalSeq(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}

func TestStatusLine_WithStatusLine_NoPreviousClears(t *testing.T) {
	eng, client := statusLineStatesEngine(t)
	item := stateItem(16)
	eng.withStatusLine(item, statusLineCommentReview("Validate"))()
	if got := statusLineSeq(client, "PVTI_state"); !equalSeq(got, []string{"Validate · comment review", "<cleared>"}) {
		t.Errorf("lines = %q", got)
	}
}

func TestStatusLine_ClaudeLimitLine(t *testing.T) {
	until := time.Date(2026, 10, 10, 18, 5, 0, 0, time.Local)
	if got := statusLineClaudeLimit(until); got != "claude-limit until 18:05" {
		t.Errorf("line = %q", got)
	}
}

func TestStatusLine_AwaitingCIWithoutPR(t *testing.T) {
	if got := statusLineAwaitingCI(0); got != "awaiting CI" {
		t.Errorf("line = %q", got)
	}
}

// Done moves that bypass advanceToNextStage must clear the line too.
func TestStatusLine_NoWorkNeededDoneMoveClears(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithCleanup())
	enableStatusLine(t, eng, client)
	item := gh.ProjectItem{Number: 15, ItemID: "PVTI_state", Repo: "owner/repo", Status: "Plan"}
	eng.setStatusLine(item, statusLineStageRunning("Plan"))

	eng.settleNoWorkNeeded(&gh.ProjectBoard{ProjectID: "PVT_1"}, item, &stages.Stage{Name: "Plan", Order: 2})
	if got := statusLineSeq(client, "PVTI_state"); !equalSeq(got, []string{"Plan · running", "<cleared>"}) {
		t.Errorf("lines = %q", got)
	}
}

func TestStatusLine_ClosedItemAdvanceClears(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithCleanup())
	enableStatusLine(t, eng, client)
	item := gh.ProjectItem{Number: 16, ItemID: "PVTI_state", Repo: "owner/repo", Status: "Plan"}
	eng.setStatusLine(item, statusLineStageRunning("Plan"))

	eng.advanceClosedItemToDone(&gh.ProjectBoard{ProjectID: "PVT_1"}, item, "opt-done", "Done")
	if got := statusLineSeq(client, "PVTI_state"); !equalSeq(got, []string{"Plan · running", "<cleared>"}) {
		t.Errorf("lines = %q", got)
	}
}

// A worker that ends with its "<Stage> · running" line still showing clears
// it; a newer line written meanwhile is left alone.
func TestStatusLine_ClearIfShowing(t *testing.T) {
	eng, client := statusLineStatesEngine(t)
	item := stateItem(17)

	eng.setStatusLine(item, statusLineStageRunning("Implement"))
	eng.clearStatusLineIfShowing(item, statusLineStageRunning("Implement"))
	if got := statusLineSeq(client, "PVTI_state"); !equalSeq(got, []string{"Implement · running", "<cleared>"}) {
		t.Fatalf("lines = %q", got)
	}

	eng.setStatusLine(item, statusLineStageRunning("Review"))
	eng.setStatusLine(item, statusLineAwaitingReview)
	eng.clearStatusLineIfShowing(item, statusLineStageRunning("Review"))
	want := []string{"Implement · running", "<cleared>", "Review · running", "awaiting review"}
	if got := statusLineSeq(client, "PVTI_state"); !equalSeq(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}
