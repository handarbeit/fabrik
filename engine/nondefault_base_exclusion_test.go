package engine

import (
	"context"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// ── nonDefaultBaseExclusion ──────────────────────────────────────────────────

func TestNonDefaultBaseExclusion_NoBaseLabel_FastPath(t *testing.T) {
	client := &mockGitHubClient{}
	// A non-git WorktreeManager (testEngine's default): if the fast path ever
	// touched it, DefaultBaseBranch/baseBranchForItem would error against a
	// non-repo directory and resolved would come back false. A clean
	// (false, "", "", true) with no such error is proof the WM was never
	// consulted for the no-label common case (R5).
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo", Labels: []string{"stage:Validate:complete"}}
	exclude, base, def, resolved := eng.nonDefaultBaseExclusion(item, "owner/repo")
	if exclude || base != "" || def != "" || !resolved {
		t.Errorf("nonDefaultBaseExclusion(no label) = (%v, %q, %q, %v), want (false, \"\", \"\", true)", exclude, base, def, resolved)
	}
}

func TestNonDefaultBaseExclusion_NonDefaultBranch_Excluded(t *testing.T) {
	client := &mockGitHubClient{}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"base:develop"}}
	exclude, base, def, resolved := eng.nonDefaultBaseExclusion(item, "owner/repo")
	if !exclude || base != "develop" || def != "main" || !resolved {
		t.Errorf("nonDefaultBaseExclusion(base:develop) = (%v, %q, %q, %v), want (true, \"develop\", \"main\", true)", exclude, base, def, resolved)
	}
}

func TestNonDefaultBaseExclusion_LabelMatchesDefault_NotExcluded(t *testing.T) {
	client := &mockGitHubClient{}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"base:main"}}
	exclude, base, def, resolved := eng.nonDefaultBaseExclusion(item, "owner/repo")
	if exclude || base != "main" || def != "main" || !resolved {
		t.Errorf("nonDefaultBaseExclusion(base:main) = (%v, %q, %q, %v), want (false, \"main\", \"main\", true)", exclude, base, def, resolved)
	}
}

// TestNonDefaultBaseExclusion_NonexistentBranch_NotExcluded pins the Prior Art
// contract: a base: label naming a branch that doesn't exist on the remote
// resolves (via baseBranchForItem's own fallback) to the repository default,
// and must NOT be excluded — only a genuinely resolvable non-default base is.
func TestNonDefaultBaseExclusion_NonexistentBranch_NotExcluded(t *testing.T) {
	client := &mockGitHubClient{}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"base:does-not-exist"}}
	exclude, base, def, resolved := eng.nonDefaultBaseExclusion(item, "owner/repo")
	if exclude || base != "main" || def != "main" || !resolved {
		t.Errorf("nonDefaultBaseExclusion(base:does-not-exist) = (%v, %q, %q, %v), want (false, \"main\", \"main\", true)", exclude, base, def, resolved)
	}
}

func TestNonDefaultBaseExclusion_UnregisteredWM_FailClosed(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// repoKey has no registered WorktreeManager at all (distinct from
	// "owner/repo", the only key testEngine registers) — mirrors
	// TestCloseIssueIfNonDefaultBase_UnregisteredRepo_NoPanic.
	item := gh.ProjectItem{Number: 42, Repo: "fail-test/unregistered", Labels: []string{"base:develop"}}
	exclude, base, def, resolved := eng.nonDefaultBaseExclusion(item, "fail-test/unregistered")
	if !exclude || base != "" || def != "" || resolved {
		t.Errorf("nonDefaultBaseExclusion(unregistered WM) = (%v, %q, %q, %v), want (true, \"\", \"\", false)", exclude, base, def, resolved)
	}
}

// ── filterNonDefaultBaseMembers ──────────────────────────────────────────────

// TestFilterNonDefaultBaseMembers_MixedBatch_ExcludesOnlyNonDefault covers AC4:
// a batch mixing a default-base member and a base:-labelled member keeps only
// the default-base member, and the excluded one gets exactly one explanatory
// comment and the exclusion label.
func TestFilterNonDefaultBaseMembers_MixedBatch_ExcludesOnlyNonDefault(t *testing.T) {
	client := &mockGitHubClient{}
	eng := setupCloseTestRepo(t, client)

	defaultItem := gh.ProjectItem{Number: 1, Repo: "owner/repo", Title: "Default base member"}
	excludedItem := gh.ProjectItem{Number: 2, Repo: "owner/repo", Title: "Non-default base member", Labels: []string{"base:develop"}}

	kept := eng.filterNonDefaultBaseMembers("owner/repo", []gh.ProjectItem{defaultItem, excludedItem})

	if len(kept) != 1 || kept[0].Number != 1 {
		t.Fatalf("filterNonDefaultBaseMembers kept = %+v, want only #1", kept)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	commentCount := 0
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 2 {
			commentCount++
		}
	}
	if commentCount != 1 {
		t.Errorf("expected exactly one explanatory comment on #2, got %d", commentCount)
	}
	labeled := false
	for _, c := range client.addLabelCalls {
		if c.issueNumber == 2 && c.labelName == nonDefaultBaseExcludedLabel {
			labeled = true
		}
	}
	if !labeled {
		t.Errorf("expected %s applied to #2, got %v", nonDefaultBaseExcludedLabel, client.addLabelCalls)
	}
	// #1 must be untouched.
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 1 {
			t.Errorf("unexpected comment on default-base member #1: %q", c.body)
		}
	}
	for _, c := range client.addLabelCalls {
		if c.issueNumber == 1 {
			t.Errorf("unexpected label on default-base member #1: %q", c.labelName)
		}
	}
}

// TestFilterNonDefaultBaseMembers_NoBaseLabels_Unchanged covers AC5: a slice of
// items with no base: label at all is returned unchanged with zero
// AddComment/AddLabelToIssue calls — the regression guard for the existing path.
func TestFilterNonDefaultBaseMembers_NoBaseLabels_Unchanged(t *testing.T) {
	client := &mockGitHubClient{}
	eng := setupCloseTestRepo(t, client)

	items := []gh.ProjectItem{
		{Number: 1, Repo: "owner/repo", Title: "One"},
		{Number: 2, Repo: "owner/repo", Title: "Two"},
	}
	kept := eng.filterNonDefaultBaseMembers("owner/repo", items)

	if len(kept) != 2 || kept[0].Number != 1 || kept[1].Number != 2 {
		t.Fatalf("filterNonDefaultBaseMembers kept = %+v, want both items unchanged", kept)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no comments for items with no base: label, got %v", client.addCommentCalls)
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no labels for items with no base: label, got %v", client.addLabelCalls)
	}
}

// TestFilterNonDefaultBaseMembers_Idempotent_CommentPostedOnce covers AC3: the
// explanatory comment is posted exactly once across repeated poll cycles — a
// second call sees the exclusion label already present and skips re-posting.
func TestFilterNonDefaultBaseMembers_Idempotent_CommentPostedOnce(t *testing.T) {
	client := &mockGitHubClient{}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{Number: 2, Repo: "owner/repo", Labels: []string{"base:develop"}}

	eng.filterNonDefaultBaseMembers("owner/repo", []gh.ProjectItem{item})

	client.mu.Lock()
	if len(client.addCommentCalls) != 1 {
		client.mu.Unlock()
		t.Fatalf("expected exactly one comment after first pass, got %d", len(client.addCommentCalls))
	}
	client.mu.Unlock()

	// Simulate the next poll cycle: the item now carries the label GitHub
	// applied on the first pass.
	item.Labels = append(item.Labels, nonDefaultBaseExcludedLabel)
	eng.filterNonDefaultBaseMembers("owner/repo", []gh.ProjectItem{item})

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) != 1 {
		t.Errorf("expected the comment to remain posted exactly once across two poll cycles, got %d", len(client.addCommentCalls))
	}
}

// TestFilterNonDefaultBaseMembers_SelfClears_WhenBaseNoLongerDiffers covers the
// self-clearing half of R3: an item previously excluded and labeled whose
// base: label is later removed (now resolves to the default again) has the
// exclusion label removed and is kept in the batch.
func TestFilterNonDefaultBaseMembers_SelfClears_WhenBaseNoLongerDiffers(t *testing.T) {
	client := &mockGitHubClient{}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{Number: 2, Repo: "owner/repo", Labels: []string{nonDefaultBaseExcludedLabel}}

	kept := eng.filterNonDefaultBaseMembers("owner/repo", []gh.ProjectItem{item})

	if len(kept) != 1 || kept[0].Number != 2 {
		t.Fatalf("filterNonDefaultBaseMembers kept = %+v, want #2 kept once its base: label is gone", kept)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	removed := false
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 2 && c.labelName == nonDefaultBaseExcludedLabel {
			removed = true
		}
	}
	if !removed {
		t.Errorf("expected %s removed once the item no longer resolves non-default, got %v", nonDefaultBaseExcludedLabel, client.removeLabelCalls)
	}
}

func TestFilterNonDefaultBaseMembers_UnresolvedWM_ExcludesWithoutComment(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 2, Repo: "fail-test/unregistered", Labels: []string{"base:develop"}}
	kept := eng.filterNonDefaultBaseMembers("fail-test/unregistered", []gh.ProjectItem{item})

	if len(kept) != 0 {
		t.Fatalf("expected the item excluded (fail-closed) when no WorktreeManager is registered, got %+v", kept)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no explanatory comment while the true base is unresolvable, got %v", client.addCommentCalls)
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no exclusion label while the true base is unresolvable, got %v", client.addLabelCalls)
	}
}

// ── routeQueuedGroup wiring ───────────────────────────────────────────────────

// TestRouteQueuedGroup_ExcludesNonDefaultBaseMember_NoDispatch proves the filter
// is actually reached from routeQueuedGroup, not just callable in isolation: a
// Queued group containing only a base:<non-default> member results in no
// worker dispatch, and the exclusion comment/label land on the member.
func TestRouteQueuedGroup_ExcludesNonDefaultBaseMember_NoDispatch(t *testing.T) {
	client := &mockGitHubClient{}
	_, _, worktreeRoot, wm := setupTrainRepo(t)

	shaOut := gitOutputDir(t, wm.baseDir, "rev-parse", "HEAD")
	mustGitDir(t, wm.baseDir, "update-ref", "refs/remotes/origin/develop", strings.TrimSpace(shaOut))

	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, nil)
	eng.registerWorktrees("owner/repo", wm.baseDir, worktreeRoot)

	item := makeTrainItem(2, "Non-default base member")
	item.Labels = append(item.Labels, "base:develop")

	eng.routeQueuedGroup(context.Background(), "owner/repo", []gh.ProjectItem{item}, "PVT_test")

	if _, ok := eng.mergeTrainInFlight.Load("owner/repo"); ok {
		t.Error("expected no worker dispatched when the only Queued member is non-default-base excluded")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	labeled := false
	for _, c := range client.addLabelCalls {
		if c.issueNumber == 2 && c.labelName == nonDefaultBaseExcludedLabel {
			labeled = true
		}
	}
	if !labeled {
		t.Errorf("expected %s applied via the real routeQueuedGroup call site, got %v", nonDefaultBaseExcludedLabel, client.addLabelCalls)
	}
	if len(client.addCommentCalls) != 1 {
		t.Errorf("expected exactly one explanatory comment, got %d", len(client.addCommentCalls))
	}
}
