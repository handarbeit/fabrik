package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
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

	// Build the prompt
	prompt := buildPrompt(stage, issue, newComments)

	// Build claude command args
	args := []string{
		"--print",    // non-interactive, print output
		"--verbose",  // include metadata
	}

	// Resume session if we have one
	sessFile := sessionFile(issue.Number, stage.Name)
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

	// Add the prompt as the final argument
	args = append(args, prompt)

	fmt.Printf("  [claude] invoking for issue #%d stage %q in %s\n", issue.Number, stage.Name, workDir)

	cmd := exec.Command("claude", args...)
	cmd.Dir = workDir
	cmd.Stderr = os.Stderr

	output, err := cmd.Output()
	if err != nil {
		return string(output), false, fmt.Errorf("claude exited with error: %w", err)
	}

	result := string(output)

	// Try to extract and save session ID from output for future resumption
	saveSessionID(sessFile, result)

	// Check if Claude indicated completion
	completed := checkCompletion(stage, result)

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

func checkCompletion(stage *stages.Stage, output string) bool {
	switch stage.Completion.Type {
	case "claude":
		return strings.Contains(output, "FABRIK_STAGE_COMPLETE")
	default:
		// For other types, completion is checked externally
		return false
	}
}

// saveSessionID attempts to extract a session ID from Claude's output.
// Claude Code --print with --verbose includes a JSON metadata line.
func saveSessionID(path string, output string) {
	// Look for session metadata in output
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
