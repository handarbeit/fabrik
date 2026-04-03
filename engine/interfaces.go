package engine

import (
	"context"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// GitHubClient defines the GitHub operations needed by the engine.
type GitHubClient interface {
	FetchProjectBoard(owner, repo string, projectNum int) (*gh.ProjectBoard, error)
	FetchItemDetails(item *gh.ProjectItem) error
	FetchStatusField(projectID string) (*gh.StatusField, error)
	AddLabelToIssue(owner, repo string, issueNumber int, labelName string) error
	RemoveLabelFromIssue(owner, repo string, issueNumber int, labelName string) error
	AddComment(owner, repo string, issueNumber int, body string) error
	AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error
	UpdateComment(owner, repo string, commentDatabaseID int, body string) error
	UpdateIssueBody(owner, repo string, issueNumber int, body string) error
	UpdateProjectItemStatus(projectID, itemID, statusFieldID, statusOptionID string) error
	GetIssueBody(owner, repo string, issueNumber int) (string, error)
	FindPRForIssue(owner, repo string, issueNumber int) (int, error)
	CreateDraftPR(owner, repo, title, head, base string, issueNumber int) (int, error)
	MarkPRReady(owner, repo string, prNumber int) error
	RateLimitStats() (rest, graphql gh.RateLimitStats)
}

// ClaudeInvoker defines the interface for invoking Claude Code.
type ClaudeInvoker interface {
	Invoke(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (output string, completed bool, usage TokenUsage, err error)
}

// RealClaudeInvoker wraps the existing InvokeClaude function.
type RealClaudeInvoker struct {
	DebugOutput bool
}

func (r *RealClaudeInvoker) Invoke(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, TokenUsage, error) {
	output, completed, usage, err := InvokeClaude(ctx, stage, issue, newComments, resume, workDir, modelOverride)
	if r.DebugOutput {
		saveDebugLog(issue.Number, stage.Name, output)
	}
	return output, completed, usage, err
}
