package engine

import (
	"context"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

// GitHubClient defines the GitHub operations needed by the engine.
type GitHubClient interface {
	FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error)
	FetchItemDetails(item *gh.ProjectItem) error
	FetchStatusField(projectID string) (*gh.StatusField, error)
	FetchLabels(owner, repo string, issueNumber int) ([]string, error)
	AddLabelToIssue(owner, repo string, issueNumber int, labelName string) error
	RemoveLabelFromIssue(owner, repo string, issueNumber int, labelName string) error
	AddComment(owner, repo string, issueNumber int, body string) (int, error)
	AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error
	AddPRReviewCommentReaction(owner, repo string, commentDatabaseID int, content string) error
	ResolveReviewThread(threadID string) error
	UpdateComment(owner, repo string, commentDatabaseID int, body string) error
	UpdateIssueBody(owner, repo string, issueNumber int, body string) error
	UpdateProjectItemStatus(projectID, itemID, statusFieldID, statusOptionID string) error
	ArchiveProjectItem(projectID, itemID string) error
	GetIssueBody(owner, repo string, issueNumber int) (string, error)
	FindPRForIssue(owner, repo string, issueNumber int) (int, error)
	FetchLinkedPR(owner, repo string, issueNumber int) (*gh.PRDetails, error)
	FetchPRMergeable(owner, repo string, prNumber int) (*bool, error)
	FetchPRMergeableState(owner, repo string, prNumber int) (string, error)
	FetchCheckRuns(owner, repo, sha string) ([]gh.CheckRun, error)
	FetchPRClosingIssues(owner, repo string, prNumber int) ([]int, error)
	FetchPRsForSHA(owner, repo, sha string) ([]int, error)
	FetchProjectItem(owner, repo string, issueNumber int) (*gh.ProjectItem, error)
	GetPRBase(owner, repo string, prNumber int) (string, error)
	UpdatePRBase(owner, repo string, prNumber int, newBase string) error
	CreateDraftPR(owner, repo, title, head, base, body string, issueNumber int) (int, error)
	MarkPRReady(owner, repo string, prNumber int) error
	MergePR(owner, repo string, prNumber int) error
	DeleteReviewRequest(owner, repo string, prNumber int, reviewers []string) error
	AddReviewRequest(owner, repo string, prNumber int, reviewers []string) error
	FetchLatestRelease(owner, repo string) (*gh.LatestRelease, error)
	FetchLabelAppliedAt(owner, repo string, issueNumber int, labelName string) (time.Time, error)
	SeedLabels(owner, repo string, stageNames []string, lockedUser string) error
	RateLimitStats() (rest, graphql gh.RateLimitStats)
	FetchProjectItemStatus(itemID string) (string, error)
	FetchProjectItemStatusBatch(projectID string) (map[string]string, error)
}

// InvokeOptions bundles per-issue override parameters for Claude invocations.
// Using a struct rather than individual parameters means future overrides
// (e.g. DisableAdaptiveThinking) are zero-churn field additions.
type InvokeOptions struct {
	ModelOverride    string // from "model:<name>" label, overrides stage.Model
	EffortOverride   string // from "effort:<level>" label, overrides stage.EffortLevel
	BaseBranch       string // actual default base branch for the managed repo (e.g. "liminis", not always "main")
	MaxTurnsOverride int    // when > 0, overrides stage.MaxTurns for this invocation; 0 means use stage.MaxTurns
	// OnPIDReady, if non-nil, is called once after cmd.Start() with the Claude subprocess PID.
	// Used by the heartbeat/liveness system to record the PID in the store.
	OnPIDReady func(pid int)
}

// ClaudeInvoker defines the interface for invoking Claude Code.
type ClaudeInvoker interface {
	Invoke(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (output string, completed bool, usage TokenUsage, err error)
	InvokeForComments(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (output string, completed bool, usage TokenUsage, err error)
}

// RealClaudeInvoker wraps the existing InvokeClaude function.
type RealClaudeInvoker struct {
	DebugOutput bool
}

func (r *RealClaudeInvoker) Invoke(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
	output, completed, usage, err := InvokeClaude(ctx, stage, issue, newComments, resume, workDir, opts)
	if r.DebugOutput {
		saveDebugLog(issue.Number, stage.Name, output)
	}
	return output, completed, usage, err
}

func (r *RealClaudeInvoker) InvokeForComments(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
	output, completed, usage, err := InvokeClaudeForComments(ctx, stage, issue, comments, workDir, opts)
	if r.DebugOutput {
		saveDebugLog(issue.Number, stage.Name+"-comment-review", output)
	}
	return output, completed, usage, err
}
