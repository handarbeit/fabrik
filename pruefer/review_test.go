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

	diff       string
	diffErr    error
	reviews    []gh.PRReview
	reviewsErr error
	submitErr  error
	token      string
	botLogin   string

	mu          sync.Mutex
	submitCalls []submitCall
	diffCalls   int
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

func (f *fakeReviewer) FetchPRReviews(owner, repo string, prNumber int) ([]gh.PRReview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reviewsErr != nil {
		return nil, f.reviewsErr
	}
	return f.reviews, nil
}

// SubmitPRReview records the call and — mirroring real GitHub, where a
// submitted review is immediately visible to a subsequent FetchPRReviews —
// also appends it to f.reviews under botLogin. This is what makes ReviewPR's
// own SHA-idempotency (alreadyReviewedAtHead) a genuine backstop in tests,
// not just in production: without it, two concurrent ReviewPR calls for the
// same PR/SHA (e.g. a duplicate event racing an in-flight review) would both
// see an empty review list and both submit, regardless of any caller-side
// locking.
func (f *fakeReviewer) SubmitPRReview(owner, repo string, prNumber int, commitSHA, body string, event gh.ReviewEvent, comments []gh.ReviewComment) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return 0, f.submitErr
	}
	f.submitCalls = append(f.submitCalls, submitCall{owner, repo, prNumber, commitSHA, body, event, comments})
	f.reviews = append(f.reviews, gh.PRReview{Author: f.botLogin, CommitID: commitSHA, State: "COMMENTED"})
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
	return &fakeReviewer{fakeCommenter: &fakeCommenter{}, token: "tok", botLogin: "pruefer-bot[bot]"}
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
