package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

// SessionDir returns the directory where Claude sessions are cached for an issue.
func SessionDir(issueNumber int) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fabrik", "sessions", fmt.Sprintf("issue-%d", issueNumber))
}

// sessionFile returns the path to the session ID file for a given issue+stage.
func sessionFile(issueNumber int, stageName string) string {
	return filepath.Join(SessionDir(issueNumber), stageName+".session")
}

// InvokeClaude runs Claude Code with the given stage configuration and issue context.
// workDir is the directory Claude should run in (typically a git worktree).
// It returns Claude's output and whether Claude indicated completion.
func InvokeClaude(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string) (string, bool, error) {
	sessDir := SessionDir(issue.Number)
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		return "", false, fmt.Errorf("creating session dir: %w", err)
	}

	prompt := buildPrompt(stage, issue, newComments)
	args := buildClaudeArgs(stage, issue.Number, resume)
	args = append(args, prompt)

	return runClaude(args, workDir, issue.Number, stage.Name)
}

// InvokeClaudeForComments runs Claude Code with a comment-review prompt.
// It uses the stage's CommentPrompt if defined, otherwise a default.
func InvokeClaudeForComments(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string) (string, bool, error) {
	sessDir := SessionDir(issue.Number)
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		return "", false, fmt.Errorf("creating session dir: %w", err)
	}

	prompt := buildCommentReviewPrompt(stage, issue, comments)
	args := buildClaudeArgs(stage, issue.Number, true) // resume existing session
	args = append(args, prompt)

	return runClaude(args, workDir, issue.Number, stage.Name+"-comment-review")
}

func buildClaudeArgs(stage *stages.Stage, issueNumber int, resume bool) []string {
	args := []string{
		"--print",
		"--verbose",
	}

	sessFile := sessionFile(issueNumber, stage.Name)
	if resume {
		if sessionID, err := os.ReadFile(sessFile); err == nil && len(sessionID) > 0 {
			args = append(args, "--resume", strings.TrimSpace(string(sessionID)))
		}
	}

	if stage.Model != "" {
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

func runClaude(args []string, workDir string, issueNumber int, label string) (string, bool, error) {
	fmt.Printf("  [claude] invoking for issue #%d (%s) in %s\n", issueNumber, label, workDir)

	cmd := exec.Command("claude", args...)
	cmd.Dir = workDir
	cmd.Stderr = os.Stderr

	output, err := cmd.Output()
	if err != nil {
		return string(output), false, fmt.Errorf("claude exited with error: %w", err)
	}

	result := string(output)

	// Try to save session ID for future resumption
	// Use the base stage name (strip "-comment-review" suffix) for session continuity
	baseName := strings.TrimSuffix(label, "-comment-review")
	saveSessionID(sessionFile(issueNumber, baseName), result)

	completed := strings.Contains(result, "FABRIK_STAGE_COMPLETE")

	return result, completed, nil
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

	if len(newComments) > 0 {
		b.WriteString("## New Comments (user feedback)\n\n")
		for _, c := range newComments {
			b.WriteString(fmt.Sprintf("**@%s** (%s):\n%s\n\n", c.Author, c.CreatedAt.Format("2006-01-02 15:04"), c.Body))
		}
	}

	b.WriteString("---\n\n")
	b.WriteString("When you have completed all work for this stage, end your response with the exact line:\n")
	b.WriteString("FABRIK_STAGE_COMPLETE\n")

	return b.String()
}

func buildCommentReviewPrompt(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment) string {
	var b strings.Builder

	// Use stage-specific comment prompt if available, otherwise default
	if stage.CommentPrompt != "" {
		b.WriteString(stage.CommentPrompt)
	} else {
		b.WriteString(defaultCommentPrompt(stage.Name))
	}

	b.WriteString("\n\n---\n\n")
	b.WriteString(fmt.Sprintf("# Issue #%d: %s\n\n", issue.Number, issue.Title))
	b.WriteString(fmt.Sprintf("URL: %s\n\n", issue.URL))
	b.WriteString("## Current Issue Body\n\n")
	b.WriteString(issue.Body)
	b.WriteString("\n\n")

	b.WriteString("## New Comments to Process\n\n")
	for _, c := range comments {
		b.WriteString(fmt.Sprintf("**@%s** (%s):\n%s\n\n", c.Author, c.CreatedAt.Format("2006-01-02 15:04"), c.Body))
	}

	b.WriteString("---\n\n")
	b.WriteString("IMPORTANT: You must output the complete updated issue body between these exact markers:\n\n")
	b.WriteString("FABRIK_ISSUE_UPDATE_BEGIN\n")
	b.WriteString("(the full updated issue body goes here)\n")
	b.WriteString("FABRIK_ISSUE_UPDATE_END\n\n")
	b.WriteString("Include the ENTIRE issue body in your update, not just the changed parts.\n")
	b.WriteString("Incorporate the information from the comments into the appropriate sections.\n")

	return b.String()
}

func defaultCommentPrompt(stageName string) string {
	return fmt.Sprintf(`You are a comment review agent for the "%s" stage.
The user has posted new comments on this issue. Your job is to:
1. Read and understand the new comments in context of the current issue body.
2. Incorporate the information from the comments into the issue body.
3. If comments answer questions, update the issue body to reflect the answers and remove the questions.
4. If comments provide corrections or clarifications, update the relevant sections.
5. Preserve all existing content that is still valid.
6. Maintain the structure and formatting of the issue body.`, stageName)
}

// extractUpdatedBody parses the updated issue body from Claude's output.
// Looks for content between FABRIK_ISSUE_UPDATE_BEGIN and FABRIK_ISSUE_UPDATE_END markers.
func extractUpdatedBody(output string) string {
	const beginMarker = "FABRIK_ISSUE_UPDATE_BEGIN"
	const endMarker = "FABRIK_ISSUE_UPDATE_END"

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

// saveSessionID attempts to extract a session ID from Claude's output.
func saveSessionID(path string, output string) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.Contains(line, "session_id") {
			var meta struct {
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal([]byte(line), &meta); err == nil && meta.SessionID != "" {
				_ = os.WriteFile(path, []byte(meta.SessionID), 0644)
				return
			}
		}
	}
}
