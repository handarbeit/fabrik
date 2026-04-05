package engine

import (
	"context"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

// mockGitHubClient implements GitHubClient for testing.
type mockGitHubClient struct {
	fetchProjectBoardFn       func(owner, repo string, projectNum int) (*gh.ProjectBoard, error)
	fetchLabelsFn             func(owner, repo string, issueNumber int) ([]string, error)
	fetchItemDetailsFn        func(item *gh.ProjectItem) error
	fetchStatusFieldFn        func(projectID string) (*gh.StatusField, error)
	addLabelToIssueFn         func(owner, repo string, issueNumber int, labelName string) error
	removeLabelFromIssueFn    func(owner, repo string, issueNumber int, labelName string) error
	addCommentFn              func(owner, repo string, issueNumber int, body string) error
	updateCommentFn           func(owner, repo string, commentDatabaseID int, body string) error
	updateIssueBodyFn         func(owner, repo string, issueNumber int, body string) error
	updateProjectItemStatusFn func(projectID, itemID, statusFieldID, statusOptionID string) error
	getIssueBodyFn            func(owner, repo string, issueNumber int) (string, error)
	findPRForIssueFn          func(owner, repo string, issueNumber int) (int, error)
	createDraftPRFn           func(owner, repo, title, head, base string, issueNumber int) (int, error)
	mergePRFn                 func(owner, repo string, prNumber int) error
	rateLimitStatsFn          func() (gh.RateLimitStats, gh.RateLimitStats)

	// Track calls
	addLabelCalls      []addLabelCall
	removeLabelCalls   []removeLabelCall
	addCommentCalls    []addCommentCall
	updateCommentCalls []updateCommentCall
	updateStatusCalls  []updateStatusCall
	mergePRCalls       []mergePRCall
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

type updateStatusCall struct {
	projectID, itemID, fieldID, optionID string
}

func (m *mockGitHubClient) FetchLabels(owner, repo string, issueNumber int) ([]string, error) {
	if m.fetchLabelsFn != nil {
		return m.fetchLabelsFn(owner, repo, issueNumber)
	}
	return nil, nil
}

func (m *mockGitHubClient) FetchProjectBoard(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
	if m.fetchProjectBoardFn != nil {
		return m.fetchProjectBoardFn(owner, repo, projectNum)
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
	m.addLabelCalls = append(m.addLabelCalls, addLabelCall{owner, repo, issueNumber, labelName})
	if m.addLabelToIssueFn != nil {
		return m.addLabelToIssueFn(owner, repo, issueNumber, labelName)
	}
	return nil
}

func (m *mockGitHubClient) RemoveLabelFromIssue(owner, repo string, issueNumber int, labelName string) error {
	m.removeLabelCalls = append(m.removeLabelCalls, removeLabelCall{owner, repo, issueNumber, labelName})
	if m.removeLabelFromIssueFn != nil {
		return m.removeLabelFromIssueFn(owner, repo, issueNumber, labelName)
	}
	return nil
}

func (m *mockGitHubClient) AddComment(owner, repo string, issueNumber int, body string) error {
	m.addCommentCalls = append(m.addCommentCalls, addCommentCall{owner, repo, issueNumber, body})
	if m.addCommentFn != nil {
		return m.addCommentFn(owner, repo, issueNumber, body)
	}
	return nil
}

func (m *mockGitHubClient) AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	return nil
}

func (m *mockGitHubClient) UpdateComment(owner, repo string, commentDatabaseID int, body string) error {
	if m.updateCommentFn != nil {
		return m.updateCommentFn(owner, repo, commentDatabaseID, body)
	}
	m.updateCommentCalls = append(m.updateCommentCalls, updateCommentCall{owner, repo, commentDatabaseID, body})
	return nil
}

func (m *mockGitHubClient) UpdateIssueBody(owner, repo string, issueNumber int, body string) error {
	if m.updateIssueBodyFn != nil {
		return m.updateIssueBodyFn(owner, repo, issueNumber, body)
	}
	return nil
}

func (m *mockGitHubClient) UpdateProjectItemStatus(projectID, itemID, statusFieldID, statusOptionID string) error {
	m.updateStatusCalls = append(m.updateStatusCalls, updateStatusCall{projectID, itemID, statusFieldID, statusOptionID})
	if m.updateProjectItemStatusFn != nil {
		return m.updateProjectItemStatusFn(projectID, itemID, statusFieldID, statusOptionID)
	}
	return nil
}

func (m *mockGitHubClient) GetIssueBody(owner, repo string, issueNumber int) (string, error) {
	if m.getIssueBodyFn != nil {
		return m.getIssueBodyFn(owner, repo, issueNumber)
	}
	return "", nil
}

func (m *mockGitHubClient) FindPRForIssue(owner, repo string, issueNumber int) (int, error) {
	if m.findPRForIssueFn != nil {
		return m.findPRForIssueFn(owner, repo, issueNumber)
	}
	return 0, nil
}

func (m *mockGitHubClient) MergePR(owner, repo string, prNumber int) error {
	m.mergePRCalls = append(m.mergePRCalls, mergePRCall{owner, repo, prNumber})
	if m.mergePRFn != nil {
		return m.mergePRFn(owner, repo, prNumber)
	}
	return nil
}

func (m *mockGitHubClient) CreateDraftPR(owner, repo, title, head, base string, issueNumber int) (int, error) {
	if m.createDraftPRFn != nil {
		return m.createDraftPRFn(owner, repo, title, head, base, issueNumber)
	}
	return 0, nil
}

func (m *mockGitHubClient) MarkPRReady(owner, repo string, prNumber int) error {
	return nil
}

func (m *mockGitHubClient) RateLimitStats() (gh.RateLimitStats, gh.RateLimitStats) {
	if m.rateLimitStatsFn != nil {
		return m.rateLimitStatsFn()
	}
	return gh.RateLimitStats{}, gh.RateLimitStats{}
}

// mockClaudeInvoker implements ClaudeInvoker for testing.
type mockClaudeInvoker struct {
	invokeFn func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, TokenUsage, error)
	calls    []claudeInvokeCall
}

type claudeInvokeCall struct {
	stageName     string
	issueNum      int
	resume        bool
	workDir       string
	modelOverride string
}

func (m *mockClaudeInvoker) Invoke(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, TokenUsage, error) {
	m.calls = append(m.calls, claudeInvokeCall{stage.Name, issue.Number, resume, workDir, modelOverride})
	if m.invokeFn != nil {
		return m.invokeFn(stage, issue, newComments, resume, workDir, modelOverride)
	}
	return "mock output", false, TokenUsage{}, nil
}

func (m *mockClaudeInvoker) InvokeForComments(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, modelOverride string) (string, bool, TokenUsage, error) {
	if m.invokeFn != nil {
		return m.invokeFn(stage, issue, comments, false, workDir, modelOverride)
	}
	return "mock comment output", false, TokenUsage{}, nil
}
