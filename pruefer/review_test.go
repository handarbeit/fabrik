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

// TestReviewPR_DiffTooLarge_Skipped covers the pathological-exhaustion case:
// "x diff content" has no "diff --git" header at all, so splitDiffFiles
// puts every byte into the unattributed preamble — nothing to exclude,
// nothing to trim away. Per trimToFit's contract, an oversized preamble can
// never be trimmed to fit, so this still hits the terminal SkipDiffTooLarge
// disposition even after the R1-R4 gate reorder/widening — see
// TestReviewPR_HugeExcludedFile_RemainderReviewed and
// TestReviewPR_SizeOnlyTrim_NoExcludedPaths_ReviewsSurvivors below for the
// realistic multi-file cases those changes actually widen.
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
	if client.addCommentCount() != 1 {
		t.Errorf("addCommentCount = %d, want 1 (a genuine too-large skip must post the notice, R5)", client.addCommentCount())
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

// --- #1462: exclude-before-measure, per-file exclusion, trim-to-fit ---

// hugeExcludedFileDiff builds a two-file diff: one enormous file
// (data/corpus.jsonl, the fantasy#1640 shape) and one small, genuinely
// reviewable file (pkg/code.go). The huge file alone pushes the raw total
// well past any of this file's test max_diff_bytes values; the small file
// alone is comfortably under all of them.
func hugeExcludedFileDiff() string {
	huge := strings.Repeat("x", 5000)
	return "diff --git a/data/corpus.jsonl b/data/corpus.jsonl\n" +
		"--- a/data/corpus.jsonl\n" +
		"+++ b/data/corpus.jsonl\n" +
		"@@ -1,1 +1,1 @@\n" +
		"-old\n" +
		"+" + huge + "\n" +
		"diff --git a/pkg/code.go b/pkg/code.go\n" +
		"--- a/pkg/code.go\n" +
		"+++ b/pkg/code.go\n" +
		"@@ -1,2 +1,3 @@\n" +
		" line1\n" +
		" line2\n" +
		"+line3\n"
}

// threeFileDiffOneVendored builds a three-file diff: one excludable vendor
// file and two ordinary source files, for proving R2's partial (not
// all-or-nothing) exclusion.
func threeFileDiffOneVendored() string {
	return "diff --git a/vendor/lib.go b/vendor/lib.go\n" +
		"--- a/vendor/lib.go\n" +
		"+++ b/vendor/lib.go\n" +
		"@@ -1,1 +1,1 @@\n" +
		"-old\n" +
		"+new\n" +
		"diff --git a/pkg/a.go b/pkg/a.go\n" +
		"--- a/pkg/a.go\n" +
		"+++ b/pkg/a.go\n" +
		"@@ -1,1 +1,1 @@\n" +
		"-old\n" +
		"+new\n" +
		"diff --git a/pkg/b.go b/pkg/b.go\n" +
		"--- a/pkg/b.go\n" +
		"+++ b/pkg/b.go\n" +
		"@@ -1,1 +1,1 @@\n" +
		"-old\n" +
		"+new\n"
}

// twoDocsFileDiff builds a two-file diff where both files match a single
// exclusion glob, for proving R3's terminal all-excluded disposition still
// applies once exclusion is per-file rather than all-or-nothing.
func twoDocsFileDiff() string {
	return "diff --git a/docs/a.md b/docs/a.md\n" +
		"--- a/docs/a.md\n" +
		"+++ b/docs/a.md\n" +
		"@@ -1,1 +1,1 @@\n" +
		"-old\n" +
		"+new\n" +
		"diff --git a/docs/b.md b/docs/b.md\n" +
		"--- a/docs/b.md\n" +
		"+++ b/docs/b.md\n" +
		"@@ -1,1 +1,1 @@\n" +
		"-old\n" +
		"+new\n"
}

// threeFileExcludeSmallTrimHuge builds a three-file diff where one small
// file is excluded by glob and a separate huge file survives exclusion but
// must still be trimmed to fit — proving exclusion and trimming can both
// apply to the same diff, at different files, in the same pass.
func threeFileExcludeSmallTrimHuge() string {
	huge := strings.Repeat("x", 5000)
	return "diff --git a/vendor/small.go b/vendor/small.go\n" +
		"--- a/vendor/small.go\n" +
		"+++ b/vendor/small.go\n" +
		"@@ -1,1 +1,1 @@\n" +
		"-old\n" +
		"+new\n" +
		"diff --git a/huge.jsonl b/huge.jsonl\n" +
		"--- a/huge.jsonl\n" +
		"+++ b/huge.jsonl\n" +
		"@@ -1,1 +1,1 @@\n" +
		"-old\n" +
		"+" + huge + "\n" +
		"diff --git a/pkg/code.go b/pkg/code.go\n" +
		"--- a/pkg/code.go\n" +
		"+++ b/pkg/code.go\n" +
		"@@ -1,2 +1,3 @@\n" +
		" line1\n" +
		" line2\n" +
		"+line3\n"
}

// hugeNoExclusionDiff mirrors hugeExcludedFileDiff's shape but with an
// ordinary (not glob-excludable-by-convention) huge file, for proving
// trim-to-fit engages on size alone with no excluded_paths configured.
func hugeNoExclusionDiff() string {
	huge := strings.Repeat("x", 5000)
	return "diff --git a/huge.jsonl b/huge.jsonl\n" +
		"--- a/huge.jsonl\n" +
		"+++ b/huge.jsonl\n" +
		"@@ -1,1 +1,1 @@\n" +
		"-old\n" +
		"+" + huge + "\n" +
		"diff --git a/pkg/code.go b/pkg/code.go\n" +
		"--- a/pkg/code.go\n" +
		"+++ b/pkg/code.go\n" +
		"@@ -1,2 +1,3 @@\n" +
		" line1\n" +
		" line2\n" +
		"+line3\n"
}

// TestReviewPR_HugeExcludedFile_RemainderReviewed is the direct regression
// test for the fantasy#1640 scenario the issue is filed against (AC1): a
// diff whose raw total exceeds max_diff_bytes purely because of one
// enormous excluded file must be reviewed on its non-excluded remainder,
// not skipped outright. Before the #1462 gate reorder, the size gate ran
// before exclusion and this PR would have hit SkipDiffTooLarge regardless
// of excluded_paths — verified by temporarily reverting review.go to the
// pre-#1462 gate order and re-running this test, which fails as expected
// (outcome.Skipped=true, Reason=SkipDiffTooLarge) confirming AC8's
// non-vacuousness requirement.
func TestReviewPR_HugeExcludedFile_RemainderReviewed(t *testing.T) {
	client := newFakeReviewer()
	client.diff = hugeExcludedFileDiff()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Reviewed the small file; the corpus file was excluded."}, nil
	}}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{MaxDiffBytes: 1000, ExcludedPaths: []string{"data/corpus.jsonl"}}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed {
		t.Fatalf("outcome = %+v, want Reviewed=true — the excluded huge file must not suppress review of the rest (AC1)", outcome)
	}
	if outcome.Err != nil {
		t.Fatalf("outcome.Err = %v, want nil", outcome.Err)
	}
	if cloneCalls.Load() != 1 {
		t.Errorf("cloneCalls = %d, want 1", cloneCalls.Load())
	}
	calls := claude.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("claude called %d times, want 1", len(calls))
	}
	if len(calls[0].OmittedExcludedPaths) != 1 || calls[0].OmittedExcludedPaths[0] != "data/corpus.jsonl" {
		t.Errorf("OmittedExcludedPaths = %+v, want [data/corpus.jsonl]", calls[0].OmittedExcludedPaths)
	}
	if len(calls[0].OmittedTrimmedPaths) != 0 {
		t.Errorf("OmittedTrimmedPaths = %+v, want none — exclusion alone brought the diff under budget", calls[0].OmittedTrimmedPaths)
	}
}

// TestReviewPR_PartialExclusion_ReviewsSurvivors pins AC2: a diff with one
// excluded and two non-excluded files yields a review covering exactly the
// two — no max_diff_bytes involvement at all, isolating R2 from R4.
func TestReviewPR_PartialExclusion_ReviewsSurvivors(t *testing.T) {
	client := newFakeReviewer()
	client.diff = threeFileDiffOneVendored()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Reviewed a.go and b.go."}, nil
	}}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{ExcludedPaths: []string{"vendor/**"}}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil (AC2)", outcome)
	}
	calls := claude.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("claude called %d times, want 1", len(calls))
	}
	if len(calls[0].OmittedExcludedPaths) != 1 || calls[0].OmittedExcludedPaths[0] != "vendor/lib.go" {
		t.Errorf("OmittedExcludedPaths = %+v, want [vendor/lib.go]", calls[0].OmittedExcludedPaths)
	}
	if len(calls[0].OmittedTrimmedPaths) != 0 {
		t.Errorf("OmittedTrimmedPaths = %+v, want none (no size gate in play)", calls[0].OmittedTrimmedPaths)
	}
}

// TestReviewPR_AllPathsExcluded_MultiFile_StillSkips pins AC3: a
// multi-file diff where every path matches an exclusion glob still returns
// SkipExcludedPath, asserted on the outcome — not inferred from logs — even
// though exclusion is now per-file rather than all-or-nothing.
func TestReviewPR_AllPathsExcluded_MultiFile_StillSkips(t *testing.T) {
	client := newFakeReviewer()
	client.diff = twoDocsFileDiff()
	claude := &mockClaudeInvoker{}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{ExcludedPaths: []string{"docs/*"}}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Skipped || outcome.Reason != SkipExcludedPath {
		t.Fatalf("outcome = %+v, want Skipped with SkipExcludedPath (AC3)", outcome)
	}
	if cloneCalls.Load() != 0 || claude.callCount() != 0 {
		t.Error("all-excluded multi-file diff must skip before cloning or invoking claude")
	}
}

// TestReviewPR_SizeOnlyTrim_NoExcludedPaths_ReviewsSurvivors proves
// trim-to-fit engages on size alone: no excluded_paths configured, one huge
// file forces the diff over max_diff_bytes, and trimming it away lets the
// remainder be reviewed rather than skipping the whole PR.
func TestReviewPR_SizeOnlyTrim_NoExcludedPaths_ReviewsSurvivors(t *testing.T) {
	client := newFakeReviewer()
	client.diff = hugeNoExclusionDiff()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Reviewed the small file only."}, nil
	}}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{MaxDiffBytes: 1000} // no ExcludedPaths at all
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil (size-only trim, no excluded_paths configured)", outcome)
	}
	calls := claude.callsSnapshot()
	if len(calls[0].OmittedExcludedPaths) != 0 {
		t.Errorf("OmittedExcludedPaths = %+v, want none (no excluded_paths configured)", calls[0].OmittedExcludedPaths)
	}
	if len(calls[0].OmittedTrimmedPaths) != 1 || calls[0].OmittedTrimmedPaths[0] != "huge.jsonl" {
		t.Errorf("OmittedTrimmedPaths = %+v, want [huge.jsonl]", calls[0].OmittedTrimmedPaths)
	}
}

// TestReviewPR_OmittedPaths_DisclosedInPromptAndNotice pins AC4: when files
// are dropped, both the prompt the model receives and the posted notice
// name them.
func TestReviewPR_OmittedPaths_DisclosedInPromptAndNotice(t *testing.T) {
	client := newFakeReviewer()
	client.diff = hugeExcludedFileDiff()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Reviewed."}, nil
	}}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{MaxDiffBytes: 1000, ExcludedPaths: []string{"data/corpus.jsonl"}}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed {
		t.Fatalf("outcome = %+v, want Reviewed=true", outcome)
	}
	calls := claude.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("claude called %d times, want 1", len(calls))
	}
	prompt := buildReviewPrompt(calls[0])
	if !strings.Contains(prompt, "data/corpus.jsonl") {
		t.Errorf("prompt does not name the omitted file, prompt = %s", prompt)
	}
	if client.addCommentCount() != 1 {
		t.Fatalf("addCommentCount = %d, want 1 (a genuinely oversized diff must post the too-large notice, R5)", client.addCommentCount())
	}
	if !strings.Contains(client.addedBodies[0], "data/corpus.jsonl") {
		t.Errorf("notice body does not name the omitted file: %s", client.addedBodies[0])
	}
}

// TestReviewPR_CrossPathNoticeIdempotency pins AC5: at most one notice
// exists per head SHA across both the diff-unavailable (#1427) and
// diff-too-large-after-fetch (#1462) paths. A PR that hits the
// diff-unavailable path on one poll and the diff-too-large-after-fetch path
// on a later poll (still the same head SHA) must never accumulate two
// notices.
func TestReviewPR_CrossPathNoticeIdempotency(t *testing.T) {
	client := newFakeReviewer()
	client.diffErr = fmt.Errorf("406 too_large: %w", gh.ErrDiffTooLarge)
	client.filesErr = fmt.Errorf("files API also unavailable")
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome1 := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr)
	if !outcome1.Skipped || outcome1.Reason != SkipDiffTooLarge {
		t.Fatalf("first outcome = %+v, want Skipped SkipDiffTooLarge (diff-unavailable path)", outcome1)
	}
	if client.addCommentCount() != 1 {
		t.Fatalf("after first call, addCommentCount = %d, want 1", client.addCommentCount())
	}

	// Second poll: GitHub now renders the diff (still oversized, nothing
	// left after the pathological-exhaustion path), same head SHA — the
	// diff-too-large-after-fetch path must recognize the diff-unavailable
	// notice's marker and post nothing further.
	client.diffErr = nil
	client.diff = "x diff content that is definitely over the cap for this test"
	cfg := Config{MaxDiffBytes: 5}
	outcome2 := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)
	if !outcome2.Skipped || outcome2.Reason != SkipDiffTooLarge {
		t.Fatalf("second outcome = %+v, want Skipped SkipDiffTooLarge (diff-too-large-after-fetch path)", outcome2)
	}
	if client.addCommentCount() != 1 {
		t.Errorf("after second call, addCommentCount = %d, want still 1 (R5: at most one notice per head SHA across both paths)", client.addCommentCount())
	}
}

// TestReviewPR_FindingOnExcludedFile_DemotedNotAnchored pins AC6: a finding
// Claude reports on an excluded file's line can never anchor as an inline
// comment, since review.go rebinds diff to the filtered subset before
// validRightAnchors runs — it must demote into the review body like any
// other unanchorable finding, not fail the submission.
func TestReviewPR_FindingOnExcludedFile_DemotedNotAnchored(t *testing.T) {
	client := newFakeReviewer()
	client.diff = threeFileDiffOneVendored()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Findings below.\n\n```json\n" +
			`[{"path": "vendor/lib.go", "line": 1, "body": "should not be anchored, it was excluded"}]` +
			"\n```\n"}, nil
	}}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{ExcludedPaths: []string{"vendor/**"}}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil", outcome)
	}
	call := client.submitCalls[0]
	if len(call.comments) != 0 {
		t.Fatalf("comments = %+v, want none — a finding on an excluded file must never anchor inline (AC6)", call.comments)
	}
	if !strings.Contains(call.body, "should not be anchored") {
		t.Errorf("body = %q, want the finding demoted into it", call.body)
	}
}

// TestReviewPR_SizeSkip_LogsSelfDiagnosingMessage pins R7/AC10: a
// max_diff_bytes skip must report the post-exclusion measured size, an
// excluded-count-out-of-total, and explicitly name the "no excluded_paths
// configured" case — not the pre-R7 message, which was byte-identical
// whether or not exclusion was configured or did anything.
func TestReviewPR_SizeSkip_LogsSelfDiagnosingMessage(t *testing.T) {
	resetLogf(t)
	var lines []string
	Logf = func(prNumber int, tag, format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	client := newFakeReviewer()
	client.diff = "x diff content"
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{MaxDiffBytes: 5}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)
	if !outcome.Skipped || outcome.Reason != SkipDiffTooLarge {
		t.Fatalf("outcome = %+v, want Skipped SkipDiffTooLarge", outcome)
	}

	var skipLine string
	for _, l := range lines {
		if strings.Contains(l, "skipping") && strings.Contains(l, "max_diff_bytes") {
			skipLine = l
		}
	}
	if skipLine == "" {
		t.Fatalf("no size-related skip log line found among: %v", lines)
	}
	if !strings.Contains(skipLine, "after excluding 0 of 0 files") {
		t.Errorf("log line %q does not report the excluded-count-out-of-total", skipLine)
	}
	if !strings.Contains(skipLine, "no excluded_paths configured") {
		t.Errorf("log line %q does not explicitly name the zero-excluded-configured case", skipLine)
	}
	if !strings.Contains(skipLine, "exceeds max_diff_bytes=5") {
		t.Errorf("log line %q does not report the configured max_diff_bytes", skipLine)
	}
}

// TestReviewPR_TrimSuccess_LogsSelfDiagnosingMessage pins R7/AC10 for the
// trim-and-review (not skip) case: a successful trim must also report the
// post-exclusion size and excluded-count-out-of-total, distinguishing a
// "trimming" log line from a "skipping" one.
func TestReviewPR_TrimSuccess_LogsSelfDiagnosingMessage(t *testing.T) {
	resetLogf(t)
	var lines []string
	Logf = func(prNumber int, tag, format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	client := newFakeReviewer()
	client.diff = threeFileExcludeSmallTrimHuge()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Reviewed."}, nil
	}}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{MaxDiffBytes: 1000, ExcludedPaths: []string{"vendor/**"}}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)
	if !outcome.Reviewed {
		t.Fatalf("outcome = %+v, want Reviewed=true", outcome)
	}
	calls := claude.callsSnapshot()
	if len(calls[0].OmittedExcludedPaths) != 1 || calls[0].OmittedExcludedPaths[0] != "vendor/small.go" {
		t.Fatalf("OmittedExcludedPaths = %+v, want [vendor/small.go]", calls[0].OmittedExcludedPaths)
	}
	if len(calls[0].OmittedTrimmedPaths) != 1 || calls[0].OmittedTrimmedPaths[0] != "huge.jsonl" {
		t.Fatalf("OmittedTrimmedPaths = %+v, want [huge.jsonl]", calls[0].OmittedTrimmedPaths)
	}

	var trimLine string
	for _, l := range lines {
		if strings.Contains(l, "trimming") {
			trimLine = l
		}
	}
	if trimLine == "" {
		t.Fatalf("no trimming log line found among: %v", lines)
	}
	if !strings.Contains(trimLine, "after excluding 1 of 3 files") {
		t.Errorf("log line %q does not report the excluded-count-out-of-total", trimLine)
	}
	if !strings.Contains(trimLine, "excluded_paths=[vendor/**]") {
		t.Errorf("log line %q does not name the configured excluded_paths", trimLine)
	}
}
