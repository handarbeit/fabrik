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
	"strconv"
	"strings"
	"syscall"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

var stageCompleteRE = regexp.MustCompile(`(?m)^FABRIK_STAGE_COMPLETE\r?$`)
var blockedOnInputRE = regexp.MustCompile(`(?m)^FABRIK_BLOCKED_ON_INPUT\r?$`)
var decomposedRE = regexp.MustCompile(`(?m)^FABRIK_DECOMPOSED\r?$`)

// CheckBlockedOnInput reports whether output contains the FABRIK_BLOCKED_ON_INPUT marker.
func CheckBlockedOnInput(output string) bool {
	return blockedOnInputRE.MatchString(output)
}

// CheckDecomposed reports whether output contains the FABRIK_DECOMPOSED marker.
// This marker signals that the Plan stage split the issue into sub-issues and
// the parent should be moved directly to Done, bypassing remaining pipeline stages.
func CheckDecomposed(output string) bool {
	return decomposedRE.MatchString(output)
}

// claudeLogf is the logging function used by runClaude. Set by the Engine
// during construction to route output through the event channel in TUI mode.
// Falls back to stderr when nil (e.g. in tests).
var claudeLogf func(issueNumber int, tag, format string, args ...any)

// claudeTUI indicates whether the TUI is active. When true, Claude's child
// process stderr is sent only to the log file (not the terminal).
var claudeTUI bool

// claudePluginDir is the path to the Fabrik plugin directory. Set by the Engine
// during construction. When non-empty, --plugin-dir is added to Claude args.
var claudePluginDir string

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
	name := fmt.Sprintf("issue-%d_%d_%s.log", issueNumber, time.Now().UnixNano(), safe)
	path := filepath.Join(debugDir, name)
	if err := os.WriteFile(path, []byte(output), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] saveDebugLog: writing %s: %v\n", path, err)
	}
}

// SessionDir returns the directory where Claude sessions are cached for an issue.
// The path is <cwd>/.fabrik/sessions/issue-N/ for single-repo projects.
// Use sessionDirForItem for multi-repo-aware paths.
func SessionDir(issueNumber int) string {
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, ".fabrik", "sessions", fmt.Sprintf("issue-%d", issueNumber))
}

// sessionDirForItem returns the session directory for an issue, namespaced by
// repo when item.Repo is set (multi-repo mode). The path is:
//   - single-repo: <cwd>/.fabrik/sessions/issue-N/
//   - multi-repo:  <cwd>/.fabrik/sessions/<owner>-<repo>/issue-N/
func sessionDirForItem(issue gh.ProjectItem) string {
	cwd, _ := os.Getwd()
	issuePart := fmt.Sprintf("issue-%d", issue.Number)
	if issue.Repo == "" {
		return filepath.Join(cwd, ".fabrik", "sessions", issuePart)
	}
	// Sanitize "owner/repo" → "owner-repo" for use as a directory name.
	repoPart := strings.ReplaceAll(issue.Repo, "/", "-")
	return filepath.Join(cwd, ".fabrik", "sessions", repoPart, issuePart)
}

// LogDir returns the directory where Claude session logs are stored for an issue.
func LogDir(issueNumber int) string {
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, ".fabrik", "logs", fmt.Sprintf("issue-%d", issueNumber))
}

// logDirForItem returns the log directory for an issue, namespaced by repo when
// item.Repo is set (multi-repo mode). The path is:
//   - single-repo: <cwd>/.fabrik/logs/issue-N/
//   - multi-repo:  <cwd>/.fabrik/logs/<owner>-<repo>/issue-N/
func logDirForItem(issue gh.ProjectItem) string {
	cwd, _ := os.Getwd()
	issuePart := fmt.Sprintf("issue-%d", issue.Number)
	if issue.Repo == "" {
		return filepath.Join(cwd, ".fabrik", "logs", issuePart)
	}
	repoPart := strings.ReplaceAll(issue.Repo, "/", "-")
	return filepath.Join(cwd, ".fabrik", "logs", repoPart, issuePart)
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

// ReadSessionID reads the session ID for a given repo, issue, and stage name.
// repo should be "owner/repo" for multi-repo projects, or "" for single-repo.
// Returns the session ID string, or empty string if the file does not exist,
// is unreadable, or is empty.
func ReadSessionID(repo string, issueNumber int, stageName string) string {
	base := filepath.Base(stageName)
	if base == "" || base == "." || base == "/" || base == string(filepath.Separator) {
		base = "default"
	}
	cwd, _ := os.Getwd()
	issuePart := fmt.Sprintf("issue-%d", issueNumber)
	var sessDir string
	if repo == "" {
		sessDir = filepath.Join(cwd, ".fabrik", "sessions", issuePart)
	} else {
		repoPart := strings.ReplaceAll(repo, "/", "-")
		sessDir = filepath.Join(cwd, ".fabrik", "sessions", repoPart, issuePart)
	}
	data, err := os.ReadFile(filepath.Join(sessDir, base+".session"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// InvokeClaude runs Claude Code with the given stage configuration and issue context.
// workDir is the directory Claude should run in (typically a git worktree).
// modelOverride, if non-empty, replaces the stage's configured model.
// It returns Claude's output, whether Claude indicated completion, and token usage.
func InvokeClaude(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, TokenUsage, error) {
	sessDir := sessionDirForItem(issue)
	if err := os.MkdirAll(sessDir, 0700); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("creating session dir: %w", err)
	}
	if err := os.Chmod(sessDir, 0700); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("setting session dir permissions: %w", err)
	}

	sessFilePath := filepath.Join(sessDir, filepath.Base(stage.Name)+".session")
	ld := logDirForItem(issue)

	prompt := buildPrompt(stage, issue, newComments)
	args := buildClaudeArgs(stage, sessFilePath, resume, modelOverride, stage.MaxTurns, hasUnrestrictedLabel(issue), workDir)

	extraEnv := buildClaudeEnv(stage)
	output, completed, usage, err := runClaude(ctx, args, prompt, workDir, issue.Number, stage.Name, sessFilePath, ld, extraEnv)
	usage.MaxTurns = stage.MaxTurns
	if err != nil {
		return output, completed, usage, err
	}
	return output, checkCompletion(stage, output), usage, nil
}

// InvokeClaudeForComments runs Claude Code with a comment-review prompt.
// It uses the stage's CommentPrompt if defined, otherwise a default.
// modelOverride, if non-empty, replaces the stage's configured model.
func InvokeClaudeForComments(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, modelOverride string) (string, bool, TokenUsage, error) {
	sessDir := sessionDirForItem(issue)
	if err := os.MkdirAll(sessDir, 0700); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("creating session dir: %w", err)
	}
	if err := os.Chmod(sessDir, 0700); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("setting session dir permissions: %w", err)
	}

	sessFilePath := filepath.Join(sessDir, filepath.Base(stage.Name)+".session")
	ld := logDirForItem(issue)

	prompt := buildCommentReviewPrompt(stage, issue, comments)
	limit := commentMaxTurns(stage)
	args := buildClaudeArgs(stage, sessFilePath, true, modelOverride, limit, hasUnrestrictedLabel(issue), workDir) // resume existing session

	extraEnv := buildClaudeEnv(stage)
	output, completed, usage, err := runClaude(ctx, args, prompt, workDir, issue.Number, stage.Name+"-comment-review", sessFilePath, ld, extraEnv)
	usage.MaxTurns = limit
	return output, completed, usage, err
}

// commentMaxTurns returns the effective max-turns limit for comment processing.
// If CommentMaxTurns > 0, that value is used explicitly. Otherwise it uses
// the stage's MaxTurns (same budget as a stage run). If both are 0 (unlimited),
// defaults to 50 as a safety cap.
func commentMaxTurns(stage *stages.Stage) int {
	if stage.CommentMaxTurns > 0 {
		return stage.CommentMaxTurns
	}
	if stage.MaxTurns > 0 {
		return stage.MaxTurns
	}
	return 50
}

// buildClaudeEnv returns environment variable overrides to inject into the claude subprocess.
// Fabrik's values are appended after os.Environ() so they take precedence (last-wins semantics).
//
// Defaults (when fields are nil/empty):
//   - CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1 (adaptive thinking disabled)
//   - CLAUDE_CODE_EFFORT_LEVEL=high (high thinking effort)
func buildClaudeEnv(stage *stages.Stage) []string {
	var env []string
	// Disable adaptive thinking by default (nil = disabled).
	if stage.DisableAdaptiveThinking == nil || *stage.DisableAdaptiveThinking {
		env = append(env, "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1")
	}
	// Set effort level; empty string = high.
	level := stage.EffortLevel
	if level == "" {
		level = "high"
	}
	env = append(env, "CLAUDE_CODE_EFFORT_LEVEL="+level)
	return env
}

func buildClaudeArgs(stage *stages.Stage, sessFilePath string, resume bool, modelOverride string, maxTurns int, unrestricted bool, workDir string) []string {
	args := []string{
		"--output-format", "stream-json",
		"--verbose",
	}

	if unrestricted {
		args = append(args, "--dangerously-skip-permissions")
	}

	if claudePluginDir != "" {
		args = append(args, "--plugin-dir", claudePluginDir)
	}

	if resume {
		if sessionID, err := os.ReadFile(sessFilePath); err == nil && len(sessionID) > 0 {
			args = append(args, "--resume", strings.TrimSpace(string(sessionID)))
		}
	}

	// Model override from labels takes precedence over stage config
	if modelOverride != "" {
		args = append(args, "--model", modelOverride)
	} else if stage.Model != "" {
		args = append(args, "--model", stage.Model)
	}

	if maxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", maxTurns))
	}

	for _, tool := range stage.AllowedTools {
		args = append(args, "--allowedTools", tool)
	}

	return args
}

// claudeResponse represents the JSON output from claude --output-format json.
type claudeResponse struct {
	Result    string   `json:"result"`
	SessionID string   `json:"session_id"`
	NumTurns  int      `json:"num_turns"`
	CostUSD   float64  `json:"total_cost_usd"`
	IsError   bool     `json:"is_error"`
	Errors    []string `json:"errors"`
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

func runClaude(ctx context.Context, args []string, prompt string, workDir string, issueNumber int, label string, sessFilePath string, logDir string, extraEnv []string) (string, bool, TokenUsage, error) {
	claudeLog(issueNumber, "claude", "invoking (%s) in %s\n", label, workDir)

	// Set up stderr: in TUI mode discard; in plain mode forward to os.Stderr.
	// Stderr is diagnostic noise from Claude CLI itself (not the structured output).
	var stderrWriter io.Writer
	if claudeTUI {
		stderrWriter = io.Discard
	} else {
		stderrWriter = os.Stderr
	}

	// Open the .log file before running Claude so stdout (NDJSON stream-json) is
	// tee'd to disk in real time. This enables fabrik watch to follow the live output.
	var stdoutWriter io.Writer
	var stdout bytes.Buffer
	stdoutWriter = &stdout

	safeLabel := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "-").Replace(label)
	if err := os.MkdirAll(logDir, 0700); err != nil {
		claudeLog(issueNumber, "warn", "could not create log dir: %v\n", err)
	} else if err := os.Chmod(logDir, 0700); err != nil {
		claudeLog(issueNumber, "warn", "could not set log dir permissions: %v\n", err)
	} else {
		now := time.Now().UTC()
		logPath := filepath.Join(logDir, fmt.Sprintf("%s-%s-%d.log", safeLabel, now.Format("20060102-150405"), now.UnixNano()))
		if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600); err != nil {
			claudeLog(issueNumber, "warn", "could not create log file %s: %v\n", logPath, err)
		} else {
			defer logFile.Close()
			// Tee stdout to both the in-memory buffer (for parsing) and the .log file (for live follow).
			stdoutWriter = io.MultiWriter(&stdout, logFile)
		}
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stderr = stderrWriter
	cmd.Stdout = stdoutWriter

	runErr := cmd.Run()
	rawOutput := stdout.Bytes()

	// Save the raw NDJSON output to an -output-*.json file for backward compatibility
	// (e.g., the existing TUI log viewer and stream-filter tooling).
	if len(rawOutput) > 0 {
		outputLogPath := filepath.Join(logDir, fmt.Sprintf("%s-output-%s.json", safeLabel, time.Now().UTC().Format("20060102-150405")))
		if err := os.WriteFile(outputLogPath, rawOutput, 0600); err != nil {
			claudeLog(issueNumber, "warn", "could not save output log: %v\n", err)
		}
	}

	resp, ok := parseClaudeJSON(bytes.TrimSpace(rawOutput))
	var text string
	var usage TokenUsage
	if ok {
		// Check for stale session ID error — delete the session file so the
		// next retry starts fresh instead of looping on the same expired ID.
		if resp.IsError && len(resp.Errors) > 0 {
			for _, errMsg := range resp.Errors {
				if strings.Contains(errMsg, "No conversation found with session ID") {
					claudeLog(issueNumber, "warn", "session expired — deleting stale session file for retry\n")
					os.Remove(sessFilePath)
					break
				}
			}
		}

		text = resp.Result
		// Fallback: if the stage is complete but FABRIK_ISSUE_UPDATE_BEGIN is absent
		// from result (emitted in an intermediate assistant turn), scan all assistant
		// messages in the raw NDJSON for the last update block and prepend it.
		if stageCompleteRE.MatchString(text) && !strings.Contains(text, "FABRIK_ISSUE_UPDATE_BEGIN") {
			if block := extractIssueUpdateFromAssistantTurns(rawOutput); block != "" {
				text = block + "\n" + text
			}
		}
		usage = tokenUsageFromResponse(resp)
		if runErr != nil {
			claudeLog(issueNumber, "claude", "used %d turns, $%.4f\n", resp.NumTurns, resp.CostUSD)
		} else {
			claudeLog(issueNumber, "claude", "completed in %d turns, $%.4f\n", resp.NumTurns, resp.CostUSD)
		}
		saveSessionIDDirect(sessFilePath, resp.SessionID)
	} else {
		claudeLog(issueNumber, "warn", "JSON parse failed (%d bytes); output not posted\n", len(rawOutput))
		text = fmt.Sprintf("⚠️ Claude output could not be parsed (raw output was %d bytes). Check logs at `%s` for details.", len(rawOutput), logDir)
	}

	if runErr != nil {
		// If the context was cancelled (shutdown), treat as interrupted regardless of
		// marker presence — the engine is going away, bookkeeping would be partial.
		if ctx.Err() != nil {
			return text, false, usage, fmt.Errorf("claude exited with error: %w", runErr)
		}
		// Check whether the agent emitted the completion marker before the error.
		// This handles the case where the agent correctly signals completion but then
		// continues doing extra work (e.g. post-completion verification) and the session
		// ends non-zero (max_turns exceeded, API error, etc.).
		if stageCompleteRE.MatchString(text) {
			claudeLog(issueNumber, "warn", "stage completed (marker found) but Claude exited with error: %v\n", runErr)
			return text, true, usage, fmt.Errorf("claude exited with error: %w", runErr)
		}
		return text, false, usage, fmt.Errorf("claude exited with error: %w", runErr)
	}

	completed := stageCompleteRE.MatchString(text)
	return text, completed, usage, nil
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

	if stage.Skill != "" {
		b.WriteString(fmt.Sprintf("You are operating as the Fabrik %s agent for issue #%d.\n", stage.Name, issue.Number))
		b.WriteString(fmt.Sprintf("Follow the instructions in the %s skill exactly.\n", stage.Skill))
	} else {
		b.WriteString(stage.Prompt)
	}
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
	b.WriteString("Context files are available in `.fabrik-context/` in your working directory:\n")
	b.WriteString("- `.fabrik-context/issue.md` — the issue body (spec)\n")
	b.WriteString("- `.fabrik-context/stage-{Name}.md` — output from prior stages (e.g. `.fabrik-context/stage-Research.md`)\n")
	b.WriteString("- `.fabrik-context/codebase-changes.md` — files changed on main since the last stage (if any)\n")
	if stage.PostToPR {
		b.WriteString("- `.fabrik-context/pr-description.md` — the linked PR description\n")
	}
	b.WriteString("\n")
	if stage.PostToPR {
		b.WriteString("Your detailed output will be posted on the PR. Provide a brief summary (2-4 sentences)\n")
		b.WriteString("for the issue between these markers:\n\n")
		b.WriteString("FABRIK_SUMMARY_BEGIN\n")
		b.WriteString("(brief summary of findings and actions taken)\n")
		b.WriteString("FABRIK_SUMMARY_END\n\n")
	}
	b.WriteString("When you have completed all work for this stage, end your response with the exact line:\n")
	b.WriteString("FABRIK_STAGE_COMPLETE\n\n")
	b.WriteString("Once you emit this marker, do not generate any further output. Continuing after the marker risks leaving the issue in a stuck state if the session ends with an error.\n\n")
	b.WriteString("If you have unresolved questions that must be answered before the stage can proceed, output instead:\n")
	b.WriteString("FABRIK_BLOCKED_ON_INPUT\n")
	b.WriteString("These two markers are mutually exclusive — output exactly one or neither.\n")

	return b.String()
}

func buildCommentReviewPrompt(stage *stages.Stage, item gh.ProjectItem, comments []gh.Comment) string {
	var b strings.Builder

	// Use stage-specific comment skill directive if available, then CommentPrompt, then default
	if stage.CommentSkill != "" {
		itemType := "issue"
		if item.IsPR {
			itemType = "PR"
		}
		b.WriteString(fmt.Sprintf("You are operating as the Fabrik %s comment reviewer for %s #%d.\n", stage.Name, itemType, item.Number))
		b.WriteString(fmt.Sprintf("Follow the instructions in the %s skill exactly.", stage.CommentSkill))
	} else if stage.CommentPrompt != "" {
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
	b.WriteString("Context files are available in `.fabrik-context/` in your working directory:\n")
	b.WriteString("- `.fabrik-context/issue.md` — the issue body (spec)\n")
	b.WriteString("- `.fabrik-context/stage-{Name}.md` — the current stage output (e.g. `.fabrik-context/stage-Specify.md`) and prior stage outputs\n")
	b.WriteString("\n")
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

// stripMarkers removes the begin/end marker block (inclusive) from the output.
func stripMarkers(output, beginMarker, endMarker string) string {
	beginIdx := strings.Index(output, beginMarker)
	if beginIdx == -1 {
		return output
	}
	endIdx := strings.Index(output[beginIdx:], endMarker)
	if endIdx == -1 {
		return output
	}
	endIdx += beginIdx + len(endMarker)
	// Also strip a trailing newline after the end marker
	if endIdx < len(output) && output[endIdx] == '\n' {
		endIdx++
	}
	return output[:beginIdx] + output[endIdx:]
}

// stripLine removes all lines that exactly match the given text from the output.
func stripLine(output, line string) string {
	var result []string
	for _, l := range strings.Split(output, "\n") {
		if strings.TrimSpace(l) != line {
			result = append(result, l)
		}
	}
	return strings.Join(result, "\n")
}

// extractSummary parses a brief summary from Claude's output.
func extractSummary(output string) string {
	return extractBetweenMarkers(output, "FABRIK_SUMMARY_BEGIN", "FABRIK_SUMMARY_END")
}

// extractIssueUpdateFromAssistantTurns scans raw NDJSON output for the last
// FABRIK_ISSUE_UPDATE_BEGIN/END block across all {"type":"assistant",...} messages.
// Returns the reconstructed "FABRIK_ISSUE_UPDATE_BEGIN\n<body>\nFABRIK_ISSUE_UPDATE_END"
// string if found, or empty string if not. Used as a fallback when the markers
// do not appear in the result field (emitted in an intermediate turn).
func extractIssueUpdateFromAssistantTurns(rawOutput []byte) string {
	var lastBlock string
	lines := bytes.Split(rawOutput, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// Only examine assistant-type messages.
		var envelope struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil || envelope.Type != "assistant" {
			continue
		}
		// Collect text content from all content blocks.
		var sb strings.Builder
		for _, block := range envelope.Message.Content {
			if block.Type == "text" {
				sb.WriteString(block.Text)
			}
		}
		text := sb.String()
		if body := extractUpdatedBody(text); body != "" {
			lastBlock = "FABRIK_ISSUE_UPDATE_BEGIN\n" + body + "\nFABRIK_ISSUE_UPDATE_END"
		}
	}
	return lastBlock
}

// parseClaudeJSON parses the JSON output from claude --output-format json.
// Handles two formats:
//   - Single result object: {"result": "...", "session_id": "...", ...}
//   - Conversation array: [{"type":"system",...}, ..., {"type":"result","result":"..."}]
func parseClaudeJSON(output []byte) (claudeResponse, bool) {
	// Try single-object format first.
	// Accept if result is non-empty OR if session_id is present (max_turns hit
	// produces a valid result message with empty result text).
	var resp claudeResponse
	if err := json.Unmarshal(output, &resp); err == nil && (resp.Result != "" || resp.SessionID != "") {
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
	if err := json.Unmarshal(raw, &resp); err == nil && (resp.Result != "" || resp.SessionID != "") {
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

// migrateSessions scans sessionRoot for old-style issue-N/ directories and moves
// each one to the per-repo layout <dirName>/issue-N/ using os.Rename.
// It reads the git remote from the corresponding worktree under worktreeRoot to
// determine the target repo. Must be called after migrateWorktrees so that
// namespaced worktree paths exist.
// logfn is optional; pass nil to suppress output.
func migrateSessions(sessionRoot, worktreeRoot string, logfn func(string)) {
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		return // no sessions directory yet
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Old-style entries match issue-N pattern.
		if len(name) < 7 || name[:6] != "issue-" {
			continue
		}
		// Parse the issue number from the dir name for the worktree scan.
		issueNumStr := name[6:]
		if _, err := strconv.Atoi(issueNumStr); err != nil {
			continue // not a valid issue number
		}
		oldPath := filepath.Join(sessionRoot, name)

		// Search two levels deep in worktreeRoot for a matching issue-N/ subdir.
		// After migrateWorktrees, layout is: worktreeRoot/<owner-repo>/issue-N/
		wtDir := findWorktreeForIssue(worktreeRoot, name)
		if wtDir == "" {
			if logfn != nil {
				logfn(fmt.Sprintf("warn: no worktree found for session %s — leaving in place\n", oldPath))
			}
			continue
		}

		// Read the git remote to determine the repo.
		cmd := exec.Command("git", "remote", "get-url", "origin")
		cmd.Dir = wtDir
		out, err := cmd.Output()
		if err != nil {
			if logfn != nil {
				logfn(fmt.Sprintf("warn: cannot read remote for worktree %s — leaving session %s in place\n", wtDir, oldPath))
			}
			continue
		}
		remoteURL := strings.TrimSpace(string(out))
		dirName := ownerRepoDirFromURL(remoteURL)
		if dirName == "" {
			if logfn != nil {
				logfn(fmt.Sprintf("warn: cannot parse repo from remote URL %q for %s — leaving session %s in place\n", remoteURL, wtDir, oldPath))
			}
			continue
		}

		newDir := filepath.Join(sessionRoot, dirName)
		newPath := filepath.Join(newDir, name)

		if _, err := os.Stat(newPath); err == nil {
			if logfn != nil {
				logfn(fmt.Sprintf("warn: migration target %s already exists — skipping %s\n", newPath, oldPath))
			}
			continue
		}

		if err := os.MkdirAll(newDir, 0700); err != nil {
			if logfn != nil {
				logfn(fmt.Sprintf("warn: cannot create dir %s: %v\n", newDir, err))
			}
			continue
		}

		if err := os.Rename(oldPath, newPath); err != nil {
			if logfn != nil {
				logfn(fmt.Sprintf("warn: rename %s → %s failed: %v\n", oldPath, newPath, err))
			}
			continue
		}
		if logfn != nil {
			logfn(fmt.Sprintf("migrated session %s → %s\n", oldPath, newPath))
		}
	}
}

// renameWithFallback attempts os.Rename(src, dst). If that fails with a
// cross-device link error (EXDEV — source and destination on different
// filesystems), it falls back to copy+delete.
func renameWithFallback(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if linkErr, ok := err.(*os.LinkError); !ok || linkErr.Err != syscall.EXDEV {
		return err
	}
	// Cross-device: copy then remove.
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return fmt.Errorf("mkdirall dst: %w", err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("copy: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("close dst: %w", err)
	}
	return os.Remove(src)
}

// migrateHomeToProject moves session and log files from the legacy home-dir
// location (~/.fabrik/sessions/ and ~/.fabrik/logs/) to the CWD-relative
// location (<fabrikDir>/.fabrik/sessions/ and <fabrikDir>/.fabrik/logs/).
// It is idempotent: files that already exist at the destination are skipped.
// It is a no-op when src == dst (e.g. when HOME == fabrikDir in tests).
// logfn is optional; pass nil to suppress output.
func migrateHomeToProject(fabrikDir string, logfn func(string)) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	for _, subdir := range []string{"sessions", "logs"} {
		src := filepath.Join(home, ".fabrik", subdir)
		dst := filepath.Join(fabrikDir, ".fabrik", subdir)

		// Same-path guard: no-op when home dir == project dir.
		if filepath.Clean(src) == filepath.Clean(dst) {
			continue
		}

		entries, err := os.ReadDir(src)
		if err != nil {
			continue // no source directory yet
		}

		for _, entry := range entries {
			srcPath := filepath.Join(src, entry.Name())
			dstPath := filepath.Join(dst, entry.Name())

			// Skip if destination already exists.
			if _, err := os.Stat(dstPath); err == nil {
				if logfn != nil {
					logfn(fmt.Sprintf("warn: migration target %s already exists — skipping %s\n", dstPath, srcPath))
				}
				continue
			}

			if err := os.MkdirAll(dst, 0700); err != nil {
				if logfn != nil {
					logfn(fmt.Sprintf("warn: cannot create dir %s: %v\n", dst, err))
				}
				continue
			}

			if err := renameWithFallback(srcPath, dstPath); err != nil {
				if logfn != nil {
					logfn(fmt.Sprintf("warn: migrate %s → %s failed: %v\n", srcPath, dstPath, err))
				}
				continue
			}
			if logfn != nil {
				logfn(fmt.Sprintf("migrated %s/%s → %s/%s\n", subdir, entry.Name(), subdir, entry.Name()))
			}
		}

		// Remove source dir if now empty.
		if remaining, err := os.ReadDir(src); err == nil && len(remaining) == 0 {
			os.Remove(src)
		}
	}
}

// findWorktreeForIssue searches two levels deep in worktreeRoot for a directory
// named issueDirName (e.g. "issue-42"). Returns the full path if found, or "".
func findWorktreeForIssue(worktreeRoot, issueDirName string) string {
	// Check direct child first (old-style, should not exist after migrateWorktrees).
	direct := filepath.Join(worktreeRoot, issueDirName)
	if fi, err := os.Stat(direct); err == nil && fi.IsDir() {
		return direct
	}
	// Scan one level of subdirs (the per-repo namespaced dirs).
	repoDirs, err := os.ReadDir(worktreeRoot)
	if err != nil {
		return ""
	}
	for _, repoDir := range repoDirs {
		if !repoDir.IsDir() {
			continue
		}
		candidate := filepath.Join(worktreeRoot, repoDir.Name(), issueDirName)
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			return candidate
		}
	}
	return ""
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
