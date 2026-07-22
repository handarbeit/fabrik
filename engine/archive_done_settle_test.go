package engine

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

func TestSettleArchiveDoneItems_PastGracePeriod_Archives(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	eng.cfg.ArchiveAfter = 24 * time.Hour
	appliedAt := time.Now().Add(-25 * time.Hour)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return appliedAt, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1, ItemID: "PVTI_1", Repo: "owner/repo",
		Status: "Done", Labels: []string{"stage:Done:complete"},
	}
	board.Items = []gh.ProjectItem{item}

	eng.settleArchiveDoneItems(board)

	if len(client.archiveProjectItemCalls) != 1 {
		t.Fatalf("expected 1 archive call, got %d", len(client.archiveProjectItemCalls))
	}
	if client.archiveProjectItemCalls[0].projectID != "PVT_1" || client.archiveProjectItemCalls[0].itemID != "PVTI_1" {
		t.Errorf("unexpected archive call: %+v", client.archiveProjectItemCalls[0])
	}
}

func TestSettleArchiveDoneItems_WithinGracePeriod_NotArchivedAndCached(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	eng.cfg.ArchiveAfter = 24 * time.Hour
	appliedAt := time.Now().Add(-1 * time.Hour) // only 1h ago; grace period is 24h
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return appliedAt, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 2, ItemID: "PVTI_2", Repo: "owner/repo",
		Status: "Done", Labels: []string{"stage:Done:complete"},
	}
	board.Items = []gh.ProjectItem{item}

	eng.settleArchiveDoneItems(board)

	if len(client.archiveProjectItemCalls) != 0 {
		t.Fatalf("expected no archive call within grace period, got %d", len(client.archiveProjectItemCalls))
	}
	if len(client.fetchLabelAppliedAtCalls) != 1 {
		t.Fatalf("expected exactly 1 FetchLabelAppliedAt call, got %d", len(client.fetchLabelAppliedAtCalls))
	}

	snap, err := eng.store.Get("owner/repo", 2)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	cached := snap.CooldownAt(archiveEligibleAtCooldownReason)
	wantEligibleAt := appliedAt.Add(24 * time.Hour)
	if cached.IsZero() || cached.Sub(wantEligibleAt).Abs() > time.Second {
		t.Errorf("expected cached eligible-at ~%v, got %v", wantEligibleAt, cached)
	}
}

func TestSettleArchiveDoneItems_MissingCompletionLabel_Skipped(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	eng.cfg.ArchiveAfter = 24 * time.Hour

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 3, ItemID: "PVTI_3", Repo: "owner/repo",
		Status: "Done", Labels: nil, // no stage:Done:complete
	}
	board.Items = []gh.ProjectItem{item}

	eng.settleArchiveDoneItems(board)

	if len(client.fetchLabelAppliedAtCalls) != 0 {
		t.Errorf("expected no FetchLabelAppliedAt call for item missing completion label, got %d", len(client.fetchLabelAppliedAtCalls))
	}
	if len(client.archiveProjectItemCalls) != 0 {
		t.Errorf("expected no archive call for item missing completion label, got %d", len(client.archiveProjectItemCalls))
	}
}

func TestSettleArchiveDoneItems_NonDoneColumn_Skipped(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	eng.cfg.ArchiveAfter = 24 * time.Hour

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 4, ItemID: "PVTI_4", Repo: "owner/repo",
		Status: "Implement", Labels: []string{"stage:Done:complete"}, // stray/impossible label, still must not archive
	}
	board.Items = []gh.ProjectItem{item}

	eng.settleArchiveDoneItems(board)

	if len(client.fetchLabelAppliedAtCalls) != 0 {
		t.Errorf("expected no FetchLabelAppliedAt call for item outside Done column, got %d", len(client.fetchLabelAppliedAtCalls))
	}
	if len(client.archiveProjectItemCalls) != 0 {
		t.Errorf("expected no archive call for item outside Done column, got %d", len(client.archiveProjectItemCalls))
	}
}

func TestSettleArchiveDoneItems_ArchiveDoneOff_DisablesScan(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	eng.cfg.ArchiveAfter = 24 * time.Hour
	eng.cfg.ArchiveDone = "off"
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return time.Now().Add(-48 * time.Hour), nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 5, ItemID: "PVTI_5", Repo: "owner/repo",
		Status: "Done", Labels: []string{"stage:Done:complete"},
	}
	board.Items = []gh.ProjectItem{item}

	eng.settleArchiveDoneItems(board)

	if len(client.fetchLabelAppliedAtCalls) != 0 {
		t.Errorf("expected no FetchLabelAppliedAt call when ArchiveDone=off, got %d", len(client.fetchLabelAppliedAtCalls))
	}
	if len(client.archiveProjectItemCalls) != 0 {
		t.Errorf("expected no archive call when ArchiveDone=off, got %d", len(client.archiveProjectItemCalls))
	}
}

func TestSettleArchiveDoneItems_SecondPass_DoesNotRefetchLabelTimestamp(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	eng.cfg.ArchiveAfter = 24 * time.Hour
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return time.Now().Add(-1 * time.Hour), nil // within grace period both passes
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 6, ItemID: "PVTI_6", Repo: "owner/repo",
		Status: "Done", Labels: []string{"stage:Done:complete"},
	}
	board.Items = []gh.ProjectItem{item}

	eng.settleArchiveDoneItems(board)
	eng.settleArchiveDoneItems(board)

	if len(client.fetchLabelAppliedAtCalls) != 1 {
		t.Errorf("expected FetchLabelAppliedAt to be called exactly once across two poll passes, got %d", len(client.fetchLabelAppliedAtCalls))
	}
}

func TestSettleArchiveDoneItems_RestartSafety_SingleCallArchivesImmediately(t *testing.T) {
	// Simulates a fresh engine (no CooldownAt cache) observing an item whose
	// completion label was already applied long enough ago — mirrors what
	// happens across an engine restart. Must archive off a single
	// FetchLabelAppliedAt call, not require an extra full grace-period wait.
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	eng.cfg.ArchiveAfter = 24 * time.Hour
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return time.Now().Add(-48 * time.Hour), nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 7, ItemID: "PVTI_7", Repo: "owner/repo",
		Status: "Done", Labels: []string{"stage:Done:complete"},
	}
	board.Items = []gh.ProjectItem{item}

	eng.settleArchiveDoneItems(board)

	if len(client.fetchLabelAppliedAtCalls) != 1 {
		t.Fatalf("expected exactly 1 FetchLabelAppliedAt call, got %d", len(client.fetchLabelAppliedAtCalls))
	}
	if len(client.archiveProjectItemCalls) != 1 {
		t.Fatalf("expected item to archive on the very next poll after restart, got %d archive calls", len(client.archiveProjectItemCalls))
	}
}

func TestSettleArchiveDoneItems_ZeroTimeAppliedAt_SkipsWithoutCaching(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	eng.cfg.ArchiveAfter = 24 * time.Hour
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return time.Time{}, nil // fail-open "not found yet" contract
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 8, ItemID: "PVTI_8", Repo: "owner/repo",
		Status: "Done", Labels: []string{"stage:Done:complete"},
	}
	board.Items = []gh.ProjectItem{item}

	eng.settleArchiveDoneItems(board)

	if len(client.archiveProjectItemCalls) != 0 {
		t.Errorf("expected no archive call when label-applied-at is unknown, got %d", len(client.archiveProjectItemCalls))
	}

	snap, err := eng.store.Get("owner/repo", 8)
	if err == nil {
		if cached := snap.CooldownAt(archiveEligibleAtCooldownReason); !cached.IsZero() {
			t.Errorf("expected no eligible-at cached when label-applied-at is unknown, got %v", cached)
		}
	}

	// Retries next poll: a second call should attempt FetchLabelAppliedAt again.
	eng.settleArchiveDoneItems(board)
	if len(client.fetchLabelAppliedAtCalls) != 2 {
		t.Errorf("expected FetchLabelAppliedAt to be retried on the next poll, got %d calls", len(client.fetchLabelAppliedAtCalls))
	}
}

func TestSettleArchiveDoneItems_SuccessfulArchive_WritesThroughCacheAndEcho(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	eng.cfg.ArchiveAfter = 0 // archive immediately once eligible
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return time.Now().Add(-1 * time.Second), nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 9, ItemID: "PVTI_9", Repo: "owner/repo",
		Status: "Done", Labels: []string{"stage:Done:complete"},
	}
	board.Items = []gh.ProjectItem{item}

	eng.settleArchiveDoneItems(board)

	if len(client.archiveProjectItemCalls) != 1 {
		t.Fatalf("expected 1 archive call, got %d", len(client.archiveProjectItemCalls))
	}
}
