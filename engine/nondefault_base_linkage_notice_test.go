package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestNoteNonDefaultBasePRLinkage_BaseNotDefault_PostsOnce covers AC1: an item
// with a base:<non-default> label and a discovered PR receives exactly one
// comment naming that PR, and the marker label is added. Written so that
// removing the label-gate (step 4 in noteNonDefaultBasePRLinkage) makes the
// second assertion fail — proving the gate, not just the first call, is
// exercised.
func TestNoteNonDefaultBasePRLinkage_BaseNotDefault_PostsOnce(t *testing.T) {
	client := &mockGitHubClient{}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{
		Number:         42,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 99,
	}
	eng.noteNonDefaultBasePRLinkage(item)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected exactly one AddComment call, got %d: %v", len(client.addCommentCalls), client.addCommentCalls)
	}
	call := client.addCommentCalls[0]
	if call.owner != "owner" || call.repo != "repo" || call.issueNumber != 42 {
		t.Errorf("unexpected AddComment args: %+v", call)
	}
	if !containsAll(call.body, "PR #99", "base:develop", "main") {
		t.Errorf("comment body missing expected content: %s", call.body)
	}
}

// TestNoteNonDefaultBasePRLinkage_RepeatedPolls_NoDuplicate covers AC1's
// non-vacuousness and AC2: two consecutive calls (simulating repeated polls)
// produce only one comment, because the second call observes the marker
// label added by the first.
func TestNoteNonDefaultBasePRLinkage_RepeatedPolls_NoDuplicate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{
		Number:         42,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 99,
	}

	eng.noteNonDefaultBasePRLinkage(item)
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("after first call: expected 1 AddComment call, got %d", len(client.addCommentCalls))
	}

	// Simulate the item on the next poll snapshot, now carrying the marker
	// label the first call added.
	item.Labels = append(item.Labels, nonDefaultBaseLinkageNoticeLabel)
	eng.noteNonDefaultBasePRLinkage(item)
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("after second call (simulated repeated poll): expected still 1 AddComment call, got %d", len(client.addCommentCalls))
	}

	// Simulate an engine restart: a fresh Engine, same client (durable label
	// state persists via item.Labels, not any in-memory Engine field).
	eng2 := setupCloseTestRepo(t, client)
	eng2.noteNonDefaultBasePRLinkage(item)
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("after third call (simulated restart): expected still 1 AddComment call, got %d", len(client.addCommentCalls))
	}
}

// TestNoteNonDefaultBasePRLinkage_DefaultBase_NoComment covers AC3: an item
// with no base: label at all receives no comment and no marker label.
func TestNoteNonDefaultBasePRLinkage_DefaultBase_NoComment(t *testing.T) {
	client := &mockGitHubClient{}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{
		Number:         42,
		Repo:           "owner/repo",
		Labels:         []string{"stage:Validate:complete"},
		LinkedPRNumber: 99,
	}
	eng.noteNonDefaultBasePRLinkage(item)

	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no AddComment call for a default-base item, got %v", client.addCommentCalls)
	}
	if hasLabel(item.Labels, nonDefaultBaseLinkageNoticeLabel) {
		t.Errorf("expected no marker label added for a default-base item")
	}
}

// TestNoteNonDefaultBasePRLinkage_BaseLabelMatchesDefault_NoComment covers the
// explicit base:main (matching the repo's actual default) case of AC3 — the
// base: label is present, but resolves to the same branch as the default, so
// the notice must still not fire.
func TestNoteNonDefaultBasePRLinkage_BaseLabelMatchesDefault_NoComment(t *testing.T) {
	client := &mockGitHubClient{}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{
		Number:         42,
		Repo:           "owner/repo",
		Labels:         []string{"base:main"},
		LinkedPRNumber: 99,
	}
	eng.noteNonDefaultBasePRLinkage(item)

	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no AddComment call for a base:main label matching the repo default, got %v", client.addCommentCalls)
	}
}

// TestNoteNonDefaultBasePRLinkage_NoLinkedPR_NoComment covers R1: no PR has
// been discovered yet, so there's nothing to name — the notice must not fire
// even though the base differs from default.
func TestNoteNonDefaultBasePRLinkage_NoLinkedPR_NoComment(t *testing.T) {
	client := &mockGitHubClient{}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{
		Number: 42,
		Repo:   "owner/repo",
		Labels: []string{"base:develop"},
		// LinkedPRNumber left zero.
	}
	eng.noteNonDefaultBasePRLinkage(item)

	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no AddComment call when no PR is linked yet, got %v", client.addCommentCalls)
	}
}

// TestNoteNonDefaultBasePRLinkage_NoWorktreeManager_SkipsWithoutPanic covers
// the unregistered-WorktreeManager self-heal path: no panic, no comment, safe
// to retry on a later poll once a WorktreeManager is registered.
func TestNoteNonDefaultBasePRLinkage_NoWorktreeManager_SkipsWithoutPanic(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	// testEngine registers a placeholder WorktreeManager at "owner/repo" (per
	// close_nondefault_base_test.go's own doc comment) — use a repo key that
	// has no registration at all to exercise the "not ok" branch.
	item := gh.ProjectItem{
		Number:         42,
		Repo:           "someother/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 99,
	}
	eng.noteNonDefaultBasePRLinkage(item)

	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no AddComment call when no WorktreeManager is registered, got %v", client.addCommentCalls)
	}
}
