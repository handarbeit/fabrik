package engine

import (
	"context"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

// mockGitHubClient implements GitHubClient for testing.
type mockGitHubClient struct {
	fetchProjectBoardFn       func(owner, repo string, projectNum int) (*gh.ProjectBoard, error)
	fetchStatusFieldFn        func(projectID string) (*gh.StatusField, error)
	addLabelToIssueFn         func(owner, repo string, issueNumber int, labelName string) error
	addCommentFn              func(owner, repo string, issueNumber int, body string) error
	updateProjectItemStatusFn func(projectID, itemID, statusFieldID, statusOptionID string) error

	// Track calls
	addLabelCalls     []addLabelCall
	addCommentCalls   []addCommentCall
	updateStatusCalls []updateStatusCall
}

type addLabelCall struct {
	owner, repo string
	issueNumber int
	labelName   string
}

type addCommentCall struct {
	owner, repo string
	issueNumber int
	body        string
}

type updateStatusCall struct {
	projectID, itemID, fieldID, optionID string
}

func (m *mockGitHubClient) FetchProjectBoard(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
	if m.fetchProjectBoardFn != nil {
		return m.fetchProjectBoardFn(owner, repo, projectNum)
	}
	return &gh.ProjectBoard{}, nil
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

func (m *mockGitHubClient) UpdateIssueBody(owner, repo string, issueNumber int, body string) error {
	return nil
}

func (m *mockGitHubClient) UpdateProjectItemStatus(projectID, itemID, statusFieldID, statusOptionID string) error {
	m.updateStatusCalls = append(m.updateStatusCalls, updateStatusCall{projectID, itemID, statusFieldID, statusOptionID})
	if m.updateProjectItemStatusFn != nil {
		return m.updateProjectItemStatusFn(projectID, itemID, statusFieldID, statusOptionID)
	}
	return nil
}

func (m *mockGitHubClient) FindPRForIssue(owner, repo string, issueNumber int) (int, error) {
	return 0, nil
}

func (m *mockGitHubClient) CreateDraftPR(owner, repo, title, head, base string, issueNumber int) (int, error) {
	return 0, nil
}

func (m *mockGitHubClient) MarkPRReady(owner, repo string, prNumber int) error {
	return nil
}

// mockClaudeInvoker implements ClaudeInvoker for testing.
type mockClaudeInvoker struct {
	invokeFn func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, error)
	calls    []claudeInvokeCall
}

type claudeInvokeCall struct {
	stageName     string
	issueNum      int
	resume        bool
	workDir       string
	modelOverride string
}

func (m *mockClaudeInvoker) Invoke(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, error) {
	m.calls = append(m.calls, claudeInvokeCall{stage.Name, issue.Number, resume, workDir, modelOverride})
	if m.invokeFn != nil {
		return m.invokeFn(stage, issue, newComments, resume, workDir, modelOverride)
	}
	return "mock output", false, nil
}
