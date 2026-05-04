package engine

import (
	"context"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// mockGitHubClient implements GitHubClient for testing.
// mu protects all *Calls slices so the mock is safe for concurrent use by
// goroutines spawned inside the engine's poll loop.
type mockGitHubClient struct {
	mu sync.Mutex

	fetchProjectBoardFn       func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error)
	fetchLabelsFn             func(owner, repo string, issueNumber int) ([]string, error)
	fetchItemDetailsFn        func(item *gh.ProjectItem) error
	fetchStatusFieldFn        func(projectID string) (*gh.StatusField, error)
	addLabelToIssueFn         func(owner, repo string, issueNumber int, labelName string) error
	removeLabelFromIssueFn    func(owner, repo string, issueNumber int, labelName string) error
	addCommentFn              func(owner, repo string, issueNumber int, body string) (int, error)
	addCommentReactionFn      func(owner, repo string, commentDatabaseID int, content string) error
	updateCommentFn           func(owner, repo string, commentDatabaseID int, body string) error
	updateIssueBodyFn         func(owner, repo string, issueNumber int, body string) error
	updateProjectItemStatusFn func(projectID, itemID, statusFieldID, statusOptionID string) error
	findPRForIssueFn          func(owner, repo string, issueNumber int) (int, error)
	fetchLinkedPRFn           func(owner, repo string, issueNumber int) (*gh.PRDetails, error)
	fetchPRMergeableFn        func(owner, repo string, prNumber int) (*bool, error)
	fetchPRMergeableStateFn   func(owner, repo string, prNumber int) (string, error)
	fetchCheckRunsFn          func(owner, repo, sha string) ([]gh.CheckRun, error)
	getPRBaseFn               func(owner, repo string, prNumber int) (string, error)
	updatePRBaseFn            func(owner, repo string, prNumber int, newBase string) error
	mergePRFn                 func(owner, repo string, prNumber int) error
	rateLimitStatsFn          func() (gh.RateLimitStats, gh.RateLimitStats)
	getIssueBodyFn            func(owner, repo string, issueNumber int) (string, error)
	markPRReadyFn             func(owner, repo string, prNumber int) error
	createDraftPRFn           func(owner, repo, title, head, base, body string, issueNumber int) (int, error)
	fetchLatestReleaseFn      func(owner, repo string) (*gh.LatestRelease, error)
	fetchLabelAppliedAtFn     func(owner, repo string, issueNumber int, labelName string) (time.Time, error)
	archiveProjectItemFn      func(projectID, itemID string) error
	deleteReviewRequestFn     func(owner, repo string, prNumber int, reviewers []string) error
	addReviewRequestFn              func(owner, repo string, prNumber int, reviewers []string) error
	fetchProjectItemStatusFn        func(itemID string) (string, error)
	fetchProjectItemStatusBatchFn   func(projectID string) (map[string]string, error)
	fetchPRClosingIssuesFn          func(owner, repo string, prNumber int) ([]int, error)
	fetchPRsForSHAFn                func(owner, repo, sha string) ([]int, error)

	// Track call counts for FetchProjectItemStatus
	fetchProjectItemStatusCalls []string

	// Track calls for FetchLabelAppliedAt
	fetchLabelAppliedAtCalls []fetchLabelAppliedAtCall

	// Track calls for ArchiveProjectItem
	archiveProjectItemCalls []archiveProjectItemCall

	// Track calls — access under mu when accessed from concurrent goroutines.
	getPRBaseCalls                  []getPRBaseCall
	updatePRBaseCalls               []updatePRBaseCall
	addLabelCalls                   []addLabelCall
	removeLabelCalls                []removeLabelCall
	addCommentCalls                 []addCommentCall
	addCommentReactionCalls         []addCommentReactionCall
	updateCommentCalls              []updateCommentCall
	updateStatusCalls               []updateStatusCall
	mergePRCalls                    []mergePRCall
	markPRReadyCalls                []markPRReadyCall
	createDraftPRCalls              []createDraftPRCall
	resolveReviewThreadCalls        []string
	addPRReviewCommentReactionCalls []prReviewCommentReactionCall
	deleteReviewRequestCalls        []reviewRequestCall
	addReviewRequestCalls           []reviewRequestCall
}

type reviewRequestCall struct {
	owner, repo string
	prNumber    int
	reviewers   []string
}

type prReviewCommentReactionCall struct {
	owner, repo string
	commentID   int
	content     string
}

type fetchLabelAppliedAtCall struct {
	owner, repo string
	issueNumber int
	labelName   string
}

type archiveProjectItemCall struct {
	projectID, itemID string
}

type markPRReadyCall struct {
	owner, repo string
	prNumber    int
}

type createDraftPRCall struct {
	owner, repo, title, head, base string
	body                           string
	issueNumber                    int
}

type getPRBaseCall struct {
	owner, repo string
	prNumber    int
}

type updatePRBaseCall struct {
	owner, repo string
	prNumber    int
	newBase     string
}

type mergePRCall struct {
	owner, repo string
	prNumber    int
}

type addLabelCall struct {
	owner, repo string
	issueNumber int
	labelName   string
}

type removeLabelCall struct {
	owner, repo string
	issueNumber int
	labelName   string
}

type addCommentCall struct {
	owner, repo string
	issueNumber int
	body        string
}

type updateCommentCall struct {
	owner, repo string
	commentID   int
	body        string
}

type addCommentReactionCall struct {
	owner, repo       string
	commentDatabaseID int
	content           string
}

type updateStatusCall struct {
	projectID, itemID, fieldID, optionID string
}

func (m *mockGitHubClient) FetchLabels(owner, repo string, issueNumber int) ([]string, error) {
	if m.fetchLabelsFn != nil {
		return m.fetchLabelsFn(owner, repo, issueNumber)
	}
	return nil, nil
}

func (m *mockGitHubClient) FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
	if m.fetchProjectBoardFn != nil {
		return m.fetchProjectBoardFn(owner, repo, projectNum, ownerType)
	}
	return &gh.ProjectBoard{}, nil
}

func (m *mockGitHubClient) FetchItemDetails(item *gh.ProjectItem) error {
	if m.fetchItemDetailsFn != nil {
		return m.fetchItemDetailsFn(item)
	}
	return nil
}

func (m *mockGitHubClient) FetchStatusField(projectID string) (*gh.StatusField, error) {
	if m.fetchStatusFieldFn != nil {
		return m.fetchStatusFieldFn(projectID)
	}
	return &gh.StatusField{Options: map[string]string{}}, nil
}

func (m *mockGitHubClient) AddLabelToIssue(owner, repo string, issueNumber int, labelName string) error {
	m.mu.Lock()
	m.addLabelCalls = append(m.addLabelCalls, addLabelCall{owner, repo, issueNumber, labelName})
	fn := m.addLabelToIssueFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, issueNumber, labelName)
	}
	return nil
}

func (m *mockGitHubClient) RemoveLabelFromIssue(owner, repo string, issueNumber int, labelName string) error {
	m.mu.Lock()
	m.removeLabelCalls = append(m.removeLabelCalls, removeLabelCall{owner, repo, issueNumber, labelName})
	fn := m.removeLabelFromIssueFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, issueNumber, labelName)
	}
	return nil
}

func (m *mockGitHubClient) AddComment(owner, repo string, issueNumber int, body string) (int, error) {
	m.mu.Lock()
	m.addCommentCalls = append(m.addCommentCalls, addCommentCall{owner, repo, issueNumber, body})
	fn := m.addCommentFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, issueNumber, body)
	}
	return 0, nil
}

func (m *mockGitHubClient) AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	m.mu.Lock()
	m.addCommentReactionCalls = append(m.addCommentReactionCalls, addCommentReactionCall{owner, repo, commentDatabaseID, content})
	fn := m.addCommentReactionFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, commentDatabaseID, content)
	}
	return nil
}

func (m *mockGitHubClient) AddPRReviewCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	m.mu.Lock()
	m.addPRReviewCommentReactionCalls = append(m.addPRReviewCommentReactionCalls, prReviewCommentReactionCall{owner, repo, commentDatabaseID, content})
	m.mu.Unlock()
	return nil
}

func (m *mockGitHubClient) ResolveReviewThread(threadID string) error {
	m.mu.Lock()
	m.resolveReviewThreadCalls = append(m.resolveReviewThreadCalls, threadID)
	m.mu.Unlock()
	return nil
}

func (m *mockGitHubClient) UpdateComment(owner, repo string, commentDatabaseID int, body string) error {
	m.mu.Lock()
	fn := m.updateCommentFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, commentDatabaseID, body)
	}
	m.mu.Lock()
	m.updateCommentCalls = append(m.updateCommentCalls, updateCommentCall{owner, repo, commentDatabaseID, body})
	m.mu.Unlock()
	return nil
}

func (m *mockGitHubClient) UpdateIssueBody(owner, repo string, issueNumber int, body string) error {
	m.mu.Lock()
	fn := m.updateIssueBodyFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, issueNumber, body)
	}
	return nil
}

func (m *mockGitHubClient) UpdateProjectItemStatus(projectID, itemID, statusFieldID, statusOptionID string) error {
	m.mu.Lock()
	m.updateStatusCalls = append(m.updateStatusCalls, updateStatusCall{projectID, itemID, statusFieldID, statusOptionID})
	fn := m.updateProjectItemStatusFn
	m.mu.Unlock()
	if fn != nil {
		return fn(projectID, itemID, statusFieldID, statusOptionID)
	}
	return nil
}

func (m *mockGitHubClient) FindPRForIssue(owner, repo string, issueNumber int) (int, error) {
	m.mu.Lock()
	fn := m.findPRForIssueFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, issueNumber)
	}
	return 0, nil
}

func (m *mockGitHubClient) FetchLinkedPR(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
	m.mu.Lock()
	fn := m.fetchLinkedPRFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, issueNumber)
	}
	return nil, nil
}

func (m *mockGitHubClient) FetchCheckRuns(owner, repo, sha string) ([]gh.CheckRun, error) {
	m.mu.Lock()
	fn := m.fetchCheckRunsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, sha)
	}
	return nil, nil
}

func (m *mockGitHubClient) FetchPRMergeable(owner, repo string, prNumber int) (*bool, error) {
	m.mu.Lock()
	fn := m.fetchPRMergeableFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, prNumber)
	}
	return nil, nil
}

func (m *mockGitHubClient) FetchPRMergeableState(owner, repo string, prNumber int) (string, error) {
	m.mu.Lock()
	fn := m.fetchPRMergeableStateFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, prNumber)
	}
	return "", nil
}

func (m *mockGitHubClient) GetPRBase(owner, repo string, prNumber int) (string, error) {
	m.mu.Lock()
	m.getPRBaseCalls = append(m.getPRBaseCalls, getPRBaseCall{owner, repo, prNumber})
	fn := m.getPRBaseFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, prNumber)
	}
	return "", nil
}

func (m *mockGitHubClient) UpdatePRBase(owner, repo string, prNumber int, newBase string) error {
	m.mu.Lock()
	m.updatePRBaseCalls = append(m.updatePRBaseCalls, updatePRBaseCall{owner, repo, prNumber, newBase})
	fn := m.updatePRBaseFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, prNumber, newBase)
	}
	return nil
}

func (m *mockGitHubClient) MergePR(owner, repo string, prNumber int) error {
	m.mu.Lock()
	m.mergePRCalls = append(m.mergePRCalls, mergePRCall{owner, repo, prNumber})
	fn := m.mergePRFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, prNumber)
	}
	return nil
}

func (m *mockGitHubClient) GetIssueBody(owner, repo string, issueNumber int) (string, error) {
	m.mu.Lock()
	fn := m.getIssueBodyFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, issueNumber)
	}
	return "", nil
}

func (m *mockGitHubClient) CreateDraftPR(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
	m.mu.Lock()
	m.createDraftPRCalls = append(m.createDraftPRCalls, createDraftPRCall{owner, repo, title, head, base, body, issueNumber})
	fn := m.createDraftPRFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, title, head, base, body, issueNumber)
	}
	return 0, nil
}

func (m *mockGitHubClient) MarkPRReady(owner, repo string, prNumber int) error {
	m.mu.Lock()
	m.markPRReadyCalls = append(m.markPRReadyCalls, markPRReadyCall{owner, repo, prNumber})
	fn := m.markPRReadyFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, prNumber)
	}
	return nil
}

func (m *mockGitHubClient) FetchLatestRelease(owner, repo string) (*gh.LatestRelease, error) {
	m.mu.Lock()
	fn := m.fetchLatestReleaseFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo)
	}
	return nil, nil
}

func (m *mockGitHubClient) FetchLabelAppliedAt(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
	m.mu.Lock()
	m.fetchLabelAppliedAtCalls = append(m.fetchLabelAppliedAtCalls, fetchLabelAppliedAtCall{owner, repo, issueNumber, labelName})
	fn := m.fetchLabelAppliedAtFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, issueNumber, labelName)
	}
	return time.Time{}, nil
}

func (m *mockGitHubClient) ArchiveProjectItem(projectID, itemID string) error {
	m.mu.Lock()
	m.archiveProjectItemCalls = append(m.archiveProjectItemCalls, archiveProjectItemCall{projectID, itemID})
	fn := m.archiveProjectItemFn
	m.mu.Unlock()
	if fn != nil {
		return fn(projectID, itemID)
	}
	return nil
}

func (m *mockGitHubClient) SeedLabels(owner, repo string, stageNames []string, lockedUser string) error {
	return nil
}

func (m *mockGitHubClient) DeleteReviewRequest(owner, repo string, prNumber int, reviewers []string) error {
	m.mu.Lock()
	m.deleteReviewRequestCalls = append(m.deleteReviewRequestCalls, reviewRequestCall{owner, repo, prNumber, reviewers})
	fn := m.deleteReviewRequestFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, prNumber, reviewers)
	}
	return nil
}

func (m *mockGitHubClient) AddReviewRequest(owner, repo string, prNumber int, reviewers []string) error {
	m.mu.Lock()
	m.addReviewRequestCalls = append(m.addReviewRequestCalls, reviewRequestCall{owner, repo, prNumber, reviewers})
	fn := m.addReviewRequestFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, prNumber, reviewers)
	}
	return nil
}

func (m *mockGitHubClient) FetchPRClosingIssues(owner, repo string, prNumber int) ([]int, error) {
	m.mu.Lock()
	fn := m.fetchPRClosingIssuesFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, prNumber)
	}
	return nil, nil
}

func (m *mockGitHubClient) FetchPRsForSHA(owner, repo, sha string) ([]int, error) {
	m.mu.Lock()
	fn := m.fetchPRsForSHAFn
	m.mu.Unlock()
	if fn != nil {
		return fn(owner, repo, sha)
	}
	return nil, nil
}

func (m *mockGitHubClient) RateLimitStats() (gh.RateLimitStats, gh.RateLimitStats) {
	if m.rateLimitStatsFn != nil {
		return m.rateLimitStatsFn()
	}
	return gh.RateLimitStats{}, gh.RateLimitStats{}
}

func (m *mockGitHubClient) FetchProjectItemStatus(itemID string) (string, error) {
	m.mu.Lock()
	m.fetchProjectItemStatusCalls = append(m.fetchProjectItemStatusCalls, itemID)
	fn := m.fetchProjectItemStatusFn
	m.mu.Unlock()
	if fn != nil {
		return fn(itemID)
	}
	return "", nil
}

func (m *mockGitHubClient) FetchProjectItemStatusBatch(projectID string) (map[string]string, error) {
	m.mu.Lock()
	fn := m.fetchProjectItemStatusBatchFn
	m.mu.Unlock()
	if fn != nil {
		return fn(projectID)
	}
	return map[string]string{}, nil
}

// mockClaudeInvoker implements ClaudeInvoker for testing.
// mu protects calls for concurrent-safe access.
type mockClaudeInvoker struct {
	mu       sync.Mutex
	invokeFn func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error)
	calls    []claudeInvokeCall
}

type claudeInvokeCall struct {
	stageName string
	issueNum  int
	resume    bool
	workDir   string
	opts      InvokeOptions
}

func (m *mockClaudeInvoker) Invoke(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
	m.mu.Lock()
	m.calls = append(m.calls, claudeInvokeCall{stage.Name, issue.Number, resume, workDir, opts})
	fn := m.invokeFn
	m.mu.Unlock()
	if fn != nil {
		return fn(stage, issue, newComments, resume, workDir, opts)
	}
	return "mock output", false, TokenUsage{}, nil
}

func (m *mockClaudeInvoker) InvokeForComments(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
	if m.invokeFn != nil {
		return m.invokeFn(stage, issue, comments, false, workDir, opts)
	}
	return "mock comment output", false, TokenUsage{}, nil
}
