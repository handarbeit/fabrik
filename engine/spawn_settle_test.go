package engine

import (
	"context"
	"fmt"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// specifyBacklogStatusField returns a StatusField with a "Backlog" and
// "Specify" option, matching the shape resolveSpecifyOptionID expects.
func specifyBacklogStatusField() *gh.StatusField {
	return &gh.StatusField{
		FieldID: "FIELD_1",
		Options: map[string]string{
			"Backlog": "OPT_Backlog",
			"Specify": "OPT_Specify",
			"Done":    "OPT_Done",
		},
		OrderedOptionNames: []string{"Backlog", "Specify", "Done"},
	}
}

// ---- recordChildPlacementFailure ----

func TestRecordChildPlacementFailure_AddsMarker(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)

	eng.recordChildPlacementFailure("owner", "child", 7)

	if len(client.addLabelCalls) != 1 {
		t.Fatalf("expected 1 AddLabelToIssue call, got %d", len(client.addLabelCalls))
	}
	c := client.addLabelCalls[0]
	if c.owner != "owner" || c.repo != "child" || c.issueNumber != 7 || c.labelName != childPlacementLabel {
		t.Errorf("unexpected call: %+v", c)
	}
}

// ---- spawnChildren integration: all three failure branches record the marker ----

func TestSpawnChildren_StatusUpdateCallFails_RecordsMarker(t *testing.T) {
	client := &mockGitHubClient{
		fetchStatusFieldFn: func(string) (*gh.StatusField, error) { return specifyBacklogStatusField(), nil },
		updateProjectItemStatusFn: func(_, _, _, _ string) error {
			return fmt.Errorf("rate limited")
		},
	}
	eng := spawnTestEngine(t, client)
	eng.statusField = specifyBacklogStatusField()

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child issue
Body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true — status-set failure is non-fatal to spawning")
	}

	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == childPlacementLabel {
			found = true
			if c.owner != "owner" || c.repo != "child" {
				t.Errorf("marker added to wrong repo: %+v", c)
			}
		}
	}
	if !found {
		t.Error("expected childPlacementLabel to be added on UpdateProjectItemStatus failure")
	}

	// fabrik:children-spawned must still be set on the parent — spawnChildren's own
	// guard is unconditional on the individual child's placement outcome.
	spawnedGuardSet := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:children-spawned" {
			spawnedGuardSet = true
		}
	}
	if !spawnedGuardSet {
		t.Error("expected fabrik:children-spawned to still be added despite the placement failure")
	}
}

func TestSpawnChildren_NilStatusField_RecordsMarker(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)
	eng.statusField = nil

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child issue
Body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == childPlacementLabel {
			found = true
		}
	}
	if !found {
		t.Error("expected childPlacementLabel to be added when statusField is nil")
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no UpdateProjectItemStatus call with nil statusField, got %d", len(client.updateStatusCalls))
	}
}

func TestSpawnChildren_NoSuitableOption_RecordsMarker(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)
	// A board with only "Backlog" as an option: resolveSpecifyOptionID's fallback
	// requires >= 2 names and skips both "Backlog" and the last column — with a
	// single-option board, no suitable option exists.
	eng.statusField = &gh.StatusField{
		FieldID:            "FIELD_1",
		Options:            map[string]string{"Backlog": "OPT_Backlog"},
		OrderedOptionNames: []string{"Backlog"},
	}

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child issue
Body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == childPlacementLabel {
			found = true
		}
	}
	if !found {
		t.Error("expected childPlacementLabel to be added when no suitable status option exists")
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no UpdateProjectItemStatus call when no option resolved, got %d", len(client.updateStatusCalls))
	}
}

// ---- settleChildPlacement ----

// TestSettleChildPlacement_SucceedsOnUnmatchedColumn is the integration test proving
// the settle path is entirely independent of stages.FindStage: item.Status is
// "Backlog", which resolves to no configured stage (verified below), yet
// settleChildPlacement still succeeds and clears the marker — because it is sourced
// from board.Items directly, not deepFetchCandidates/itemMayNeedWork.
func TestSettleChildPlacement_SucceedsOnUnmatchedColumn(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)
	eng.statusField = specifyBacklogStatusField()

	item := gh.ProjectItem{
		Number: 9, ItemID: "PVTI_9", Repo: "owner/child", Status: "Backlog",
		Labels: []string{childPlacementLabel},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	// Sanity: confirm this is exactly the scenario that would never reach
	// deepFetchCandidates via itemMayNeedWork (stage == nil short-circuit).
	if stages.FindStage(eng.cfg.Stages, item.Status) != nil {
		t.Fatal("test setup invalid: \"Backlog\" must not match any configured stage")
	}
	if eng.itemMayNeedWork(item) {
		t.Fatal("test setup invalid: itemMayNeedWork must return false for an unmatched column (that's the bug this settle scan works around)")
	}

	eng.settleChildPlacement(board, item)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 UpdateProjectItemStatus call, got %d", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Specify" {
		t.Errorf("expected Specify option, got %q", client.updateStatusCalls[0].optionID)
	}

	markerCleared := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == childPlacementLabel {
			markerCleared = true
		}
	}
	if !markerCleared {
		t.Error("expected childPlacementLabel to be removed after a successful placement")
	}
}

func TestSettleChildPlacement_CallFails_RecordsRetryNoMarkerClear(t *testing.T) {
	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(_, _, _, _ string) error {
			return fmt.Errorf("rate limited")
		},
	}
	eng := spawnTestEngine(t, client)
	eng.statusField = specifyBacklogStatusField()

	item := gh.ProjectItem{
		Number: 9, ItemID: "PVTI_9", Repo: "owner/child", Status: "Backlog",
		Labels: []string{childPlacementLabel},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	eng.settleChildPlacement(board, item)

	for _, c := range client.removeLabelCalls {
		if c.labelName == childPlacementLabel {
			t.Error("marker must not be removed when the settle attempt fails")
		}
	}
}

func TestSettleChildPlacement_NoSuitableOption_RecordsRetry(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)
	eng.statusField = nil // sf == nil -> resolveSpecifyOptionID returns ""

	item := gh.ProjectItem{
		Number: 9, ItemID: "PVTI_9", Repo: "owner/child", Status: "Backlog",
		Labels: []string{childPlacementLabel},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	eng.settleChildPlacement(board, item)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no UpdateProjectItemStatus call, got %d", len(client.updateStatusCalls))
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == childPlacementLabel {
			t.Error("marker must not be removed when no status option is resolvable")
		}
	}
}

// ---- recordChildPlacementRetry / escalateChildPlacementFailure ----

func TestRecordChildPlacementRetry_EscalatesAtMaxRetries(t *testing.T) {
	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(_, _, _, _ string) error {
			return fmt.Errorf("rate limited")
		},
	}
	eng := spawnTestEngine(t, client)
	eng.statusField = specifyBacklogStatusField()
	eng.cfg.MaxRetries = 2

	item := gh.ProjectItem{
		Number: 9, ItemID: "PVTI_9", Repo: "owner/child", Status: "Backlog",
		Labels: []string{childPlacementLabel},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	for i := 0; i < eng.cfg.MaxRetries; i++ {
		eng.settleChildPlacement(board, item)
	}

	pausedAdded := false
	markerRemoved := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == childPlacementLabel {
			markerRemoved = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused to be added after MaxRetries settle failures")
	}
	if !markerRemoved {
		t.Error("expected childPlacementLabel to be removed on escalation")
	}
	if len(client.addCommentCalls) == 0 {
		t.Error("expected an explanatory escalation comment to be posted on the child")
	}
}

func TestEscalateChildPlacementFailure_PostsOnChildOwnerRepo(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)
	eng.cfg.MaxRetries = 1

	item := gh.ProjectItem{
		Number: 9, ItemID: "PVTI_9", Repo: "owner/child", Status: "Backlog",
		Labels: []string{childPlacementLabel},
	}

	eng.escalateChildPlacementFailure(item)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 AddComment call, got %d", len(client.addCommentCalls))
	}
	c := client.addCommentCalls[0]
	if c.owner != "owner" || c.repo != "child" || c.issueNumber != 9 {
		t.Errorf("comment posted to wrong issue: %+v", c)
	}
}

// TestEscalateChildPlacementFailure_NotifiesParent verifies escalation also posts a
// best-effort comment on the parent issue, recovered via the childFooter regex.
func TestEscalateChildPlacementFailure_NotifiesParent(t *testing.T) {
	childBody := "Child spec body." + childFooter("owner", "repo", 42)
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.Body = childBody
			return nil
		},
	}
	eng := spawnTestEngine(t, client)
	eng.cfg.MaxRetries = 1

	item := gh.ProjectItem{
		Number: 9, ItemID: "PVTI_9", Repo: "owner/child", Status: "Backlog",
		Labels: []string{childPlacementLabel},
	}

	eng.escalateChildPlacementFailure(item)

	var parentComment *addCommentCall
	for i, c := range client.addCommentCalls {
		if c.owner == "owner" && c.repo == "repo" && c.issueNumber == 42 {
			parentComment = &client.addCommentCalls[i]
		}
	}
	if parentComment == nil {
		t.Fatal("expected a best-effort comment on the parent issue owner/repo#42")
	}
}

// TestEscalateChildPlacementFailure_NoFooter_ParentNotificationSkipped verifies that
// escalation still completes (child paused + commented) even when the parent link
// cannot be recovered — the requirement that parent notification never blocks the
// child's own escalation.
func TestEscalateChildPlacementFailure_NoFooter_ParentNotificationSkipped(t *testing.T) {
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.Body = "A body with no spawned-by footer at all."
			return nil
		},
	}
	eng := spawnTestEngine(t, client)
	eng.cfg.MaxRetries = 1

	item := gh.ProjectItem{
		Number: 9, ItemID: "PVTI_9", Repo: "owner/child", Status: "Backlog",
		Labels: []string{childPlacementLabel},
	}

	eng.escalateChildPlacementFailure(item)

	// Only the child's own escalation comment should exist — no parent comment.
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected exactly 1 comment (child only), got %d", len(client.addCommentCalls))
	}
	pausedAdded := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	if !pausedAdded {
		t.Error("expected the child's own escalation (fabrik:paused) to complete despite missing parent link")
	}
}

// TestEscalateChildPlacementFailure_DeepFetchFails_ParentNotificationSkipped verifies
// that a failing FetchItemDetails call during parent-link recovery is swallowed and
// does not affect the child's own escalation.
func TestEscalateChildPlacementFailure_DeepFetchFails_ParentNotificationSkipped(t *testing.T) {
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			return fmt.Errorf("network error")
		},
	}
	eng := spawnTestEngine(t, client)
	eng.cfg.MaxRetries = 1

	item := gh.ProjectItem{
		Number: 9, ItemID: "PVTI_9", Repo: "owner/child", Status: "Backlog",
		Labels: []string{childPlacementLabel},
	}

	eng.escalateChildPlacementFailure(item)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected exactly 1 comment (child only), got %d", len(client.addCommentCalls))
	}
}

// ---- clearChildPlacementMarker (closed-child short-circuit) ----

// TestClearChildPlacementMarker_NoPauseOrComment verifies that clearing the marker
// (the action the poll.go settle scan takes for a closed child carrying the marker)
// never pauses or comments — it is a pure marker removal, distinct from escalation.
func TestClearChildPlacementMarker_NoPauseOrComment(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)

	item := gh.ProjectItem{
		Number: 9, ItemID: "PVTI_9", Repo: "owner/child", Status: "Backlog", IsClosed: true,
		Labels: []string{childPlacementLabel},
	}

	eng.clearChildPlacementMarker(item, "owner", "child")

	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no comment for a plain marker clear, got %d", len(client.addCommentCalls))
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("expected no fabrik:paused for a plain marker clear")
		}
	}
	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == childPlacementLabel {
			markerRemoved = true
		}
	}
	if !markerRemoved {
		t.Error("expected childPlacementLabel to be removed")
	}
}

// ---- parseParentFromChildBody ----

func TestParseParentFromChildBody(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantOwner  string
		wantRepo   string
		wantNumber int
		wantOK     bool
	}{
		{
			name:       "real childFooter output",
			body:       "Some spec body.\n" + childFooter("acme", "widgets", 123),
			wantOwner:  "acme",
			wantRepo:   "widgets",
			wantNumber: 123,
			wantOK:     true,
		},
		{
			name:   "no footer at all",
			body:   "A plain body with no back-reference.",
			wantOK: false,
		},
		{
			name:   "empty body",
			body:   "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, number, ok := parseParentFromChildBody(tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if owner != tc.wantOwner || repo != tc.wantRepo || number != tc.wantNumber {
				t.Errorf("got (%q, %q, %d), want (%q, %q, %d)", owner, repo, number, tc.wantOwner, tc.wantRepo, tc.wantNumber)
			}
		})
	}
}
