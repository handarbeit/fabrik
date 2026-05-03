package boardcache

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
)

// ---------------------------------------------------------------------------
// Mock ReadClient for testing
// ---------------------------------------------------------------------------

type mockClient struct {
	fetchItemDetailsCount int
	fetchCheckRunsCount   int
	fetchLinkedPRCount    int
	fetchLabelsCount      int

	itemDetailsResult  *gh.ProjectItem
	checkRunsResult    []gh.CheckRun
	linkedPRResult     *gh.PRDetails
	labelsResult       []string
	projectBoardResult *gh.ProjectBoard
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

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

var nopLog = func(format string, args ...any) {}

func seedCache(t *testing.T) *CacheImpl {
	t.Helper()
	c := NewCacheImpl(&mockClient{}, nopLog)
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

func TestFetchProjectBoardFallsBackWhenEmpty(t *testing.T) {
	mc := &mockClient{projectBoardResult: &gh.ProjectBoard{
		ProjectID: "PID", Items: []gh.ProjectItem{{Number: 5, Repo: "o/r"}},
	}}
	c := NewCacheImpl(mc, nopLog)
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
	c := NewCacheImpl(mc, nopLog)
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
// Delta: issue_comment.created — idempotent by comment ID
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
	// Make sure item #1 is deep-fetched first (so Comments isn't nil from fallback).
	c.mu.Lock()
	c.deepFetched[itemKey("owner/repo", 1)] = true
	c.mu.Unlock()

	payload := issueCommentPayloadJSON("created", "owner/repo", 1, "C_abc", 100, "test comment", "alice")
	c.ApplyDelta("issue_comment", payload)

	board, _ := c.FetchProjectBoard("", "", 0, "")
	var item *gh.ProjectItem
	for i := range board.Items {
		if board.Items[i].Number == 1 {
			item = &board.Items[i]
			break
		}
	}
	if item == nil {
		t.Fatal("item #1 not found")
	}
	// FetchProjectBoard returns shallow copy; comments are in cache.
	// We need to check via FetchItemDetails.
	c.mu.RLock()
	cached := c.items[itemKey("owner/repo", 1)]
	c.mu.RUnlock()
	if len(cached.Comments) != 1 || cached.Comments[0].ID != "C_abc" {
		t.Errorf("expected comment C_abc, got %+v", cached.Comments)
	}
}

func TestDeltaIssueCommentCreatedIdempotent(t *testing.T) {
	c := seedCache(t)
	c.mu.Lock()
	c.deepFetched[itemKey("owner/repo", 1)] = true
	c.mu.Unlock()

	payload := issueCommentPayloadJSON("created", "owner/repo", 1, "C_abc", 100, "test", "alice")
	c.ApplyDelta("issue_comment", payload)
	c.ApplyDelta("issue_comment", payload) // duplicate

	c.mu.RLock()
	cached := c.items[itemKey("owner/repo", 1)]
	c.mu.RUnlock()
	if len(cached.Comments) != 1 {
		t.Errorf("expected exactly 1 comment after duplicate, got %d", len(cached.Comments))
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

	c.mu.RLock()
	item := c.items[itemKey("owner/repo", 1)]
	labels := cloneStrings(item.Labels)
	c.mu.RUnlock()

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

	c.mu.RLock()
	item := c.items[itemKey("owner/repo", 1)]
	count := 0
	for _, l := range item.Labels {
		if l == "enhancement" {
			count++
		}
	}
	c.mu.RUnlock()

	if count != 1 {
		t.Errorf("label 'enhancement' should appear exactly once, got %d", count)
	}
}

func TestDeltaIssuesUnlabeled(t *testing.T) {
	c := seedCache(t)
	// Seed has "enhancement" on item #1.
	payload := issuesLabeledPayloadJSON("unlabeled", "owner/repo", 1, "enhancement")
	c.ApplyDelta("issues", payload)

	c.mu.RLock()
	item := c.items[itemKey("owner/repo", 1)]
	for _, l := range item.Labels {
		if l == "enhancement" {
			c.mu.RUnlock()
			t.Error("label 'enhancement' should have been removed")
			return
		}
	}
	c.mu.RUnlock()
}

// ---------------------------------------------------------------------------
// Delta: pull_request — update linkedPRs and shaToKey
// ---------------------------------------------------------------------------

func pullRequestPayloadJSON(action, repo string, prNum int, sha, state string, merged, draft bool) []byte {
	p := pullRequestPayload{Action: action}
	p.Repository.FullName = repo
	p.PullRequest.Number = prNum
	p.PullRequest.Head.SHA = sha
	p.PullRequest.State = state
	p.PullRequest.Merged = merged
	p.PullRequest.Draft = draft
	b, _ := json.Marshal(p)
	return b
}

func TestDeltaPullRequestOpened(t *testing.T) {
	c := seedCache(t)
	// Set LinkedPRNumber on item #1 so shaToKey can be populated.
	c.mu.Lock()
	c.items[itemKey("owner/repo", 1)].LinkedPRNumber = 42
	c.mu.Unlock()

	payload := pullRequestPayloadJSON("opened", "owner/repo", 42, "sha123", "open", false, false)
	c.ApplyDelta("pull_request", payload)

	c.mu.RLock()
	pr, ok := c.linkedPRs[prKey("owner/repo", 42)]
	shaKey, shaOK := c.shaToKey["sha123"]
	c.mu.RUnlock()

	if !ok || pr == nil {
		t.Error("expected linkedPRs entry for PR #42")
	}
	if pr != nil && pr.HeadSHA != "sha123" {
		t.Errorf("expected HeadSHA sha123, got %q", pr.HeadSHA)
	}
	if !shaOK || shaKey != itemKey("owner/repo", 1) {
		t.Errorf("expected shaToKey[sha123] = owner/repo#1, got %q (ok=%v)", shaKey, shaOK)
	}
}

func TestDeltaPullRequestSynchronizeUpdatesShaTKey(t *testing.T) {
	c := seedCache(t)
	c.mu.Lock()
	c.items[itemKey("owner/repo", 1)].LinkedPRNumber = 42
	c.mu.Unlock()

	// Initial SHA
	c.ApplyDelta("pull_request", pullRequestPayloadJSON("opened", "owner/repo", 42, "sha_old", "open", false, false))
	// New push — SHA changes
	c.ApplyDelta("pull_request", pullRequestPayloadJSON("synchronize", "owner/repo", 42, "sha_new", "open", false, false))

	c.mu.RLock()
	_, oldOK := c.shaToKey["sha_old"]
	_, newOK := c.shaToKey["sha_new"]
	c.mu.RUnlock()

	if oldOK {
		t.Error("old SHA should have been removed from shaToKey")
	}
	if !newOK {
		t.Error("new SHA should be in shaToKey")
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
	c.mu.Lock()
	c.items[itemKey("owner/repo", 1)].LinkedPRNumber = 42
	c.mu.Unlock()

	payload := pullRequestReviewPayloadJSON("submitted", "owner/repo", 42, 1001, "approved", "alice")
	c.ApplyDelta("pull_request_review", payload)

	c.mu.RLock()
	item := c.items[itemKey("owner/repo", 1)]
	reviews := clonePRReviews(item.LinkedPRReviews)
	c.mu.RUnlock()

	if len(reviews) != 1 || reviews[0].Author != "alice" || reviews[0].State != "APPROVED" {
		t.Errorf("unexpected reviews: %+v", reviews)
	}
}

func TestDeltaPullRequestReviewSubmittedUpsert(t *testing.T) {
	c := seedCache(t)
	c.mu.Lock()
	c.items[itemKey("owner/repo", 1)].LinkedPRNumber = 42
	c.mu.Unlock()

	// First review: changes_requested
	c.ApplyDelta("pull_request_review", pullRequestReviewPayloadJSON("submitted", "owner/repo", 42, 1001, "changes_requested", "alice"))
	// Second review with same ID: approve (author re-reviews)
	c.ApplyDelta("pull_request_review", pullRequestReviewPayloadJSON("submitted", "owner/repo", 42, 1001, "approved", "alice"))

	c.mu.RLock()
	item := c.items[itemKey("owner/repo", 1)]
	reviews := clonePRReviews(item.LinkedPRReviews)
	c.mu.RUnlock()

	if len(reviews) != 1 {
		t.Errorf("upsert: expected 1 review after re-review, got %d", len(reviews))
	}
	if reviews[0].State != "APPROVED" {
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
	c.mu.Lock()
	c.items[itemKey("owner/repo", 1)].LinkedPRNumber = 42
	c.mu.Unlock()

	payload := pullRequestReviewCommentPayloadJSON("created", "owner/repo", 42, 200, "RC_node_1", "looks good", "bob")
	c.ApplyDelta("pull_request_review_comment", payload)

	c.mu.RLock()
	item := c.items[itemKey("owner/repo", 1)]
	comments := cloneComments(item.LinkedPRReviewThreadComments)
	c.mu.RUnlock()

	if len(comments) != 1 || comments[0].ID != "RC_node_1" || comments[0].Author != "bob" {
		t.Errorf("unexpected review thread comments: %+v", comments)
	}
}

func TestDeltaPullRequestReviewCommentCreatedIdempotent(t *testing.T) {
	c := seedCache(t)
	c.mu.Lock()
	c.items[itemKey("owner/repo", 1)].LinkedPRNumber = 42
	c.mu.Unlock()

	payload := pullRequestReviewCommentPayloadJSON("created", "owner/repo", 42, 200, "RC_node_1", "looks good", "bob")
	c.ApplyDelta("pull_request_review_comment", payload)
	c.ApplyDelta("pull_request_review_comment", payload) // duplicate

	c.mu.RLock()
	item := c.items[itemKey("owner/repo", 1)]
	count := len(item.LinkedPRReviewThreadComments)
	c.mu.RUnlock()

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
	// First run: failure
	c.ApplyDelta("check_run", checkRunPayloadJSON("completed", "owner/repo", 9001, "build", "completed", "failure", "sha_abc"))
	// Same ID: success (re-run)
	c.ApplyDelta("check_run", checkRunPayloadJSON("completed", "owner/repo", 9001, "build", "completed", "success", "sha_abc"))

	c.mu.RLock()
	runs := c.checkRuns["sha_abc"]
	c.mu.RUnlock()

	if len(runs) != 1 {
		t.Errorf("upsert: expected 1 run after re-run, got %d", len(runs))
	}
	if runs[0].Conclusion != "success" {
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

	c.mu.RLock()
	item := c.items[itemKey("owner/repo", 1)]
	status := item.Status
	c.mu.RUnlock()

	if status != "Plan" {
		t.Errorf("expected status 'Plan', got %q", status)
	}
}

// ---------------------------------------------------------------------------
// FetchCheckRuns — cache miss → fallback → populate
// ---------------------------------------------------------------------------

func TestFetchCheckRunsFallback(t *testing.T) {
	mc := &mockClient{checkRunsResult: []gh.CheckRun{{ID: 42, Name: "test", Status: "completed", Conclusion: "success"}}}
	c := NewCacheImpl(mc, nopLog)

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
	c := NewCacheImpl(mc, nopLog)
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
	c := NewCacheImpl(&mockClient{}, logFn)
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

	c.mu.RLock()
	status := c.items[itemKey("owner/repo", 1)].Status
	c.mu.RUnlock()

	if status != "Plan" {
		t.Errorf("expected status 'Plan' after reconcile, got %q", status)
	}
	if !strings.Contains(logBuf.String(), "reconciliation") {
		t.Errorf("expected [reconciliation] log, got: %q", logBuf.String())
	}
}

func TestReconcilePreservesDeepFields(t *testing.T) {
	c := seedCache(t)
	c.mu.Lock()
	c.items[itemKey("owner/repo", 1)].Comments = []gh.Comment{{ID: "C1", Body: "preserved"}}
	c.deepFetched[itemKey("owner/repo", 1)] = true
	c.mu.Unlock()

	c.Reconcile(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Plan"},
		},
	})

	c.mu.RLock()
	item := c.items[itemKey("owner/repo", 1)]
	preserved := len(item.Comments) == 1 && item.Comments[0].ID == "C1"
	deepFetched := c.deepFetched[itemKey("owner/repo", 1)]
	c.mu.RUnlock()

	if !preserved {
		t.Error("expected deep-fetched comments to be preserved after reconcile")
	}
	if !deepFetched {
		t.Error("expected deepFetched flag to be preserved after reconcile")
	}
}

func TestReconcileRemovesStaleItems(t *testing.T) {
	c := seedCache(t) // has items #1 and #2

	c.Reconcile(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Research"},
			// Item #2 removed from board
		},
	})

	c.mu.RLock()
	_, item2Exists := c.items[itemKey("owner/repo", 2)]
	c.mu.RUnlock()

	if item2Exists {
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

	c.mu.RLock()
	item := c.items[itemKey("owner/repo", 1)]
	for _, l := range item.Labels {
		if l == "blocked" {
			c.mu.RUnlock()
			t.Error("delta should not have been applied when paused")
			return
		}
	}
	c.mu.RUnlock()
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

	c.mu.RLock()
	item := c.items[itemKey("owner/repo", 1)]
	found := false
	for _, l := range item.Labels {
		if l == "newlabel" {
			found = true
		}
	}
	c.mu.RUnlock()

	if !found {
		t.Error("delta should have been applied after resume")
	}
}

// ---------------------------------------------------------------------------
// Delta: UpdatedAt bumped so itemMayNeedWork picks up webhook-changed items
// ---------------------------------------------------------------------------

func TestDeltaBumpsUpdatedAt(t *testing.T) {
	c := seedCache(t)

	// Record item #1's initial UpdatedAt (zero from seed).
	c.mu.RLock()
	before := c.items[itemKey("owner/repo", 1)].UpdatedAt
	c.mu.RUnlock()

	payload := issuesLabeledPayloadJSON("labeled", "owner/repo", 1, "newlabel")
	c.ApplyDelta("issues", payload)

	c.mu.RLock()
	after := c.items[itemKey("owner/repo", 1)].UpdatedAt
	c.mu.RUnlock()

	if !after.After(before) {
		t.Errorf("expected UpdatedAt to advance after labeled delta; before=%v after=%v", before, after)
	}
}

// ---------------------------------------------------------------------------
// Delta: pull_request_review_comment resets deepFetched for ReviewThreadID
// ---------------------------------------------------------------------------

func TestDeltaReviewCommentResetsDeepFetched(t *testing.T) {
	c := seedCache(t)
	c.mu.Lock()
	c.items[itemKey("owner/repo", 1)].LinkedPRNumber = 42
	// Mark as already deep-fetched.
	c.deepFetched[itemKey("owner/repo", 1)] = true
	c.mu.Unlock()

	payload := pullRequestReviewCommentPayloadJSON("created", "owner/repo", 42, 200, "RC_node_2", "inline comment", "alice")
	c.ApplyDelta("pull_request_review_comment", payload)

	c.mu.RLock()
	df := c.deepFetched[itemKey("owner/repo", 1)]
	c.mu.RUnlock()

	if df {
		t.Error("expected deepFetched to be reset after review comment delta so next FetchItemDetails fetches ReviewThreadID from GitHub")
	}
}

// ---------------------------------------------------------------------------
// Paused read methods fall through to GitHub (stream-health failover)
// ---------------------------------------------------------------------------

func TestFetchProjectBoardFallsThroughWhenPaused(t *testing.T) {
	mc := &mockClient{projectBoardResult: &gh.ProjectBoard{
		ProjectID: "PID2", Items: []gh.ProjectItem{{Number: 42, Repo: "o/r"}},
	}}
	c := NewCacheImpl(mc, nopLog)
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
	c := NewCacheImpl(mc, nopLog)
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
	c := NewCacheImpl(mc, nopLog)
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
