package pruefer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// fakeReviewer implements GitHubReviewer in-memory for review.go's tests.
type fakeReviewer struct {
	*fakeCommenter

	diff       string
	diffErr    error
	reviews    []gh.PRReview
	reviewsErr error
	submitErr  error
	token      string

	mu          sync.Mutex
	submitCalls []submitCall
	diffCalls   int
}

type submitCall struct {
	owner, repo string
	prNumber    int
	commitSHA   string
	body        string
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

func (f *fakeReviewer) FetchPRReviews(owner, repo string, prNumber int) ([]gh.PRReview, error) {
	if f.reviewsErr != nil {
		return nil, f.reviewsErr
	}
	return f.reviews, nil
}

func (f *fakeReviewer) SubmitPRReview(owner, repo string, prNumber int, commitSHA, body string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return 0, f.submitErr
	}
	f.submitCalls = append(f.submitCalls, submitCall{owner, repo, prNumber, commitSHA, body})
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
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (string, error) {
		return "Looks fine, one nit.", nil
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
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (string, error) {
		return "", fmt.Errorf("claude crashed")
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
