package pruefer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// fakeReviewer implements GitHubReviewer in-memory for review.go's tests.
type fakeReviewer struct {
	*fakeCommenter

	diff             string
	diffErr          error
	filesResult      []string
	filesErr         error
	reviews          []gh.PRReview
	reviewsErr       error
	threads          []gh.PRReviewThread
	threadsTruncated bool
	threadsErr       error
	submitErr        error
	token            string

	mu          sync.Mutex
	submitCalls []submitCall
	diffCalls   int
	filesCalls  int
}

type submitCall struct {
	owner, repo string
	prNumber    int
	commitSHA   string
	body        string
	event       gh.ReviewEvent
	comments    []gh.ReviewComment
}

func (f *fakeReviewer) FetchPRDiff(owner, repo string, prNumber int) (string, error) {
	f.mu.Lock()
	f.diffCalls++
	f.mu.Unlock()
	if f.diffErr != nil {
		return "", f.diffErr
	}
	return f.diff, nil
}

func (f *fakeReviewer) FetchPRFiles(owner, repo string, prNumber int) ([]string, error) {
	f.mu.Lock()
	f.filesCalls++
	f.mu.Unlock()
	if f.filesErr != nil {
		return nil, f.filesErr
	}
	return f.filesResult, nil
}

func (f *fakeReviewer) filesCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.filesCalls
}

func (f *fakeReviewer) FetchPRReviews(owner, repo string, prNumber int) ([]gh.PRReview, error) {
	if f.reviewsErr != nil {
		return nil, f.reviewsErr
	}
	return f.reviews, nil
}

func (f *fakeReviewer) FetchPRReviewThreads(owner, repo string, prNumber int) ([]gh.PRReviewThread, bool, error) {
	if f.threadsErr != nil {
		return nil, false, f.threadsErr
	}
	return f.threads, f.threadsTruncated, nil
}

func (f *fakeReviewer) SubmitPRReview(owner, repo string, prNumber int, commitSHA, body string, event gh.ReviewEvent, comments []gh.ReviewComment) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return 0, f.submitErr
	}
	f.submitCalls = append(f.submitCalls, submitCall{owner, repo, prNumber, commitSHA, body, event, comments})
	return len(f.submitCalls), nil
}

func (f *fakeReviewer) Token() string { return f.token }

func (f *fakeReviewer) submitCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.submitCalls)
}

func (f *fakeReviewer) diffCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.diffCalls
}

// fakeClone returns a CloneFunc that records calls and returns a fresh temp
// dir without touching the network or git at all. Safe for concurrent use
// (daemon_test.go dispatches ReviewPR from multiple goroutines).
func fakeClone(t *testing.T, err error) (CloneFunc, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	fn := func(ctx context.Context, owner, repo, token string, prNumber int) (string, func(), error) {
		calls.Add(1)
		if err != nil {
			return "", func() {}, err
		}
		return t.TempDir(), func() {}, nil
	}
	return fn, &calls
}

func newFakeReviewer() *fakeReviewer {
	return &fakeReviewer{fakeCommenter: &fakeCommenter{}, token: "tok"}
}

func TestReviewPR_EligiblePR_SubmitsExactlyOneReview(t *testing.T) {
	client := newFakeReviewer()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Looks fine, one nit.", NumTurns: 3, CostUSD: 0.05}, nil
	}}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1", Title: "Add feature"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed {
		t.Fatalf("outcome = %+v, want Reviewed=true", outcome)
	}
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v, want nil", outcome.Err)
	}
	if cloneCalls.Load() != 1 {
		t.Errorf("clone called %d times, want 1", cloneCalls.Load())
	}
	if claude.callCount() != 1 {
		t.Errorf("claude called %d times, want 1", claude.callCount())
	}
	if client.submitCallCount() != 1 {
		t.Fatalf("SubmitPRReview called %d times, want exactly 1", client.submitCallCount())
	}
	call := client.submitCalls[0]
	if call.commitSHA != "sha1" || call.body != "Looks fine, one nit." {
		t.Errorf("submitCall = %+v", call)
	}
	if call.event != gh.ReviewEventComment {
		t.Errorf("submitCall.event = %+v, want ReviewEventComment (default Config has no threshold set)", call.event)
	}
	if outcome.NumTurns != 3 || outcome.CostUSD != 0.05 {
		t.Errorf("outcome NumTurns/CostUSD = %d/%v, want 3/0.05", outcome.NumTurns, outcome.CostUSD)
	}
}

func TestReviewPR_SeverityAboveThreshold_SubmitsRequestChanges(t *testing.T) {
	client := newFakeReviewer()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Found a real problem.\n\n```json\n" +
			`[{"path": "a.go", "line": 1, "body": "sql injection", "severity": "critical"}]` +
			"\n```\n"}, nil
	}}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{RequestChangesThreshold: SeverityHigh}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil", outcome)
	}
	call := client.submitCalls[0]
	if call.event != gh.ReviewEventRequestChanges {
		t.Errorf("submitCall.event = %+v, want ReviewEventRequestChanges (critical finding meets the high threshold)", call.event)
	}
}

func TestReviewPR_SeverityBelowThreshold_SubmitsComment(t *testing.T) {
	client := newFakeReviewer()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Minor nit.\n\n```json\n" +
			`[{"path": "a.go", "line": 1, "body": "consider renaming", "severity": "low"}]` +
			"\n```\n"}, nil
	}}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{RequestChangesThreshold: SeverityHigh}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil", outcome)
	}
	call := client.submitCalls[0]
	if call.event != gh.ReviewEventComment {
		t.Errorf("submitCall.event = %+v, want ReviewEventComment (low finding does not meet the high threshold)", call.event)
	}
}

func TestReviewPR_ToggleOff_CriticalFindingStillSubmitsComment(t *testing.T) {
	client := newFakeReviewer()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Found a real problem.\n\n```json\n" +
			`[{"path": "a.go", "line": 1, "body": "sql injection", "severity": "critical"}]` +
			"\n```\n"}, nil
	}}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil", outcome)
	}
	call := client.submitCalls[0]
	if call.event != gh.ReviewEventComment {
		t.Errorf("submitCall.event = %+v, want ReviewEventComment (toggle off means always COMMENT regardless of severity)", call.event)
	}
}

// TestReviewPR_FixedThenReReviewed_DoesNotReBlock proves the "a fixed-then-
// re-reviewed SHA does not re-block" acceptance criterion structurally:
// decideEvent has no memory across calls, so a clean re-review at a new SHA
// independently produces COMMENT even though the prior SHA's review at the
// same PR number requested changes. The actual unblock mechanism in
// production is GitHub's own stale-review dismissal on push (see
// cmd/pruefer/README.md) — this test only proves Pruefer's own decision
// carries no cross-call state that could interfere with that.
func TestReviewPR_FixedThenReReviewed_DoesNotReBlock(t *testing.T) {
	client := newFakeReviewer()
	call := 0
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		call++
		if call == 1 {
			return ReviewResult{Text: "Bad.\n\n```json\n" +
				`[{"path": "a.go", "line": 1, "body": "sql injection", "severity": "critical"}]` +
				"\n```\n"}, nil
		}
		return ReviewResult{Text: "Fixed.\n\n```json\n[]\n```\n"}, nil
	}}
	clone, _ := fakeClone(t, nil)
	cfg := Config{RequestChangesThreshold: SeverityHigh}

	pr1 := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome1 := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr1)
	if !outcome1.Reviewed || outcome1.Err != nil {
		t.Fatalf("first outcome = %+v, want Reviewed=true, Err=nil", outcome1)
	}

	pr2 := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha2"}
	outcome2 := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr2)
	if !outcome2.Reviewed || outcome2.Err != nil {
		t.Fatalf("second outcome = %+v, want Reviewed=true, Err=nil", outcome2)
	}

	if len(client.submitCalls) != 2 {
		t.Fatalf("submitCalls = %d, want 2", len(client.submitCalls))
	}
	if client.submitCalls[0].event != gh.ReviewEventRequestChanges {
		t.Errorf("submitCalls[0].event = %+v, want ReviewEventRequestChanges", client.submitCalls[0].event)
	}
	if client.submitCalls[1].event != gh.ReviewEventComment {
		t.Errorf("submitCalls[1].event = %+v, want ReviewEventComment (clean re-review at a new SHA must not inherit the prior block)", client.submitCalls[1].event)
	}
}

func TestReviewPR_PopulatesBaseBranchAndMaxWallTime(t *testing.T) {
	client := newFakeReviewer()
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1", BaseRef: "main"}
	cfg := Config{MaxWallTime: 10 * time.Minute}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed {
		t.Fatalf("outcome = %+v, want Reviewed=true", outcome)
	}
	calls := claude.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("claude called %d times, want 1", len(calls))
	}
	if calls[0].BaseBranch != "main" {
		t.Errorf("ReviewRequest.BaseBranch = %q, want %q", calls[0].BaseBranch, "main")
	}
	if calls[0].MaxWallTime != 10*time.Minute {
		t.Errorf("ReviewRequest.MaxWallTime = %v, want %v", calls[0].MaxWallTime, 10*time.Minute)
	}
}

func TestReviewPR_RepollSameSHA_DoesNotReReview(t *testing.T) {
	client := newFakeReviewer()
	client.reviews = []gh.PRReview{{Author: "pruefer-bot[bot]", CommitID: "sha1"}}
	claude := &mockClaudeInvoker{}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Skipped || outcome.Reason != SkipAlreadyReviewed {
		t.Fatalf("outcome = %+v, want Skipped with SkipAlreadyReviewed", outcome)
	}
	if cloneCalls.Load() != 0 {
		t.Error("expected no clone when the PR was already reviewed at this SHA")
	}
	if claude.callCount() != 0 {
		t.Error("expected no claude invocation when the PR was already reviewed at this SHA")
	}
	if client.submitCallCount() != 0 {
		t.Error("expected no review submission when the PR was already reviewed at this SHA")
	}
}

func TestReviewPR_DraftPR_Skipped(t *testing.T) {
	client := newFakeReviewer()
	claude := &mockClaudeInvoker{}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1", Draft: true}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Skipped || outcome.Reason != SkipDraft {
		t.Fatalf("outcome = %+v, want Skipped with SkipDraft", outcome)
	}
	if cloneCalls.Load() != 0 || claude.callCount() != 0 || client.submitCallCount() != 0 {
		t.Error("draft PR must not clone, invoke claude, or submit a review")
	}
	if client.diffCalls != 0 {
		t.Error("draft PR must not even fetch the diff (cheap checks run first)")
	}
}

func TestReviewPR_SelfAuthored_Skipped(t *testing.T) {
	client := newFakeReviewer()
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "pruefer-bot[bot]", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Skipped || outcome.Reason != SkipSelfAuthored {
		t.Fatalf("outcome = %+v, want Skipped with SkipSelfAuthored", outcome)
	}
	if claude.callCount() != 0 || client.submitCallCount() != 0 {
		t.Error("self-authored PR must not invoke claude or submit a review")
	}
}

func TestReviewPR_ForceReview_BypassesAlreadyReviewed(t *testing.T) {
	client := newFakeReviewer()
	client.reviews = []gh.PRReview{{Author: "pruefer-bot[bot]", CommitID: "sha1"}}
	client.comments = []gh.Comment{{DatabaseID: 42, Body: "/pruefer review"}}
	claude := &mockClaudeInvoker{}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed {
		t.Fatalf("outcome = %+v, want Reviewed=true (forced re-review)", outcome)
	}
	if cloneCalls.Load() != 1 || claude.callCount() != 1 || client.submitCallCount() != 1 {
		t.Error("forced re-review must clone, invoke claude, and submit exactly one review")
	}
	if !client.comments[0].HasReaction("ROCKET") {
		t.Error("expected the /pruefer review comment to be marked processed (ROCKET reaction)")
	}
}

func TestReviewPR_DiffTooLarge_Skipped(t *testing.T) {
	client := newFakeReviewer()
	client.diff = "x diff content"
	claude := &mockClaudeInvoker{}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{MaxDiffBytes: 5} // "x diff content" is well over 5 bytes
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Skipped || outcome.Reason != SkipDiffTooLarge {
		t.Fatalf("outcome = %+v, want Skipped with SkipDiffTooLarge", outcome)
	}
	if cloneCalls.Load() != 0 || claude.callCount() != 0 {
		t.Error("oversized diff must skip before cloning or invoking claude")
	}
}

func TestReviewPR_ExcludedPath_Skipped(t *testing.T) {
	client := newFakeReviewer()
	client.diff = "diff --git a/docs/readme.md b/docs/readme.md\n+change\n"
	claude := &mockClaudeInvoker{}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{ExcludedPaths: []string{"docs/*"}}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Skipped || outcome.Reason != SkipExcludedPath {
		t.Fatalf("outcome = %+v, want Skipped with SkipExcludedPath", outcome)
	}
	if cloneCalls.Load() != 0 || claude.callCount() != 0 {
		t.Error("excluded-path PR must skip before cloning or invoking claude")
	}
}

func TestReviewPR_ClaudeFailure_PostsNothing(t *testing.T) {
	client := newFakeReviewer()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{}, fmt.Errorf("claude crashed")
	}}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if outcome.Err == nil {
		t.Fatal("expected a non-nil error when claude invocation fails")
	}
	if cloneCalls.Load() != 1 {
		t.Errorf("expected exactly one clone attempt, got %d", cloneCalls.Load())
	}
	if client.submitCallCount() != 0 {
		t.Error("expected no review submission when claude invocation fails — post nothing, not a stub")
	}
}

func TestReviewPR_CloneFailure_PostsNothing(t *testing.T) {
	client := newFakeReviewer()
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, fmt.Errorf("clone failed"))

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if outcome.Err == nil {
		t.Fatal("expected a non-nil error when cloning fails")
	}
	if claude.callCount() != 0 {
		t.Error("expected no claude invocation when cloning fails")
	}
	if client.submitCallCount() != 0 {
		t.Error("expected no review submission when cloning fails")
	}
}

func TestReviewPR_SubmitFailure_ReturnsError(t *testing.T) {
	client := newFakeReviewer()
	client.submitErr = fmt.Errorf("422 already reviewed")
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if outcome.Err == nil {
		t.Fatal("expected a non-nil error when SubmitPRReview fails")
	}
	if outcome.Reviewed {
		t.Error("Reviewed must be false when submission failed")
	}
}

// TestReviewPR_DiffTooLarge_FallbackAlsoFails_SkippedNoErr pins acceptance
// criterion 2: a 406 too_large diff whose files-API fallback also fails must
// produce Skipped:true, Reason:SkipDiffTooLarge, Err:nil — the same terminal
// disposition as the existing max_diff_bytes skip, not the Err-returning
// path a pre-#1427 diff-fetch failure always took.
func TestReviewPR_DiffTooLarge_FallbackAlsoFails_SkippedNoErr(t *testing.T) {
	client := newFakeReviewer()
	client.diffErr = fmt.Errorf("406 too_large: %w", gh.ErrDiffTooLarge)
	client.filesErr = fmt.Errorf("files API also unavailable")
	claude := &mockClaudeInvoker{}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Skipped {
		t.Errorf("outcome.Skipped = false, want true")
	}
	if outcome.Reason != SkipDiffTooLarge {
		t.Errorf("outcome.Reason = %q, want %q", outcome.Reason, SkipDiffTooLarge)
	}
	if outcome.Err != nil {
		t.Errorf("outcome.Err = %v, want nil", outcome.Err)
	}
	if cloneCalls.Load() != 0 || claude.callCount() != 0 || client.submitCallCount() != 0 {
		t.Error("a terminal too-large skip must not clone, invoke claude, or submit a review")
	}
	if client.filesCallCount() != 1 {
		t.Errorf("filesCallCount = %d, want 1 (fallback must be attempted)", client.filesCallCount())
	}
}

// TestReviewPR_DiffTooLarge_FallbackAlsoFails_PostsNoticeOnce covers R4: the
// terminal skip must post exactly one idempotent PR notice, even across
// repeated ReviewPR calls for the same head SHA (e.g. repeated polls of a
// permanently-406 PR).
func TestReviewPR_DiffTooLarge_FallbackAlsoFails_PostsNoticeOnce(t *testing.T) {
	client := newFakeReviewer()
	client.diffErr = fmt.Errorf("406 too_large: %w", gh.ErrDiffTooLarge)
	client.filesErr = fmt.Errorf("files API also unavailable")
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	for i := 0; i < 3; i++ {
		outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)
		if !outcome.Skipped || outcome.Reason != SkipDiffTooLarge {
			t.Fatalf("call %d: outcome = %+v, want Skipped with SkipDiffTooLarge", i, outcome)
		}
	}
	if client.addCommentCount() != 1 {
		t.Fatalf("addCommentCount = %d, want exactly 1 across 3 polls of the same head SHA", client.addCommentCount())
	}
}

// TestReviewPR_DiffTooLarge_FallbackSucceeds_ReviewsUsingFallbackPaths pins
// acceptance criterion 3: when the files-API fallback succeeds, ReviewPR
// proceeds to invoke Claude and submit a review, and excluded_paths matching
// uses the fallback's path list (not an empty/absent changed-path set).
func TestReviewPR_DiffTooLarge_FallbackSucceeds_ReviewsUsingFallbackPaths(t *testing.T) {
	client := newFakeReviewer()
	client.diffErr = fmt.Errorf("406 too_large: %w", gh.ErrDiffTooLarge)
	client.filesResult = []string{"engine/claude.go", "engine/poll.go"}
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Reviewed via clone; diff was unavailable."}, nil
	}}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed {
		t.Fatalf("outcome = %+v, want Reviewed=true", outcome)
	}
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v, want nil", outcome.Err)
	}
	if cloneCalls.Load() != 1 || claude.callCount() != 1 || client.submitCallCount() != 1 {
		t.Error("a successful fallback must clone, invoke claude, and submit exactly one review")
	}
	if client.addCommentCount() != 0 {
		t.Error("a successful fallback must not post the diff-unavailable notice")
	}
}

// TestReviewPR_DiffTooLarge_FallbackSucceeds_ExcludedPathsSkip proves the
// fallback's path list — not an empty/unknown set — feeds excluded_paths
// matching: every fallback-reported path matching the exclusion glob must
// still skip the PR, exactly as it would for a normally-fetched diff.
func TestReviewPR_DiffTooLarge_FallbackSucceeds_ExcludedPathsSkip(t *testing.T) {
	client := newFakeReviewer()
	client.diffErr = fmt.Errorf("406 too_large: %w", gh.ErrDiffTooLarge)
	client.filesResult = []string{"docs/readme.md", "docs/faq.md"}
	claude := &mockClaudeInvoker{}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{ExcludedPaths: []string{"docs/*"}}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Skipped || outcome.Reason != SkipExcludedPath {
		t.Fatalf("outcome = %+v, want Skipped with SkipExcludedPath", outcome)
	}
	if cloneCalls.Load() != 0 || claude.callCount() != 0 {
		t.Error("fully-excluded fallback paths must skip before cloning or invoking claude")
	}
	if client.addCommentCount() != 0 {
		t.Error("an excluded-path skip is not a too-large terminal skip and must not post a notice")
	}
}

// TestReviewPR_GenericDiffFetchError_StillReturnsErr proves the classifier
// is narrow: a diff-fetch failure that is NOT gh.ErrDiffTooLarge (e.g. a
// transient network error) must keep today's Err-returning, naturally-
// retried-next-poll behavior — it must not be treated as a terminal skip.
func TestReviewPR_GenericDiffFetchError_StillReturnsErr(t *testing.T) {
	client := newFakeReviewer()
	client.diffErr = fmt.Errorf("context deadline exceeded")
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if outcome.Err == nil {
		t.Fatal("expected a non-nil Err for a generic (non-ErrDiffTooLarge) diff fetch failure")
	}
	if outcome.Skipped {
		t.Error("a generic diff fetch failure must not be classified as Skipped")
	}
	if client.filesCallCount() != 0 {
		t.Error("the files-API fallback must only be attempted for gh.ErrDiffTooLarge, not generic errors")
	}
}

// TestReviewPR_FetchedThreadsReachReviewRequest pins R1's data-plumbing
// chain: a thread returned by FetchPRReviewThreads must reach
// ReviewRequest.ReviewThreads unchanged.
func TestReviewPR_FetchedThreadsReachReviewRequest(t *testing.T) {
	client := newFakeReviewer()
	client.threads = []gh.PRReviewThread{
		{ID: "t1", Path: "a.go", Line: 10, IsResolved: false, Comments: []gh.PRReviewThreadComment{{Author: "reviewer", Body: "prior finding"}}},
	}
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil", outcome)
	}
	calls := claude.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("claude called %d times, want 1", len(calls))
	}
	if len(calls[0].ReviewThreads) != 1 || calls[0].ReviewThreads[0].ID != "t1" {
		t.Errorf("ReviewRequest.ReviewThreads = %+v, want the fetched thread", calls[0].ReviewThreads)
	}
}

// TestReviewPR_ThreadsTruncatedReachesReviewRequest pins the #1497 review
// finding's fix: FetchPRReviewThreads' fetch-layer truncation signal must
// reach ReviewRequest.ReviewThreadsTruncated unchanged, alongside the
// threads themselves, so buildReviewPrompt can note the incomplete fetch.
func TestReviewPR_ThreadsTruncatedReachesReviewRequest(t *testing.T) {
	client := newFakeReviewer()
	client.threads = []gh.PRReviewThread{
		{ID: "t1", Path: "a.go", Line: 10, Comments: []gh.PRReviewThreadComment{{Author: "reviewer", Body: "prior finding"}}},
	}
	client.threadsTruncated = true
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil", outcome)
	}
	calls := claude.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("claude called %d times, want 1", len(calls))
	}
	if !calls[0].ReviewThreadsTruncated {
		t.Error("ReviewRequest.ReviewThreadsTruncated = false, want true")
	}
}

// TestReviewPR_ThreadFetchError_DegradesWithoutFailingReview pins the
// non-fatal degrade decision: a FetchPRReviewThreads error must not fail the
// review — it proceeds with nil threads, mirroring today's cold-read
// behavior for that one pass.
func TestReviewPR_ThreadFetchError_DegradesWithoutFailingReview(t *testing.T) {
	client := newFakeReviewer()
	client.threadsErr = fmt.Errorf("graphql timeout")
	client.threadsTruncated = true // must not leak through despite the error
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed {
		t.Errorf("outcome.Reviewed = false, want true (a thread-fetch error must not fail the review)")
	}
	if outcome.Err != nil {
		t.Errorf("outcome.Err = %v, want nil", outcome.Err)
	}
	calls := claude.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("claude called %d times, want 1", len(calls))
	}
	if calls[0].ReviewThreads != nil {
		t.Errorf("ReviewRequest.ReviewThreads = %+v, want nil on fetch error", calls[0].ReviewThreads)
	}
	if calls[0].ReviewThreadsTruncated {
		t.Error("ReviewRequest.ReviewThreadsTruncated = true, want false on fetch error (must not leak the fake's stale value)")
	}
}

func TestReviewPR_ExcludedAuthor_Skipped(t *testing.T) {
	client := newFakeReviewer()
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "dependabot[bot]", HeadSHA: "sha1"}
	cfg := Config{ExcludedAuthors: []string{"dependabot[bot]"}}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Skipped || outcome.Reason != SkipExcludedAuthor {
		t.Fatalf("outcome = %+v, want Skipped with SkipExcludedAuthor", outcome)
	}
}

// reviewTestDiff is a small synthetic unified diff with two hunks: one
// adding a line at engine/claude.go:954 (a valid RIGHT-side anchor) and one
// touching only context around line 10 of docs/readme.md (so line 999 in
// that file is never a valid anchor).
const reviewTestDiff = `diff --git a/engine/claude.go b/engine/claude.go
index 1111111..2222222 100644
--- a/engine/claude.go
+++ b/engine/claude.go
@@ -952,2 +952,3 @@ func classify() {
 line952
 line953
+line954
diff --git a/docs/readme.md b/docs/readme.md
index 3333333..4444444 100644
--- a/docs/readme.md
+++ b/docs/readme.md
@@ -9,2 +9,2 @@
 line9
 line10
`

func TestReviewPR_FindingsMappedToChangedLines_PostsInlineComments(t *testing.T) {
	client := newFakeReviewer()
	client.diff = reviewTestDiff
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Reviewed the change. Looks solid.\n\n```json\n" +
			`[{"path": "engine/claude.go", "line": 954, "body": "consider a comment here"}]` +
			"\n```\n"}, nil
	}}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil", outcome)
	}
	if client.submitCallCount() != 1 {
		t.Fatalf("SubmitPRReview called %d times, want exactly 1", client.submitCallCount())
	}
	call := client.submitCalls[0]
	if len(call.comments) != 1 {
		t.Fatalf("submitted %d comments, want exactly 1", len(call.comments))
	}
	if call.comments[0].Path != "engine/claude.go" || call.comments[0].Line != 954 {
		t.Errorf("comment = %+v, want Path=engine/claude.go Line=954", call.comments[0])
	}
	if call.body != "Reviewed the change. Looks solid." {
		t.Errorf("body = %q, want the prose summary only (no demoted-findings section)", call.body)
	}
}

func TestReviewPR_UnanchorableFinding_DemotedToBody(t *testing.T) {
	client := newFakeReviewer()
	client.diff = reviewTestDiff
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Mixed bag.\n\n```json\n" +
			`[{"path": "engine/claude.go", "line": 954, "body": "anchorable"},` +
			`{"path": "docs/readme.md", "line": 999, "body": "not in the diff at all"}]` +
			"\n```\n"}, nil
	}}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil", outcome)
	}
	call := client.submitCalls[0]
	if len(call.comments) != 1 || call.comments[0].Line != 954 {
		t.Fatalf("comments = %+v, want exactly the anchorable finding at line 954", call.comments)
	}
	if !strings.Contains(call.body, "Additional findings") || !strings.Contains(call.body, "not in the diff at all") {
		t.Errorf("body = %q, want the unanchorable finding demoted into it under an explicit heading", call.body)
	}
}

func TestReviewPR_NoAnchorableFindings_BodyOnly(t *testing.T) {
	client := newFakeReviewer()
	client.diff = reviewTestDiff
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Looks fine, one nit."}, nil
	}}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil", outcome)
	}
	call := client.submitCalls[0]
	if len(call.comments) != 0 {
		t.Errorf("comments = %+v, want none (plain-prose review, no fenced findings)", call.comments)
	}
	if call.body != "Looks fine, one nit." {
		t.Errorf("body = %q, want the unmodified prose", call.body)
	}
}

func TestReviewPR_UnanchorableFinding_NeverPassedToSubmit(t *testing.T) {
	// Guards the 422 risk at the unit level: a finding whose (path, line) is
	// not a valid RIGHT-side anchor in the PR's diff must never reach
	// SubmitPRReview's comments argument, since GitHub rejects the entire
	// review if any one comment's anchor is invalid.
	client := newFakeReviewer()
	client.diff = reviewTestDiff
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Findings below.\n\n```json\n" +
			`[{"path": "engine/claude.go", "line": 12345, "body": "line does not exist in this diff"}]` +
			"\n```\n"}, nil
	}}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil (unanchorable finding must not fail the review)", outcome)
	}
	call := client.submitCalls[0]
	for _, c := range call.comments {
		if c.Line == 12345 {
			t.Fatalf("unanchorable finding at line 12345 was passed to SubmitPRReview's comments: %+v", call.comments)
		}
	}
	if len(call.comments) != 0 {
		t.Errorf("comments = %+v, want none", call.comments)
	}
	if !strings.Contains(call.body, "line does not exist in this diff") {
		t.Errorf("body = %q, want the unanchorable finding demoted into it", call.body)
	}
}
