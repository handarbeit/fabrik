package boardcache

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/internal/itemstate"
)

// ---------------------------------------------------------------------------
// Mock ReadClient for testing
// ---------------------------------------------------------------------------

type mockClient struct {
	fetchItemDetailsCount     int
	fetchCheckRunsCount       int
	fetchLinkedPRCount        int
	fetchLabelsCount          int
	fetchPRClosingIssuesCount int
	fetchPRsForSHACount       int
	fetchProjectItemCount     int

	itemDetailsResult  *gh.ProjectItem
	checkRunsResult    []gh.CheckRun
	linkedPRResult     *gh.PRDetails
	labelsResult       []string
	projectBoardResult *gh.ProjectBoard
	projectItemResult  *gh.ProjectItem

	fetchPRClosingIssuesFn func(owner, repo string, prNumber int) ([]int, error)
	fetchPRsForSHAFn       func(owner, repo, sha string) ([]int, error)
	fetchProjectItemFn     func(owner, repo string, issueNumber int) (*gh.ProjectItem, error)
}

func (m *mockClient) FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
	if m.projectBoardResult != nil {
		return m.projectBoardResult, nil
	}
	return &gh.ProjectBoard{ProjectID: "PID", Items: nil}, nil
}

func (m *mockClient) FetchItemDetails(item *gh.ProjectItem) error {
	m.fetchItemDetailsCount++
	if m.itemDetailsResult != nil {
		// Mirror the real FetchItemDetails behaviour: populate Number and Repo only
		// when the caller hasn't already set them (e.g. projects_v2_item.created path
		// passes a bare ProjectItem{ID: nodeID} with Number=0 and Repo="").
		if item.Number == 0 && m.itemDetailsResult.Number > 0 {
			item.Number = m.itemDetailsResult.Number
		}
		if item.Repo == "" && m.itemDetailsResult.Repo != "" {
			item.Repo = m.itemDetailsResult.Repo
		}
		item.Body = m.itemDetailsResult.Body
		item.Comments = cloneComments(m.itemDetailsResult.Comments)
		item.LinkedPRNumber = m.itemDetailsResult.LinkedPRNumber
		item.LinkedPRReviews = clonePRReviews(m.itemDetailsResult.LinkedPRReviews)
		item.LinkedPRReviewRequests = cloneReviewRequests(m.itemDetailsResult.LinkedPRReviewRequests)
		item.LinkedPRReviewThreadComments = cloneComments(m.itemDetailsResult.LinkedPRReviewThreadComments)
		item.LinkedPRResolvedThreadCount = m.itemDetailsResult.LinkedPRResolvedThreadCount
		item.Author = m.itemDetailsResult.Author
		item.Assignees = cloneStrings(m.itemDetailsResult.Assignees)
		item.BlockedBy = cloneDependencies(m.itemDetailsResult.BlockedBy)
	}
	return nil
}

func (m *mockClient) FetchCheckRuns(owner, repo, sha string) ([]gh.CheckRun, error) {
	m.fetchCheckRunsCount++
	return m.checkRunsResult, nil
}

func (m *mockClient) FetchLinkedPR(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
	m.fetchLinkedPRCount++
	return m.linkedPRResult, nil
}

func (m *mockClient) FetchPRMergeable(owner, repo string, prNumber int) (*bool, error) {
	return nil, nil
}

func (m *mockClient) FetchPRMergeableState(owner, repo string, prNumber int) (string, error) {
	return "", nil
}

func (m *mockClient) FetchLabels(owner, repo string, issueNumber int) ([]string, error) {
	m.fetchLabelsCount++
	return m.labelsResult, nil
}

func (m *mockClient) FetchStatusField(projectID string) (*gh.StatusField, error) {
	return &gh.StatusField{FieldID: "SF1", Options: map[string]string{"Research": "opt1"}}, nil
}

func (m *mockClient) RateLimitStats() (rest, graphql gh.RateLimitStats) {
	return gh.RateLimitStats{}, gh.RateLimitStats{}
}

func (m *mockClient) FetchPRClosingIssues(owner, repo string, prNumber int) ([]int, error) {
	m.fetchPRClosingIssuesCount++
	if m.fetchPRClosingIssuesFn != nil {
		return m.fetchPRClosingIssuesFn(owner, repo, prNumber)
	}
	return nil, nil
}

func (m *mockClient) FetchProjectItem(owner, repo string, issueNumber int) (*gh.ProjectItem, error) {
	m.fetchProjectItemCount++
	if m.fetchProjectItemFn != nil {
		return m.fetchProjectItemFn(owner, repo, issueNumber)
	}
	if m.projectItemResult != nil {
		cp := *m.projectItemResult
		return &cp, nil
	}
	return nil, nil
}

func (m *mockClient) FetchPRsForSHA(owner, repo, sha string) ([]int, error) {
	m.fetchPRsForSHACount++
	if m.fetchPRsForSHAFn != nil {
		return m.fetchPRsForSHAFn(owner, repo, sha)
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Test helpers — Store-based accessors
// ---------------------------------------------------------------------------

var nopLog = func(format string, args ...any) {}

// testGetState returns a deep-copy of the ItemState for (repo, number).
// Fails the test immediately if not found.
func testGetState(t *testing.T, c *CacheImpl, repo string, number int) itemstate.ItemState {
	t.Helper()
	snap, err := c.store.Get(repo, number)
	if err != nil {
		t.Fatalf("testGetState: %s#%d: %v", repo, number, err)
	}
	return snap.State()
}

// testSetDeepFetched marks an item as deep-fetched in the Store by applying
// an ItemDeepFetched mutation with the item's current shallow state.
func testSetDeepFetched(c *CacheImpl, repo string, number int) {
	snap, err := c.store.Get(repo, number)
	if err != nil {
		return
	}
	pi := snapshotToProjectItem(snap)
	c.store.Apply(itemstate.ItemDeepFetched{Repo: repo, Number: number, FreshState: pi})
}

// testIsDeepFetched returns whether the item has a non-zero LastDeepFetchAt.
func testIsDeepFetched(c *CacheImpl, repo string, number int) bool {
	snap, err := c.store.Get(repo, number)
	if err != nil {
		return false
	}
	return !snap.State().LastDeepFetchAt.IsZero()
}

// testSetLinkedPR sets up item (repo, issNum) as linked to prNum in the Store
// so delta handlers can route events by PR number via store.GetByPRKey.
func testSetLinkedPR(c *CacheImpl, repo string, issNum, prNum int) {
	c.store.Apply(itemstate.PRHeadSHAUpdated{Repo: repo, Number: issNum, LinkedPRNum: prNum})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func seedCache(t *testing.T) *CacheImpl {
	t.Helper()
	c := NewCacheImpl(&mockClient{}, itemstate.NewStore(nil), nopLog)
	board := &gh.ProjectBoard{
		ProjectID: "PID",
		Title:     "Test Board",
		OwnerType: "organization",
		Items: []gh.ProjectItem{
			{
				ID:     "I_001",
				ItemID: "PVTI_001",
				Number: 1,
				Title:  "Issue One",
				Repo:   "owner/repo",
				Status: "Research",
				Labels: []string{"enhancement"},
			},
			{
				ID:     "I_002",
				ItemID: "PVTI_002",
				Number: 2,
				Title:  "Issue Two",
				Repo:   "owner/repo",
				Status: "Plan",
			},
		},
	}
	c.Bootstrap(board)
	return c
}

// ---------------------------------------------------------------------------
// Bootstrap / FetchProjectBoard tests
// ---------------------------------------------------------------------------

func TestBootstrapPopulatesItems(t *testing.T) {
	c := seedCache(t)
	board, err := c.FetchProjectBoard("owner", "repo", 1, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(board.Items) != 2 {
		t.Errorf("want 2 items, got %d", len(board.Items))
	}
}

// TestBootstrapNotifiesObserver verifies that Bootstrap fires observer notifications
// for every item in the board, matching production ordering in engine/poll.go where
// observers are subscribed before Bootstrap is called.
func TestBootstrapNotifiesObserver(t *testing.T) {
	store := itemstate.NewStore(nil)

	var mu sync.Mutex
	var received []itemstate.Change
	// Subscribe BEFORE Bootstrap — matching production ordering in engine/poll.go.
	store.Subscribe(itemstate.ObserverFunc(func(c itemstate.Change, _ itemstate.Snapshot) {
		mu.Lock()
		received = append(received, c)
		mu.Unlock()
	}))

	mc := &mockClient{}
	c := NewCacheImpl(mc, store, nopLog)

	board := &gh.ProjectBoard{
		ProjectID: "PID",
		Title:     "T",
		OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_1", Repo: "owner/repo", Number: 1, Status: "Research", Title: "Issue 1"},
			{ID: "I_2", Repo: "owner/repo", Number: 2, Status: "Implement", Title: "Issue 2"},
		},
	}
	c.Bootstrap(board)

	mu.Lock()
	got := len(received)
	mu.Unlock()

	if got != 2 {
		t.Fatalf("expected 2 observer Changes after Bootstrap, got %d", got)
	}
	for _, ch := range received {
		if ch.Fields == 0 {
			t.Errorf("Change for #%d has zero Fields; want non-zero", ch.Number)
		}
	}
}

func TestNewCacheImplNilStorePanic(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewCacheImpl(adapter, nil, logFn) should have panicked")
		}
		msg, ok := r.(string)
		if !ok || msg != "boardcache.NewCacheImpl: store must not be nil" {
			t.Errorf("unexpected panic value: %v", r)
		}
	}()
	NewCacheImpl(&mockClient{}, nil, nopLog)
}

func TestFetchProjectBoardFallsBackWhenEmpty(t *testing.T) {
	mc := &mockClient{projectBoardResult: &gh.ProjectBoard{
		ProjectID: "PID", Items: []gh.ProjectItem{{Number: 5, Repo: "o/r"}},
	}}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	board, err := c.FetchProjectBoard("o", "r", 1, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(board.Items) != 1 || board.Items[0].Number != 5 {
		t.Error("expected fallback board item #5")
	}
}

// ---------------------------------------------------------------------------
// FetchItemDetails — cache miss → fallback → populate
// ---------------------------------------------------------------------------

func TestFetchItemDetailsFallbackPopulatesCache(t *testing.T) {
	mc := &mockClient{
		itemDetailsResult: &gh.ProjectItem{
			Body:           "body text",
			Author:         "alice",
			LinkedPRNumber: 99,
			Comments: []gh.Comment{
				{ID: "C1", DatabaseID: 1, Author: "bob", Body: "hello"},
			},
		},
	}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	board := &gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Research"}},
	}
	c.Bootstrap(board)

	item := gh.ProjectItem{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Research"}
	if err := c.FetchItemDetails(&item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}
	if item.Body != "body text" {
		t.Errorf("want body 'body text', got %q", item.Body)
	}
	if len(item.Comments) != 1 {
		t.Errorf("want 1 comment, got %d", len(item.Comments))
	}
	if item.LinkedPRNumber != 99 {
		t.Errorf("want LinkedPRNumber 99, got %d", item.LinkedPRNumber)
	}
	if mc.fetchItemDetailsCount != 1 {
		t.Errorf("expected exactly 1 fallback call, got %d", mc.fetchItemDetailsCount)
	}

	// Second call — must be served from cache without another fallback call.
	item2 := gh.ProjectItem{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Research"}
	if err := c.FetchItemDetails(&item2); err != nil {
		t.Fatalf("FetchItemDetails second: %v", err)
	}
	if mc.fetchItemDetailsCount != 1 {
		t.Errorf("expected no additional fallback calls, got %d", mc.fetchItemDetailsCount)
	}
	if item2.Body != "body text" {
		t.Errorf("second call: want body 'body text', got %q", item2.Body)
	}
}

// ---------------------------------------------------------------------------
// Delta: issue_comment.created — idempotent by DatabaseID
// ---------------------------------------------------------------------------

func issueCommentPayloadJSON(action, repo string, issueNum int, nodeID string, dbID int, body, user string) []byte {
	p := issueCommentPayload{Action: action}
	p.Repository.FullName = repo
	p.Issue.Number = issueNum
	p.Comment.NodeID = nodeID
	p.Comment.DatabaseID = dbID
	p.Comment.Body = body
	p.Comment.User.Login = user
	p.Comment.CreatedAt = time.Now().Format(time.RFC3339)
	b, _ := json.Marshal(p)
	return b
}

func TestDeltaIssueCommentCreated(t *testing.T) {
	c := seedCache(t)

	payload := issueCommentPayloadJSON("created", "owner/repo", 1, "C_abc", 100, "test comment", "alice")
	c.ApplyDelta("issue_comment", payload)

	s := testGetState(t, c, "owner/repo", 1)
	if len(s.Comments) != 1 || s.Comments[0].ID != "C_abc" {
		t.Errorf("expected comment C_abc, got %+v", s.Comments)
	}
}

func TestDeltaIssueCommentCreatedIdempotent(t *testing.T) {
	c := seedCache(t)

	payload := issueCommentPayloadJSON("created", "owner/repo", 1, "C_abc", 100, "test", "alice")
	c.ApplyDelta("issue_comment", payload)
	c.ApplyDelta("issue_comment", payload) // duplicate

	s := testGetState(t, c, "owner/repo", 1)
	if len(s.Comments) != 1 {
		t.Errorf("expected exactly 1 comment after duplicate, got %d", len(s.Comments))
	}
}

// ---------------------------------------------------------------------------
// Delta: issues.labeled / unlabeled — set membership
// ---------------------------------------------------------------------------

func issuesLabeledPayloadJSON(action, repo string, issueNum int, label string) []byte {
	p := issuesPayload{Action: action}
	p.Repository.FullName = repo
	p.Issue.Number = issueNum
	p.Label.Name = label
	b, _ := json.Marshal(p)
	return b
}

func TestDeltaIssuesLabeled(t *testing.T) {
	c := seedCache(t)
	payload := issuesLabeledPayloadJSON("labeled", "owner/repo", 1, "bug")
	c.ApplyDelta("issues", payload)

	s := testGetState(t, c, "owner/repo", 1)
	labels := cloneStrings(s.Labels)

	found := false
	for _, l := range labels {
		if l == "bug" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected label 'bug', got %v", labels)
	}
}

func TestDeltaIssuesLabeledIdempotent(t *testing.T) {
	c := seedCache(t)
	payload := issuesLabeledPayloadJSON("labeled", "owner/repo", 1, "enhancement")
	c.ApplyDelta("issues", payload)
	c.ApplyDelta("issues", payload) // add same label twice

	s := testGetState(t, c, "owner/repo", 1)
	count := 0
	for _, l := range s.Labels {
		if l == "enhancement" {
			count++
		}
	}

	if count != 1 {
		t.Errorf("label 'enhancement' should appear exactly once, got %d", count)
	}
}

func TestDeltaIssuesUnlabeled(t *testing.T) {
	c := seedCache(t)
	// Seed has "enhancement" on item #1.
	payload := issuesLabeledPayloadJSON("unlabeled", "owner/repo", 1, "enhancement")
	c.ApplyDelta("issues", payload)

	s := testGetState(t, c, "owner/repo", 1)
	for _, l := range s.Labels {
		if l == "enhancement" {
			t.Error("label 'enhancement' should have been removed")
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Delta: pull_request — update linkedPRs and SHA index in Store
// ---------------------------------------------------------------------------

func pullRequestPayloadJSON(action, repo string, prNum int, sha, state string, merged, draft bool) []byte {
	p := pullRequestPayload{Action: action}
	p.Repository.FullName = repo
	p.PullRequest.Number = prNum
	p.PullRequest.Title = "Test PR Title"
	p.PullRequest.Head.SHA = sha
	p.PullRequest.State = state
	p.PullRequest.Merged = merged
	p.PullRequest.Draft = draft
	b, _ := json.Marshal(p)
	return b
}

func TestDeltaPullRequestOpened(t *testing.T) {
	c := seedCache(t)
	// Set item #1 as linked to PR #42 so the delta handler routes correctly.
	testSetLinkedPR(c, "owner/repo", 1, 42)

	payload := pullRequestPayloadJSON("opened", "owner/repo", 42, "sha123", "open", false, false)
	c.ApplyDelta("pull_request", payload)

	s := testGetState(t, c, "owner/repo", 1)
	if s.LinkedPR == nil || s.LinkedPR.Number != 42 {
		t.Fatalf("expected LinkedPR.Number=42 after pull_request.opened, got %v", s.LinkedPR)
	}
	if s.LinkedPR.HeadSHA != "sha123" {
		t.Errorf("expected HeadSHA sha123, got %q", s.LinkedPR.HeadSHA)
	}
	if s.LinkedPR.Title == "" {
		t.Errorf("expected non-empty LinkedPR.Title after pull_request.opened")
	}
}

func TestDeltaPullRequestSynchronizeUpdatesShaTKey(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)

	// Initial SHA.
	c.ApplyDelta("pull_request", pullRequestPayloadJSON("opened", "owner/repo", 42, "sha_old", "open", false, false))
	// New push — SHA changes.
	c.ApplyDelta("pull_request", pullRequestPayloadJSON("synchronize", "owner/repo", 42, "sha_new", "open", false, false))

	s := testGetState(t, c, "owner/repo", 1)
	if s.LinkedPR != nil && s.LinkedPR.HeadSHA == "sha_old" {
		t.Error("old SHA should have been replaced in item's LinkedPR.HeadSHA")
	}
	if s.LinkedPR == nil || s.LinkedPR.HeadSHA != "sha_new" {
		t.Error("new SHA should be stored in item's LinkedPR.HeadSHA")
	}
}

// ---------------------------------------------------------------------------
// Delta: pull_request_review.submitted — upsert by DatabaseID
// ---------------------------------------------------------------------------

func pullRequestReviewPayloadJSON(action, repo string, prNum, reviewID int, state, login string) []byte {
	p := pullRequestReviewPayload{Action: action}
	p.Repository.FullName = repo
	p.PullRequest.Number = prNum
	p.Review.DatabaseID = reviewID
	p.Review.State = state
	p.Review.User.Login = login
	b, _ := json.Marshal(p)
	return b
}

func TestDeltaPullRequestReviewSubmitted(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)

	payload := pullRequestReviewPayloadJSON("submitted", "owner/repo", 42, 1001, "approved", "alice")
	c.ApplyDelta("pull_request_review", payload)

	s := testGetState(t, c, "owner/repo", 1)
	var reviews []gh.PRReview
	if s.LinkedPR != nil {
		reviews = clonePRReviews(s.LinkedPR.Reviews)
	}

	if len(reviews) != 1 || reviews[0].Author != "alice" || reviews[0].State != "APPROVED" {
		t.Errorf("unexpected reviews: %+v", reviews)
	}
}

func TestDeltaPullRequestReviewSubmittedUpsert(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)

	// First review: changes_requested.
	c.ApplyDelta("pull_request_review", pullRequestReviewPayloadJSON("submitted", "owner/repo", 42, 1001, "changes_requested", "alice"))
	// Second review with same ID: approve (author re-reviews).
	c.ApplyDelta("pull_request_review", pullRequestReviewPayloadJSON("submitted", "owner/repo", 42, 1001, "approved", "alice"))

	s := testGetState(t, c, "owner/repo", 1)
	var reviews []gh.PRReview
	if s.LinkedPR != nil {
		reviews = clonePRReviews(s.LinkedPR.Reviews)
	}

	if len(reviews) != 1 {
		t.Errorf("upsert: expected 1 review after re-review, got %d", len(reviews))
	}
	if len(reviews) > 0 && reviews[0].State != "APPROVED" {
		t.Errorf("expected APPROVED state after upsert, got %q", reviews[0].State)
	}
}

// ---------------------------------------------------------------------------
// Delta: pull_request_review_comment.created — idempotent by NodeID
// ---------------------------------------------------------------------------

func pullRequestReviewCommentPayloadJSON(action, repo string, prNum, dbID int, nodeID, body, user string) []byte {
	p := pullRequestReviewCommentPayload{Action: action}
	p.Repository.FullName = repo
	p.PullRequest.Number = prNum
	p.Comment.NodeID = nodeID
	p.Comment.DatabaseID = dbID
	p.Comment.Body = body
	p.Comment.User.Login = user
	p.Comment.CreatedAt = time.Now().Format(time.RFC3339)
	p.Comment.DiffHunk = "@@ -1,3 +1,4 @@"
	p.Comment.Path = "main.go"
	b, _ := json.Marshal(p)
	return b
}

func TestDeltaPullRequestReviewCommentCreated(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)

	payload := pullRequestReviewCommentPayloadJSON("created", "owner/repo", 42, 200, "RC_node_1", "looks good", "bob")
	c.ApplyDelta("pull_request_review_comment", payload)

	s := testGetState(t, c, "owner/repo", 1)
	var comments []gh.Comment
	if s.LinkedPR != nil {
		comments = cloneComments(s.LinkedPR.ThreadComments)
	}

	if len(comments) != 1 || comments[0].ID != "RC_node_1" || comments[0].Author != "bob" {
		t.Errorf("unexpected review thread comments: %+v", comments)
	}
}

func TestDeltaPullRequestReviewCommentCreatedIdempotent(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)

	payload := pullRequestReviewCommentPayloadJSON("created", "owner/repo", 42, 200, "RC_node_1", "looks good", "bob")
	c.ApplyDelta("pull_request_review_comment", payload)
	c.ApplyDelta("pull_request_review_comment", payload) // duplicate

	s := testGetState(t, c, "owner/repo", 1)
	var count int
	if s.LinkedPR != nil {
		count = len(s.LinkedPR.ThreadComments)
	}

	if count != 1 {
		t.Errorf("expected exactly 1 review thread comment after duplicate, got %d", count)
	}
}

// ---------------------------------------------------------------------------
// Delta: check_run.completed — upsert by ID
// ---------------------------------------------------------------------------

func checkRunPayloadJSON(action, repo string, id int64, name, status, conclusion, sha string) []byte {
	p := checkRunPayload{Action: action}
	p.Repository.FullName = repo
	p.CheckRun.ID = id
	p.CheckRun.Name = name
	p.CheckRun.Status = status
	p.CheckRun.Conclusion = conclusion
	p.CheckRun.HeadSHA = sha
	b, _ := json.Marshal(p)
	return b
}

func TestDeltaCheckRunCompleted(t *testing.T) {
	c := seedCache(t)
	payload := checkRunPayloadJSON("completed", "owner/repo", 9001, "build", "completed", "success", "sha_abc")
	c.ApplyDelta("check_run", payload)

	c.mu.RLock()
	runs := c.checkRuns["sha_abc"]
	c.mu.RUnlock()

	if len(runs) != 1 || runs[0].ID != 9001 || runs[0].Conclusion != "success" {
		t.Errorf("unexpected check runs: %+v", runs)
	}
}

func TestDeltaCheckRunUpsertById(t *testing.T) {
	c := seedCache(t)
	// First run: failure.
	c.ApplyDelta("check_run", checkRunPayloadJSON("completed", "owner/repo", 9001, "build", "completed", "failure", "sha_abc"))
	// Same ID: success (re-run).
	c.ApplyDelta("check_run", checkRunPayloadJSON("completed", "owner/repo", 9001, "build", "completed", "success", "sha_abc"))

	c.mu.RLock()
	runs := c.checkRuns["sha_abc"]
	c.mu.RUnlock()

	if len(runs) != 1 {
		t.Errorf("upsert: expected 1 run after re-run, got %d", len(runs))
	}
	if len(runs) > 0 && runs[0].Conclusion != "success" {
		t.Errorf("expected success after upsert, got %q", runs[0].Conclusion)
	}
}

// ---------------------------------------------------------------------------
// Delta: projects_v2_item.edited — updates Status via ItemID
// ---------------------------------------------------------------------------

func projectsV2ItemPayloadJSON(action, itemID, statusName string) []byte {
	p := projectsV2ItemPayload{Action: action}
	p.ProjectsV2Item.ID = itemID
	p.Changes.FieldValue.FieldType = "single_select"
	p.Changes.FieldValue.To.Name = statusName
	b, _ := json.Marshal(p)
	return b
}

func TestDeltaProjectsV2ItemEdited(t *testing.T) {
	c := seedCache(t)
	// Item #1 has ItemID "PVTI_001", status "Research".
	payload := projectsV2ItemPayloadJSON("edited", "PVTI_001", "Plan")
	c.ApplyDelta("projects_v2_item", payload)

	s := testGetState(t, c, "owner/repo", 1)
	if s.Status != "Plan" {
		t.Errorf("expected status 'Plan', got %q", s.Status)
	}
}

// ---------------------------------------------------------------------------
// FetchCheckRuns — cache miss → fallback → populate
// ---------------------------------------------------------------------------

func TestFetchCheckRunsFallback(t *testing.T) {
	mc := &mockClient{checkRunsResult: []gh.CheckRun{{ID: 42, Name: "test", Status: "completed", Conclusion: "success"}}}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)

	runs, err := c.FetchCheckRuns("owner", "repo", "sha_xyz")
	if err != nil {
		t.Fatalf("FetchCheckRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != 42 {
		t.Errorf("unexpected runs: %+v", runs)
	}
	if mc.fetchCheckRunsCount != 1 {
		t.Errorf("expected 1 fallback call, got %d", mc.fetchCheckRunsCount)
	}

	// Second call — from cache.
	runs2, err := c.FetchCheckRuns("owner", "repo", "sha_xyz")
	if err != nil {
		t.Fatalf("FetchCheckRuns second: %v", err)
	}
	if mc.fetchCheckRunsCount != 1 {
		t.Errorf("expected no additional fallback, got %d", mc.fetchCheckRunsCount)
	}
	if len(runs2) != 1 {
		t.Errorf("expected 1 cached run, got %d", len(runs2))
	}
}

// ---------------------------------------------------------------------------
// FetchLabels — from cache
// ---------------------------------------------------------------------------

func TestFetchLabelsFromCache(t *testing.T) {
	c := seedCache(t)
	labels, err := c.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if len(labels) != 1 || labels[0] != "enhancement" {
		t.Errorf("unexpected labels: %v", labels)
	}
}

func TestFetchLabelsFallbackOnMiss(t *testing.T) {
	mc := &mockClient{labelsResult: []string{"foo"}}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	// No bootstrap — empty cache.
	labels, err := c.FetchLabels("owner", "repo", 99)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if mc.fetchLabelsCount != 1 {
		t.Errorf("expected 1 fallback call, got %d", mc.fetchLabelsCount)
	}
	if len(labels) != 1 || labels[0] != "foo" {
		t.Errorf("unexpected labels: %v", labels)
	}
}

// ---------------------------------------------------------------------------
// Reconcile — wholesale replace, drift logging
// ---------------------------------------------------------------------------

func TestReconcileReplacesShallowData(t *testing.T) {
	var logBuf strings.Builder
	logFn := func(format string, args ...any) {
		logBuf.WriteString(format)
	}
	c := NewCacheImpl(&mockClient{}, itemstate.NewStore(nil), logFn)
	c.Bootstrap(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Research"},
		},
	})

	// Reconcile with updated status.
	c.Reconcile(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Plan"},
		},
	})

	s := testGetState(t, c, "owner/repo", 1)
	if s.Status != "Plan" {
		t.Errorf("expected status 'Plan' after reconcile, got %q", s.Status)
	}
	if !strings.Contains(logBuf.String(), "reconciliation") {
		t.Errorf("expected [reconciliation] log, got: %q", logBuf.String())
	}
}

func TestReconcilePreservesDeepFields(t *testing.T) {
	c := seedCache(t)
	// Set comments and mark as deep-fetched via ItemDeepFetched mutation.
	snap, _ := c.store.Get("owner/repo", 1)
	pi := snapshotToProjectItem(snap)
	pi.Comments = []gh.Comment{{ID: "C1", Body: "preserved"}}
	c.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: pi})

	c.Reconcile(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Plan"},
		},
	})

	s := testGetState(t, c, "owner/repo", 1)
	preserved := len(s.Comments) == 1 && s.Comments[0].ID == "C1"
	deepFetched := !s.LastDeepFetchAt.IsZero()

	if !preserved {
		t.Error("expected deep-fetched comments to be preserved after reconcile")
	}
	if !deepFetched {
		t.Error("expected LastDeepFetchAt to be preserved after reconcile")
	}
}

func TestReconcileLinkageDriftInvalidatesDeepCache(t *testing.T) {
	mc := &mockClient{
		itemDetailsResult: &gh.ProjectItem{
			LinkedPRNumber: 502,
		},
	}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	c.Bootstrap(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Research",
				LinkedPRNumber: 0, LinkedPRNumberShallow: 0},
		},
	})

	// Mark as deep-fetched with LinkedPRNumber=0 (linkage not yet visible on GitHub).
	testSetDeepFetched(c, "owner/repo", 1)

	// Board now shows LinkedPRNumberShallow=502 — linkage has materialized.
	c.Reconcile(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Research",
				LinkedPRNumberShallow: 502},
		},
	})

	// LastDeepFetchAt must be zeroed so next FetchItemDetails hits GitHub.
	if testIsDeepFetched(c, "owner/repo", 1) {
		t.Error("expected deep cache to be invalidated when linkage drift is detected")
	}

	// Next FetchItemDetails call must fall through to the mock (cache miss).
	beforeCount := mc.fetchItemDetailsCount
	item := &gh.ProjectItem{Number: 1, Repo: "owner/repo", ID: "I_001"}
	if err := c.FetchItemDetails(item); err != nil {
		t.Fatalf("FetchItemDetails returned error: %v", err)
	}
	if mc.fetchItemDetailsCount <= beforeCount {
		t.Error("expected FetchItemDetails to call fallback after deep cache was invalidated")
	}
	// The mock should have populated LinkedPRNumber=502 into item.
	if item.LinkedPRNumber != 502 {
		t.Errorf("expected LinkedPRNumber=502 after re-fetch, got %d", item.LinkedPRNumber)
	}
}

func TestReconcileRemovesStaleItems(t *testing.T) {
	c := seedCache(t) // has items #1 and #2

	c.Reconcile(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Research"},
			// Item #2 removed from board.
		},
	})

	_, err := c.store.Get("owner/repo", 2)
	if err == nil {
		t.Error("item #2 should have been removed from cache after reconcile")
	}
}

// ---------------------------------------------------------------------------
// Pause / Resume — delta application gate
// ---------------------------------------------------------------------------

func TestPauseStopsDeltaApplication(t *testing.T) {
	c := seedCache(t)
	c.Pause()

	payload := issuesLabeledPayloadJSON("labeled", "owner/repo", 1, "blocked")
	c.ApplyDelta("issues", payload)

	s := testGetState(t, c, "owner/repo", 1)
	for _, l := range s.Labels {
		if l == "blocked" {
			t.Error("delta should not have been applied when paused")
			return
		}
	}
}

func TestResumeReenablesDeltaApplication(t *testing.T) {
	c := seedCache(t)
	c.Pause()
	c.Reconcile(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Research", Labels: []string{"enhancement"}},
			{ID: "I_002", ItemID: "PVTI_002", Number: 2, Repo: "owner/repo", Status: "Plan"},
		},
	})
	c.Resume()

	payload := issuesLabeledPayloadJSON("labeled", "owner/repo", 1, "newlabel")
	c.ApplyDelta("issues", payload)

	s := testGetState(t, c, "owner/repo", 1)
	found := false
	for _, l := range s.Labels {
		if l == "newlabel" {
			found = true
		}
	}

	if !found {
		t.Error("delta should have been applied after resume")
	}
}

// ---------------------------------------------------------------------------
// Delta: UpdatedAt bumped so itemMayNeedWork picks up webhook-changed items
// ---------------------------------------------------------------------------

func TestDeltaBumpsUpdatedAt(t *testing.T) {
	c := seedCache(t)
	key := itemKey("owner/repo", 1)

	// Record initial localDeltaAt (zero before any webhook).
	c.mu.RLock()
	before := c.localDeltaAt[key]
	c.mu.RUnlock()

	payload := issuesLabeledPayloadJSON("labeled", "owner/repo", 1, "newlabel")
	c.ApplyDelta("issues", payload)

	c.mu.RLock()
	after := c.localDeltaAt[key]
	c.mu.RUnlock()

	if !after.After(before) {
		t.Errorf("expected localDeltaAt to advance after labeled delta; before=%v after=%v", before, after)
	}
}

// ---------------------------------------------------------------------------
// Delta: pull_request_review_comment resets deep cache for ReviewThreadID
// ---------------------------------------------------------------------------

func TestDeltaReviewCommentResetsDeepFetched(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)
	// Mark as already deep-fetched.
	testSetDeepFetched(c, "owner/repo", 1)

	payload := pullRequestReviewCommentPayloadJSON("created", "owner/repo", 42, 200, "RC_node_2", "inline comment", "alice")
	c.ApplyDelta("pull_request_review_comment", payload)

	if testIsDeepFetched(c, "owner/repo", 1) {
		t.Error("expected deep cache to be reset after review comment delta so next FetchItemDetails fetches ReviewThreadID from GitHub")
	}
}

// ---------------------------------------------------------------------------
// Paused read methods fall through to GitHub (stream-health failover)
// ---------------------------------------------------------------------------

func TestFetchProjectBoardFallsThroughWhenPaused(t *testing.T) {
	mc := &mockClient{projectBoardResult: &gh.ProjectBoard{
		ProjectID: "PID2", Items: []gh.ProjectItem{{Number: 42, Repo: "o/r"}},
	}}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	c.Bootstrap(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Research"}},
	})
	c.Pause()

	board, err := c.FetchProjectBoard("o", "r", 1, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	// Should return the fallback's board (ProjectID: "PID2"), not the cached one.
	if board.ProjectID != "PID2" {
		t.Errorf("expected fallback board (PID2) when paused, got %q", board.ProjectID)
	}
}

func TestFetchLabelsFallsThroughWhenPaused(t *testing.T) {
	mc := &mockClient{labelsResult: []string{"live-label"}}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	c.Bootstrap(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{{ID: "I_1", Number: 1, Repo: "owner/repo", Labels: []string{"cached-label"}}},
	})
	c.Pause()

	labels, err := c.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if len(labels) != 1 || labels[0] != "live-label" {
		t.Errorf("expected live-label from fallback when paused, got %v", labels)
	}
	if mc.fetchLabelsCount != 1 {
		t.Errorf("expected exactly 1 fallback call, got %d", mc.fetchLabelsCount)
	}
}

func TestFetchCheckRunsFallsThroughWhenPaused(t *testing.T) {
	mc := &mockClient{checkRunsResult: []gh.CheckRun{{ID: 99, Name: "live", Status: "completed", Conclusion: "success"}}}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	// Pre-populate cache check runs so we can verify the paused path bypasses them.
	c.mu.Lock()
	c.checkRuns["sha_x"] = []gh.CheckRun{{ID: 1, Name: "cached"}}
	c.mu.Unlock()
	c.Pause()

	runs, err := c.FetchCheckRuns("owner", "repo", "sha_x")
	if err != nil {
		t.Fatalf("FetchCheckRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != 99 {
		t.Errorf("expected live run (ID=99) from fallback when paused, got %+v", runs)
	}
}

// ---------------------------------------------------------------------------
// UpdateItemStatus tests
// ---------------------------------------------------------------------------

func TestUpdateItemStatusSetsStatusAndUpdatedAt(t *testing.T) {
	c := seedCache(t)
	key := ItemKey("owner/repo", 1)

	before := time.Now()
	c.UpdateItemStatus(key, "Plan")
	after := time.Now()

	s := testGetState(t, c, "owner/repo", 1)
	if s.Status != "Plan" {
		t.Errorf("want Status %q, got %q", "Plan", s.Status)
	}
	// UpdateItemStatus bumps localDeltaAt (not item.UpdatedAt directly).
	c.mu.RLock()
	deltaAt := c.localDeltaAt[key]
	c.mu.RUnlock()

	if deltaAt.Before(before) || deltaAt.After(after) {
		t.Errorf("localDeltaAt %v not in expected range [%v, %v]", deltaAt, before, after)
	}
}

func TestUpdateItemStatusNoopForUnknownKey(t *testing.T) {
	c := seedCache(t)
	// Should not panic; just log and return.
	c.UpdateItemStatus("nonexistent/repo#999", "Done")
}

func TestUpdateItemStatusIsIdempotent(t *testing.T) {
	c := seedCache(t)
	key := ItemKey("owner/repo", 1)
	c.UpdateItemStatus(key, "Plan")
	c.UpdateItemStatus(key, "Plan")

	s := testGetState(t, c, "owner/repo", 1)
	if s.Status != "Plan" {
		t.Errorf("want Status %q after repeated calls, got %q", "Plan", s.Status)
	}
}

// ---------------------------------------------------------------------------
// ApplyStatusBatch tests
// ---------------------------------------------------------------------------

func TestApplyStatusBatchUpdatesDriftedItems(t *testing.T) {
	c := seedCache(t)
	// PVTI_001 is item #1 (status "Research"); drift it to "Plan".
	c.ApplyStatusBatch(map[string]string{"PVTI_001": "Plan"})

	s := testGetState(t, c, "owner/repo", 1)
	if s.Status != "Plan" {
		t.Errorf("want Status %q, got %q", "Plan", s.Status)
	}
}

func TestApplyStatusBatchLeavesUndriftedItemsUnchanged(t *testing.T) {
	c := seedCache(t)
	key2 := ItemKey("owner/repo", 2)

	c.mu.RLock()
	origDeltaAt := c.localDeltaAt[key2]
	c.mu.RUnlock()

	// Item 2 already has Status "Plan" — sending same value should not bump localDeltaAt.
	c.ApplyStatusBatch(map[string]string{"PVTI_002": "Plan"})

	c.mu.RLock()
	newDeltaAt := c.localDeltaAt[key2]
	c.mu.RUnlock()

	if newDeltaAt != origDeltaAt {
		t.Error("localDeltaAt should not change when Status is already up to date")
	}
}

func TestApplyStatusBatchSkipsUnknownItemIDs(t *testing.T) {
	c := seedCache(t)
	// Should not panic or add entries for unknown PVTI IDs.
	c.ApplyStatusBatch(map[string]string{"PVTI_UNKNOWN": "Done"})
	if len(c.store.All()) != 2 {
		t.Errorf("item count should remain 2, got %d", len(c.store.All()))
	}
}

// ---------------------------------------------------------------------------
// GetItemID and ProjectID tests
// ---------------------------------------------------------------------------

func TestGetItemIDReturnsNodeID(t *testing.T) {
	c := seedCache(t)
	key := ItemKey("owner/repo", 1)
	itemID, ok := c.GetItemID(key)
	if !ok {
		t.Fatal("GetItemID returned !ok for known key")
	}
	if itemID != "PVTI_001" {
		t.Errorf("want %q, got %q", "PVTI_001", itemID)
	}
}

func TestGetItemIDReturnsFalseForUnknownKey(t *testing.T) {
	c := seedCache(t)
	_, ok := c.GetItemID("nonexistent/repo#999")
	if ok {
		t.Error("expected !ok for unknown key")
	}
}

func TestProjectIDReturnsBootstrappedID(t *testing.T) {
	c := seedCache(t)
	if got := c.ProjectID(); got != "PID" {
		t.Errorf("want %q, got %q", "PID", got)
	}
}

func TestProjectIDReturnsEmptyBeforeBootstrap(t *testing.T) {
	c := NewCacheImpl(&mockClient{}, itemstate.NewStore(nil), nopLog)
	if got := c.ProjectID(); got != "" {
		t.Errorf("want empty string before bootstrap, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Auto-heal: PR-targeted delta handlers repair stale LinkedPRNumber
// ---------------------------------------------------------------------------

// seedCacheWithStalePRLink seeds a cache with item #1 at LinkedPRNumber=0 and deep-fetched.
func seedCacheWithStalePRLink(t *testing.T, mc *mockClient) *CacheImpl {
	t.Helper()
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	board := &gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo",
				Status: "Implement", LinkedPRNumber: 0},
		},
	}
	c.Bootstrap(board)
	// Mark as deep-fetched so we can verify deep cache is cleared by heal.
	testSetDeepFetched(c, "owner/repo", 1)
	return c
}

func TestAutoHealPullRequestReviewSubmitted(t *testing.T) {
	mc := &mockClient{
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{1}, nil // PR #42 closes issue #1
		},
	}
	c := seedCacheWithStalePRLink(t, mc)

	payload := pullRequestReviewPayloadJSON("submitted", "owner/repo", 42, 2001, "approved", "reviewer")
	c.ApplyDelta("pull_request_review", payload)

	s := testGetState(t, c, "owner/repo", 1)
	var linkedPR int
	var reviews []gh.PRReview
	if s.LinkedPR != nil {
		linkedPR = s.LinkedPR.Number
		reviews = clonePRReviews(s.LinkedPR.Reviews)
	}
	df := testIsDeepFetched(c, "owner/repo", 1)

	if linkedPR != 42 {
		t.Errorf("expected LinkedPRNumber=42 after heal, got %d", linkedPR)
	}
	if len(reviews) != 1 || reviews[0].Author != "reviewer" || reviews[0].State != "APPROVED" {
		t.Errorf("unexpected reviews after heal: %+v", reviews)
	}
	if df {
		t.Error("expected deep cache to be cleared after auto-heal")
	}
	if mc.fetchPRClosingIssuesCount != 1 {
		t.Errorf("expected exactly 1 REST call, got %d", mc.fetchPRClosingIssuesCount)
	}
}

func TestAutoHealPullRequestReviewCommentCreated(t *testing.T) {
	mc := &mockClient{
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{1}, nil
		},
	}
	c := seedCacheWithStalePRLink(t, mc)

	payload := pullRequestReviewCommentPayloadJSON("created", "owner/repo", 42, 300, "RC_heal_1", "inline comment", "reviewer")
	c.ApplyDelta("pull_request_review_comment", payload)

	s := testGetState(t, c, "owner/repo", 1)
	var linkedPR int
	var comments []gh.Comment
	if s.LinkedPR != nil {
		linkedPR = s.LinkedPR.Number
		comments = cloneComments(s.LinkedPR.ThreadComments)
	}
	df := testIsDeepFetched(c, "owner/repo", 1)

	if linkedPR != 42 {
		t.Errorf("expected LinkedPRNumber=42 after heal, got %d", linkedPR)
	}
	if len(comments) != 1 || comments[0].ID != "RC_heal_1" {
		t.Errorf("unexpected comments after heal: %+v", comments)
	}
	if df {
		t.Error("expected deep cache to be cleared after auto-heal")
	}
	if mc.fetchPRClosingIssuesCount != 1 {
		t.Errorf("expected exactly 1 REST call, got %d", mc.fetchPRClosingIssuesCount)
	}
}

func TestAutoHealCheckRunCompleted(t *testing.T) {
	mc := &mockClient{
		fetchPRsForSHAFn: func(owner, repo, sha string) ([]int, error) {
			return []int{42}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{1}, nil
		},
	}
	c := seedCacheWithStalePRLink(t, mc)

	before := time.Now()
	payload := checkRunPayloadJSON("completed", "owner/repo", 5001, "ci", "completed", "success", "sha_heal")
	c.ApplyDelta("check_run", payload)

	s := testGetState(t, c, "owner/repo", 1)
	var linkedPR int
	if s.LinkedPR != nil {
		linkedPR = s.LinkedPR.Number
	}
	// SHA indexed in Store when item's LinkedPR.HeadSHA is set.
	shaIndexed := s.LinkedPR != nil && s.LinkedPR.HeadSHA == "sha_heal"
	df := testIsDeepFetched(c, "owner/repo", 1)
	c.mu.RLock()
	deltaAt := c.localDeltaAt[itemKey("owner/repo", 1)]
	c.mu.RUnlock()

	if linkedPR != 42 {
		t.Errorf("expected LinkedPRNumber=42 after heal, got %d", linkedPR)
	}
	if !shaIndexed {
		t.Errorf("expected SHA sha_heal to be stored in item's LinkedPR.HeadSHA")
	}
	if df {
		t.Error("expected deep cache to be cleared after auto-heal")
	}
	if !deltaAt.After(before) {
		t.Error("expected localDeltaAt to be bumped after auto-heal")
	}
	if mc.fetchPRsForSHACount != 1 {
		t.Errorf("expected 1 FetchPRsForSHA call, got %d", mc.fetchPRsForSHACount)
	}
	if mc.fetchPRClosingIssuesCount != 1 {
		t.Errorf("expected 1 FetchPRClosingIssues call, got %d", mc.fetchPRClosingIssuesCount)
	}
}

func TestAutoHealDropsWhenNoPRClosingKeyword(t *testing.T) {
	var logBuf strings.Builder
	logFn := func(format string, args ...any) { logBuf.WriteString(format) }

	mc := &mockClient{
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return nil, nil // PR body has no closing reference
		},
	}
	c := seedCacheWithStalePRLink2(t, logFn)
	c.fallback = mc

	payload := pullRequestReviewPayloadJSON("submitted", "owner/repo", 99, 3001, "approved", "bot")
	c.ApplyDelta("pull_request_review", payload)

	s := testGetState(t, c, "owner/repo", 1)
	var linkedPR int
	var reviews []gh.PRReview
	if s.LinkedPR != nil {
		linkedPR = s.LinkedPR.Number
		reviews = clonePRReviews(s.LinkedPR.Reviews)
	}

	if linkedPR != 0 {
		t.Errorf("expected LinkedPRNumber=0 (no mutation), got %d", linkedPR)
	}
	if len(reviews) != 0 {
		t.Errorf("expected no reviews appended on drop, got %+v", reviews)
	}
	if !strings.Contains(logBuf.String(), "dropped") {
		t.Errorf("expected 'dropped' in log, got: %q", logBuf.String())
	}
}

func TestAutoHealDropsSilentlyWhenIssuNotInCache(t *testing.T) {
	mc := &mockClient{
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{999}, nil // issue #999 not in cache
		},
	}
	c := seedCacheWithStalePRLink(t, mc)

	payload := pullRequestReviewPayloadJSON("submitted", "owner/repo", 77, 4001, "approved", "bot")
	c.ApplyDelta("pull_request_review", payload)

	s := testGetState(t, c, "owner/repo", 1)
	var linkedPR int
	var reviews []gh.PRReview
	if s.LinkedPR != nil {
		linkedPR = s.LinkedPR.Number
		reviews = clonePRReviews(s.LinkedPR.Reviews)
	}

	if linkedPR != 0 {
		t.Errorf("expected no mutation (issue not in cache), got LinkedPRNumber=%d", linkedPR)
	}
	if len(reviews) != 0 {
		t.Errorf("expected no reviews appended for unmanaged issue, got %+v", reviews)
	}
}

func TestAutoHealNegativeCachePreventsRepeatedRESTCall(t *testing.T) {
	mc := &mockClient{
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return nil, nil // no closing reference — will populate negative cache
		},
	}
	c := seedCacheWithStalePRLink(t, mc)

	// First delta — triggers REST call and populates negative cache.
	payload := pullRequestReviewPayloadJSON("submitted", "owner/repo", 55, 5001, "approved", "bot")
	c.ApplyDelta("pull_request_review", payload)
	if mc.fetchPRClosingIssuesCount != 1 {
		t.Fatalf("expected 1 REST call after first delta, got %d", mc.fetchPRClosingIssuesCount)
	}

	// Second delta for same PR — negative cache should suppress the REST call.
	payload2 := pullRequestReviewPayloadJSON("submitted", "owner/repo", 55, 5002, "approved", "bot2")
	c.ApplyDelta("pull_request_review", payload2)
	if mc.fetchPRClosingIssuesCount != 1 {
		t.Errorf("expected negative cache to suppress second REST call, got %d calls total", mc.fetchPRClosingIssuesCount)
	}
}

func TestAutoHealPullRequestDelta(t *testing.T) {
	mc := &mockClient{
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{1}, nil // PR #42 closes issue #1
		},
	}
	c := seedCacheWithStalePRLink(t, mc)

	before := time.Now()
	payload := pullRequestPayloadJSON("opened", "owner/repo", 42, "sha_heal_pr", "open", false, false)
	c.ApplyDelta("pull_request", payload)

	s := testGetState(t, c, "owner/repo", 1)
	var linkedPR int
	if s.LinkedPR != nil {
		linkedPR = s.LinkedPR.Number
	}
	shaIndexed := s.LinkedPR != nil && s.LinkedPR.HeadSHA == "sha_heal_pr"
	df := testIsDeepFetched(c, "owner/repo", 1)
	c.mu.RLock()
	deltaAt := c.localDeltaAt[itemKey("owner/repo", 1)]
	c.mu.RUnlock()

	if linkedPR != 42 {
		t.Errorf("expected LinkedPRNumber=42 after heal, got %d", linkedPR)
	}
	if !shaIndexed {
		t.Errorf("expected SHA sha_heal_pr to be stored in item's LinkedPR.HeadSHA")
	}
	if df {
		t.Error("expected deep cache to be cleared after auto-heal")
	}
	if !deltaAt.After(before) {
		t.Error("expected localDeltaAt to be bumped after auto-heal")
	}
	if mc.fetchPRClosingIssuesCount != 1 {
		t.Errorf("expected exactly 1 REST call, got %d", mc.fetchPRClosingIssuesCount)
	}
}

// seedCacheWithStalePRLink variant accepting a custom logFn for log-capture tests.
func seedCacheWithStalePRLink2(t *testing.T, logFn func(string, ...any)) *CacheImpl {
	t.Helper()
	c := NewCacheImpl(&mockClient{}, itemstate.NewStore(nil), logFn)
	board := &gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo",
				Status: "Implement", LinkedPRNumber: 0},
		},
	}
	c.Bootstrap(board)
	testSetDeepFetched(c, "owner/repo", 1)
	return c
}

// ---------------------------------------------------------------------------
// ApplyLabelAdded tests
// ---------------------------------------------------------------------------

func TestApplyLabelAdded(t *testing.T) {
	c := seedCache(t) // item #1 has labels: ["enhancement"]
	key := ItemKey("owner/repo", 1)

	before := time.Now()
	c.ApplyLabelAdded(key, "fabrik:awaiting-ci")

	labels, err := c.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	found := false
	for _, l := range labels {
		if l == "fabrik:awaiting-ci" {
			found = true
		}
	}
	if !found {
		t.Errorf("want fabrik:awaiting-ci in labels after ApplyLabelAdded, got %v", labels)
	}

	// localDeltaAt must be bumped.
	c.mu.RLock()
	deltaAt := c.localDeltaAt[key]
	c.mu.RUnlock()
	if !deltaAt.After(before) {
		t.Error("want localDeltaAt bumped after ApplyLabelAdded")
	}
}

func TestApplyLabelAddedIdempotent(t *testing.T) {
	c := seedCache(t)
	key := ItemKey("owner/repo", 1)

	c.ApplyLabelAdded(key, "enhancement") // already present

	labels, err := c.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	count := 0
	for _, l := range labels {
		if l == "enhancement" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("want exactly 1 'enhancement' label, got %d; labels=%v", count, labels)
	}
}

// ---------------------------------------------------------------------------
// ApplyLabelRemoved tests
// ---------------------------------------------------------------------------

func TestApplyLabelRemoved(t *testing.T) {
	c := seedCache(t) // item #1 has labels: ["enhancement"]
	key := ItemKey("owner/repo", 1)

	before := time.Now()
	c.ApplyLabelRemoved(key, "enhancement")

	labels, err := c.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	for _, l := range labels {
		if l == "enhancement" {
			t.Errorf("want 'enhancement' removed, still present in %v", labels)
		}
	}

	c.mu.RLock()
	deltaAt := c.localDeltaAt[key]
	c.mu.RUnlock()
	if !deltaAt.After(before) {
		t.Error("want localDeltaAt bumped after ApplyLabelRemoved")
	}
}

func TestApplyLabelRemovedNoopWhenAbsent(t *testing.T) {
	c := seedCache(t)
	key := ItemKey("owner/repo", 1)

	before := time.Now()
	c.ApplyLabelRemoved(key, "nonexistent-label")

	// localDeltaAt must NOT be bumped — no state change.
	c.mu.RLock()
	deltaAt := c.localDeltaAt[key]
	c.mu.RUnlock()
	if deltaAt.After(before) {
		t.Error("want localDeltaAt NOT bumped for no-op ApplyLabelRemoved")
	}
}

// ---------------------------------------------------------------------------
// Subscribe tests
// ---------------------------------------------------------------------------

func TestCacheImplSubscribeReceivesChanges(t *testing.T) {
	c := seedCache(t)
	key := ItemKey("owner/repo", 1)

	var mu sync.Mutex
	var calls []itemstate.Change
	unsub := c.Subscribe(itemstate.ObserverFunc(func(ch itemstate.Change, _ itemstate.Snapshot) {
		mu.Lock()
		calls = append(calls, ch)
		mu.Unlock()
	}))
	defer unsub()

	c.ApplyLabelAdded(key, "fabrik:awaiting-ci")

	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n == 0 {
		t.Fatal("want observer called after ApplyLabelAdded, got 0 calls")
	}
	if calls[0].Fields&itemstate.LabelsChanged == 0 {
		t.Errorf("want LabelsChanged flag set, got flags=%b", calls[0].Fields)
	}
}

func TestCacheImplSubscribeUnsubscribeStopsCalls(t *testing.T) {
	c := seedCache(t)
	key := ItemKey("owner/repo", 1)

	var mu sync.Mutex
	var calls int
	unsub := c.Subscribe(itemstate.ObserverFunc(func(_ itemstate.Change, _ itemstate.Snapshot) {
		mu.Lock()
		calls++
		mu.Unlock()
	}))
	unsub()

	c.ApplyLabelAdded(key, "fabrik:awaiting-ci")

	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 0 {
		t.Errorf("want 0 calls after unsubscribe, got %d", n)
	}
}

// TestPullRequestDeltaFiresLinkedPRChangedObserver verifies the end-to-end
// path: a pull_request "opened" webhook delta emits LinkedPRChanged via the
// Store observer, and the resulting snapshot has LinkedPR.Title populated.
// Mirrors TestCacheImplSubscribeReceivesChanges but exercises the PR-details
// mutation path introduced in Phase 5 F2 (issue #562).
func TestPullRequestDeltaFiresLinkedPRChangedObserver(t *testing.T) {
	c := seedCache(t)
	// Establish PR linkage: item #1 → PR #42.
	testSetLinkedPR(c, "owner/repo", 1, 42)

	var mu sync.Mutex
	var seenLinkedPRChanged bool
	var titleAfterChange string
	unsub := c.Subscribe(itemstate.ObserverFunc(func(ch itemstate.Change, snap itemstate.Snapshot) {
		if ch.Fields&itemstate.LinkedPRChanged != 0 {
			mu.Lock()
			seenLinkedPRChanged = true
			if lpr := snap.LinkedPR(); lpr != nil {
				titleAfterChange = lpr.Title
			}
			mu.Unlock()
		}
	}))
	defer unsub()

	// "opened" delta carries title/state/draft and updates LinkedPR details.
	c.ApplyDelta("pull_request", pullRequestPayloadJSON("opened", "owner/repo", 42, "sha-abc", "open", false, false))

	mu.Lock()
	saw := seenLinkedPRChanged
	title := titleAfterChange
	mu.Unlock()

	if !saw {
		t.Error("want LinkedPRChanged observer fired after pull_request.opened delta; got none")
	}
	if title == "" {
		t.Error("want non-empty LinkedPR.Title in observer snapshot; got empty")
	}
}

// ---------------------------------------------------------------------------
// SubscribePause tests
// ---------------------------------------------------------------------------

func TestSubscribePauseFiresOnPause(t *testing.T) {
	c := seedCache(t)
	var got []bool
	unsub := c.SubscribePause(func(paused bool) { got = append(got, paused) })
	defer unsub()

	c.Pause()
	if len(got) != 1 || !got[0] {
		t.Errorf("want [true] after Pause, got %v", got)
	}
	c.Resume()
	if len(got) != 2 || got[1] {
		t.Errorf("want [true, false] after Resume, got %v", got)
	}
}

func TestSubscribePauseUnsubscribeStopsCalls(t *testing.T) {
	c := seedCache(t)
	var calls int
	unsub := c.SubscribePause(func(bool) { calls++ })
	unsub()

	c.Pause()
	c.Resume()
	if calls != 0 {
		t.Errorf("want 0 calls after unsubscribe, got %d", calls)
	}
}

// ---------------------------------------------------------------------------
// ApplyCommentAdded tests
// ---------------------------------------------------------------------------

func TestApplyCommentAdded(t *testing.T) {
	c := seedCache(t)
	key := ItemKey("owner/repo", 1)

	// First set up deep-fetch state so FetchItemDetails serves from cache.
	testSetDeepFetched(c, "owner/repo", 1)

	comment := gh.Comment{
		ID:         "C_new",
		DatabaseID: 42,
		Author:     "fabrik-bot",
		Body:       "Stage complete.",
		CreatedAt:  time.Now(),
	}

	before := time.Now()
	c.ApplyCommentAdded(key, comment)

	// FetchItemDetails should now return the comment from cache (no fallback).
	mc := &mockClient{} // empty fallback — fallback hit would panic on nil
	c.fallback = mc
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	if err := c.FetchItemDetails(&item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}
	if mc.fetchItemDetailsCount != 0 {
		t.Errorf("want cache hit (no fallback), got %d fallback calls", mc.fetchItemDetailsCount)
	}
	found := false
	for _, cm := range item.Comments {
		if cm.DatabaseID == 42 {
			found = true
		}
	}
	if !found {
		t.Errorf("want comment #42 in item.Comments after ApplyCommentAdded, got %v", item.Comments)
	}

	c.mu.RLock()
	deltaAt := c.localDeltaAt[key]
	c.mu.RUnlock()
	if !deltaAt.After(before) {
		t.Error("want localDeltaAt bumped after ApplyCommentAdded")
	}
}

// ---------------------------------------------------------------------------
// Unknown key — no-op tests
// ---------------------------------------------------------------------------

func TestWriteThroughNoopOnUnknownKey(t *testing.T) {
	c := seedCache(t)
	unknownKey := ItemKey("owner/repo", 9999)

	c.ApplyLabelAdded(unknownKey, "foo")
	c.ApplyLabelRemoved(unknownKey, "foo")
	c.ApplyCommentAdded(unknownKey, gh.Comment{DatabaseID: 1, Body: "hello"})

	// localDeltaAt must not contain the unknown key.
	c.mu.RLock()
	_, ok := c.localDeltaAt[unknownKey]
	c.mu.RUnlock()
	if ok {
		t.Error("want localDeltaAt NOT updated for unknown key")
	}
}

// ---------------------------------------------------------------------------
// Concurrent write-through test
// ---------------------------------------------------------------------------

func TestConcurrentWriteThrough(t *testing.T) {
	c := seedCache(t)
	key := ItemKey("owner/repo", 1)

	const iterations = 100
	var wg sync.WaitGroup
	wg.Add(iterations * 2)

	for i := 0; i < iterations; i++ {
		go func() {
			defer wg.Done()
			c.ApplyLabelAdded(key, "concurrent-label")
		}()
		go func() {
			defer wg.Done()
			c.ApplyLabelRemoved(key, "concurrent-label")
		}()
	}
	wg.Wait()

	// Final label state must be consistent (no panic, no data race).
	labels, err := c.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	count := 0
	for _, l := range labels {
		if l == "concurrent-label" {
			count++
		}
	}
	if count > 1 {
		t.Errorf("want at most 1 'concurrent-label', got %d; labels=%v", count, labels)
	}
}

// ---------------------------------------------------------------------------
// RegisterItemID
// ---------------------------------------------------------------------------

func TestRegisterItemID(t *testing.T) {
	t.Run("populates itemID and GetItemID returns it", func(t *testing.T) {
		// Bootstrap an item without an itemID (simulates issues.opened path).
		c := NewCacheImpl(&mockClient{}, itemstate.NewStore(nil), nopLog)
		board := &gh.ProjectBoard{
			ProjectID: "PID", OwnerType: "organization",
			Items: []gh.ProjectItem{{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Research"}},
		}
		c.Bootstrap(board)

		key := ItemKey("owner/repo", 1)
		// Item exists but has no itemID.
		if id, ok := c.GetItemID(key); ok || id != "" {
			t.Fatalf("expected no itemID before RegisterItemID, got %q ok=%v", id, ok)
		}

		c.RegisterItemID(key, "PVTI_new")

		id, ok := c.GetItemID(key)
		if !ok {
			t.Fatal("GetItemID returned !ok after RegisterItemID")
		}
		if id != "PVTI_new" {
			t.Errorf("GetItemID = %q, want %q", id, "PVTI_new")
		}
	})

	t.Run("other fields remain intact after RegisterItemID", func(t *testing.T) {
		c := NewCacheImpl(&mockClient{}, itemstate.NewStore(nil), nopLog)
		board := &gh.ProjectBoard{
			ProjectID: "PID", OwnerType: "organization",
			Items: []gh.ProjectItem{
				{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Research", Labels: []string{"bug"}},
			},
		}
		c.Bootstrap(board)

		key := ItemKey("owner/repo", 1)
		c.RegisterItemID(key, "PVTI_xyz")

		s := testGetState(t, c, "owner/repo", 1)
		if s.ItemID != "PVTI_xyz" {
			t.Errorf("ItemID = %q, want %q", s.ItemID, "PVTI_xyz")
		}
		if s.Status != "Research" {
			t.Errorf("Status = %q, want %q", s.Status, "Research")
		}
		if len(s.Labels) != 1 || s.Labels[0] != "bug" {
			t.Errorf("Labels = %v, want [bug]", s.Labels)
		}
	})

	t.Run("no-op when key not in Store", func(t *testing.T) {
		c := NewCacheImpl(&mockClient{}, itemstate.NewStore(nil), nopLog)
		// Empty cache — no Bootstrap.
		key := ItemKey("owner/repo", 99)
		c.RegisterItemID(key, "PVTI_phantom") // must not create phantom entry
		if _, ok := c.GetItemID(key); ok {
			t.Error("RegisterItemID created a phantom Store entry for an unknown key")
		}
	})

	t.Run("idempotent when called twice with same value", func(t *testing.T) {
		c := NewCacheImpl(&mockClient{}, itemstate.NewStore(nil), nopLog)
		board := &gh.ProjectBoard{
			ProjectID: "PID", OwnerType: "organization",
			Items: []gh.ProjectItem{{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Research"}},
		}
		c.Bootstrap(board)
		key := ItemKey("owner/repo", 1)

		c.RegisterItemID(key, "PVTI_same")
		c.RegisterItemID(key, "PVTI_same") // second call — same value

		id, ok := c.GetItemID(key)
		if !ok || id != "PVTI_same" {
			t.Errorf("after double call, GetItemID = %q ok=%v, want PVTI_same true", id, ok)
		}
	})

	t.Run("no-op when itemID is empty", func(t *testing.T) {
		c := NewCacheImpl(&mockClient{}, itemstate.NewStore(nil), nopLog)
		board := &gh.ProjectBoard{
			ProjectID: "PID", OwnerType: "organization",
			Items: []gh.ProjectItem{{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Research"}},
		}
		c.Bootstrap(board)
		key := ItemKey("owner/repo", 1)

		c.RegisterItemID(key, "") // empty — must be ignored
		if id, ok := c.GetItemID(key); ok || id != "" {
			t.Errorf("RegisterItemID(\"\") should be no-op, got %q ok=%v", id, ok)
		}
	})
}

// ---------------------------------------------------------------------------
// Additional payload builders for Phase 3-D delta coverage tests
// ---------------------------------------------------------------------------

// issuesOpenedPayloadJSON builds an issues.opened payload with full issue data.
func issuesOpenedPayloadJSON(repo string, issNum int, nodeID, title, body string, labels, assignees []string) []byte {
	p := issuesPayload{Action: "opened"}
	p.Repository.FullName = repo
	p.Issue.Number = issNum
	p.Issue.NodeID = nodeID
	p.Issue.Title = title
	p.Issue.Body = body
	for _, l := range labels {
		p.Issue.Labels = append(p.Issue.Labels, struct {
			Name string `json:"name"`
		}{Name: l})
	}
	for _, a := range assignees {
		p.Issue.Assignees = append(p.Issue.Assignees, struct {
			Login string `json:"login"`
		}{Login: a})
	}
	b, _ := json.Marshal(p)
	return b
}

// issuesActionPayloadJSON builds a simple issues payload (closed/reopened/deleted/transferred)
// with just repo and issue number.
func issuesActionPayloadJSON(action, repo string, issNum int) []byte {
	p := issuesPayload{Action: action}
	p.Repository.FullName = repo
	p.Issue.Number = issNum
	b, _ := json.Marshal(p)
	return b
}

// issuesEditedPayloadJSON builds an issues.edited payload.
func issuesEditedPayloadJSON(repo string, issNum int, title, body string) []byte {
	p := issuesPayload{Action: "edited"}
	p.Repository.FullName = repo
	p.Issue.Number = issNum
	p.Issue.Title = title
	p.Issue.Body = body
	b, _ := json.Marshal(p)
	return b
}

// issuesAssignedPayloadJSON builds an issues.assigned/unassigned payload with
// the post-mutation full assignee list.
func issuesAssignedPayloadJSON(action, repo string, issNum int, assignees []string) []byte {
	p := issuesPayload{Action: action}
	p.Repository.FullName = repo
	p.Issue.Number = issNum
	for _, a := range assignees {
		p.Issue.Assignees = append(p.Issue.Assignees, struct {
			Login string `json:"login"`
		}{Login: a})
	}
	b, _ := json.Marshal(p)
	return b
}

// projectsV2ItemCreatedPayloadJSON builds a projects_v2_item.created payload.
// boardItemID is the PVTI_xxx board-side item ID stored in projects_v2_item.id.
func projectsV2ItemCreatedPayloadJSON(contentNodeID, contentType, boardItemID string) []byte {
	p := projectsV2ItemPayload{Action: "created"}
	p.ProjectsV2Item.ID = boardItemID
	p.ProjectsV2Item.ContentNodeID = contentNodeID
	p.ProjectsV2Item.ContentType = contentType
	b, _ := json.Marshal(p)
	return b
}

// projectsV2ItemRemovedPayloadJSON builds a projects_v2_item.deleted or .archived payload.
func projectsV2ItemRemovedPayloadJSON(action, itemID string) []byte {
	p := projectsV2ItemPayload{Action: action}
	p.ProjectsV2Item.ID = itemID
	b, _ := json.Marshal(p)
	return b
}

// pullRequestDraftPayloadJSON builds a pull_request.ready_for_review or .converted_to_draft payload.
func pullRequestDraftPayloadJSON(action, repo string, prNum int) []byte {
	p := pullRequestPayload{Action: action}
	p.Repository.FullName = repo
	p.PullRequest.Number = prNum
	b, _ := json.Marshal(p)
	return b
}

// pullRequestReviewRequestedPayloadJSON builds a pull_request.review_requested payload.
// allReviewers is the full list after adding; singleLogin/singleType is the one being added.
func pullRequestReviewRequestedPayloadJSON(repo string, prNum int, allLogins []string, singleLogin string) []byte {
	p := pullRequestPayload{Action: "review_requested"}
	p.Repository.FullName = repo
	p.PullRequest.Number = prNum
	for _, login := range allLogins {
		p.PullRequest.RequestedReviewers = append(p.PullRequest.RequestedReviewers, struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		}{Login: login, Type: "User"})
	}
	p.RequestedReviewer.Login = singleLogin
	p.RequestedReviewer.Type = "User"
	b, _ := json.Marshal(p)
	return b
}

// pullRequestReviewRequestRemovedPayloadJSON builds a pull_request.review_request_removed payload.
func pullRequestReviewRequestRemovedPayloadJSON(repo string, prNum int, removedLogin string) []byte {
	p := pullRequestPayload{Action: "review_request_removed"}
	p.Repository.FullName = repo
	p.PullRequest.Number = prNum
	p.RequestedReviewer.Login = removedLogin
	p.RequestedReviewer.Type = "User"
	b, _ := json.Marshal(p)
	return b
}
