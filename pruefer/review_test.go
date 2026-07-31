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
	if f.reviewsErr != nil {
		return nil, f.reviewsErr
	}
	return f.reviews, nil
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

// TestReviewPR_ForceReview_AllPathsExcluded_MarksProcessed is the
// SkipExcludedPath analog of TestReviewPR_ForceReview_StillTooLarge_
// MarksProcessedWithoutDuplicateNotice: a forced "/pruefer review" against a
// PR whose every touched path matches excluded_paths must still mark the
// command processed, not just acknowledged — otherwise there is no notice
// (unlike the too-large path) to serve as a durable "nothing to do here"
// signal, and the command would be re-acknowledged and re-skipped on every
// poll forever.
func TestReviewPR_ForceReview_AllPathsExcluded_MarksProcessed(t *testing.T) {
	client := newFakeReviewer()
	client.diff = "diff --git a/docs/readme.md b/docs/readme.md\n+change\n"
	client.comments = []gh.Comment{{DatabaseID: 1, Body: "/pruefer review"}}
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
	reviewComment := client.comments[0]
	if !reviewComment.HasReaction("EYES") || !reviewComment.HasReaction("ROCKET") {
		t.Errorf("review command reactions = %+v, want both EYES and ROCKET (seen and processed, even though nothing was reviewable)", reviewComment.Reactions)
	}
}

// oversizedDiff builds a two-file diff: a small, always-reviewable
// engine/claude.go change, plus a "corpus" file whose body alone is
// bloatBytes long — standing in for the issue's 17 MB JSONL corpus.
func oversizedDiff(bloatBytes int) string {
	small := `diff --git a/engine/claude.go b/engine/claude.go
index 1111111..2222222 100644
--- a/engine/claude.go
+++ b/engine/claude.go
@@ -952,2 +952,3 @@ func classify() {
 line952
 line953
+line954
`
	corpus := "diff --git a/corpus/data.jsonl b/corpus/data.jsonl\n" +
		"index 3333333..4444444 100644\n" +
		"--- a/corpus/data.jsonl\n" +
		"+++ b/corpus/data.jsonl\n" +
		"@@ -1,1 +1,1 @@\n" +
		"-old\n" +
		"+" + strings.Repeat("x", bloatBytes) + "\n"
	return small + corpus
}

// TestReviewPR_ExcessEntirelyInExcludedPath_ReviewsNotSkipped is the
// FR-1 acceptance case: a diff whose over-cap bytes are entirely inside a
// configured excluded_paths glob must be reviewed, not skipped — the
// operator's escape hatch must be reachable, not dead code shadowed by a
// whole-diff size measurement running first.
func TestReviewPR_ExcessEntirelyInExcludedPath_ReviewsNotSkipped(t *testing.T) {
	client := newFakeReviewer()
	client.diff = oversizedDiff(10_000)
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Reviewed the small change."}, nil
	}}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{MaxDiffBytes: 1000, ExcludedPaths: []string{"corpus/**"}}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil (excess is entirely in an excluded path)", outcome)
	}
	if cloneCalls.Load() != 1 || claude.callCount() != 1 {
		t.Errorf("clone/claude calls = %d/%d, want 1/1", cloneCalls.Load(), claude.callCount())
	}
	if client.addCommentCallCount() != 0 {
		t.Error("expected no too-large notice — the diff reviews cleanly once the corpus file is excluded")
	}
	calls := claude.callsSnapshot()
	if len(calls) != 1 || len(calls[0].ExcludedPaths) != 1 || calls[0].ExcludedPaths[0] != "corpus/data.jsonl" {
		t.Errorf("ReviewRequest.ExcludedPaths = %v, want [corpus/data.jsonl]", calls[0].ExcludedPaths)
	}
	call := client.submitCalls[0]
	if !strings.Contains(call.body, "Omitted from this review") || !strings.Contains(call.body, "corpus/data.jsonl") {
		t.Errorf("submitted body = %q, want an Omitted-from-this-review section naming corpus/data.jsonl", call.body)
	}
}

// TestReviewPR_OverCap_PostsOneNoticeAndSkip proves FR-2/FR-3: an over-cap
// diff with nothing configured to exclude it gets exactly one PR comment
// naming the size, cap, and dominant path, and Skipped/SkipDiffTooLarge.
func TestReviewPR_OverCap_PostsOneNoticeAndSkip(t *testing.T) {
	client := newFakeReviewer()
	client.diff = oversizedDiff(10_000)
	claude := &mockClaudeInvoker{}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	// Cap so small even the small file plus preamble can't fit once the
	// corpus file is dropped by FR-5's trim — forces the notice path.
	cfg := Config{MaxDiffBytes: 50}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Skipped || outcome.Reason != SkipDiffTooLarge {
		t.Fatalf("outcome = %+v, want Skipped with SkipDiffTooLarge", outcome)
	}
	if outcome.SizeDetail == nil {
		t.Fatal("outcome.SizeDetail = nil, want a populated detail")
	}
	if outcome.SizeDetail.MaxBytes != 50 {
		t.Errorf("SizeDetail.MaxBytes = %d, want 50", outcome.SizeDetail.MaxBytes)
	}
	if cloneCalls.Load() != 0 || claude.callCount() != 0 {
		t.Error("over-cap PR must skip before cloning or invoking claude")
	}
	if client.addCommentCallCount() != 1 {
		t.Fatalf("AddComment called %d times, want exactly 1", client.addCommentCallCount())
	}
	notice := client.addedCalls[0].body
	if !strings.Contains(notice, "corpus/data.jsonl") {
		t.Errorf("notice = %q, want the dominant path named", notice)
	}
	if !strings.Contains(notice, diffTooLargeMarker("sha1")) {
		t.Errorf("notice = %q, want the per-SHA idempotency marker embedded", notice)
	}
}

// TestReviewPR_OverCap_CommentsFetchFailed_DoesNotPostNotice covers a PR
// review finding: if FetchIssueComments fails, ReviewPR must not treat that
// failure as "no notice exists yet" when deciding whether to post one — a
// transient fetch error must never risk a duplicate too-large notice for the
// same head SHA. The over-cap skip itself still happens (that determination
// doesn't depend on comments), but no comment is posted this poll.
func TestReviewPR_OverCap_CommentsFetchFailed_DoesNotPostNotice(t *testing.T) {
	client := newFakeReviewer()
	client.diff = oversizedDiff(10_000)
	client.fetchErr = fmt.Errorf("transient GitHub API error")
	claude := &mockClaudeInvoker{}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{MaxDiffBytes: 50}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Skipped || outcome.Reason != SkipDiffTooLarge {
		t.Fatalf("outcome = %+v, want Skipped with SkipDiffTooLarge", outcome)
	}
	if outcome.SizeDetail == nil {
		t.Fatal("outcome.SizeDetail = nil, want a populated detail")
	}
	if client.addCommentCallCount() != 0 {
		t.Errorf("AddComment called %d times, want 0 — a failed comments fetch must not be treated as proof no notice exists", client.addCommentCallCount())
	}
	if cloneCalls.Load() != 0 || claude.callCount() != 0 {
		t.Error("over-cap PR must skip before cloning or invoking claude")
	}
}

// TestReviewPR_OverCap_RepolledSameSHA_DoesNotDuplicateNotice covers FR-4:
// once a too-large notice exists for a head SHA, a later poll must not
// re-fetch the diff, re-measure, or post a second notice.
func TestReviewPR_OverCap_RepolledSameSHA_DoesNotDuplicateNotice(t *testing.T) {
	client := newFakeReviewer()
	client.diff = oversizedDiff(10_000)
	claude := &mockClaudeInvoker{}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{MaxDiffBytes: 50}

	first := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)
	if !first.Skipped || first.Reason != SkipDiffTooLarge {
		t.Fatalf("first outcome = %+v, want Skipped with SkipDiffTooLarge", first)
	}
	if client.addCommentCallCount() != 1 {
		t.Fatalf("after first poll, AddComment called %d times, want 1", client.addCommentCallCount())
	}
	firstDiffCalls := client.diffCallCount()

	second := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)
	if !second.Skipped || second.Reason != SkipDiffTooLarge {
		t.Fatalf("second outcome = %+v, want Skipped with SkipDiffTooLarge", second)
	}
	if client.addCommentCallCount() != 1 {
		t.Errorf("after second poll, AddComment called %d times, want still 1 (no duplicate notice)", client.addCommentCallCount())
	}
	if client.diffCallCount() != firstDiffCalls {
		t.Errorf("second poll fetched the diff again (calls %d -> %d) — FR-4 requires recognizing the existing notice before FetchPRDiff", firstDiffCalls, client.diffCallCount())
	}
	if cloneCalls.Load() != 0 || claude.callCount() != 0 {
		t.Error("neither poll should clone or invoke claude for a diff that never fits")
	}
}

// TestReviewPR_ForceReview_BypassesExistingTooLargeNotice_ReviewsWhenFixed
// covers a PR review finding on this feature: an operator who fixes
// excluded_paths (or max_diff_bytes) after a too-large notice has already
// fired, then comments "/pruefer review" to ask for a re-check, must get a
// genuine fresh measurement — not an immediate skip against the stale
// notice, which would silently strand the request exactly like the
// "indistinguishable from still reviewing" failure mode this feature exists
// to eliminate.
func TestReviewPR_ForceReview_BypassesExistingTooLargeNotice_ReviewsWhenFixed(t *testing.T) {
	client := newFakeReviewer()
	client.diff = oversizedDiff(10_000)
	client.comments = []gh.Comment{
		{DatabaseID: 1, Body: buildTooLargeNoticeBody(DiffSizeDetail{MeasuredBytes: 20_000, MaxBytes: 1000}, "sha1")},
		{DatabaseID: 2, Body: "/pruefer review"},
	}
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Reviewed after config fix."}, nil
	}}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{MaxDiffBytes: 1000, ExcludedPaths: []string{"corpus/**"}} // operator fixed the config
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true (forced re-check after config fix)", outcome)
	}
	if client.diffCallCount() != 1 {
		t.Errorf("diff fetched %d times, want 1 — forceReview must bypass the stale notice and re-measure", client.diffCallCount())
	}
	if cloneCalls.Load() != 1 || claude.callCount() != 1 {
		t.Errorf("clone/claude calls = %d/%d, want 1/1", cloneCalls.Load(), claude.callCount())
	}
	reviewComment := client.comments[1]
	if !reviewComment.HasReaction("EYES") || !reviewComment.HasReaction("ROCKET") {
		t.Errorf("review command reactions = %+v, want both EYES and ROCKET", reviewComment.Reactions)
	}
}

// TestReviewPR_ForceReview_StillTooLarge_MarksProcessedWithoutDuplicateNotice
// is the other half of the same finding: if a forced re-check still finds
// the diff over cap, ReviewPR must not post a second notice comment for the
// same head SHA (FR-2's "exactly one notice per head SHA" holds even under
// a forced re-check), but must still mark the "/pruefer review" command
// processed so the operator gets feedback the notice still stands, and the
// command isn't retried forever.
func TestReviewPR_ForceReview_StillTooLarge_MarksProcessedWithoutDuplicateNotice(t *testing.T) {
	client := newFakeReviewer()
	client.diff = oversizedDiff(10_000)
	client.comments = []gh.Comment{
		{DatabaseID: 1, Body: buildTooLargeNoticeBody(DiffSizeDetail{MeasuredBytes: 20_000, MaxBytes: 50}, "sha1")},
		{DatabaseID: 2, Body: "/pruefer review"},
	}
	claude := &mockClaudeInvoker{}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{MaxDiffBytes: 50} // still too small — the forced re-check finds the same verdict
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Skipped || outcome.Reason != SkipDiffTooLarge {
		t.Fatalf("outcome = %+v, want Skipped with SkipDiffTooLarge", outcome)
	}
	if client.diffCallCount() != 1 {
		t.Errorf("diff fetched %d times, want 1 — forceReview must still trigger a fresh measurement", client.diffCallCount())
	}
	if client.addCommentCallCount() != 0 {
		t.Errorf("AddComment called %d times, want 0 — must not duplicate the existing notice for this head SHA", client.addCommentCallCount())
	}
	if cloneCalls.Load() != 0 || claude.callCount() != 0 {
		t.Error("still-too-large diff must not clone or invoke claude")
	}
	reviewComment := client.comments[1]
	if !reviewComment.HasReaction("EYES") || !reviewComment.HasReaction("ROCKET") {
		t.Errorf("review command reactions = %+v, want both EYES and ROCKET (seen and processed, even though the verdict didn't change)", reviewComment.Reactions)
	}
}

// TestReviewPR_TrimsLargestFileAndReviewsRest is the FR-5 best-effort case:
// no excluded_paths configured, but dropping the single oversized file
// brings the rest under cap, so ReviewPR reviews the remainder and reports
// the dropped file as omitted rather than skipping wholesale.
func TestReviewPR_TrimsLargestFileAndReviewsRest(t *testing.T) {
	client := newFakeReviewer()
	client.diff = oversizedDiff(10_000)
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Reviewed what fit."}, nil
	}}
	clone, cloneCalls := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	// Small file diff block is ~140 bytes; cap sits between that and the
	// combined (small+corpus) total, so trimming the corpus file alone fits.
	cfg := Config{MaxDiffBytes: 500}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want Reviewed=true, Err=nil (trimming the oversized file should let the rest review)", outcome)
	}
	if cloneCalls.Load() != 1 || claude.callCount() != 1 {
		t.Errorf("clone/claude calls = %d/%d, want 1/1", cloneCalls.Load(), claude.callCount())
	}
	if client.addCommentCallCount() != 0 {
		t.Error("expected no too-large notice — FR-5's trim succeeded")
	}
	calls := claude.callsSnapshot()
	if len(calls) != 1 || len(calls[0].ExcludedPaths) != 1 || calls[0].ExcludedPaths[0] != "corpus/data.jsonl" {
		t.Errorf("ReviewRequest.ExcludedPaths = %v, want [corpus/data.jsonl] (auto-dropped)", calls[0].ExcludedPaths)
	}
	call := client.submitCalls[0]
	if !strings.Contains(call.body, "Omitted from this review") || !strings.Contains(call.body, "corpus/data.jsonl") {
		t.Errorf("submitted body = %q, want an Omitted-from-this-review section naming the auto-dropped file", call.body)
	}
}

// TestReviewPR_TrimExhausted_StillOverCap_NoticesAndSkips covers the case
// where even dropping every file leaves the diff over cap (e.g. the
// remaining "small" content, plus diff overhead, is itself larger than the
// configured cap) — FR-5 is explicitly best-effort, not a replacement for
// the notice path.
func TestReviewPR_TrimExhausted_StillOverCap_NoticesAndSkips(t *testing.T) {
	client := newFakeReviewer()
	client.diff = oversizedDiff(10_000)
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{MaxDiffBytes: 10} // even the small file's diff block alone exceeds this
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr)

	if !outcome.Skipped || outcome.Reason != SkipDiffTooLarge {
		t.Fatalf("outcome = %+v, want Skipped with SkipDiffTooLarge", outcome)
	}
	if claude.callCount() != 0 {
		t.Error("expected no claude invocation when trimming cannot bring the diff under cap")
	}
	if client.addCommentCallCount() != 1 {
		t.Errorf("AddComment called %d times, want exactly 1", client.addCommentCallCount())
	}
	if outcome.SizeDetail == nil || len(outcome.SizeDetail.TrimAttempted) == 0 {
		t.Fatalf("outcome.SizeDetail = %+v, want TrimAttempted populated — a trim was actually attempted here", outcome.SizeDetail)
	}
	notice := client.addedCalls[0].body
	if !strings.Contains(notice, "also tried automatically dropping") {
		t.Errorf("notice = %q, want it to say Pruefer already tried auto-dropping the largest file(s)", notice)
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
