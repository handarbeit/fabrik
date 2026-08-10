package engine

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestCatchUpLoop_NonYolo_ReviewReinvoke_Fires verifies that Phase 1 of the
// catch-up loop dispatches dispatchReviewReinvoke even when the item has no
// fabrik:yolo or fabrik:cruise label. This is the core behavior added by
// issue #392: inline PR review thread comments must be addressed on all issues,
// not just yolo/cruise ones.
func TestCatchUpLoop_NonYolo_ReviewReinvoke_Fires(t *testing.T) {
	threadComment := gh.Comment{
		ID:             "PRRC_nonyolo_1",
		DatabaseID:     301,
		Author:         "copilot",
		Body:           "Please fix the error handling.",
		ReviewThreadID: "RT_nonyolo_1",
	}

	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 55,
						ItemID: "PVTI_55",
						Status: "Implement",
						Repo:   "owner/repo",
						// No yolo or cruise label — this is a normal issue
						Labels: []string{"stage:Implement:complete"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRReviewThreadComments = []gh.Comment{threadComment}
			return nil
		},
	}

	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#55"] = true
	eng.mayNeedWorkMu.Unlock()

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	// ReviewCycles should be 1 — dispatchReviewReinvoke was dispatched.
	snap55, _ := eng.store.Get("owner/repo", 55)
	if snap55.ReviewCycles("Implement") != 1 {
		t.Errorf("ReviewCycles(Implement) = %d; want 1 (reinvoke must fire for non-yolo items with unresolved review threads)", snap55.ReviewCycles("Implement"))
	}

	// No stage advancement should have occurred (Phase 2 is gated on yolo/cruise).
	client.mu.Lock()
	statusCalls := len(client.updateStatusCalls)
	client.mu.Unlock()
	if statusCalls != 0 {
		t.Errorf("updateStatusCalls = %d; want 0 (non-yolo items must not auto-advance)", statusCalls)
	}
}

// TestCatchUpLoop_NonYolo_NoThreads_NoAdvance verifies that a non-yolo item
// with stage:X:complete but no unresolved review thread comments does NOT
// auto-advance. Phase 2 advancement remains gated on yolo/cruise/auto_advance.
func TestCatchUpLoop_NonYolo_NoThreads_NoAdvance(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 56,
						ItemID: "PVTI_56",
						Status: "Implement",
						Repo:   "owner/repo",
						// No yolo or cruise label, no review threads
						Labels: []string{"stage:Implement:complete"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			// No review thread comments — nothing to reinvoke or advance.
			item.LinkedPRReviewThreadComments = nil
			return nil
		},
	}

	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#56"] = true
	eng.mayNeedWorkMu.Unlock()

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	// No advancement and no reinvoke cycle.
	client.mu.Lock()
	statusCalls := len(client.updateStatusCalls)
	client.mu.Unlock()
	if statusCalls != 0 {
		t.Errorf("updateStatusCalls = %d; want 0 (non-yolo items must not auto-advance)", statusCalls)
	}
	snap56, _ := eng.store.Get("owner/repo", 56)
	if snap56.ReviewCycles("Implement") != 0 {
		t.Errorf("ReviewCycles(Implement) = %d; want 0 (no review threads, no reinvoke)", snap56.ReviewCycles("Implement"))
	}
}

// TestCatchUpLoop_ReviewReinvoke_AllNoticeThread_NoInvocation verifies the
// #1221 chokepoint end to end via dispatchReviewReinvoke: when every
// unresolved review-thread comment is a bot service notice,
// buildReviewThreadComments (used by both precheck and build) does not
// exclude it, so the reinvoke still dispatches and ReviewCycles is still
// incremented — but processComments's chokepoint filter empties the working
// slice before Claude is invoked. This is the documented, accepted trade-off
// (dispatch/cycle-count waste is left unfixed; only invocation is guarded).
func TestCatchUpLoop_ReviewReinvoke_AllNoticeThread_NoInvocation(t *testing.T) {
	noticeComment := gh.Comment{
		ID:             "PRRC_notice_1",
		DatabaseID:     302,
		Author:         "gemini-code-assist[bot]",
		Body:           "You have reached your daily quota limit.",
		ReviewThreadID: "RT_notice_1",
	}

	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 57,
						ItemID: "PVTI_57",
						Status: "Implement",
						Repo:   "owner/repo",
						Labels: []string{"stage:Implement:complete"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRReviewThreadComments = []gh.Comment{noticeComment}
			return nil
		},
	}

	claude := &mockClaudeInvoker{}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.claude = claude
	eng.cfg.MaxReviewCycles = 5
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#57"] = true
	eng.mayNeedWorkMu.Unlock()

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	// Documented, accepted trade-off: ReviewCycles still increments because the
	// dispatch/cycle gate uses the unfiltered buildReviewThreadComments count.
	snap57, _ := eng.store.Get("owner/repo", 57)
	if snap57.ReviewCycles("Implement") != 1 {
		t.Errorf("ReviewCycles(Implement) = %d; want 1 (dispatch/cycle-count gate is unfiltered by design)", snap57.ReviewCycles("Implement"))
	}

	// The chokepoint fix: no Claude invocation for an all-notice thread.
	if len(claude.forCommentsCalls) != 0 {
		t.Errorf("expected 0 InvokeForComments calls (all-notice review thread), got %d", len(claude.forCommentsCalls))
	}
}

// TestHandleReviewGate_NoOpReinvoke_LeavesCycleCounterUnchanged is AC3: a
// generic COMMENTED overview (no actionable findings, per #1045's removal of
// the CHANGES_REQUESTED-only filter) triggers exactly one reinvoke, and that
// reinvoke — having landed no commit — leaves ReviewCycles unchanged rather
// than spending the budget that protects genuine findings (req 2). Uses a
// real git worktree (skipIfNoGit) so dispatchReviewReinvoke's gitHeadSHA
// before/after comparison is genuinely exercised, not vacuously skipped for
// lack of a real HEAD to compare (see the #1221-chokepoint doc comment on
// dispatchReviewReinvoke for why a fake worktree can't exercise this path).
func TestHandleReviewGate_NoOpReinvoke_LeavesCycleCounterUnchanged(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Simulates the no-op contract (req 3): a generic overview with no
			// actionable findings — summarize, change nothing, no commit.
			return "Reviewed the overview; nothing actionable to address.", false, TokenUsage{}, nil
		},
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: boolPtr(true)},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, stgs)
	eng.cfg.MaxReviewCycles = 3

	// A real reinvoke always targets a worktree an earlier Implement run
	// already created — dispatchReviewReinvoke's gitHeadSHA-before snapshot
	// needs that worktree to exist, or there is no HEAD to compare and the
	// no-op check is (correctly) inert. Pre-create it here to simulate that.
	if _, err := eng.worktreesFor("owner/repo").EnsureWorktree(30, "main", false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         30,
		Repo:           "owner/repo",
		Labels:         []string{"stage:Implement:complete"},
		LinkedPRNumber: 99,
		LinkedPRReviews: []gh.PRReview{
			{Author: "copilot-pull-request-reviewer", State: "COMMENTED", Body: "## Pull request overview\n\nLGTM.", DatabaseID: 1001},
		},
	}
	advancedItems := make(map[string]bool)
	pctx := &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stgs[0],
		hasComplete:   true,
		advancedItems: advancedItems,
	}

	got := eng.handleReviewGate(pctx)
	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()

	if len(claude.forCommentsCalls) != 1 {
		t.Fatalf("expected exactly 1 InvokeForComments call, got %d", len(claude.forCommentsCalls))
	}
	snap, _ := eng.store.Get("owner/repo", 30)
	if got := snap.ReviewCycles("Implement"); got != 0 {
		t.Errorf("ReviewCycles(Implement) = %d; want 0 (increment + no-op decrement must net to unchanged)", got)
	}
}

// TestHandleReviewGate_FiveNoOpReinvokes_DoNotExhaustBudget_SixthGenuineFindingAddressed
// is AC4, the single most important case in #1045: five consecutive no-op
// reinvokes (distinct junk COMMENTED overviews, each with its own
// DatabaseID so dedup doesn't just skip them outright) must not pause the
// issue even though MaxReviewCycles is set below 5 — because each one nets
// back to zero (req 2) rather than draining the shared budget — and a
// substantive finding arriving after them must still dispatch and be
// addressed (a real commit landing, and the cycle counter genuinely
// incrementing this time).
func TestHandleReviewGate_FiveNoOpReinvokes_DoNotExhaustBudget_SixthGenuineFindingAddressed(t *testing.T) {
	skipIfNoGit(t)

	const genuineMarker = "ACTIONABLE_FIX_NEEDED"

	client := &mockGitHubClient{}
	var seenComments [][]gh.Comment
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			seenComments = append(seenComments, comments)
			genuine := false
			for _, c := range comments {
				if strings.Contains(c.Body, genuineMarker) {
					genuine = true
				}
			}
			if !genuine {
				// No-op contract (req 3): nothing actionable, no commit.
				return "Nothing actionable in this overview.", false, TokenUsage{}, nil
			}
			// A genuine finding: simulate fixing it with a real commit.
			cmd := exec.Command("git", "commit", "--allow-empty", "-m", "address review finding")
			cmd.Dir = workDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git commit in mock: %s: %v", out, err)
			}
			return "Addressed the finding.", false, TokenUsage{}, nil
		},
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: boolPtr(true)},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, stgs)
	// Deliberately below 5 — if a no-op dispatch spent the budget, the fifth
	// no-op alone would already trip the cycle limit and pause the issue.
	eng.cfg.MaxReviewCycles = 3

	// See TestHandleReviewGate_NoOpReinvoke_LeavesCycleCounterUnchanged's
	// comment: the no-op check needs a pre-existing worktree to compare
	// HEAD against, matching the real "Implement already ran" precondition.
	if _, err := eng.worktreesFor("owner/repo").EnsureWorktree(31, "main", false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	baseItem := gh.ProjectItem{
		Number:         31,
		Repo:           "owner/repo",
		Labels:         []string{"stage:Implement:complete"},
		LinkedPRNumber: 100,
	}

	dispatchRound := func(review gh.PRReview) {
		item := baseItem
		item.LinkedPRReviews = []gh.PRReview{review}
		advancedItems := make(map[string]bool)
		pctx := &phase1Ctx{
			ctx:           context.Background(),
			board:         board,
			item:          item,
			stage:         stgs[0],
			hasComplete:   true,
			advancedItems: advancedItems,
		}
		if got := eng.handleReviewGate(pctx); !got {
			t.Fatalf("handleReviewGate: expected true (item claimed) for review %d, got false", review.DatabaseID)
		}
		eng.wg.Wait()
	}

	// Five distinct no-op junk overviews, one dispatch each.
	for i := 0; i < 5; i++ {
		dispatchRound(gh.PRReview{
			Author:     "copilot-pull-request-reviewer",
			State:      "COMMENTED",
			Body:       "## Pull request overview\n\nLGTM, no issues.",
			DatabaseID: 2000 + i,
		})

		client.mu.Lock()
		labelNames := make([]string, len(client.addLabelCalls))
		for j, c := range client.addLabelCalls {
			labelNames[j] = c.labelName
		}
		client.mu.Unlock()
		for _, l := range labelNames {
			if l == "fabrik:paused" {
				t.Fatalf("round %d: fabrik:paused was applied — a no-op reinvoke must not exhaust MaxReviewCycles", i+1)
			}
		}

		snap, _ := eng.store.Get("owner/repo", 31)
		if got := snap.ReviewCycles("Implement"); got != 0 {
			t.Errorf("round %d: ReviewCycles(Implement) = %d; want 0 (no-op must net to unchanged)", i+1, got)
		}
	}

	if len(seenComments) != 5 {
		t.Fatalf("expected exactly 5 InvokeForComments calls after 5 rounds, got %d", len(seenComments))
	}

	// Sixth round: a genuine finding must still be addressed.
	dispatchRound(gh.PRReview{
		Author:     "handarbeit-pruefer",
		State:      "COMMENTED",
		Body:       "**File:** engine/foo.go\n" + genuineMarker + ": missing nil check.",
		DatabaseID: 2005,
	})

	if len(seenComments) != 6 {
		t.Fatalf("expected exactly 6 InvokeForComments calls after the genuine finding, got %d", len(seenComments))
	}
	snap, _ := eng.store.Get("owner/repo", 31)
	if got := snap.ReviewCycles("Implement"); got != 1 {
		t.Errorf("ReviewCycles(Implement) = %d; want 1 (the genuine finding is a real attempt and must count)", got)
	}

	client.mu.Lock()
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Fatal("fabrik:paused was applied — the issue must never have been paused across all 6 rounds")
		}
	}
	client.mu.Unlock()
}

// TestHandleReviewGate_BlockedNoOpReinvokes_ReachCycleLimitViaReviewBlockedCycles
// is the R3/ARM2 regression test for issue #1518. #1045's ReviewCycleDecremented
// refund can hold ReviewCycles at 0 indefinitely when every reinvoke happens to
// land no new commit — but unlike the advisory junk-overview shape
// TestHandleReviewGate_FiveNoOpReinvokes_DoNotExhaustBudget_SixthGenuineFindingAddressed
// pins (where the refund must forgive forever, by design), these no-op
// reinvokes are each dispatched while an authoritative gate is still blocked
// on an unresolved CHANGES_REQUESTED verdict. ReviewBlockedCycles — never
// refunded — is what makes the loop still terminate: reverting
// handleReviewGate to use ReviewCycles alone as dispatchWithCycleLimit's
// comparand (undoing the max(ReviewCycles, ReviewBlockedCycles) change) turns
// this test red — the loop would keep dispatching no-op reinvokes forever,
// never reaching MaxReviewCycles.
func TestHandleReviewGate_BlockedNoOpReinvokes_ReachCycleLimitViaReviewBlockedCycles(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "CHANGES_REQUESTED", nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Every reinvoke is a no-op on HEAD, regardless of content — the point
			// of this test is that ReviewBlockedCycles does not care.
			return "Looked at it, no commit pushed.", false, TokenUsage{}, nil
		},
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, stgs)
	eng.cfg.MaxReviewCycles = 3

	// See TestHandleReviewGate_NoOpReinvoke_LeavesCycleCounterUnchanged's
	// comment: the no-op check needs a pre-existing worktree to compare HEAD
	// against, matching the real "Implement already ran" precondition.
	if _, err := eng.worktreesFor("owner/repo").EnsureWorktree(32, "main", false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	baseItem := gh.ProjectItem{
		Number:         32,
		Repo:           "owner/repo",
		Labels:         []string{"stage:Implement:complete"},
		LinkedPRNumber: 101,
	}

	dispatchRound := func(review gh.PRReview) bool {
		item := baseItem
		item.LinkedPRReviews = []gh.PRReview{review}
		advancedItems := make(map[string]bool)
		pctx := &phase1Ctx{
			ctx:           context.Background(),
			board:         board,
			item:          item,
			stage:         stgs[0],
			hasComplete:   true,
			advancedItems: advancedItems,
		}
		if got := eng.handleReviewGate(pctx); !got {
			t.Fatalf("handleReviewGate: expected true (item claimed) for review %d, got false", review.DatabaseID)
		}
		eng.wg.Wait()
		return advancedItems["owner/repo#32"]
	}

	// MaxReviewCycles rounds, each against a fresh, distinct, still-blocking
	// CHANGES_REQUESTED review — every one dispatches (ReviewCycles nets back
	// to 0 via the #1045 refund, but ReviewBlockedCycles keeps climbing since
	// blocked was true at each dispatch).
	for i := 0; i < eng.cfg.MaxReviewCycles; i++ {
		dispatched := dispatchRound(gh.PRReview{
			Author:     "alice",
			State:      "CHANGES_REQUESTED",
			Body:       fmt.Sprintf("please address finding %d", i),
			DatabaseID: 3000 + i,
		})
		if !dispatched {
			t.Fatalf("round %d: expected reinvoke dispatch, got none", i+1)
		}
		snap, _ := eng.store.Get("owner/repo", 32)
		if got := snap.ReviewCycles("Implement"); got != 0 {
			t.Errorf("round %d: ReviewCycles(Implement) = %d; want 0 (no-op refund, #1045)", i+1, got)
		}
		if got := snap.ReviewBlockedCycles("Implement"); got != i+1 {
			t.Errorf("round %d: ReviewBlockedCycles(Implement) = %d; want %d (never refunded)", i+1, got, i+1)
		}
	}

	// One more round: max(ReviewCycles, ReviewBlockedCycles) is now at
	// MaxReviewCycles — must pause instead of dispatching yet another no-op.
	dispatched := dispatchRound(gh.PRReview{
		Author:     "alice",
		State:      "CHANGES_REQUESTED",
		Body:       "please address finding 999",
		DatabaseID: 3999,
	})
	if dispatched {
		t.Error("expected NO further reinvoke dispatch once ReviewBlockedCycles reached MaxReviewCycles")
	}
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused via pauseForReviewCycleLimit once ReviewBlockedCycles reached MaxReviewCycles; labels added: %v", labelNames)
	}
}

// TestProcessComments_MergesReviewThreadComments verifies that processComments
// automatically merges unresolved PR review thread comments from
// item.LinkedPRReviewThreadComments into the working slice. This closes the
// race where a user nudge arrives before the catch-up loop Phase 1 fires —
// the review thread comments are addressed in the same invocation, receive 🚀
// reactions, and have their threads resolved.
func TestProcessComments_MergesReviewThreadComments(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "Addressed the review feedback.", false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Implement", Order: 1}

	// Item has a linked PR review thread comment that is unresolved.
	reviewThreadComment := gh.Comment{
		ID:             "PRRC_merge_1",
		DatabaseID:     401,
		Author:         "copilot",
		Body:           "Please add error handling here.",
		ReviewThreadID: "RT_merge_1",
	}
	item := gh.ProjectItem{
		Number:                       20,
		Repo:                         "owner/repo",
		Body:                         "spec",
		LinkedPRReviewThreadComments: []gh.Comment{reviewThreadComment},
	}

	// User nudge — just a conversation comment, NOT the review thread comment.
	userComment := gh.Comment{
		ID:         "IC_nudge_1",
		DatabaseID: 402,
		Author:     "user",
		Body:       "Please address the Copilot feedback.",
	}

	err := eng.processComments(context.Background(), board, item, stage, []gh.Comment{userComment})
	if err != nil {
		t.Fatalf("processComments: %v", err)
	}

	// ResolveReviewThread must have been called for the review thread.
	client.mu.Lock()
	resolvedThreads := make([]string, len(client.resolveReviewThreadCalls))
	copy(resolvedThreads, client.resolveReviewThreadCalls)
	client.mu.Unlock()

	found := false
	for _, tid := range resolvedThreads {
		if tid == "RT_merge_1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ResolveReviewThread to be called for RT_merge_1; got calls: %v", resolvedThreads)
	}

	// The ROCKET reaction must have been added to the review thread comment.
	client.mu.Lock()
	rocketCalls := make([]prReviewCommentReactionCall, len(client.addPRReviewCommentReactionCalls))
	copy(rocketCalls, client.addPRReviewCommentReactionCalls)
	client.mu.Unlock()

	rocketFound := false
	for _, rc := range rocketCalls {
		if rc.commentID == 401 && rc.content == "rocket" {
			rocketFound = true
			break
		}
	}
	if !rocketFound {
		t.Errorf("expected ROCKET reaction to be added to review comment 401; reaction calls: %v", rocketCalls)
	}
}

// TestCatchUpLoop_YoloIssue_ReviewReinvoke_StillFires verifies that Phase 1
// review reinvoke continues to work correctly for yolo-labeled items
// (regression guard for the existing TestCatchUpLoop_InFlightGuard behavior).
func TestCatchUpLoop_YoloIssue_ReviewReinvoke_StillFires(t *testing.T) {
	threadComment := gh.Comment{
		ID:             "PRRC_yolo_1",
		DatabaseID:     501,
		Author:         "copilot",
		Body:           "Fix this.",
		ReviewThreadID: "RT_yolo_1",
	}

	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 57,
						ItemID: "PVTI_57",
						Status: "Implement",
						Repo:   "owner/repo",
						Labels: []string{"stage:Implement:complete", "fabrik:yolo"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRReviewThreadComments = []gh.Comment{threadComment}
			return nil
		},
	}

	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#57"] = true
	eng.mayNeedWorkMu.Unlock()

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	snap57, _ := eng.store.Get("owner/repo", 57)
	if snap57.ReviewCycles("Implement") != 1 {
		t.Errorf("ReviewCycles(Implement) = %d; want 1 (yolo items must still trigger review reinvoke)", snap57.ReviewCycles("Implement"))
	}
}
