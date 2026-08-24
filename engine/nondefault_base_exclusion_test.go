package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestMarkNonDefaultBaseExcluded_CommentFails_LabelNotAppliedRetriesNextPoll is a
// Review-stage regression test (found by handarbeit-pruefer on PR #1652): the
// label is the sole idempotency gate for R3's one-time comment (item.Comments
// isn't reliably populated for Queued items), so applying it unconditionally
// after a possibly-failed AddComment would silently and permanently lose the
// explanation for that member — hasLabel would short-circuit every later poll
// with no retry path. The label must only be applied once the comment is
// confirmed posted; a failed attempt must leave the label off so the next
// poll's filterNonDefaultBaseMembers call retries naturally.
func TestMarkNonDefaultBaseExcluded_CommentFails_LabelNotAppliedRetriesNextPoll(t *testing.T) {
	var mu sync.Mutex
	failComment := true
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			if failComment {
				return 0, fmt.Errorf("simulated transient AddComment failure")
			}
			return 1, nil
		},
	}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{Number: 2, Repo: "owner/repo", Labels: []string{"base:develop"}}

	// First poll: AddComment fails.
	kept := eng.filterNonDefaultBaseMembers("owner/repo", []gh.ProjectItem{item})
	if len(kept) != 0 {
		t.Fatalf("expected #2 still excluded despite the comment failure, got %+v", kept)
	}
	client.mu.Lock()
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected exactly one comment attempt after the first (failed) poll, got %d", len(client.addCommentCalls))
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected the label NOT applied after a failed comment post, got %v", client.addLabelCalls)
	}
	client.mu.Unlock()

	// Second poll: item.Labels is unchanged (still no exclusion label, since the
	// first attempt never applied it) — the comment must be retried.
	mu.Lock()
	failComment = false
	mu.Unlock()
	eng.filterNonDefaultBaseMembers("owner/repo", []gh.ProjectItem{item})

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) != 2 {
		t.Errorf("expected the comment retried on the next poll (2 total attempts), got %d", len(client.addCommentCalls))
	}
	labeled := false
	for _, c := range client.addLabelCalls {
		if c.issueNumber == 2 && c.labelName == nonDefaultBaseExcludedLabel {
			labeled = true
		}
	}
	if !labeled {
		t.Errorf("expected the label applied once the retried comment succeeds, got %v", client.addLabelCalls)
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

// TestRouteQueuedGroup_RunawayGuardHook2_NeverPausesNonDefaultBaseExcluded is a
// Review-stage regression test (found by handarbeit-pruefer on PR #1652): Hook
// 2's runaway-guard pause (routeQueuedGroup) used to sweep the pre-filter
// `items` slice unconditionally, so a member excluded this same poll for a
// non-default base — just told via markNonDefaultBaseExcluded's own comment
// that it "remains safely in Queued, not paused, not failed, not moved" —
// could still be paused and runaway-alerted in the very same call if the
// runaway guard had already tripped. A default-base sibling in the same
// batch must still be paused/alerted as before (sanity: the fix must not
// blunt Hook 2 generally).
func TestRouteQueuedGroup_RunawayGuardHook2_NeverPausesNonDefaultBaseExcluded(t *testing.T) {
	client := &mockGitHubClient{}
	_, _, worktreeRoot, wm := setupTrainRepo(t)

	shaOut := gitOutputDir(t, wm.baseDir, "rev-parse", "HEAD")
	mustGitDir(t, wm.baseDir, "update-ref", "refs/remotes/origin/develop", strings.TrimSpace(shaOut))

	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, nil)
	eng.registerWorktrees("owner/repo", wm.baseDir, worktreeRoot)
	eng.cfg.MaxTrainTrialsPerWindow = 1
	eng.cfg.TrainTrialWindowDuration = time.Hour

	repoKey := "owner/repo"
	eng.recordTrial(repoKey) // trips the counter (threshold 1)

	defaultItem := makeTrainItem(1, "Default base member")
	excludedItem := makeTrainItem(2, "Non-default base member")
	excludedItem.Labels = append(excludedItem.Labels, "base:develop")

	eng.routeQueuedGroup(context.Background(), repoKey, []gh.ProjectItem{defaultItem, excludedItem}, "PVT_test")

	client.mu.Lock()
	defer client.mu.Unlock()

	// #2 (non-default-base excluded): must get the exclusion comment/label,
	// and must NOT be paused or runaway-alerted.
	excludedLabeled, excludedPaused, excludedAlerted := false, false, false
	for _, c := range client.addLabelCalls {
		if c.issueNumber == 2 {
			if c.labelName == nonDefaultBaseExcludedLabel {
				excludedLabeled = true
			}
			if c.labelName == "fabrik:paused" {
				excludedPaused = true
			}
		}
	}
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 2 && strings.Contains(c.body, "runaway guard") {
			excludedAlerted = true
		}
	}
	if !excludedLabeled {
		t.Errorf("expected #2 to get %s despite the runaway guard being tripped", nonDefaultBaseExcludedLabel)
	}
	if excludedPaused {
		t.Error("expected #2 (non-default-base excluded) never paused by the runaway guard — its own comment promises it stays safely in Queued")
	}
	if excludedAlerted {
		t.Error("expected #2 (non-default-base excluded) never runaway-alerted")
	}

	// #1 (default base): sanity — Hook 2 must still pause/alert normally.
	defaultPaused, defaultAlerted := false, false
	for _, c := range client.addLabelCalls {
		if c.issueNumber == 1 && c.labelName == "fabrik:paused" {
			defaultPaused = true
		}
	}
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 1 && strings.Contains(c.body, "runaway guard") {
			defaultAlerted = true
		}
	}
	if !defaultPaused {
		t.Error("expected #1 (default base) still paused by the runaway guard (sanity)")
	}
	if !defaultAlerted {
		t.Error("expected #1 (default base) still runaway-alerted (sanity)")
	}

	if _, ok := eng.mergeTrainInFlight.Load(repoKey); ok {
		t.Error("expected no worker dispatched when the runaway guard is already tripped")
	}
}

// TestRouteQueuedGroup_MixedBatch_NonDefaultBaseMemberNeverInTrial is the
// AC1 regression test: end-to-end through the real routeQueuedGroup ->
// dispatchMergeTrainWorker -> runMergeTrainWorker path (real git, no mocks
// standing in for trial-branch assembly), a batch mixing a default-base
// member (#1) and a base:develop member (#2) forms a trial containing only
// #1: #2's linked PR is never even looked up by the trial-assembly path
// (fetchTrainMembers), #2 is never merged into the trial branch, and #2 is
// never closed/advanced as a landed member — it stays untouched in Queued.
//
// AC1 requires this be proved non-vacuous: with the
// filterNonDefaultBaseMembers call in routeQueuedGroup temporarily commented
// out, this exact test was confirmed to FAIL — #2's PR was looked up,
// merged into the trial, and landed/closed as part of a 2-member integration
// PR that targeted "main" despite #2's base:develop label — before the guard
// was restored. See the PR description for the observed failure output.
func TestRouteQueuedGroup_MixedBatch_NonDefaultBaseMemberNeverInTrial(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, worktreeRoot, wm := setupTrainRepo(t)

	sha1 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-1", "file1.txt", "content1\n")
	sha2 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-2", "file2.txt", "content2\n")

	// Register "develop" as an existing non-default branch on origin, exactly
	// as setupCloseTestRepo does, so #2's base:develop label resolves cleanly
	// (not via the nonexistent-branch fallback).
	shaOut := gitOutputDir(t, wm.baseDir, "rev-parse", "HEAD")
	mustGitDir(t, wm.baseDir, "update-ref", "refs/remotes/origin/develop", strings.TrimSpace(shaOut))

	var mu sync.Mutex
	var createdPRs []createDraftPRCall
	linkedPRLookups := map[int]int{}

	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			mu.Lock()
			linkedPRLookups[issueNumber]++
			mu.Unlock()
			switch issueNumber {
			case 1:
				return &gh.PRDetails{Number: 10, HeadSHA: sha1, State: "open"}, nil
			case 2:
				return &gh.PRDetails{Number: 11, HeadSHA: sha2, State: "open"}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			mu.Lock()
			createdPRs = append(createdPRs, createDraftPRCall{owner, repo, title, head, base, body, issueNumber})
			mu.Unlock()
			return 99, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil // CI green immediately
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, nil)
	eng.registerWorktrees("owner/repo", wm.baseDir, worktreeRoot)

	defaultItem := makeTrainItem(1, "Default base member")
	nonDefaultItem := makeTrainItem(2, "Non-default base member")
	nonDefaultItem.Labels = append(nonDefaultItem.Labels, "base:develop")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.routeQueuedGroup(ctx, "owner/repo", []gh.ProjectItem{defaultItem, nonDefaultItem}, "PVT_test")

	// dispatchMergeTrainWorker launches the worker in a goroutine tracked by
	// eng.wg — wait for it to finish before asserting, mirroring
	// TestDispatchMergeTrainWorker_SkipsWhenAlreadyAssembling's wg.Wait() idiom.
	done := make(chan struct{})
	go func() {
		eng.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the merge-train worker to finish")
	}

	mu.Lock()
	if linkedPRLookups[2] != 0 {
		t.Errorf("expected #2's linked PR never looked up by trial assembly (filtered pre-dispatch), got %d lookup(s)", linkedPRLookups[2])
	}
	if linkedPRLookups[1] == 0 {
		t.Error("expected #1's linked PR to be looked up (sanity: the default-base member must still be processed)")
	}
	if len(createdPRs) != 1 {
		t.Errorf("expected exactly 1 draft PR (default-base member only), got %d: %+v", len(createdPRs), createdPRs)
	} else if createdPRs[0].base != "main" {
		t.Errorf("expected draft PR base %q, got %q", "main", createdPRs[0].base)
	}
	mu.Unlock()

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.closeIssueCalls {
		if c.issueNumber == 2 {
			t.Error("expected #2 never closed as a landed train member — it must stay untouched in Queued")
		}
	}
	closedIssue1 := false
	for _, c := range client.closeIssueCalls {
		if c.issueNumber == 1 {
			closedIssue1 = true
		}
	}
	if !closedIssue1 {
		t.Error("expected #1 closed as a landed train member (sanity: the default-base member must still land)")
	}
}
