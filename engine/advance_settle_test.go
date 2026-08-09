package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// missingDoneOptionErr mimics advanceToNextStage's own "no status option"
// error text — the case #1422 reports.
func missingDoneOptionErr() error {
	return fmt.Errorf("no status option %q found on project board (available: %v)", "Done", []string{"Validate"})
}

// TestRecordAdvanceOutcome_FirstFailure_LabelsAndComments covers Acceptance #1:
// a failed terminal advance leaves the item carrying the durable label and posts
// exactly one explanatory comment naming the missing option and the failing stage.
func TestRecordAdvanceOutcome_FirstFailure_LabelsAndComments(t *testing.T) {
	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(projectID, itemID, statusFieldID, statusOptionID string) error {
			return missingDoneOptionErr()
		},
	}
	stgs := terminalAdvanceStages()
	eng := testEngineWithStages(t, client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 93, ItemID: "PVTI_93", Repo: "owner/repo", Status: "Validate"}
	stage := stages.FindStage(stgs, "Validate")

	if err := eng.recordAdvanceOutcome(board, item, stage); err == nil {
		t.Fatal("expected recordAdvanceOutcome to return the underlying error")
	}

	added := addedLabelNames(client.addLabelCalls)
	if !containsLabel(added, awaitingAdvanceLabel) {
		t.Errorf("expected %s label added, got %v", awaitingAdvanceLabel, added)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected exactly one comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "Validate") {
		t.Errorf("expected comment to name the stalled stage, got %q", body)
	}
	if !strings.Contains(body, "Done") {
		t.Errorf("expected comment to name the missing status option, got %q", body)
	}
	if len(client.addCommentReactionCalls) != 0 {
		t.Errorf("expected the first-failure comment not to be rocket-reacted, got %d reactions", len(client.addCommentReactionCalls))
	}
}

// TestMarkAdvanceFailureOutstanding_RepeatFailure_NoSecondComment covers R5 /
// Acceptance #5 at the unit level: a second consecutive failure while the
// marker is already present must not post a second comment, but must still
// count toward the retry budget.
func TestMarkAdvanceFailureOutstanding_RepeatFailure_NoSecondComment(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := terminalAdvanceStages()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRetries = 10

	item := gh.ProjectItem{Number: 94, ItemID: "PVTI_94", Repo: "owner/repo", Status: "Validate", Labels: []string{awaitingAdvanceLabel}}
	stage := stages.FindStage(stgs, "Validate")
	aerr := missingDoneOptionErr()

	eng.markAdvanceFailureOutstanding(item, stage, aerr)
	eng.markAdvanceFailureOutstanding(item, stage, aerr)

	client.mu.Lock()
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no comment when the marker is already present, got %d", len(client.addCommentCalls))
	}
	client.mu.Unlock()

	snap, serr := eng.store.Get("owner/repo", 94)
	if serr != nil {
		t.Fatalf("store.Get: %v", serr)
	}
	if got := snap.Attempts(advanceAwaitingRetryStage); got != 2 {
		t.Errorf("expected retry count 2 after two failing passes, got %d", got)
	}
}

// TestRecordAdvanceOutcome_SuccessClearsMarker covers R2/Acceptance #2's
// "recovers without a restart" half at the unit level: once the underlying
// advance succeeds, the marker and retry counter are cleared.
func TestRecordAdvanceOutcome_SuccessClearsMarker(t *testing.T) {
	fail := true
	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(projectID, itemID, statusFieldID, statusOptionID string) error {
			if fail {
				return missingDoneOptionErr()
			}
			return nil
		},
	}
	stgs := terminalAdvanceStages()
	eng := testEngineWithStages(t, client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 95, ItemID: "PVTI_95", Repo: "owner/repo", Status: "Validate"}
	stage := stages.FindStage(stgs, "Validate")

	if err := eng.recordAdvanceOutcome(board, item, stage); err == nil {
		t.Fatal("expected first attempt to fail")
	}

	// Simulate the marker having landed on GitHub for the retry pass.
	item.Labels = []string{awaitingAdvanceLabel}
	fail = false

	if err := eng.recordAdvanceOutcome(board, item, stage); err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}

	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == awaitingAdvanceLabel {
			markerRemoved = true
		}
	}
	if !markerRemoved {
		t.Error("expected awaiting-advance marker removed after a successful advance")
	}
}

// TestRecordAdvanceOutcome_EscalatesAtMaxRetries covers R3/Acceptance #6:
// after MaxRetries failed passes, the item is paused, the settle label
// removed, and an escalation comment posted.
func TestRecordAdvanceOutcome_EscalatesAtMaxRetries(t *testing.T) {
	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(projectID, itemID, statusFieldID, statusOptionID string) error {
			return missingDoneOptionErr()
		},
	}
	stgs := terminalAdvanceStages()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRetries = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 96, ItemID: "PVTI_96", Repo: "owner/repo", Status: "Validate"}
	stage := stages.FindStage(stgs, "Validate")

	for i := 0; i < eng.cfg.MaxRetries; i++ {
		_ = eng.recordAdvanceOutcome(board, item, stage)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	pausedAdded := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused to be added after MaxRetries settle failures")
	}

	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == awaitingAdvanceLabel {
			markerRemoved = true
		}
	}
	if !markerRemoved {
		t.Errorf("expected %s to be removed on escalation", awaitingAdvanceLabel)
	}

	found := false
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 96 && strings.Contains(c.body, fmt.Sprintf("%d attempt", eng.cfg.MaxRetries)) {
			found = true
		}
	}
	if !found {
		t.Error("expected an explanatory escalation comment naming the attempt count")
	}
}

// TestSettleAwaitingAdvanceScan_SkipsWithoutMarkerOrPaused verifies the scan
// only acts on items carrying the durable marker, and leaves a paused item
// alone (mirrors settleNonDefaultBaseCloses's own guards).
func TestSettleAwaitingAdvanceScan_SkipsWithoutMarkerOrPaused(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := terminalAdvanceStages()
	eng := testEngineWithStages(t, client, stgs)

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 97, Repo: "owner/repo", Status: "Validate", Labels: []string{"stage:Validate:complete"}},
			{Number: 98, Repo: "owner/repo", Status: "Validate", Labels: []string{awaitingAdvanceLabel, "fabrik:paused"}},
		},
	}

	eng.settleAwaitingAdvanceScan(board, make(map[string]bool))

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no advance attempt for an unmarked or paused item, got %d", len(client.updateStatusCalls))
	}
}

// TestSettleAwaitingAdvanceScan_NotAdmittedByItemMayNeedWork covers R2/
// Acceptance #4: the scan must fire for an item that itemMayNeedWork /
// selectDeepFetchCandidates would not admit — sourced from board.Items, not
// dispatch admission. A closed item at a gate-checked stage missing its own
// stage:<Name>:complete label is the concrete case itemMayNeedWork rejects.
func TestSettleAwaitingAdvanceScan_NotAdmittedByItemMayNeedWork(t *testing.T) {
	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(projectID, itemID, statusFieldID, statusOptionID string) error { return nil },
	}
	stgs := terminalAdvanceStages()
	eng := testEngineWithStages(t, client, stgs)

	item := gh.ProjectItem{
		Number:   99,
		ItemID:   "PVTI_99",
		Repo:     "owner/repo",
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{awaitingAdvanceLabel}, // deliberately no stage:Validate:complete
	}

	if eng.itemMayNeedWork(item) {
		t.Fatal("test setup invalid: expected itemMayNeedWork to reject this item")
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{item}}
	eng.settleAwaitingAdvanceScan(board, make(map[string]bool))

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected the settle scan to retry the advance despite itemMayNeedWork rejecting the item, got %d status calls", len(client.updateStatusCalls))
	}
}

// TestSettleAwaitingAdvanceScan_RepeatedFailures_ExactlyOneCommentAcrossPolls
// covers Acceptance #5 at the settle-scan level: the initial failure (which
// applies the marker) plus several subsequent settle passes in the same
// failing state must produce exactly one comment in total, not one per poll.
func TestSettleAwaitingAdvanceScan_RepeatedFailures_ExactlyOneCommentAcrossPolls(t *testing.T) {
	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(projectID, itemID, statusFieldID, statusOptionID string) error {
			return missingDoneOptionErr()
		},
	}
	stgs := terminalAdvanceStages()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRetries = 0 // unlimited retries — isolate the comment-count assertion from escalation

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 100, ItemID: "PVTI_100", Repo: "owner/repo", Status: "Validate"}
	stage := stages.FindStage(stgs, "Validate")

	// The initial failure, as it would happen at either real call site
	// (advanceValidateTerminalItem / advanceConvergedPRToDone) via recordAdvanceOutcome.
	if err := eng.recordAdvanceOutcome(board, item, stage); err == nil {
		t.Fatal("expected the initial advance to fail")
	}

	// The marker has now landed on GitHub; subsequent poll passes see it.
	item.Labels = []string{awaitingAdvanceLabel}
	board.Items = []gh.ProjectItem{item}

	for i := 0; i < 3; i++ {
		eng.settleAwaitingAdvanceScan(board, make(map[string]bool))
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) != 1 {
		t.Errorf("expected exactly one comment across the initial failure and 3 settle passes, got %d", len(client.addCommentCalls))
	}
}

// TestAwaitingAdvance_EndToEnd_RecoversAndClearsDownstreamBlocked is the full
// scenario from #1422's Problem section: a terminal advance stranded by a
// missing board Status option (1) escalates visibly on first failure, (2)
// self-heals via the settle scan once the option is added, with no engine
// restart, and (3) the specific reported harm — a downstream dependent's
// fabrik:blocked staying stuck on a blocker that has already shipped — is
// what actually clears, not merely "the advance succeeded" (Acceptance #1, #2).
func TestAwaitingAdvance_EndToEnd_RecoversAndClearsDownstreamBlocked(t *testing.T) {
	optionMissing := true
	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(projectID, itemID, statusFieldID, statusOptionID string) error {
			if optionMissing {
				return missingDoneOptionErr()
			}
			return nil
		},
	}
	stgs := terminalAdvanceStages()
	eng := testEngineWithStages(t, client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	blocker := gh.ProjectItem{Number: 50, ItemID: "PVTI_50", Repo: "owner/repo", Status: "Validate"}
	stage := stages.FindStage(stgs, "Validate")

	// Poll 1: terminal advance fails — the item is stranded, gets the durable
	// label and a one-time explanatory comment (Acceptance #1).
	if err := eng.recordAdvanceOutcome(board, blocker, stage); err == nil {
		t.Fatal("expected the initial advance to fail (missing Done status option)")
	}
	client.mu.Lock()
	commentsAfterFirstFailure := len(client.addCommentCalls)
	client.mu.Unlock()
	if commentsAfterFirstFailure != 1 {
		t.Fatalf("expected exactly one comment after the first failure, got %d", commentsAfterFirstFailure)
	}

	// The marker has landed on GitHub; reflect it in the next poll's board snapshot.
	blocker.Labels = []string{awaitingAdvanceLabel}
	board.Items = []gh.ProjectItem{blocker}

	// Poll 2: board is still misconfigured — the settle scan retries and fails
	// again, without posting a second comment (R5).
	eng.settleAwaitingAdvanceScan(board, make(map[string]bool))
	client.mu.Lock()
	commentsAfterSecondFailure := len(client.addCommentCalls)
	client.mu.Unlock()
	if commentsAfterSecondFailure != 1 {
		t.Fatalf("expected still exactly one comment after a second failing poll, got %d", commentsAfterSecondFailure)
	}

	// Operator fixes the board: the missing Status option now exists.
	optionMissing = false

	// Poll 3: settle scan retries and succeeds — no engine restart, no manual
	// re-dispatch (Acceptance #2).
	eng.settleAwaitingAdvanceScan(board, make(map[string]bool))
	client.mu.Lock()
	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == awaitingAdvanceLabel {
			markerRemoved = true
		}
	}
	client.mu.Unlock()
	if !markerRemoved {
		t.Fatal("expected the awaiting-advance marker cleared once the missing option exists")
	}

	// The blocker issue is now closed — via GitHub's own Closes #N auto-close
	// (default base branch) or Fabrik's own closeIssueIfNonDefaultBase, neither
	// of which settleAwaitingAdvanceScan itself performs (see its doc comment
	// on why it does not duplicate closeIssueIfNonDefaultBase). Model that here
	// as a fresh Reconcile would have recorded it in the Store, exactly as
	// TestCheckDependencies_StorePreemptsStaleDepState does.
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 50, Repo: "owner/repo"}})
	eng.store.Apply(itemstate.IssueClosed{Repo: "owner/repo", Number: 50})

	// The downstream dependent still carries fabrik:blocked and a stale
	// (pre-indexer-lag) OPEN view of the blocker via BlockedBy — exactly the
	// "quietly working" harm #1422 reports: a human can see the blocker is
	// finished, but the dependent doesn't know yet.
	dependent := gh.ProjectItem{
		Number: 51,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:blocked"},
		BlockedBy: []gh.Dependency{
			{Number: 50, State: "OPEN", Repo: "owner/repo"},
		},
	}
	depStage := stages.FindStage(stgs, "Implement")

	blocked := eng.checkDependencies(board, dependent, depStage)

	if blocked {
		t.Error("expected the dependent to no longer be blocked once the blocker is closed")
	}
	depUnblocked := false
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 51 && c.labelName == "fabrik:blocked" {
			depUnblocked = true
		}
	}
	if !depUnblocked {
		t.Error("expected fabrik:blocked removed from the dependent — this is the specific cross-issue harm #1422 reports, not just the advance succeeding")
	}
}

// TestValidatePRTerminalAdvance_AdvanceFails_AppliesAwaitingAdvanceLabel is a
// wiring-level check (Acceptance #3): it exercises the real call site,
// runValidatePRTerminalAdvance's merged-PR path, rather than calling
// recordAdvanceOutcome directly, so a regression that reverts the
// pr_terminal_advance.go wiring back to a bare advanceToNextStage call is
// actually caught by the suite. Confirmed to fail red against such a
// revert before this test was added.
func TestValidatePRTerminalAdvance_AdvanceFails_AppliesAwaitingAdvanceLabel(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, Merged: true, State: "closed"}, nil
		},
		updateProjectItemStatusFn: func(projectID, itemID, statusFieldID, statusOptionID string) error {
			return missingDoneOptionErr()
		},
	}
	stgs := terminalAdvanceStages()
	eng := testEngineWithStages(t, client, stgs)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	item := gh.ProjectItem{
		Number: 101, ItemID: "PVTI_101", Repo: "owner/repo", Status: "Validate",
		Labels: []string{"stage:Implement:complete"},
	}
	advancedItems := make(map[string]bool)
	eng.runValidatePRTerminalAdvance(board, []gh.ProjectItem{item}, advancedItems)

	client.mu.Lock()
	defer client.mu.Unlock()
	if !containsLabel(addedLabelNames(client.addLabelCalls), awaitingAdvanceLabel) {
		t.Errorf("expected %s applied via the real runValidatePRTerminalAdvance call site, got labels %v", awaitingAdvanceLabel, addedLabelNames(client.addLabelCalls))
	}
	if len(client.addCommentCalls) != 1 {
		t.Errorf("expected exactly one explanatory comment, got %d", len(client.addCommentCalls))
	}
}

// TestCheckAutoMergeConvergence_AdvanceFails_AppliesAwaitingAdvanceLabel is
// the merge_gate.go sibling of the wiring check above: exercises
// checkAutoMergeConvergence's real merged-PR path into
// advanceConvergedPRToDone, rather than calling recordAdvanceOutcome
// directly, so a regression that reverts the merge_gate.go wiring is caught.
func TestCheckAutoMergeConvergence_AdvanceFails_AppliesAwaitingAdvanceLabel(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "closed", Merged: true, AutoMergeEnabled: true}, nil
		},
		updateProjectItemStatusFn: func(projectID, itemID, statusFieldID, statusOptionID string) error {
			return missingDoneOptionErr()
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 102, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{Status: PRMergeTerminal, PR: &gh.PRDetails{Number: 10}}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, settle, false)

	client.mu.Lock()
	defer client.mu.Unlock()
	if !containsLabel(addedLabelNames(client.addLabelCalls), awaitingAdvanceLabel) {
		t.Errorf("expected %s applied via the real checkAutoMergeConvergence call site, got labels %v", awaitingAdvanceLabel, addedLabelNames(client.addLabelCalls))
	}
	if len(client.addCommentCalls) != 1 {
		t.Errorf("expected exactly one explanatory comment, got %d", len(client.addCommentCalls))
	}
}

// TestRunValidatePRTerminalAdvance_SettlesIntoStablePause is the regression
// test for Pruefer's PR #1469 review (both findings): it drives
// runValidatePRTerminalAdvance — the real, unconditionally-re-entered
// self-heal call site — across four simulated polls with a permanently
// broken board (updateProjectItemStatusFn always fails) and MaxRetries=2,
// updating item.Labels between polls exactly as GitHub would after each
// poll's mutations (mirroring TestAwaitingAdvance_EndToEnd's idiom).
//
// Confirmed to fail red against a revert of awaitingAdvanceStuckOrReset's
// guard in pr_terminal_advance.go: poll 3 would show fabrik:paused stripped
// and re-added (plus a spurious extra comment pair) instead of the item
// resting untouched, and poll 4 would show fabrik:paused re-applied after a
// single failure instead of getting a fresh MaxRetries budget.
func TestRunValidatePRTerminalAdvance_SettlesIntoStablePause(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, Merged: true, State: "closed"}, nil
		},
		updateProjectItemStatusFn: func(projectID, itemID, statusFieldID, statusOptionID string) error {
			return missingDoneOptionErr()
		},
	}
	stgs := terminalAdvanceStages()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRetries = 2

	item := gh.ProjectItem{
		Number: 103, ItemID: "PVTI_103", Repo: "owner/repo", Status: "Validate",
		Labels: []string{"stage:Implement:complete"},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	resetClient := func() {
		client.mu.Lock()
		client.addLabelCalls = nil
		client.removeLabelCalls = nil
		client.addCommentCalls = nil
		client.mu.Unlock()
	}
	poll := func() {
		eng.runValidatePRTerminalAdvance(board, []gh.ProjectItem{item}, make(map[string]bool))
	}

	// Poll 1 (first failure): marker applied, one explanatory comment, not yet
	// escalated (1 < MaxRetries=2).
	poll()
	client.mu.Lock()
	if !containsLabel(addedLabelNames(client.addLabelCalls), awaitingAdvanceLabel) {
		t.Fatalf("poll 1: expected %s added, got %v", awaitingAdvanceLabel, addedLabelNames(client.addLabelCalls))
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("poll 1: expected exactly one comment, got %d", len(client.addCommentCalls))
	}
	if containsLabel(addedLabelNames(client.addLabelCalls), "fabrik:paused") {
		t.Fatal("poll 1: did not expect fabrik:paused yet (only 1 of 2 retries used)")
	}
	client.mu.Unlock()
	item.Labels = append(item.Labels, awaitingAdvanceLabel)
	resetClient()

	// Poll 2 (second failure, hits MaxRetries=2): escalates — fabrik:paused
	// added, awaitingAdvanceLabel removed, exactly one (escalation) comment,
	// no duplicate first-failure comment.
	poll()
	client.mu.Lock()
	if !containsLabel(addedLabelNames(client.addLabelCalls), "fabrik:paused") {
		t.Fatalf("poll 2: expected fabrik:paused added on escalation, got %v", addedLabelNames(client.addLabelCalls))
	}
	if !containsLabel(removedLabelNames(client.removeLabelCalls), awaitingAdvanceLabel) {
		t.Fatalf("poll 2: expected %s removed on escalation, got %v", awaitingAdvanceLabel, removedLabelNames(client.removeLabelCalls))
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("poll 2: expected exactly one (escalation) comment, got %d", len(client.addCommentCalls))
	}
	client.mu.Unlock()
	item.Labels = []string{"stage:Implement:complete", "fabrik:paused"} // awaitingAdvanceLabel removed, fabrik:paused added
	resetClient()

	// Poll 3 (still stuck, unresolved by an operator): this is Pruefer's
	// Finding 1. Before the fix, this poll would strip fabrik:paused,
	// retry, fail, and immediately re-escalate — producing a fresh pair of
	// label/comment mutations. With the fix, the item is left completely
	// untouched: no label calls, no comments, fabrik:paused stays put.
	poll()
	client.mu.Lock()
	if len(client.addLabelCalls) != 0 || len(client.removeLabelCalls) != 0 {
		t.Errorf("poll 3: expected zero label mutations while still stuck-paused, got added=%v removed=%v",
			addedLabelNames(client.addLabelCalls), removedLabelNames(client.removeLabelCalls))
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("poll 3: expected zero comments while still stuck-paused, got %d", len(client.addCommentCalls))
	}
	client.mu.Unlock()
	resetClient()
	// item.Labels intentionally unchanged — still ["stage:Implement:complete", "fabrik:paused"].

	// Poll 4 (operator manually removes fabrik:paused, board still not
	// fixed): this is Pruefer's Finding 2. The item must get a fresh
	// MaxRetries budget — a single subsequent failure must NOT immediately
	// re-escalate.
	item.Labels = []string{"stage:Implement:complete"}
	poll()
	client.mu.Lock()
	defer client.mu.Unlock()
	if !containsLabel(addedLabelNames(client.addLabelCalls), awaitingAdvanceLabel) {
		t.Errorf("poll 4: expected %s re-applied on the fresh failure, got %v", awaitingAdvanceLabel, addedLabelNames(client.addLabelCalls))
	}
	if containsLabel(addedLabelNames(client.addLabelCalls), "fabrik:paused") {
		t.Error("poll 4: did not expect an immediate re-escalation (fabrik:paused) — the retry counter should have been reset to a fresh budget")
	}
	if len(client.addCommentCalls) != 1 {
		t.Errorf("poll 4: expected exactly one (fresh first-failure) comment, got %d", len(client.addCommentCalls))
	}
}
