package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

var stageCompleteRE = regexp.MustCompile(`(?m)^FABRIK_STAGE_COMPLETE\r?$`)

var (
	tmuxOnce      sync.Once
	tmuxAvailBool bool
)

// tmuxAvailable reports whether tmux is on PATH. Result is cached after first call.
func tmuxAvailable() bool {
	tmuxOnce.Do(func() {
		_, err := exec.LookPath("tmux")
		tmuxAvailBool = err == nil
	})
	return tmuxAvailBool
}

// sanitizeTmuxName returns a tmux-safe session name of the form fabrik-N-stage.
// Non-alphanumeric characters (except hyphens) are replaced with hyphens;
// consecutive and leading/trailing hyphens are collapsed.
func sanitizeTmuxName(issueNumber int, stageName string) string {
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, stageName)
	safe = strings.ToLower(safe)
	for strings.Contains(safe, "--") {
		safe = strings.ReplaceAll(safe, "--", "-")
	}
	safe = strings.Trim(safe, "-")
	if safe == "" {
		safe = "stage"
	}
	return fmt.Sprintf("fabrik-%d-%s", issueNumber, safe)
}

// shellQuote returns s enclosed in single quotes, safe for POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// claudeLogf is the logging function used by runClaude. Set by the Engine
// during construction to route output through the event channel in TUI mode.
// Falls back to stderr when nil (e.g. in tests).
var claudeLogf func(issueNumber int, tag, format string, args ...any)

// claudeTUI indicates whether the TUI is active. When true, Claude's child
// process stderr is sent only to the log file (not the terminal).
var claudeTUI bool

func claudeLog(issueNumber int, tag, format string, args ...any) {
	if claudeLogf != nil {
		claudeLogf(issueNumber, tag, format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "[#%d %s] "+format, append([]any{issueNumber, tag}, args...)...)
}

// TokenUsage holds token consumption data from a single Claude invocation.
type TokenUsage struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	CostUSD             float64
	TurnsUsed           int
	MaxTurns            int
}

// add returns a new TokenUsage that is the sum of t and other.
func (t TokenUsage) add(other TokenUsage) TokenUsage {
	return TokenUsage{
		InputTokens:         t.InputTokens + other.InputTokens,
		OutputTokens:        t.OutputTokens + other.OutputTokens,
		CacheCreationTokens: t.CacheCreationTokens + other.CacheCreationTokens,
		CacheReadTokens:     t.CacheReadTokens + other.CacheReadTokens,
		CostUSD:             t.CostUSD + other.CostUSD,
	}
}

// saveDebugLog writes Claude's output for a stage invocation to
// .fabrik/debug/issue-{N}_{epoch}_{label}.log in the current working directory.
// Errors are non-fatal — a warning is printed to stderr.
func saveDebugLog(issueNumber int, label string, output string) {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] saveDebugLog: getting cwd: %v\n", err)
		return
	}
	debugDir := filepath.Join(cwd, ".fabrik", "debug")
	if err := os.MkdirAll(debugDir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] saveDebugLog: creating debug dir: %v\n", err)
		return
	}
	// Sanitize label for use in filename.
	safe := filepath.Base(label)
	if safe == "" || safe == "." || safe == string(filepath.Separator) {
		safe = "stage"
	}
	name := fmt.Sprintf("issue-%d_%d_%s.log", issueNumber, time.Now().Unix(), safe)
	path := filepath.Join(debugDir, name)
	if err := os.WriteFile(path, []byte(output), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] saveDebugLog: writing %s: %v\n", path, err)
	}
}

// SessionDir returns the directory where Claude sessions are cached for an issue.
func SessionDir(issueNumber int) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fabrik", "sessions", fmt.Sprintf("issue-%d", issueNumber))
}

// LogDir returns the directory where Claude session logs are stored for an issue.
func LogDir(issueNumber int) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fabrik", "logs", fmt.Sprintf("issue-%d", issueNumber))
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
// noTmux skips tmux wrapping and runs Claude directly (e.g. for CI environments).
// It returns Claude's output, whether Claude indicated completion, and token usage.
func InvokeClaude(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string, noTmux bool) (string, bool, TokenUsage, error) {
	sessDir := SessionDir(issue.Number)
	if err := os.MkdirAll(sessDir, 0700); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("creating session dir: %w", err)
	}
	if err := os.Chmod(sessDir, 0700); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("setting session dir permissions: %w", err)
	}

	prompt := buildPrompt(stage, issue, newComments)
	args := buildClaudeArgs(stage, issue.Number, resume, modelOverride)

	output, _, usage, err := runClaude(ctx, args, prompt, workDir, issue.Number, stage.Name, noTmux)
	usage.MaxTurns = stage.MaxTurns
	if err != nil {
		return output, false, usage, err
	}
	return output, checkCompletion(stage, output), usage, nil
}

// InvokeClaudeForComments runs Claude Code with a comment-review prompt.
// It uses the stage's CommentPrompt if defined, otherwise a default.
// modelOverride, if non-empty, replaces the stage's configured model.
// noTmux skips tmux wrapping and runs Claude directly.
func InvokeClaudeForComments(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, modelOverride string, noTmux bool) (string, bool, TokenUsage, error) {
	sessDir := SessionDir(issue.Number)
	if err := os.MkdirAll(sessDir, 0700); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("creating session dir: %w", err)
	}
	if err := os.Chmod(sessDir, 0700); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("setting session dir permissions: %w", err)
	}

	prompt := buildCommentReviewPrompt(stage, issue, comments)
	args := buildClaudeArgs(stage, issue.Number, true, modelOverride) // resume existing session

	output, completed, usage, err := runClaude(ctx, args, prompt, workDir, issue.Number, stage.Name+"-comment-review", noTmux)
	usage.MaxTurns = stage.MaxTurns
	return output, completed, usage, err
}

func buildClaudeArgs(stage *stages.Stage, issueNumber int, resume bool, modelOverride string) []string {
	args := []string{
		"--output-format", "json",
		"--verbose",
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
	// ModelUsage contains per-model accumulated token counts for the full session.
	// These are more accurate than the top-level "usage" field, which reflects only
	// the last API call rather than the entire multi-turn session.
	ModelUsage map[string]struct {
		InputTokens         int `json:"inputTokens"`
		OutputTokens        int `json:"outputTokens"`
		CacheCreationTokens int `json:"cacheCreationInputTokens"`
		CacheReadTokens     int `json:"cacheReadInputTokens"`
	} `json:"modelUsage"`
	// Usage is the per-request token count, used as fallback when ModelUsage is absent.
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func runClaude(ctx context.Context, args []string, prompt string, workDir string, issueNumber int, label string, noTmux bool) (string, bool, TokenUsage, error) {
	claudeLog(issueNumber, "claude", "invoking (%s) in %s\n", label, workDir)

	if !noTmux && tmuxAvailable() {
		sessionName := sanitizeTmuxName(issueNumber, label)
		output, completed, usage, err := runClaudeInTmux(ctx, args, prompt, workDir, issueNumber, label, sessionName)
		if err == nil || ctx.Err() != nil {
			return output, completed, usage, err
		}
		claudeLog(issueNumber, "warn", "tmux session failed (%v); falling back to direct execution\n", err)
	} else if !noTmux {
		claudeLog(issueNumber, "warn", "tmux not found on PATH; running Claude directly\n")
	}

	// Set up stderr: in TUI mode, only to log file; in plain mode, tee to os.Stderr + log file.
	var stderrWriter io.Writer
	if claudeTUI {
		stderrWriter = io.Discard
	} else {
		stderrWriter = os.Stderr
	}
	logDir := LogDir(issueNumber)
	if err := os.MkdirAll(logDir, 0700); err != nil {
		claudeLog(issueNumber, "warn", "could not create log dir: %v\n", err)
	} else if err := os.Chmod(logDir, 0700); err != nil {
		claudeLog(issueNumber, "warn", "could not set log dir permissions: %v\n", err)
	} else {
		safeLabel := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "-").Replace(label)
		now := time.Now().UTC()
		logPath := filepath.Join(logDir, fmt.Sprintf("%s-%s-%d.log", safeLabel, now.Format("20060102-150405"), now.UnixNano()))
		if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600); err != nil {
			claudeLog(issueNumber, "warn", "could not create log file %s: %v\n", logPath, err)
		} else {
			defer logFile.Close()
			if claudeTUI {
				stderrWriter = logFile
			} else {
				stderrWriter = io.MultiWriter(os.Stderr, logFile)
			}
		}
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stderr = stderrWriter

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	runErr := cmd.Run()
	rawOutput := stdout.Bytes()

	baseName := strings.TrimSuffix(label, "-comment-review")

	resp, ok := parseClaudeJSON(bytes.TrimSpace(rawOutput))
	var text string
	var usage TokenUsage
	if ok {
		text = resp.Result
		usage = tokenUsageFromResponse(resp)
		if runErr != nil {
			claudeLog(issueNumber, "claude", "used %d turns, $%.4f\n", resp.NumTurns, resp.CostUSD)
		} else {
			claudeLog(issueNumber, "claude", "completed in %d turns, $%.4f\n", resp.NumTurns, resp.CostUSD)
		}
		saveSessionIDDirect(sessionFile(issueNumber, baseName), resp.SessionID)
	} else {
		claudeLog(issueNumber, "warn", "JSON parse failed (%d bytes); output not posted\n", len(rawOutput))
		text = fmt.Sprintf("⚠️ Claude output could not be parsed (raw output was %d bytes). Check logs at `~/.fabrik/logs/issue-%d/` for details.", len(rawOutput), issueNumber)
	}

	if runErr != nil {
		return text, false, usage, fmt.Errorf("claude exited with error: %w", runErr)
	}

	completed := stageCompleteRE.MatchString(text)
	return text, completed, usage, nil
}

// runClaudeInTmux runs Claude inside a named detached tmux session so the user
// can observe it with `tmux attach -t <sessionName>`. The prompt is written to a
// temp file (stdin redirect) and stdout is captured to another temp file for
// post-run parsing. Stderr stays in the tmux pane. The exit code is written to a
// third temp file so errors are propagated correctly.
func runClaudeInTmux(ctx context.Context, args []string, prompt string, workDir string, issueNumber int, label string, sessionName string) (string, bool, TokenUsage, error) {
	claudeLog(issueNumber, "claude", "starting tmux session %q (attach: tmux attach -t %s)\n", sessionName, sessionName)

	// Write prompt to temp file (tmux can't receive stdin from the Go process).
	promptFile, err := os.CreateTemp("", "fabrik-prompt-*")
	if err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("creating prompt temp file: %w", err)
	}
	promptPath := promptFile.Name()
	defer os.Remove(promptPath)
	if _, err := promptFile.WriteString(prompt); err != nil {
		promptFile.Close()
		return "", false, TokenUsage{}, fmt.Errorf("writing prompt: %w", err)
	}
	promptFile.Close()

	// Create output temp file.
	outputFile, err := os.CreateTemp("", "fabrik-output-*")
	if err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("creating output temp file: %w", err)
	}
	outputPath := outputFile.Name()
	outputFile.Close()
	defer os.Remove(outputPath)

	// Create exit-code temp file.
	exitFile, err := os.CreateTemp("", "fabrik-exit-*")
	if err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("creating exit temp file: %w", err)
	}
	exitPath := exitFile.Name()
	exitFile.Close()
	defer os.Remove(exitPath)

	// Write a shell script so we can pass args without shell-quoting headaches
	// and capture Claude's exit code.
	scriptFile, err := os.CreateTemp("", "fabrik-tmux-*.sh")
	if err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("creating tmux script: %w", err)
	}
	scriptPath := scriptFile.Name()
	defer os.Remove(scriptPath)

	// Resolve the claude binary path so the script works even when claude is
	// not available as a plain name inside the tmux shell (e.g. in PATH-restricted
	// or function-based environments).
	claudeBin := "claude"
	if resolved, err := exec.LookPath("claude"); err == nil {
		claudeBin = resolved
	}

	// In tmux mode, use stream-json so the pane shows real-time activity.
	// stdout is tee'd: raw stream-json goes to the capture file for parsing,
	// and a human-readable view is piped through `fabrik _stream-filter` for
	// the tmux pane.
	tmuxArgs := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "json" && len(tmuxArgs) > 0 && tmuxArgs[len(tmuxArgs)-1] == "--output-format" {
			arg = "stream-json"
		}
		tmuxArgs = append(tmuxArgs, arg)
	}

	// Resolve the fabrik binary for the stream filter (must be absolute since
	// tmux runs the script from a different working directory).
	fabrikBin, err := os.Executable()
	if err != nil {
		fabrikBin = os.Args[0]
	}

	var sb strings.Builder
	sb.WriteString("#!/bin/sh\n")
	sb.WriteString("cd " + shellQuote(workDir) + " || exit 1\n")
	sb.WriteString(shellQuote(claudeBin))
	for _, arg := range tmuxArgs {
		sb.WriteString(" " + shellQuote(arg))
	}
	sb.WriteString(" < " + shellQuote(promptPath))
	sb.WriteString(" | tee " + shellQuote(outputPath))
	sb.WriteString(" | " + shellQuote(fabrikBin) + " _stream-filter\n")
	sb.WriteString("echo ${PIPESTATUS[0]:-$?} > " + shellQuote(exitPath) + "\n")

	if _, err := scriptFile.WriteString(sb.String()); err != nil {
		scriptFile.Close()
		return "", false, TokenUsage{}, fmt.Errorf("writing tmux script: %w", err)
	}
	scriptFile.Close()
	if err := os.Chmod(scriptPath, 0700); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("chmod tmux script: %w", err)
	}

	// Kill any pre-existing session with this name (leftover from a previous interrupted run).
	// Use "=<name>" for exact-match to avoid accidentally killing a differently-named session.
	exec.Command("tmux", "kill-session", "-t", "="+sessionName).Run() //nolint:errcheck

	// Start detached tmux session running the script.
	if err := exec.Command("tmux", "new-session", "-d", "-s", sessionName, scriptPath).Run(); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("starting tmux session %q: %w", sessionName, err)
	}

	// Poll until the session exits, respecting context cancellation.
	// Use "=<name>" to force exact-match in tmux has-session (avoids prefix
	// collisions, e.g. fabrik-7 matching fabrik-74).
	exactTarget := "=" + sessionName
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			exec.Command("tmux", "kill-session", "-t", exactTarget).Run() //nolint:errcheck
			return "", false, TokenUsage{}, ctx.Err()
		case <-ticker.C:
			// Use CommandContext so the poll call itself respects cancellation.
			if err := exec.CommandContext(ctx, "tmux", "has-session", "-t", exactTarget).Run(); err != nil {
				// Session is gone — Claude finished.
				goto sessionDone
			}
		}
	}

sessionDone:
	// Read Claude's output.
	rawOutput, err := os.ReadFile(outputPath)
	if err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("reading tmux output: %w", err)
	}

	// Determine exit code. An empty or missing exit file means Claude was killed
	// before the script could write the exit code — treat that as an error.
	var runErr error
	exitBytes, exitReadErr := os.ReadFile(exitPath)
	if exitReadErr != nil || strings.TrimSpace(string(exitBytes)) == "" {
		runErr = fmt.Errorf("claude exited abnormally (no exit code written)")
	} else if code := strings.TrimSpace(string(exitBytes)); code != "0" {
		runErr = fmt.Errorf("exit status %s", code)
	}

	baseName := strings.TrimSuffix(label, "-comment-review")
	resp, ok := parseClaudeJSON(bytes.TrimSpace(rawOutput))
	var result string
	var usage TokenUsage
	if ok {
		result = resp.Result
		usage = tokenUsageFromResponse(resp)
		if runErr != nil {
			claudeLog(issueNumber, "claude", "used %d turns, $%.4f\n", resp.NumTurns, resp.CostUSD)
		} else {
			claudeLog(issueNumber, "claude", "completed in %d turns, $%.4f\n", resp.NumTurns, resp.CostUSD)
		}
		saveSessionIDDirect(sessionFile(issueNumber, baseName), resp.SessionID)
	} else {
		claudeLog(issueNumber, "warn", "JSON parse failed (%d bytes); output not posted\n", len(rawOutput))
		result = fmt.Sprintf("⚠️ Claude output could not be parsed (raw output was %d bytes). Check logs at `~/.fabrik/logs/issue-%d/` for details.", len(rawOutput), issueNumber)
	}

	if runErr != nil {
		return result, false, usage, fmt.Errorf("claude exited with error: %w", runErr)
	}

	completed := stageCompleteRE.MatchString(result)
	return result, completed, usage, nil
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

// formatStatsFooter returns a one-line stats summary suitable for appending to a comment.
// Returns empty string when no stats are available (e.g. JSON parse fallback).
func formatStatsFooter(usage TokenUsage, completed bool) string {
	if usage.TurnsUsed == 0 && usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return ""
	}
	var completion string
	if !completed {
		completion = " Stage incomplete."
	}
	if usage.MaxTurns > 0 {
		return fmt.Sprintf("\n\n---\nUsed %d/%d turns, %dk input / %dk output tokens.%s",
			usage.TurnsUsed, usage.MaxTurns, usage.InputTokens/1000, usage.OutputTokens/1000, completion)
	}
	return fmt.Sprintf("\n\n---\nUsed %d turns, %dk input / %dk output tokens.%s",
		usage.TurnsUsed, usage.InputTokens/1000, usage.OutputTokens/1000, completion)
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

// parseClaudeJSON parses the JSON output from claude --output-format json
// or --output-format stream-json.
// Handles three formats:
//   - Single result object: {"result": "...", "session_id": "...", ...}
//   - Conversation array: [{"type":"system",...}, ..., {"type":"result","result":"..."}]
//   - NDJSON (stream-json): one JSON object per line, last "result" line has the response
func parseClaudeJSON(output []byte) (claudeResponse, bool) {
	// Try single-object format first.
	var resp claudeResponse
	if err := json.Unmarshal(output, &resp); err == nil && resp.Result != "" {
		return resp, true
	}

	// Try JSON array format.
	var messages []json.RawMessage
	if err := json.Unmarshal(output, &messages); err == nil && len(messages) > 0 {
		for i := len(messages) - 1; i >= 0; i-- {
			if found, ok := tryParseResultMessage(messages[i]); ok {
				return found, true
			}
		}
	}

	// Try NDJSON (stream-json): one JSON object per line.
	lines := bytes.Split(output, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		if found, ok := tryParseResultMessage(line); ok {
			return found, true
		}
	}

	return claudeResponse{}, false
}

// tryParseResultMessage checks if raw JSON is a "result" type message and
// returns the parsed claudeResponse if so.
func tryParseResultMessage(raw []byte) (claudeResponse, bool) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Type != "result" {
		return claudeResponse{}, false
	}
	var resp claudeResponse
	if err := json.Unmarshal(raw, &resp); err == nil && resp.Result != "" {
		return resp, true
	}
	return claudeResponse{}, false
}

// tokenUsageFromResponse converts a claudeResponse to TokenUsage.
// Token counts are summed across all models in ModelUsage for accuracy;
// CostUSD comes from the top-level total_cost_usd field.
// Falls back to the per-request Usage field when ModelUsage is absent.
func tokenUsageFromResponse(resp claudeResponse) TokenUsage {
	usage := TokenUsage{CostUSD: resp.CostUSD, TurnsUsed: resp.NumTurns}
	for _, m := range resp.ModelUsage {
		usage.InputTokens += m.InputTokens
		usage.OutputTokens += m.OutputTokens
		usage.CacheCreationTokens += m.CacheCreationTokens
		usage.CacheReadTokens += m.CacheReadTokens
	}
	// Fall back to the per-request Usage field when ModelUsage is absent.
	if len(resp.ModelUsage) == 0 {
		usage.InputTokens = resp.Usage.InputTokens
		usage.OutputTokens = resp.Usage.OutputTokens
	}
	return usage
}

// saveSessionIDDirect saves a known session ID to disk for future resumption.
func saveSessionIDDirect(path, sessionID string) {
	if sessionID == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		claudeLog(0, "warn", "failed to create session dir for %s: %v\n", path, err)
		return
	}
	if err := os.WriteFile(path, []byte(sessionID), 0600); err != nil {
		claudeLog(0, "warn", "failed to save session id to %s: %v\n", path, err)
	}
}
