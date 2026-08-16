package engine

import (
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestCreditedPRFromLabel covers the fabrik:credited-pr:<N> parsing helper:
// the happy path, an absent label, and malformed suffixes that must be
// ignored rather than misparsed.
func TestCreditedPRFromLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		wantN  int
		wantOK bool
	}{
		{"present", []string{"stage:Validate:complete", creditedPRLabelPrefix + "42"}, 42, true},
		{"absent", []string{"stage:Validate:complete"}, 0, false},
		{"non-numeric-suffix", []string{creditedPRLabelPrefix + "abc"}, 0, false},
		{"zero-suffix", []string{creditedPRLabelPrefix + "0"}, 0, false},
		{"negative-suffix", []string{creditedPRLabelPrefix + "-1"}, 0, false},
		{"first-valid-wins", []string{creditedPRLabelPrefix + "abc", creditedPRLabelPrefix + "7"}, 7, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, ok := creditedPRFromLabel(gh.ProjectItem{Labels: c.labels})
			if n != c.wantN || ok != c.wantOK {
				t.Errorf("creditedPRFromLabel(%v) = (%d, %v), want (%d, %v)", c.labels, n, ok, c.wantN, c.wantOK)
			}
		})
	}
}

// TestSettleLandingVerification_AC1_MergeTrainCreditNotMerged_ConfirmedFailure
// simulates #1614's shape: Done + closed issue + fabrik:awaiting-landing-verification
// + fabrik:credited-pr:<N> where PR N is not merged. The item must be
// reopened, moved off Done (back to Validate), labeled with the distinguishing
// failure label, have both feature-owned labels removed, and receive an
// explanatory comment — all within one scan pass (AC1: "within one poll
// cycle", never gated by the retry counter).
func TestSettleLandingVerification_AC1_MergeTrainCreditNotMerged_ConfirmedFailure(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) {
			if prNumber != 99 {
				t.Fatalf("unexpected FetchPRMerged call for PR #%d, want #99", prNumber)
			}
			return false, nil
		},
	}
	stgs := terminalAdvanceStages()
	eng := testEngineWithStages(t, client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1614, ItemID: "PVTI_1614", Repo: "owner/repo", Status: "Done", IsClosed: true,
		Labels: []string{awaitingLandingVerificationLabel, creditedPRLabelPrefix + "99"},
	}

	eng.settleLandingVerificationItem(board, item)

	client.mu.Lock()
	defer client.mu.Unlock()

	if len(client.reopenIssueCalls) != 1 || client.reopenIssueCalls[0].issueNumber != 1614 {
		t.Errorf("expected exactly one ReopenIssue(1614) call, got %v", client.reopenIssueCalls)
	}

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected exactly one board move, got %v", client.updateStatusCalls)
	}
	if client.updateStatusCalls[0].optionID != "OPT_Validate" {
		t.Errorf("expected move to Validate's option ID, got %q", client.updateStatusCalls[0].optionID)
	}

	added := addedLabelNames(client.addLabelCalls)
	if !containsLabel(added, landingVerificationFailedLabel) {
		t.Errorf("expected %s added, got %v", landingVerificationFailedLabel, added)
	}

	removed := removedLabelNames(client.removeLabelCalls)
	if !containsLabel(removed, awaitingLandingVerificationLabel) {
		t.Errorf("expected %s removed, got %v", awaitingLandingVerificationLabel, removed)
	}
	if !containsLabel(removed, creditedPRLabelPrefix+"99") {
		t.Errorf("expected %s removed, got %v", creditedPRLabelPrefix+"99", removed)
	}

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected exactly one comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "#99") {
		t.Errorf("expected comment to name the credited PR #99, got %q", body)
	}
	if !strings.Contains(body, landingVerificationFailedLabel) {
		t.Errorf("expected comment to name the distinguishing label, got %q", body)
	}
}

// TestSettleLandingVerification_AC2_MergeTrainCreditMerged_Untouched covers the
// merge-train happy path: the credited integration/singleton PR did merge, so
// the item is left alone — only the feature's own markers are cleared.
func TestSettleLandingVerification_AC2_MergeTrainCreditMerged_Untouched(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) {
			if prNumber != 7 {
				t.Fatalf("unexpected FetchPRMerged call for PR #%d, want #7", prNumber)
			}
			return true, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 5, ItemID: "PVTI_5", Repo: "owner/repo", Status: "Done", IsClosed: true,
		Labels: []string{awaitingLandingVerificationLabel, creditedPRLabelPrefix + "7"},
	}

	eng.settleLandingVerificationItem(board, item)

	client.mu.Lock()
	defer client.mu.Unlock()

	if len(client.reopenIssueCalls) != 0 {
		t.Errorf("expected no ReopenIssue call for a landed item, got %v", client.reopenIssueCalls)
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no board move for a landed item, got %v", client.updateStatusCalls)
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no comment for a landed item, got %v", client.addCommentCalls)
	}
	added := addedLabelNames(client.addLabelCalls)
	if containsLabel(added, landingVerificationFailedLabel) {
		t.Errorf("did not expect %s added, got %v", landingVerificationFailedLabel, added)
	}
	removed := removedLabelNames(client.removeLabelCalls)
	if !containsLabel(removed, awaitingLandingVerificationLabel) {
		t.Errorf("expected %s cleared, got %v", awaitingLandingVerificationLabel, removed)
	}
	if !containsLabel(removed, creditedPRLabelPrefix+"7") {
		t.Errorf("expected %s cleared, got %v", creditedPRLabelPrefix+"7", removed)
	}
}

// TestSettleLandingVerification_AC2_OrdinaryPath_BranchDeletedAfterMerge_Untouched
// covers R3: a branch legitimately deleted after a real merge is not a
// failure. There is no fabrik:credited-pr label (the ordinary auto-merge
// path never applies one), so the scan falls back to FetchLinkedPR — which
// reports the item's own PR, merged. The mechanism never inspects the
// item's own branch at all, so a deleted branch cannot even surface here.
func TestSettleLandingVerification_AC2_OrdinaryPath_BranchDeletedAfterMerge_Untouched(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 321, Merged: true, State: "closed"}, nil
		},
		fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) {
			if prNumber != 321 {
				t.Fatalf("unexpected FetchPRMerged call for PR #%d, want #321", prNumber)
			}
			return true, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 3, ItemID: "PVTI_3", Repo: "owner/repo", Status: "Done", IsClosed: true,
		Labels: []string{awaitingLandingVerificationLabel},
	}

	eng.settleLandingVerificationItem(board, item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.reopenIssueCalls) != 0 || len(client.updateStatusCalls) != 0 || len(client.addCommentCalls) != 0 {
		t.Errorf("expected a genuinely landed item left untouched, got reopen=%v move=%v comment=%v",
			client.reopenIssueCalls, client.updateStatusCalls, client.addCommentCalls)
	}
	removed := removedLabelNames(client.removeLabelCalls)
	if !containsLabel(removed, awaitingLandingVerificationLabel) {
		t.Errorf("expected %s cleared, got %v", awaitingLandingVerificationLabel, removed)
	}
}

// TestSettleLandingVerification_AC2_RebaseAfterLandingFixture_Untouched models
// the confirmed false-positive fixtures from the issue thread (#1573, #1523,
// #1520): a branch rebased onto a later base after landing, which a naive
// branch-tip-ancestry check would wrongly flag. Mechanism 1 (crediting-PR
// merge state) never inspects the branch at all, so this is
// indistinguishable from an ordinary landed item — the credited PR (here
// standing in for #1553, the batch PR that actually carried #1523) reports
// merged, and the item is left untouched.
func TestSettleLandingVerification_AC2_RebaseAfterLandingFixture_Untouched(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) {
			if prNumber != 1553 {
				t.Fatalf("unexpected FetchPRMerged call for PR #%d, want #1553", prNumber)
			}
			return true, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	// Models #1523: a merge-train member whose own branch drifted (rebased
	// onto a later base) after landing via batch PR #1553.
	item := gh.ProjectItem{
		Number: 1523, ItemID: "PVTI_1523", Repo: "owner/repo", Status: "Done", IsClosed: true,
		Labels: []string{awaitingLandingVerificationLabel, creditedPRLabelPrefix + "1553"},
	}

	eng.settleLandingVerificationItem(board, item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.reopenIssueCalls) != 0 {
		t.Errorf("rebase-after-landing fixture must not be reopened (this is exactly the false positive AC2 forbids), got %v", client.reopenIssueCalls)
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no failure comment for a rebase-after-landing fixture, got %v", client.addCommentCalls)
	}
}

// TestSettleLandingVerification_AC3_BaseLabelHonoredInFailureComment covers
// R4: an item carrying base:<branch> must have its failure comment name that
// branch, not the repository default — the mechanism's merge/no-merge
// decision is base-independent (it never touches the branch), but the
// comment naming it wrong would still mislead an operator.
func TestSettleLandingVerification_AC3_BaseLabelHonoredInFailureComment(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) { return false, nil },
	}
	eng := setupCloseTestRepo(t, client) // real git repo; "develop" exists on origin

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 42, ItemID: "PVTI_42", Repo: "owner/repo", Status: "Done", IsClosed: true,
		Labels: []string{awaitingLandingVerificationLabel, creditedPRLabelPrefix + "10", "base:develop"},
	}

	eng.settleLandingVerificationItem(board, item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected exactly one comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "develop") {
		t.Errorf("expected comment to name the base:<branch> target %q, got %q", "develop", body)
	}
	if strings.Contains(body, "base branch `main`") {
		t.Errorf("expected comment NOT to name the repository default instead of the base: label, got %q", body)
	}
}

// TestSettleLandingVerification_AC4_TransientFetchPRMergedError_NeverReopens
// covers R5/AC4: a transient failure of the verification query must not
// reopen the issue. It retries and, at MaxRetries, escalates to
// fabrik:paused — never via a reopen.
func TestSettleLandingVerification_AC4_TransientFetchPRMergedError_NeverReopens(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) {
			return false, fmt.Errorf("GitHub API returned 503: service unavailable")
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 2

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 8, ItemID: "PVTI_8", Repo: "owner/repo", Status: "Done", IsClosed: true,
		Labels: []string{awaitingLandingVerificationLabel, creditedPRLabelPrefix + "1"},
	}

	for i := 0; i < eng.cfg.MaxRetries; i++ {
		eng.settleLandingVerificationItem(board, item)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.reopenIssueCalls) != 0 {
		t.Errorf("a transient verification failure must never reopen the issue, got %v", client.reopenIssueCalls)
	}
	added := addedLabelNames(client.addLabelCalls)
	if !containsLabel(added, "fabrik:paused") {
		t.Errorf("expected escalation to fabrik:paused after %d failed passes, got %v", eng.cfg.MaxRetries, added)
	}
	if containsLabel(added, landingVerificationFailedLabel) {
		t.Errorf("an inconclusive result must never apply the confirmed-failure label, got %v", added)
	}
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected an escalation comment")
	}
	lastComment := client.addCommentCalls[len(client.addCommentCalls)-1].body
	if !strings.Contains(lastComment, "inconclusive") {
		t.Errorf("expected the escalation comment to be worded as inconclusive (not a confirmed failure), got %q", lastComment)
	}
}

// TestSettleLandingVerification_MissingCreditingReference_Inconclusive covers
// the case where neither a fabrik:credited-pr:<N> label nor a linked PR can
// be found: absence of evidence is not evidence of a false COMPLETED, so
// this goes through the same retry-then-escalate path as a transient error —
// never an immediate reopen.
func TestSettleLandingVerification_MissingCreditingReference_Inconclusive(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil // no linked PR found
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 1

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 9, ItemID: "PVTI_9", Repo: "owner/repo", Status: "Done", IsClosed: true,
		Labels: []string{awaitingLandingVerificationLabel},
	}

	eng.settleLandingVerificationItem(board, item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.reopenIssueCalls) != 0 {
		t.Errorf("a missing crediting reference must never reopen the issue, got %v", client.reopenIssueCalls)
	}
	added := addedLabelNames(client.addLabelCalls)
	if !containsLabel(added, "fabrik:paused") {
		t.Errorf("expected escalation to fabrik:paused after MaxRetries=1, got %v", added)
	}
}

// TestSettleLandingVerification_SkipsPausedItems mirrors every other settle
// scan's own paused-item guard: an operator investigating a paused item must
// not be fought by this scan.
func TestSettleLandingVerification_SkipsPausedItems(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) {
			t.Fatal("FetchPRMerged should not be called for a paused item")
			return false, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 1, Repo: "owner/repo",
				Labels: []string{awaitingLandingVerificationLabel, creditedPRLabelPrefix + "1", "fabrik:paused"},
			},
		},
	}

	eng.settleLandingVerification(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.reopenIssueCalls) != 0 {
		t.Errorf("expected no action on a paused item, got %v", client.reopenIssueCalls)
	}
}

// TestSettleLandingVerification_SkipsItemsWithoutMarker verifies the scan
// only acts on items carrying the durable marker — old, pre-feature Done
// items are naturally out of scope.
func TestSettleLandingVerification_SkipsItemsWithoutMarker(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) {
			t.Fatal("FetchPRMerged should not be called for an item without the marker")
			return false, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{Number: 1, Repo: "owner/repo", Status: "Done", Labels: []string{"stage:Done:complete"}},
		},
	}

	eng.settleLandingVerification(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.reopenIssueCalls) != 0 {
		t.Errorf("expected no action on an item without the marker, got %v", client.reopenIssueCalls)
	}
}

// TestMoveItemToValidate_MissingOption_ReturnsError covers moveItemToValidate's
// own error path directly — a board with no "Validate" status option.
func TestMoveItemToValidate_MissingOption_ReturnsError(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.statusField = &gh.StatusField{FieldID: "FIELD_1", Options: map[string]string{"Research": "OPT_Research"}}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo"}

	if err := eng.moveItemToValidate(board, item); err == nil {
		t.Fatal("expected an error when no Validate status option exists")
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no UpdateProjectItemStatus call when the option lookup fails, got %v", client.updateStatusCalls)
	}
}

// TestMoveItemToValidate_Success covers the direct-success path.
func TestMoveItemToValidate_Success(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo"}

	if err := eng.moveItemToValidate(board, item); err != nil {
		t.Fatalf("moveItemToValidate: %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.updateStatusCalls) != 1 || client.updateStatusCalls[0].optionID != "OPT_Validate" {
		t.Errorf("expected one UpdateProjectItemStatus call targeting Validate, got %v", client.updateStatusCalls)
	}
}
