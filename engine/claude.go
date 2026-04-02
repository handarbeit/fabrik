package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

var stageCompleteRE = regexp.MustCompile(`(?m)^FABRIK_STAGE_COMPLETE\r?$`)

// SessionDir returns the directory where Claude sessions are cached for an issue.
func SessionDir(issueNumber int) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fabrik", "sessions", fmt.Sprintf("issue-%d", issueNumber))
}

// sessionFile returns the path to the session ID file for a given issue+stage.
// stageName is sanitized to prevent path traversal: filepath.Base strips directory
// components, and an additional check rejects names that are empty, ".", or the
// path separator (e.g. filepath.Base("/") == "/"), falling back to "default".
func sessionFile(issueNumber int, stageName string) string {
	base := filepath.Base(stageName)
	if base == "" || base == "." || base == "/" || base == string(filepath.Separator) {
		base = "default"
	}
	return filepath.Join(SessionDir(issueNumber), base+".session")
}

// InvokeClaude runs Claude Code with the given stage configuration and issue context.
// workDir is the directory Claude should run in (typically a git worktree).
// modelOverride, if non-empty, replaces the stage's configured model.
// It returns Claude's output and whether Claude indicated completion.
func InvokeClaude(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, error) {
	sessDir := SessionDir(issue.Number)
	if err := os.MkdirAll(sessDir, 0700); err != nil {
		return "", false, fmt.Errorf("creating session dir: %w", err)
	}
	if err := os.Chmod(sessDir, 0700); err != nil {
		return "", false, fmt.Errorf("setting session dir permissions: %w", err)
	}

	prompt := buildPrompt(stage, issue, newComments)
	args := buildClaudeArgs(stage, issue.Number, resume, modelOverride)

	output, _, err := runClaude(ctx, args, prompt, workDir, issue.Number, stage.Name)
	if err != nil {
		return output, false, err
	}
	return output, checkCompletion(stage, output), nil
}

// InvokeClaudeForComments runs Claude Code with a comment-review prompt.
// It uses the stage's CommentPrompt if defined, otherwise a default.
// modelOverride, if non-empty, replaces the stage's configured model.
func InvokeClaudeForComments(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, modelOverride string) (string, bool, error) {
	sessDir := SessionDir(issue.Number)
	if err := os.MkdirAll(sessDir, 0700); err != nil {
		return "", false, fmt.Errorf("creating session dir: %w", err)
	}
	if err := os.Chmod(sessDir, 0700); err != nil {
		return "", false, fmt.Errorf("setting session dir permissions: %w", err)
	}

	prompt := buildCommentReviewPrompt(stage, issue, comments)
	args := buildClaudeArgs(stage, issue.Number, true, modelOverride) // resume existing session

	return runClaude(ctx, args, prompt, workDir, issue.Number, stage.Name+"-comment-review")
}

func buildClaudeArgs(stage *stages.Stage, issueNumber int, resume bool, modelOverride string) []string {
	args := []string{
		"--print",
		"--output-format", "json",
	}

	sessFile := sessionFile(issueNumber, stage.Name)
	if resume {
		if sessionID, err := os.ReadFile(sessFile); err == nil && len(sessionID) > 0 {
			args = append(args, "--resume", strings.TrimSpace(string(sessionID)))
		}
	}

	// Model override from labels takes precedence over stage config
	if modelOverride != "" {
		args = append(args, "--model", modelOverride)
	} else if stage.Model != "" {
		args = append(args, "--model", stage.Model)
	}

	if stage.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", stage.MaxTurns))
	}

	for _, tool := range stage.AllowedTools {
		args = append(args, "--allowedTools", tool)
	}

	return args
}

// claudeResponse represents the JSON output from claude --output-format json.
type claudeResponse struct {
	Result    string  `json:"result"`
	SessionID string  `json:"session_id"`
	NumTurns  int     `json:"num_turns"`
	CostUSD   float64 `json:"total_cost_usd"`
	IsError   bool    `json:"is_error"`
}

func runClaude(ctx context.Context, args []string, prompt string, workDir string, issueNumber int, label string) (string, bool, error) {
	logf(issueNumber, "claude", "invoking (%s) in %s\n", label, workDir)

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stderr = os.Stderr

	output, err := cmd.Output()
	if err != nil {
		// Try to parse JSON even on error (max_turns produces exit code 1 but valid JSON)
		if text, sessionID, turns, cost := parseClaudeJSON(output); text != "" {
			logf(issueNumber, "claude", "used %d turns, $%.4f\n", turns, cost)
			baseName := strings.TrimSuffix(label, "-comment-review")
			saveSessionIDDirect(sessionFile(issueNumber, baseName), sessionID)
			return text, false, fmt.Errorf("claude exited with error: %w", err)
		}
		return string(output), false, fmt.Errorf("claude exited with error: %w", err)
	}

	text, sessionID, turns, cost := parseClaudeJSON(output)
	if text == "" {
		// Fallback: output wasn't valid JSON, use raw
		text = string(output)
	}

	logf(issueNumber, "claude", "completed in %d turns, $%.4f\n", turns, cost)

	// Save session ID for future resumption
	baseName := strings.TrimSuffix(label, "-comment-review")
	saveSessionIDDirect(sessionFile(issueNumber, baseName), sessionID)

	// Simple marker check — callers with the stage use checkCompletion for robustness.
	completed := stageCompleteRE.MatchString(text)

	return text, completed, nil
}

// checkCompletion returns true if Claude's output indicates the stage is complete.
// The only supported type is "claude" (also the default when type is unset).
func checkCompletion(stage *stages.Stage, output string) bool {
	switch stage.Completion.Type {
	case "", "claude":
		return stageCompleteRE.MatchString(output)
	default:
		return false
	}
}

func buildPrompt(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment) string {
	var b strings.Builder

	b.WriteString(stage.Prompt)
	b.WriteString("\n\n---\n\n")
	b.WriteString(fmt.Sprintf("# Issue #%d: %s\n\n", issue.Number, issue.Title))
	b.WriteString(fmt.Sprintf("URL: %s\n\n", issue.URL))
	b.WriteString("## Spec / Issue Body\n\n")
	b.WriteString(issue.Body)
	b.WriteString("\n\n")

	if len(issue.Labels) > 0 {
		b.WriteString("## Labels\n\n")
		b.WriteString(strings.Join(issue.Labels, ", "))
		b.WriteString("\n\n")
	}

	if len(issue.Comments) > 0 {
		b.WriteString("## Prior Discussion\n\n")
		for _, c := range issue.Comments {
			b.WriteString(fmt.Sprintf("**@%s** (%s):\n%s\n\n", c.Author, c.CreatedAt.Format("2006-01-02 15:04"), c.Body))
		}
	}

	if len(newComments) > 0 {
		b.WriteString("## New Comments\n\n")
		for _, c := range newComments {
			b.WriteString(fmt.Sprintf("**@%s** (%s):\n%s\n\n", c.Author, c.CreatedAt.Format("2006-01-02 15:04"), c.Body))
		}
	}

	b.WriteString("---\n\n")
	if stage.PostToPR {
		b.WriteString("Your detailed output will be posted on the PR. Provide a brief summary (2-4 sentences)\n")
		b.WriteString("for the issue between these markers:\n\n")
		b.WriteString("FABRIK_SUMMARY_BEGIN\n")
		b.WriteString("(brief summary of findings and actions taken)\n")
		b.WriteString("FABRIK_SUMMARY_END\n\n")
	}
	b.WriteString("When you have completed all work for this stage, end your response with the exact line:\n")
	b.WriteString("FABRIK_STAGE_COMPLETE\n")

	return b.String()
}

func buildCommentReviewPrompt(stage *stages.Stage, item gh.ProjectItem, comments []gh.Comment) string {
	var b strings.Builder

	// Use stage-specific comment prompt if available, otherwise default
	if stage.CommentPrompt != "" {
		b.WriteString(stage.CommentPrompt)
	} else if item.IsPR {
		b.WriteString(defaultPRCommentPrompt())
	} else {
		b.WriteString(defaultCommentPrompt(stage.Name))
	}

	b.WriteString("\n\n---\n\n")

	if item.IsPR {
		b.WriteString(fmt.Sprintf("# PR #%d: %s\n\n", item.Number, item.Title))
		b.WriteString(fmt.Sprintf("URL: %s\n\n", item.URL))
		b.WriteString("## Current PR Description\n\n")
	} else {
		b.WriteString(fmt.Sprintf("# Issue #%d: %s\n\n", item.Number, item.Title))
		b.WriteString(fmt.Sprintf("URL: %s\n\n", item.URL))
		b.WriteString("## Current Issue Body\n\n")
	}
	b.WriteString(item.Body)
	b.WriteString("\n\n")

	b.WriteString("## New Comments to Process\n\n")
	for _, c := range comments {
		b.WriteString(fmt.Sprintf("**@%s** (%s):\n%s\n\n", c.Author, c.CreatedAt.Format("2006-01-02 15:04"), c.Body))
	}

	b.WriteString("---\n\n")
	b.WriteString("First, perform any actions requested in the comments using available tools.\n")
	if item.IsPR {
		b.WriteString("Then, if the PR description needs updating, output the complete updated PR description between these exact markers:\n\n")
	} else {
		b.WriteString("Then, if the issue body needs updating, output the complete updated issue body between these exact markers:\n\n")
	}
	b.WriteString("FABRIK_ISSUE_UPDATE_BEGIN\n")
	if item.IsPR {
		b.WriteString("(the full updated PR description goes here)\n")
	} else {
		b.WriteString("(the full updated issue body goes here)\n")
	}
	b.WriteString("FABRIK_ISSUE_UPDATE_END\n\n")
	if item.IsPR {
		b.WriteString("Include the ENTIRE PR description in your update, not just the changed parts.\n")
		b.WriteString("If no PR description changes are needed, you may omit the markers.\n")
	} else {
		b.WriteString("Include the ENTIRE issue body in your update, not just the changed parts.\n")
		b.WriteString("If no issue body changes are needed, you may omit the markers.\n")
	}

	return b.String()
}

func defaultCommentPrompt(stageName string) string {
	return fmt.Sprintf(`You are a comment review agent for the "%s" stage.
The user has posted new comments on this issue. Your job is to:
1. Read and understand the new comments in context of the current issue body.
2. If comments request actions (e.g., linking a pull request, running a command, making code changes), perform those actions using available tools.
3. If comments provide information, corrections, or answers to questions, incorporate them into the issue body.
4. Preserve all existing content that is still valid.
5. Maintain the structure and formatting of the issue body.`, stageName)
}

func defaultPRCommentPrompt() string {
	return `You are a PR comment review agent.
New comments have been posted on this pull request. These may include:
- Review feedback from humans or automated bots (e.g., GitHub Copilot, Gemini code review)
- Requests for code changes or clarifications
- Suggestions for improving the PR description

Your job is to:
1. Read and understand the new comments in context of the current PR description and code changes.
2. Make any requested code changes in the checked-out worktree/issue branch, following the existing fabrik workflow.
3. Update the PR description as needed to reflect the current state of the changes.
4. Respond to review feedback by addressing the concerns raised.
5. If comments from automated review bots suggest improvements, evaluate and apply them where appropriate.
6. Preserve all existing PR description content that is still valid.
7. Maintain the structure and formatting of the PR description.`
}

// extractBetweenMarkers extracts content between a BEGIN/END marker pair.
// Returns empty string if markers are not found.
func extractBetweenMarkers(output, beginMarker, endMarker string) string {
	beginIdx := strings.Index(output, beginMarker)
	if beginIdx == -1 {
		return ""
	}

	// Move past the marker and any trailing newline
	bodyStart := beginIdx + len(beginMarker)
	if bodyStart < len(output) && output[bodyStart] == '\n' {
		bodyStart++
	}

	endIdx := strings.Index(output[bodyStart:], endMarker)
	if endIdx == -1 {
		return ""
	}

	body := output[bodyStart : bodyStart+endIdx]
	return strings.TrimSpace(body)
}

// extractUpdatedBody parses the updated issue/PR body from Claude's output.
func extractUpdatedBody(output string) string {
	return extractBetweenMarkers(output, "FABRIK_ISSUE_UPDATE_BEGIN", "FABRIK_ISSUE_UPDATE_END")
}

// extractSummary parses a brief summary from Claude's output.
func extractSummary(output string) string {
	return extractBetweenMarkers(output, "FABRIK_SUMMARY_BEGIN", "FABRIK_SUMMARY_END")
}

// parseClaudeJSON parses the JSON output from claude --output-format json.
// Returns the text result, session ID, turn count, and cost.
// Returns empty strings/zero values if parsing fails.
func parseClaudeJSON(output []byte) (text, sessionID string, turns int, cost float64) {
	var resp claudeResponse
	if err := json.Unmarshal(output, &resp); err != nil {
		return "", "", 0, 0
	}
	return resp.Result, resp.SessionID, resp.NumTurns, resp.CostUSD
}

// saveSessionIDDirect saves a known session ID to disk for future resumption.
func saveSessionIDDirect(path, sessionID string) {
	if sessionID == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to create session dir for %s: %v\n", path, err)
		return
	}
	if err := os.WriteFile(path, []byte(sessionID), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to save session id to %s: %v\n", path, err)
	}
}
