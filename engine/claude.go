package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

var stageCompleteRE = regexp.MustCompile(`(?m)^FABRIK_STAGE_COMPLETE\r?$`)
var blockedOnInputRE = regexp.MustCompile(`(?m)^FABRIK_BLOCKED_ON_INPUT\r?$`)
var noWorkNeededRE = regexp.MustCompile(`(?m)^FABRIK_NO_WORK_NEEDED\r?$`)

// usageLimitTerminalReason is the CLI's structural terminal_reason value for
// a genuine account usage-limit exit, per the Agent SDK's published
// terminal_reason enum (named alongside "max_turns" in the same changelog
// entry that #1178 already proved this installed CLI version emits). This is
// the sole positive trigger for claudeUsageLimitError — see
// classifyUsageLimitExit and #1183.
const usageLimitTerminalReason = "blocking_limit"

// claudeUsageLimitError signals that a Claude invocation exited because the
// account's usage limit (session or weekly) was exhausted, not because the
// stage genuinely failed. The stage never ran to completion, so this
// condition must be excluded from max_retries — see handleUsageLimitExit
// in item.go, which is the sole consumer (via errors.As).
type claudeUsageLimitError struct {
	// Message describes the structural field that triggered detection (see
	// classifyUsageLimitExit), for logging.
	Message string
	// ResetTime is always "" — the structural detector never parses a reset
	// time from prose. Kept so computeUsageLimitResetDeadline's existing
	// fallback-when-empty path (claudeUsageLimitFallbackBackoff) is exercised
	// unconditionally; do not populate this from matched text (#1183).
	ResetTime string
}

func (e *claudeUsageLimitError) Error() string {
	if e.ResetTime != "" {
		return fmt.Sprintf("claude usage limit hit: %s (resets %s)", e.Message, e.ResetTime)
	}
	return fmt.Sprintf("claude usage limit hit: %s", e.Message)
}

// claudeTurnLimitError signals that a Claude invocation exited because it
// exhausted its configured turn budget (CLI-reported subtype
// "error_max_turns"), not because the stage genuinely failed. Unlike
// claudeUsageLimitError, this does NOT short-circuit finalizeStageOutcome /
// processComments with an early return: the existing retry/escalation
// machinery (StageAttempted, commitWIP, StageRetryIncremented, MaxRetries)
// must still run exactly as it does today for a turn-cap exit — that
// behavior is pre-existing and deliberate (see #1081/#448), and out of scope
// for #1178. This sentinel only changes what feeds the InvocationRecorded
// write, so history/TUI can render the run as incomplete-but-resumable
// rather than as a genuine fault.
type claudeTurnLimitError struct {
	// TerminalReason is the CLI's own terminal_reason field, if present, for logging.
	TerminalReason string
	// NumTurns is the CLI-reported turn count at exit, for logging.
	NumTurns int
}

func (e *claudeTurnLimitError) Error() string {
	return fmt.Sprintf("claude exited: turn limit reached (num_turns=%d)", e.NumTurns)
}

// apiErrorTerminalReason is the CLI's structural terminal_reason value for a
// transient Anthropic-side API error — observed live on 2026-08-08 (#1458) as
// eight exits across two issues, each at 1 turn and $0.0000. Unlike
// usageLimitTerminalReason, this is per-invocation and self-resolving: it
// says nothing about the account as a whole, so it must never trigger
// activateClaudeSuspension (see claudeAPIErrorExit's doc comment and #1458 R3).
const apiErrorTerminalReason = "api_error"

// claudeAPIErrorExit signals that a Claude invocation exited because of a
// transient Anthropic-side API error, not because the stage genuinely
// failed. The stage never ran, so this condition must be excluded from
// max_retries — see handleAPIErrorExit in item.go, which is the sole
// consumer (via errors.As).
//
// Deliberately a distinct type from claudeUsageLimitError, not a second
// value recognized by classifyUsageLimitExit itself: claudeUsageLimitError
// is also the trigger, by Go type, for two behaviors that must NOT apply
// here — activateClaudeSuspension (item.go/comments.go, forbidden by R3,
// since an api_error is per-invocation, not account-wide) and the
// comment-processing circuit breaker bypass (comments.go, forbidden by R6's
// spirit, since exempting api_error from the breaker would leave the
// comment-triggered dispatch path with no bound at all). Being a distinct
// type means neither errors.As(&claudeUsageLimitError{}) check ever matches
// a *claudeAPIErrorExit, so both call sites need no changes. See ADR-1458.
type claudeAPIErrorExit struct {
	// TerminalReason is the CLI's own terminal_reason field, for logging.
	TerminalReason string
	// NumTurns is the CLI-reported turn count at exit, for logging.
	NumTurns int
	// CostUSD is the CLI-reported cost at exit, for logging.
	CostUSD float64
}

func (e *claudeAPIErrorExit) Error() string {
	return fmt.Sprintf("claude exited: transient api_error (terminal_reason=%q, num_turns=%d, cost=$%.4f)", e.TerminalReason, e.NumTurns, e.CostUSD)
}

// classifyAPIErrorExit determines whether a Claude invocation exited on a
// transient Anthropic-side API error, using only the CLI's own structured
// result object (resp) — never text the assistant itself wrote, per the same
// structural-only discipline #1183 established for classifyUsageLimitExit.
//
// resp.TerminalReason == "api_error" is the CLI's structural signal. usage is
// kept as the same belt-and-suspenders exclusion gate classifyUsageLimitExit
// uses: an invocation that consumed turns and incurred real cost is never
// classified as a did-not-run exit, regardless of what TerminalReason says
// (#1458 R5 could not empirically verify CostUSD on a real api_error sample,
// so this guard is reused unchanged rather than dropped — see ADR-1458).
func classifyAPIErrorExit(resp claudeResponse, usage TokenUsage) (msg string, detected bool) {
	if resp.TerminalReason != apiErrorTerminalReason {
		return "", false
	}
	if usage.TurnsUsed > 0 && usage.CostUSD > 0 {
		return "", false
	}
	return fmt.Sprintf("terminal_reason=%q", resp.TerminalReason), true
}

// classifyUsageLimitExit determines whether a Claude invocation exited on a
// genuine account usage-limit condition, using only the CLI's own structured
// result object (resp) — never text the assistant itself wrote. This
// replaces the original prose-matching detectUsageLimitExit, which scanned
// raw invocation output for Anthropic's usage-limit exit message and
// self-triggered on any stage whose output merely discussed usage limits.
//
// That was not hypothetical in this repository: on 2026-07-27 issue #1178's
// Implement stage, which was writing tests about usage-limit detection,
// quoted #1084's example message and suspended Claude dispatch account-wide
// for ~11 hours after a plain turn-cap exit (51 turns, $2.28). See #1183.
//
// resp.TerminalReason == "blocking_limit" is the CLI's structural signal for
// a genuine usage-limit exit (see usageLimitTerminalReason's doc comment for
// sourcing). usage is kept as a belt-and-suspenders exclusion gate carried
// over from the original prose-based guard (#1184): a genuine usage-limit
// exit terminates the invocation immediately (0 turns, $0.00), so an
// invocation that consumed turns and incurred cost is never classified as
// one, regardless of what TerminalReason says.
func classifyUsageLimitExit(resp claudeResponse, usage TokenUsage) (msg string, detected bool) {
	if resp.TerminalReason != usageLimitTerminalReason {
		return "", false
	}
	if usage.TurnsUsed > 0 && usage.CostUSD > 0 {
		return "", false
	}
	return fmt.Sprintf("terminal_reason=%q", resp.TerminalReason), true
}

// defaultAllowedTools is the comprehensive set of tools Fabrik permits by default
// when a stage does not specify allowed_tools. This ensures headless Claude Code
// invocations work deterministically regardless of the user's global settings.
// When a stage sets allowed_tools, that list replaces (not extends) these defaults.
var defaultAllowedTools = []string{
	"Read", "Edit", "Write", "Glob", "Grep", "TodoWrite", "Skill", "Task",
	"Bash(git:*)", "Bash(gh:*)", "Bash(go:*)", "Bash(npm:*)", "Bash(npx:*)", "Bash(yarn:*)", "Bash(pnpm:*)",
	"Bash(make:*)", "Bash(cargo:*)", "Bash(python:*)", "Bash(pip:*)", "Bash(uv:*)", "Bash(pytest:*)",
	"Bash(ls:*)", "Bash(cat:*)", "Bash(rm:*)", "Bash(cp:*)", "Bash(mv:*)", "Bash(mkdir:*)", "Bash(find:*)",
}

// disallowedTools lists harness tools that must never reach a headless Fabrik
// stage worker, regardless of stage config or the unrestricted/dontAsk split.
// Unlike --allowedTools (a call-time permission filter that does not prevent a
// tool from being offered or invoked — see #1372 and #1365), --disallowedTools
// is a construction-time exclusion: the tool is absent from the schema Claude
// is given, so there is no permission check to reason around or bypass.
var disallowedTools = []string{
	// ScheduleWakeup promises cross-turn resumption via a scheduled wakeup.
	// A headless Fabrik stage has no scheduler to deliver that wakeup: the
	// turn ends, the stage exits without FABRIK_STAGE_COMPLETE, and the next
	// retry re-derives the identical stall. See #1345, #1365.
	"ScheduleWakeup",
	// Workflow promises a <task-notification> on completion after spawning
	// background subagents against the session budget. It is more damaging
	// than ScheduleWakeup: nothing ever delivers the notification in a
	// headless stage, so the failure mode is the stall plus the spend. See
	// #1345, #1365.
	"Workflow",
}

// CheckBlockedOnInput reports whether output contains the FABRIK_BLOCKED_ON_INPUT marker.
func CheckBlockedOnInput(output string) bool {
	return blockedOnInputRE.MatchString(output)
}

// CheckNoWorkNeeded reports whether output contains the FABRIK_NO_WORK_NEEDED marker.
// This marker signals that the emitting stage determined no code or documentation
// changes are required. It must co-occur with FABRIK_STAGE_COMPLETE; when both are
// present the engine skips all remaining non-cleanup stages (adding dummy completion
// labels and "skipped" comments) and moves the issue directly to Done without a PR.
func CheckNoWorkNeeded(output string) bool {
	return noWorkNeededRE.MatchString(output)
}

// claudeLogf is the logging function used by runClaude. Set by the Engine
// during construction to route output through the event channel in TUI mode.
// Falls back to stderr when nil (e.g. in tests).
var claudeLogf func(issueNumber int, tag, format string, args ...any)

// claudeTurnProgress is called after each user event (logical turn start) during a
// Claude invocation. Set by the Engine during construction (same block as claudeLogf).
// nil when no TUI is active (tests, plain-text mode).
var claudeTurnProgress func(issueNumber, turnsUsed, maxTurns int)

// claudeTUI indicates whether the TUI is active. When true, Claude's child
// process stderr is sent only to the log file (not the terminal).
var claudeTUI bool

// claudePluginDir is the path to the Fabrik plugin directory. Set by the Engine
// during construction. When non-empty, --plugin-dir is added to Claude args.
var claudePluginDir string

// claudeNameFlagSupported records whether the installed claude binary accepts
// -n/--name, as determined once at startup by probeClaudeNameFlagSupport (see
// engine.New). Zero value is false ("unsupported"), so every existing call
// path that never touches this var — including the entire pre-#1284 test
// suite — keeps producing today's exact argv with no --name flag. Only a
// successfully parsed, positive probe result flips it; any ambiguity (binary
// missing, --help errors, unexpected output) must fail safe to false rather
// than risk killing every worker on a fleet running an older claude binary.
var claudeNameFlagSupported bool

// claudeWaitDelay is how long runClaude waits for stdout pipe drain after the
// Claude process exits before giving up and processing buffered output. Set by
// the Engine during construction from Config.ClaudeWaitDelay. Default (0) is
// resolved to 30s in runClaude.
var claudeWaitDelay time.Duration

// claudeInactivityTimeout is the maximum time runClaude will wait without
// receiving any streamed output before killing the Claude process. Hardcoded
// at 15 minutes; overridable in tests.
var claudeInactivityTimeout = 15 * time.Minute

// claudeKillGraceSigInt is the grace window between SIGINT and SIGTERM in the
// kill escalation sequence. Default 10s; set from Config.KillGraceSigInt in New().
// Zero means skip the SIGINT step (SIGTERM → SIGKILL only).
var claudeKillGraceSigInt = 10 * time.Second

// claudeKillGraceSigTerm is the grace window between SIGTERM and SIGKILL.
// Default 10s; set from Config.KillGraceSigTerm in New().
var claudeKillGraceSigTerm = 10 * time.Second

// claudeGHToken is the engine's resolved GitHub token (Config.Token). Set by
// the Engine during construction. Injected into every Claude worker's
// environment as GH_TOKEN and GITHUB_TOKEN so the worker's gh invocations
// always authenticate as the same identity as the engine itself, regardless
// of the launching shell's ambient environment. Empty skips injection.
var claudeGHToken string

// claudeGHHost is the engine's resolved GHES host (Config.GHESHost). Set by
// the Engine during construction, mirroring claudeGHToken's package-var
// pattern. Injected into every Claude worker's environment as GH_HOST so the
// worker's gh invocations target the same GitHub instance as the engine
// itself (FR-6, ADR-1391) — without this, a stage worker's `gh` calls (e.g.
// fabrik-validate's Pre-Completion Gate) would silently hit github.com even
// when the engine is talking to a GHES instance. Empty (the default) skips
// injection entirely, preserving today's behavior byte-for-byte.
var claudeGHHost string

// claudeAnthropicAPIKey is the engine's resolved FABRIK_ANTHROPIC_API_KEY
// (read from os.Getenv, which reflects .env by the time Engine.New runs —
// see config.LoadDotenv). Set once by the Engine during construction,
// mirroring claudeGHToken's package-var pattern, since this value is read
// once at process start and never changes mid-run. When non-empty,
// buildClaudeEnv translates it into an explicit ANTHROPIC_API_KEY override
// on every worker invocation (R6); FABRIK_ANTHROPIC_API_KEY itself is never
// forwarded (R8). Empty (the default) means API billing is not opted into —
// buildClaudeEnv's default-deny scrub (R2-R5) still removes any ambient
// ANTHROPIC_API_KEY regardless of what this variable holds (R7).
var claudeAnthropicAPIKey string

// claudeAnthropicEnvPassthrough is the engine's resolved, parsed
// FABRIK_ANTHROPIC_ENV_PASSTHROUGH allow-list (R14-R19): exact variable
// names to re-inherit from the ambient environment into every worker
// invocation, overriding the Anthropic auth namespace scrub (R2-R5) for
// only the names listed. Set once by the Engine during construction via
// parseAnthropicEnvPassthrough, mirroring claudeGHToken/claudeAnthropicAPIKey.
// Nil/empty (the default) means no passthrough — the scrub applies
// unconditionally.
var claudeAnthropicEnvPassthrough []string

// killReasonCtxKey is the context key for kill reason annotation.
type killReasonCtxKey struct{}

// killReasonHolder stores a mutable kill reason string via atomic.Value.
// Stored in the per-issue context via context.WithValue(ctx, killReasonCtxKey{}, holder)
// so that kill sites can read the reason set by the cancellation path.
type killReasonHolder struct {
	val atomic.Value
}

// issueCtxEntry holds the cancel function and reason holder for a per-issue context.
type issueCtxEntry struct {
	cancel context.CancelFunc
	holder *killReasonHolder
}

// activityWriter wraps an io.Writer and updates a shared atomic timestamp on
// every Write call. Used by the inactivity watchdog to detect stuck sessions.
type activityWriter struct {
	inner        io.Writer
	lastActivity *atomic.Int64
}

func (w *activityWriter) Write(p []byte) (int, error) {
	w.lastActivity.Store(time.Now().UnixNano())
	return w.inner.Write(p)
}

// turnCountingWriter wraps an io.Writer, counts logical turns from the NDJSON
// stream in real time, and calls claudeTurnProgress after each user event.
// It buffers bytes until '\n', then checks each line for type == "user".
// Safe for use from a single goroutine only (one per runClaude invocation).
type turnCountingWriter struct {
	inner       io.Writer
	issueNumber int
	maxTurns    int
	buf         []byte
	count       int
}

func (w *turnCountingWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		line := w.buf[:idx+1]
		w.buf = w.buf[idx+1:]
		if isUserTurnLine(line) {
			w.count++
			if claudeTurnProgress != nil {
				claudeTurnProgress(w.issueNumber, w.count, w.maxTurns)
			}
		}
	}
	return w.inner.Write(p)
}

// isUserTurnLine returns true if line is a JSON object with type == "user".
func isUserTurnLine(line []byte) bool {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return false
	}
	var envelope struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(line, &envelope) == nil && envelope.Type == "user"
}

func claudeLog(issueNumber int, tag, format string, args ...any) {
	if claudeLogf != nil {
		claudeLogf(issueNumber, tag, format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "[#%d %s] "+format, append([]any{issueNumber, tag}, args...)...)
}

// TokenUsage is a type alias for itemstate.TokenUsage. Using an alias ensures
// engine.TokenUsage and itemstate.TokenUsage are the same type with zero-cost
// assignment between the two packages (Phase 3-E).
type TokenUsage = itemstate.TokenUsage

// addTokenUsage returns a new TokenUsage that is the sum of a and b.
func addTokenUsage(a, b TokenUsage) TokenUsage {
	return TokenUsage{
		InputTokens:         a.InputTokens + b.InputTokens,
		OutputTokens:        a.OutputTokens + b.OutputTokens,
		CacheCreationTokens: a.CacheCreationTokens + b.CacheCreationTokens,
		CacheReadTokens:     a.CacheReadTokens + b.CacheReadTokens,
		CostUSD:             a.CostUSD + b.CostUSD,
		TurnsUsed:           a.TurnsUsed + b.TurnsUsed,
	}
}

// saveDebugLog writes Claude's output for a stage invocation to
// .fabrik/debug/issue-{N}_{epoch}_{label}.log in the current working directory.
// Errors are non-fatal — a warning is printed to stderr.
func saveDebugLog(issueNumber int, label string, output string) {
	cwd, err := os.Getwd()
	if err != nil {
		claudeLog(issueNumber, "warn", "saveDebugLog: getting cwd: %v\n", err)
		return
	}
	debugDir := filepath.Join(cwd, ".fabrik", "debug")
	if err := os.MkdirAll(debugDir, 0700); err != nil {
		claudeLog(issueNumber, "warn", "saveDebugLog: creating debug dir: %v\n", err)
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
		claudeLog(issueNumber, "warn", "saveDebugLog: writing %s: %v\n", path, err)
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

// sanitizeStageName sanitizes a stage name for use as a session-file basename,
// preventing path traversal: filepath.Base strips directory components, and an
// additional check rejects names that are empty, ".", or the path separator
// (e.g. filepath.Base("/") == "/"), falling back to "default".
func sanitizeStageName(stageName string) string {
	base := filepath.Base(stageName)
	if base == "" || base == "." || base == "/" || base == string(filepath.Separator) {
		base = "default"
	}
	return base
}

// sessionFile returns the path to the session ID file for a given issue+stage.
func sessionFile(issueNumber int, stageName string) string {
	return filepath.Join(SessionDir(issueNumber), sanitizeStageName(stageName)+".session")
}

// ReadSessionID reads the session ID for a given repo, issue, and stage name.
// repo should be "owner/repo" for multi-repo projects, or "" for single-repo.
// Returns the session ID string, or empty string if the file does not exist,
// is unreadable, or is empty.
func ReadSessionID(repo string, issueNumber int, stageName string) string {
	base := sanitizeStageName(stageName)
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
	id, _ := classifySessionFile(data, err)
	return id
}

// resumeStatus classifies the outcome of attempting to load a session ID for resume.
type resumeStatus int

const (
	// resumeFound indicates a usable, non-blank session ID was loaded.
	resumeFound resumeStatus = iota
	// resumeAbsent indicates the session file does not exist.
	resumeAbsent
	// resumeUnreadable indicates the session file exists but could not be read
	// (an I/O error other than "does not exist").
	resumeUnreadable
	// resumeBlank indicates the session file is present and readable but its
	// trimmed content is empty (zero bytes or whitespace-only).
	resumeBlank
)

func (s resumeStatus) String() string {
	switch s {
	case resumeFound:
		return "found"
	case resumeAbsent:
		return "absent"
	case resumeUnreadable:
		return "unreadable"
	case resumeBlank:
		return "blank"
	default:
		return "unknown"
	}
}

// classifySessionFile classifies the result of an os.ReadFile call on a session
// file, applying trim-before-check so whitespace-only content is treated the
// same as zero-byte content (both resumeBlank). This is the single source of
// truth for "is this session ID usable" — an untrimmed length check here would
// let a whitespace-only file pass as "found" with an empty ID.
func classifySessionFile(data []byte, err error) (id string, status resumeStatus) {
	if err != nil {
		if os.IsNotExist(err) {
			return "", resumeAbsent
		}
		return "", resumeUnreadable
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return "", resumeBlank
	}
	return trimmed, resumeFound
}

// readSessionIDForResume reads and classifies the session file at path,
// preserving the underlying read error (if any) for logging purposes.
func readSessionIDForResume(path string) (id string, status resumeStatus, readErr error) {
	data, err := os.ReadFile(path)
	id, status = classifySessionFile(data, err)
	return id, status, err
}

// resolveResumeSessionID loads the session ID to pass as --resume for a stage
// invocation. When resume is false, it returns "" without logging. When resume
// is true and a usable session ID is found, it returns the ID without logging
// (the healthy path must produce no new log output). Otherwise it logs a
// warning distinguishing why the session ID wasn't usable (absent / unreadable
// / blank) and returns "", signaling that buildClaudeArgs should proceed as a
// fresh session.
func resolveResumeSessionID(issueNumber int, stageName, sessFilePath string, resume bool) string {
	if !resume {
		return ""
	}
	id, status, readErr := readSessionIDForResume(sessFilePath)
	switch status {
	case resumeFound:
		return id
	case resumeAbsent:
		claudeLog(issueNumber, "warn", "stage %q: no session file at %s — resume requested but none exists yet; proceeding as a fresh session\n", stageName, sessFilePath)
	case resumeUnreadable:
		claudeLog(issueNumber, "warn", "stage %q: session file %s could not be read (%v) — resume requested but unusable; proceeding as a fresh session\n", stageName, sessFilePath, readErr)
	case resumeBlank:
		claudeLog(issueNumber, "warn", "stage %q: session file %s is blank — resume requested but no session ID recorded; proceeding as a fresh session\n", stageName, sessFilePath)
	}
	return ""
}

// InvokeClaude runs Claude Code with the given stage configuration and issue context.
// workDir is the directory Claude should run in (typically a git worktree).
// opts.ModelOverride, if non-empty, replaces the stage's configured model.
// opts.EffortOverride, if non-empty, replaces the stage's configured effort level.
// It returns Claude's output, whether Claude indicated completion, and token usage.
func InvokeClaude(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
	sessDir := sessionDirForItem(issue)
	if err := os.MkdirAll(sessDir, 0700); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("creating session dir: %w", err)
	}
	if err := os.Chmod(sessDir, 0700); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("setting session dir permissions: %w", err)
	}

	sessFilePath := filepath.Join(sessDir, sanitizeStageName(stage.Name)+".session")
	ld := logDirForItem(issue)

	prompt := buildPrompt(stage, issue, newComments, opts.BaseBranch, opts.CorrectiveHint)
	effectiveBudget := stage.MaxTurns
	if opts.MaxTurnsOverride > 0 {
		effectiveBudget = opts.MaxTurnsOverride
	}
	resumeSessionID := resolveResumeSessionID(issue.Number, stage.Name, sessFilePath, resume)
	sessionName := sessionNameSentinel(issue.Repo, issue.Number, stage.Name)
	args := buildClaudeArgs(stage, resumeSessionID, opts.ModelOverride, effectiveBudget, hasUnrestrictedLabel(issue), workDir, sessionName)

	extraEnv := buildClaudeEnv(stage, issue, workDir, opts, os.Environ())
	sigIntGrace, sigTermGrace := effectiveKillGrace(opts.SigIntGrace, opts.SigTermGrace)
	wallTime := scaledWallTime(stage.MaxWallTime, effectiveBudget, stage.MaxTurns)
	output, completed, usage, err := runClaude(ctx, args, prompt, workDir, issue.Number, stage.Name, sessFilePath, ld, extraEnv, wallTime, effectiveBudget, opts.OnPIDReady, sigIntGrace, sigTermGrace)
	usage.MaxTurns = effectiveBudget
	if err != nil {
		return output, completed, usage, err
	}
	return output, checkCompletion(stage, output), usage, nil
}

// InvokeClaudeForComments runs Claude Code with a comment-review prompt.
// It uses the stage's CommentPrompt if defined, otherwise a default.
// opts.ModelOverride, if non-empty, replaces the stage's configured model.
// opts.EffortOverride, if non-empty, replaces the stage's configured effort level.
func InvokeClaudeForComments(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
	sessDir := sessionDirForItem(issue)
	if err := os.MkdirAll(sessDir, 0700); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("creating session dir: %w", err)
	}
	if err := os.Chmod(sessDir, 0700); err != nil {
		return "", false, TokenUsage{}, fmt.Errorf("setting session dir permissions: %w", err)
	}

	sessFilePath := filepath.Join(sessDir, sanitizeStageName(stage.Name)+".session")
	ld := logDirForItem(issue)

	prompt := buildCommentReviewPrompt(stage, issue, comments, opts.BaseBranch)
	base := commentMaxTurns(stage)
	limit := base
	if opts.MaxTurnsOverride > 0 {
		limit = opts.MaxTurnsOverride
	}
	resumeSessionID := resolveResumeSessionID(issue.Number, stage.Name, sessFilePath, true) // resume existing session
	sessionName := sessionNameSentinel(issue.Repo, issue.Number, stage.Name)
	args := buildClaudeArgs(stage, resumeSessionID, opts.ModelOverride, limit, hasUnrestrictedLabel(issue), workDir, sessionName)

	extraEnv := buildClaudeEnv(stage, issue, workDir, opts, os.Environ())
	sigIntGrace, sigTermGrace := effectiveKillGrace(opts.SigIntGrace, opts.SigTermGrace)
	wallTime := scaledWallTime(stage.MaxWallTime, limit, base)
	output, completed, usage, err := runClaude(ctx, args, prompt, workDir, issue.Number, stage.Name+"-comment-review", sessFilePath, ld, extraEnv, wallTime, limit, opts.OnPIDReady, sigIntGrace, sigTermGrace)
	usage.MaxTurns = limit
	return output, completed, usage, err
}

// effectiveKillGrace resolves the effective SIGINT and SIGTERM grace windows.
//
// Sentinel semantics:
//   - 0  → inherit engine default (claudeKillGraceSigInt / claudeKillGraceSigTerm)
//   - -1 → skip this signal step (convert to 0 which killProcGroupGraceful treats as skip)
//   - >0 → use this explicit value (stage-level override)
//
// This lets item.go convey three distinct states via a single Duration field without
// requiring a separate "was-set" boolean.
func effectiveKillGrace(sigInt, sigTerm time.Duration) (time.Duration, time.Duration) {
	switch {
	case sigInt == 0:
		sigInt = claudeKillGraceSigInt
	case sigInt < 0:
		sigInt = 0 // convert skip sentinel to actual zero (killProcGroupGraceful checks > 0)
	}
	switch {
	case sigTerm == 0:
		sigTerm = claudeKillGraceSigTerm
	case sigTerm < 0:
		sigTerm = 0
	}
	return sigInt, sigTerm
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

// scaledWallTime scales base proportionately to how far effectiveBudget exceeds
// baseBudget, so a turn-budget pre-grant (e.g. fabrik:extend-turns' 2x first-invocation
// grant) gets matching wall-clock headroom instead of being killed on a clock sized for
// the un-extended case. Returns base unchanged (no scaling) when there is no cap
// (base <= 0), no baseline to scale against (baseBudget <= 0, e.g. an unlimited
// stage.MaxTurns), or no extension is in effect (effectiveBudget <= baseBudget) — which
// covers every ordinary invocation and every progress-based extension iteration, since
// those already reset back to the un-multiplied budget before their own fresh runClaude
// call.
func scaledWallTime(base time.Duration, effectiveBudget, baseBudget int) time.Duration {
	if base <= 0 || baseBudget <= 0 || effectiveBudget <= baseBudget {
		return base
	}
	return base * time.Duration(effectiveBudget) / time.Duration(baseBudget)
}

// buildClaudeEnv returns environment variable overrides to inject into the claude subprocess.
// Fabrik's values are appended after baseEnv (typically os.Environ()) so they
// take precedence (last-wins semantics, via mergeEnv).
//
// opts.EffortOverride, when non-empty, supersedes stage.EffortLevel.
//
// Defaults (when fields are nil/empty):
//   - CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1 (adaptive thinking disabled)
//   - CLAUDE_CODE_EFFORT_LEVEL=high (high thinking effort)
//
// FABRIK_* invocation facts (#1288): FABRIK_ISSUE and FABRIK_WORKTREE are derived
// directly from issue.Number/workDir — both are guaranteed non-zero/non-empty on
// every real invocation (an empty workDir would break the invocation itself, since
// it also becomes cmd.Dir). FABRIK_REPO prefers issue.Repo, falling back to
// opts.FabrikRepo (the caller's e.defaultRepo()) only for the rare item that
// reaches here before a deep-fetch has backfilled issue.Repo, so the "always
// present" guarantee holds even then. FABRIK_REPO is always added to the returned
// overrides, even when the resolved value is empty (both sources unset) — unlike
// FABRIK_ROOT/FABRIK_PR below, omitting it entirely in that case would leave
// mergeEnv with no override key to strip, letting an ambient FABRIK_REPO already
// in the engine process's own environment (e.g. the distinct engine-startup-config
// FABRIK_REPO) leak into the worker unmodified. FABRIK_ROOT and FABRIK_PR come
// from opts, resolved by the caller's Engine-level resolveFabrikEnvOpts (repo.go)
// since buildClaudeEnv itself has no access to fabrikDir or the GitHub client.
// FABRIK_PR is omitted entirely — never emitted as "0" — when opts.PRNumber is 0,
// so a naive consumer never mistakes "no PR yet" for a real PR number; this is
// safe because FABRIK_PR's documented contract is conditional presence, unlike
// FABRIK_REPO's "always."
//
// Anthropic auth namespace scrub (#1346, R2-R9, R14-R19): baseEnv (the true
// ambient environment the subprocess would otherwise inherit) is scanned for
// every ANTHROPIC_*-prefixed key plus the enumerated claudeCodeAuthSelectors,
// and a mergeEnv removal sentinel (see mergeEnv) is emitted for each —
// default-deny, so a newly-introduced upstream ANTHROPIC_* billing variable
// is denied automatically, without a Fabrik code change. A key named in the
// resolved claudeAnthropicEnvPassthrough allow-list is exempted from the
// scrub and re-inherited from baseEnv unchanged. Finally, claudeAnthropicAPIKey
// (resolved once from FABRIK_ANTHROPIC_API_KEY) is translated into an explicit
// ANTHROPIC_API_KEY override — the only supported way to opt into API billing
// through this variable — emitted last so it wins even over a passthrough
// entry also naming ANTHROPIC_API_KEY (Go's os/exec resolves a duplicate
// cmd.Env key by last occurrence).
func buildClaudeEnv(stage *stages.Stage, issue gh.ProjectItem, workDir string, opts InvokeOptions, baseEnv []string) []string {
	var env []string
	// Always emit CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING so mergeEnv can filter
	// any ambient value from the parent process. Default (nil) disables it.
	if stage.DisableAdaptiveThinking == nil || *stage.DisableAdaptiveThinking {
		env = append(env, "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1")
	} else {
		env = append(env, "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=0")
	}
	// Set effort level: label override takes precedence, then stage config, then default "high".
	level := opts.EffortOverride
	if level == "" {
		level = stage.EffortLevel
	}
	if level == "" {
		level = "high"
	}
	env = append(env, "CLAUDE_CODE_EFFORT_LEVEL="+level)
	if claudeGHToken != "" {
		env = append(env, "GH_TOKEN="+claudeGHToken, "GITHUB_TOKEN="+claudeGHToken)
	}
	if claudeGHHost != "" {
		env = append(env, "GH_HOST="+claudeGHHost)
	}

	env = append(env, "FABRIK_ISSUE="+strconv.Itoa(issue.Number))
	// Always add a FABRIK_REPO entry to overrides, even when the resolved value
	// is empty (both issue.Repo and opts.FabrikRepo unset — practically
	// unreachable, but possible in pure multi-repo mode with an unbackfilled
	// item). mergeEnv only strips a base-env key that appears in overrides; if
	// FABRIK_REPO were omitted here entirely, an ambient FABRIK_REPO already in
	// the engine process's own environment (e.g. the distinct engine-startup-config
	// FABRIK_REPO documented in USER_GUIDE.md) would pass straight through to the
	// worker unmodified — silently contradicting the "worker-injected value always
	// wins" guarantee documented there.
	fabrikRepo := issue.Repo
	if fabrikRepo == "" {
		fabrikRepo = opts.FabrikRepo
	}
	env = append(env, "FABRIK_REPO="+fabrikRepo)
	if workDir != "" {
		env = append(env, "FABRIK_WORKTREE="+workDir)
	}
	if opts.FabrikRoot != "" {
		env = append(env, "FABRIK_ROOT="+opts.FabrikRoot)
	}
	if opts.PRNumber != 0 {
		env = append(env, "FABRIK_PR="+strconv.Itoa(opts.PRNumber))
	}

	passthrough := passthroughSet(claudeAnthropicEnvPassthrough)
	env = append(env, scrubAnthropicAuthEnv(baseEnv, passthrough)...)
	for _, key := range claudeAnthropicEnvPassthrough {
		// A passthrough entry only ever re-adds a key inside the scrubbed
		// Anthropic auth namespace (ANTHROPIC_*-prefixed or a
		// claudeCodeAuthSelectors entry) — never one of Fabrik's own
		// computed overrides emitted earlier in this function (GH_TOKEN,
		// GITHUB_TOKEN, the FABRIK_* invocation facts,
		// CLAUDE_CODE_EFFORT_LEVEL, CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING),
		// and never the two control variables themselves. mergeEnv's bare
		// removal sentinel (appended below) only strips a key from base — it
		// cannot retract an earlier "KEY=VALUE" entry already present in
		// this same overrides slice, so without this namespace restriction
		// an operator naming e.g. GH_TOKEN in FABRIK_ANTHROPIC_ENV_PASSTHROUGH
		// (deliberately or by typo) would append a second, later GH_TOKEN
		// entry from the ambient environment — and since os/exec resolves a
		// duplicate cmd.Env key by last occurrence, that stale/attacker
		// ambient value would silently win over the token Fabrik computed.
		// This also makes R19's "outside the namespace is a no-op" claim
		// actually true for every key, not just ones buildClaudeEnv never
		// itself overrides. (#1346 Validate-stage review finding.)
		if !isAnthropicAuthNamespaceKey(key) {
			continue
		}
		if val, ok := envLookup(baseEnv, key); ok {
			env = append(env, key+"="+val)
		}
	}
	// FABRIK_ANTHROPIC_API_KEY and FABRIK_ANTHROPIC_ENV_PASSTHROUGH are
	// Fabrik-internal control variables, read by the engine from its own
	// process environment (os.Getenv in Engine.New) — which means they are
	// themselves present in os.Environ(), the very baseEnv the subprocess
	// would otherwise inherit unfiltered. Without an explicit removal
	// sentinel here, they would leak straight through to the worker (R8,
	// R17), the same ambient-leak failure mode FABRIK_REPO's own handling
	// above already guards against.
	env = append(env, "FABRIK_ANTHROPIC_API_KEY", "FABRIK_ANTHROPIC_ENV_PASSTHROUGH")
	if claudeAnthropicAPIKey != "" {
		env = append(env, "ANTHROPIC_API_KEY="+claudeAnthropicAPIKey)
	}
	return env
}

// claudeCodeAuthSelectors is the enumerated set of CLAUDE_CODE_*-prefixed
// variables that select a non-subscription auth/billing path or supply raw
// credentials directly, verified against the installed Claude Code binary
// (#1346 Research). Unlike ANTHROPIC_*, CLAUDE_CODE_* is a much broader
// general-configuration namespace — it already carries Fabrik's own non-auth
// CLAUDE_CODE_EFFORT_LEVEL/CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING — so it is
// not wildcard-scrubbed; only these specific names are removed. A new,
// not-yet-enumerated CLAUDE_CODE_* billing selector introduced upstream would
// require a Fabrik code change to be scrubbed; this is an accepted residual
// risk, documented in ADR-1346.
var claudeCodeAuthSelectors = []string{
	"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS",
	"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
	"CLAUDE_CODE_USE_MANTLE",
	"CLAUDE_CODE_USE_GATEWAY",
}

// scrubAnthropicAuthEnv returns bare mergeEnv removal-sentinel entries (see
// mergeEnv) for every inherited ANTHROPIC_*-prefixed key found in baseEnv,
// plus every name in claudeCodeAuthSelectors (emitted unconditionally,
// regardless of presence in baseEnv — mirroring buildClaudeEnv's FABRIK_REPO
// reasoning: a removal sentinel for a key baseEnv doesn't have is a harmless
// no-op, but omitting it would leave mergeEnv with nothing to strip if the
// key were present). Keys named in passthrough (the resolved
// FABRIK_ANTHROPIC_ENV_PASSTHROUGH allow-list, R14-R19) are exempted. This is
// a default-deny namespace scrub, not a deny-list: a newly-introduced
// upstream ANTHROPIC_* variable is denied automatically, without a Fabrik
// code change (R2). Matching is on the exact parsed key — the same "up to
// the first '='" extraction mergeEnv itself uses — never a substring, so
// e.g. FANTASY_ANTHROPIC_API_KEY is untouched (R5).
func scrubAnthropicAuthEnv(baseEnv []string, passthrough map[string]bool) []string {
	var removals []string
	seen := make(map[string]bool)
	for _, kv := range baseEnv {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		key := kv[:i]
		if !strings.HasPrefix(key, "ANTHROPIC_") {
			continue
		}
		if passthrough[key] || seen[key] {
			continue
		}
		seen[key] = true
		removals = append(removals, key)
	}
	for _, key := range claudeCodeAuthSelectors {
		if passthrough[key] {
			continue
		}
		removals = append(removals, key)
	}
	return removals
}

// isAnthropicAuthNamespaceKey reports whether key belongs to the scrubbed
// Anthropic auth namespace — the same universe scrubAnthropicAuthEnv removes
// from and buildClaudeEnv's passthrough loop is allowed to re-add from:
// every ANTHROPIC_*-prefixed key, plus the enumerated claudeCodeAuthSelectors.
// Used to keep FABRIK_ANTHROPIC_ENV_PASSTHROUGH scoped to that namespace so a
// passthrough entry can never re-add one of Fabrik's own computed override
// keys (GH_TOKEN, the FABRIK_* invocation facts, CLAUDE_CODE_EFFORT_LEVEL,
// CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING) from the ambient environment.
func isAnthropicAuthNamespaceKey(key string) bool {
	if strings.HasPrefix(key, "ANTHROPIC_") {
		return true
	}
	for _, sel := range claudeCodeAuthSelectors {
		if key == sel {
			return true
		}
	}
	return false
}

// passthroughSet builds a lookup set from the resolved
// FABRIK_ANTHROPIC_ENV_PASSTHROUGH allow-list (R14-R19, claudeAnthropicEnvPassthrough).
func passthroughSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

// envLookup returns the value of key in env (a "KEY=VALUE" slice, typically
// os.Environ()) and whether it was found. Matches mergeEnv's own key
// extraction convention (up to the first '=').
func envLookup(env []string, key string) (string, bool) {
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && kv[:i] == key {
			return kv[i+1:], true
		}
	}
	return "", false
}

// parseAnthropicEnvPassthrough parses the raw FABRIK_ANTHROPIC_ENV_PASSTHROUGH
// value (R14) into a list of exact variable names: comma-separated, each
// entry trimmed of surrounding whitespace, empty entries (from a leading/
// trailing/doubled comma, or an entirely blank input) dropped.
func parseAnthropicEnvPassthrough(raw string) []string {
	if raw == "" {
		return nil
	}
	var names []string
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// mergeEnv builds a subprocess environment from base (typically os.Environ()),
// with overrides applied on top. Keys present in overrides are removed from
// base first so that overrides take effect even when the base already contains
// the same key (Go's os/exec does not deduplicate on its own; if a key
// appeared twice in cmd.Env, the value from the *last* occurrence wins, so
// stripping the base entry before appending the override is what guarantees
// the override — not the append order alone).
//
// An override entry may take one of two forms:
//   - "KEY=VALUE" — the normal add/shadow form. KEY is stripped from base (if
//     present) and "KEY=VALUE" is appended to the result.
//   - "KEY" (no "=") — a removal-only sentinel. KEY is stripped from base (if
//     present) and nothing is appended in its place. This is distinct from
//     "KEY=" (an intentional empty value, which IS appended — see
//     buildClaudeEnv's FABRIK_REPO handling) and is how a caller expresses
//     "remove this key from whatever the subprocess would otherwise inherit,
//     full stop" with no replacement value of its own.
func mergeEnv(base, overrides []string) []string {
	if len(overrides) == 0 {
		return base
	}
	keys := make(map[string]bool, len(overrides))
	for _, kv := range overrides {
		if i := strings.IndexByte(kv, '='); i > 0 {
			keys[kv[:i]] = true
		} else if i < 0 && kv != "" {
			keys[kv] = true // bare removal sentinel
		}
	}
	result := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 && keys[kv[:i]] {
			continue // overrides provides or removes this key
		}
		result = append(result, kv)
	}
	for _, kv := range overrides {
		if strings.IndexByte(kv, '=') < 0 {
			continue // removal sentinel only — nothing to append
		}
		result = append(result, kv)
	}
	return result
}

// probeClaudeNameFlagSupport runs `claude --help` once with a short timeout to
// determine whether the installed binary supports -n/--name. Any error path —
// the binary is missing from PATH, --help exits non-zero, or the command times
// out — fails safe to false. This is a one-time, process-lifetime probe (see
// claudeNameFlagSupported); it must never be called per-invocation.
func probeClaudeNameFlagSupport() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "--help").CombinedOutput()
	if err != nil {
		return false
	}
	return parseNameFlagSupport(string(out))
}

// parseNameFlagSupport checks whether claude --help output advertises the
// -n/--name flag. Matches the precise documented form ("--name <name>") rather
// than a bare "--name" substring, to avoid a false positive from unrelated
// help text that happens to mention "name".
func parseNameFlagSupport(helpText string) bool {
	return strings.Contains(helpText, "--name <name>")
}

// sanitizeSentinelComponent collapses any run of whitespace in s to a single
// "-", then replaces any ":" or "#" with "-". The sentinel must stay a single
// shell token so a naive `ps | grep` keeps working; no shell is involved in
// launching claude (args are passed as an argv slice), so whitespace is the
// only character that would break that. ":" and "#" are additionally replaced
// because they are the sentinel's own field delimiters
// (fabrik:<repo>#<issue>:<stage>) — left alone, a stage name containing
// either (e.g. "Review #2") would make the rendered sentinel ambiguous to
// split back into fields by position, even though nothing in the engine does
// so today.
func sanitizeSentinelComponent(s string) string {
	joined := strings.Join(strings.Fields(s), "-")
	joined = strings.ReplaceAll(joined, ":", "-")
	return strings.ReplaceAll(joined, "#", "-")
}

// sessionNameSentinel builds the --name value passed to every worker
// invocation: fabrik:<owner>/<repo>#<issue>:<stage>. It is deterministic for a
// given (repo, issueNumber, stageName) and purely observational — nothing in
// the engine parses it back or branches on it. repo is expected to already be
// "owner/repo" (as populated from the GitHub GraphQL response on real board
// items); an empty repo falls back to the literal "unknown/repo" rather than
// producing a malformed sentinel.
func sessionNameSentinel(repo string, issueNumber int, stageName string) string {
	if repo == "" {
		repo = "unknown/repo"
	}
	return fmt.Sprintf("fabrik:%s#%d:%s", sanitizeSentinelComponent(repo), issueNumber, sanitizeSentinelComponent(stageName))
}

func buildClaudeArgs(stage *stages.Stage, resumeSessionID string, modelOverride string, maxTurns int, unrestricted bool, workDir string, sessionName string) []string {
	args := []string{
		"--output-format", "stream-json",
		"--verbose",
	}

	if unrestricted {
		args = append(args, "--dangerously-skip-permissions")
	} else {
		args = append(args, "--permission-mode", "dontAsk")
	}

	// Applies unconditionally on both invocation paths above: --disallowedTools
	// is a construction-time exclusion, not a permission-mode behavior, so it
	// must not live inside the unrestricted branch. See disallowedTools doc.
	for _, tool := range disallowedTools {
		args = append(args, "--disallowedTools", tool)
	}

	if claudePluginDir != "" {
		args = append(args, "--plugin-dir", claudePluginDir)
	}

	if resumeSessionID != "" {
		args = append(args, "--resume", resumeSessionID)
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

	tools := stage.AllowedTools
	if !unrestricted && len(tools) == 0 {
		tools = defaultAllowedTools
	}
	if !unrestricted && !stage.ReadOnly && workDir != "" {
		tools = applyWorktreeBoundary(tools, workDir)
	}
	for _, tool := range tools {
		args = append(args, "--allowedTools", tool)
	}

	if claudeNameFlagSupported && sessionName != "" {
		args = append(args, "--name", sessionName)
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
	Subtype   string   `json:"subtype"`
	// TerminalReason is the CLI's more explicit structural classification
	// (e.g. "max_turns"), captured for logging/future use alongside Subtype.
	// Only Subtype is consulted for the error_max_turns branch condition below.
	TerminalReason string `json:"terminal_reason"`
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

func runClaude(ctx context.Context, args []string, prompt string, workDir string, issueNumber int, label string, sessFilePath string, logDir string, extraEnv []string, maxWallTime time.Duration, maxTurns int, onPIDReady func(int), sigIntGrace, sigTermGrace time.Duration) (string, bool, TokenUsage, error) {
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
	var stdout bytes.Buffer
	stdoutWriter, logFile := openStageLog(issueNumber, logDir, label, &stdout)
	if logFile != nil {
		defer func() {
			if cerr := logFile.Close(); cerr != nil {
				claudeLog(issueNumber, "warn", "could not close log file %s: %v\n", logFile.Name(), cerr)
			}
		}()
	}

	// Per-invocation context: with wall-time timeout if configured, or plain parent ctx.
	// Clock starts here (after log setup) — at process spawn time, satisfying R2.
	stageCtx := ctx
	stageCancel := context.CancelFunc(func() {})
	if maxWallTime > 0 {
		stageCtx, stageCancel = context.WithTimeout(ctx, maxWallTime)
	}
	defer stageCancel()

	// Track last-write timestamp for the inactivity watchdog.
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	// Wrap stdoutWriter with a turn-counting writer that fires claudeTurnProgress on
	// each user event (logical turn start), then with the activity writer for inactivity tracking.
	tcw := &turnCountingWriter{inner: stdoutWriter, issueNumber: issueNumber, maxTurns: maxTurns}
	stdoutWriter = &activityWriter{inner: tcw, lastActivity: &lastActivity}

	cmd := exec.CommandContext(stageCtx, "claude", args...)
	cmd.Dir = workDir
	cmd.Env = mergeEnv(os.Environ(), extraEnv)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stderr = stderrWriter
	cmd.Stdout = stdoutWriter
	waitDelay := claudeWaitDelay
	if waitDelay <= 0 {
		waitDelay = 30 * time.Second
	}
	cmd.WaitDelay = waitDelay
	setCmdProcAttr(cmd)

	// watchdogCtx is used to stop the inactivity goroutine after cmd.Wait returns.
	watchdogCtx, watchdogCancel := context.WithCancel(context.Background())
	defer watchdogCancel() // ensures goroutine is cleaned up even on panic

	// Override cmd.Cancel to use a graceful SIGINT → SIGTERM → SIGKILL sequence
	// (targeting the process group) when stageCtx is cancelled. cmd.Cancel is only
	// invoked by Go's exec cancel goroutine, started inside cmd.Start() after
	// cmd.Process is set — so cmd.Process.Pid is safe to read here.
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			// Determine kill reason: wall-time deadline > context-annotated reason > fallback.
			var reason string
			if maxWallTime > 0 && stageCtx.Err() == context.DeadlineExceeded {
				reason = "max_wall_time"
				claudeLog(issueNumber, "warn", "stage %q exceeded max_wall_time (%s) — killing Claude process\n", label, maxWallTime)
			} else {
				if h, ok := ctx.Value(killReasonCtxKey{}).(*killReasonHolder); ok && h != nil {
					if v, ok := h.val.Load().(string); ok && v != "" {
						reason = v
					}
				}
				if reason == "" {
					reason = "context_cancel"
				}
				claudeLog(issueNumber, "warn", "stage %q context cancelled (reason=%s) — killing Claude process\n", label, reason)
			}
			killProcGroupGraceful(cmd.Process.Pid, issueNumber, label, reason, sigIntGrace, sigTermGrace)
		}
		return nil
	}

	// Split Start + Wait so we can capture the PID before starting the watchdog
	// goroutine, avoiding a data race on cmd.Process between the goroutine and
	// cmd.Start (which writes cmd.Process).
	if err := cmd.Start(); err != nil {
		watchdogCancel()
		return "", false, TokenUsage{}, fmt.Errorf("starting claude: %w", err)
	}
	// cmd.Process is written once by cmd.Start and never modified again.
	// Capture it here (same goroutine, post-Start) for the watchdog.
	pid := cmd.Process.Pid
	if onPIDReady != nil {
		onPIDReady(pid)
	}

	// Inactivity watchdog: kills the process group if no stdout is received for
	// claudeInactivityTimeout, indicating a stuck session regardless of wall time.
	// Stopped via watchdogCtx after cmd.Wait returns.
	var inactivityFired atomic.Bool
	go func(pid int) {
		timer := time.NewTimer(claudeInactivityTimeout)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				since := time.Since(time.Unix(0, lastActivity.Load()))
				if since >= claudeInactivityTimeout {
					claudeLog(issueNumber, "warn", "stage %q idle for %s with no output — killing Claude process\n", label, claudeInactivityTimeout)
					inactivityFired.Store(true)
					killProcGroupGraceful(pid, issueNumber, label, "inactivity_timeout", sigIntGrace, sigTermGrace)
					return
				}
				timer.Reset(claudeInactivityTimeout - since)
			case <-watchdogCtx.Done():
				return
			}
		}
	}(pid)

	runErr := cmd.Wait()
	watchdogCancel() // stop the watchdog goroutine promptly
	killProcGroup(cmd, issueNumber, label)
	rawOutput := stdout.Bytes()

	// wasTimedOut is true when this process was terminated by our own timeout
	// (wall-time deadline or inactivity watchdog) rather than an external engine
	// shutdown. When true, the ctx.Err() engine-shutdown guard is bypassed so
	// we still process whatever output was collected before the kill.
	wasTimedOut := inactivityFired.Load() || (stageCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil)

	return interpretClaudeResult(ctx, issueNumber, rawOutput, runErr, wasTimedOut, sessFilePath, logDir)
}

// openStageLog opens (creating logDir if necessary) a new timestamped .log
// file for this invocation and returns an io.Writer that tees stdout to both
// the in-memory buffer (for parsing) and the .log file (for `fabrik watch` to
// follow live), plus the opened file so the caller can defer its Close. If
// logDir cannot be created/chmod'd or the log file cannot be opened, it warns
// and returns stdout alone with a nil file — invocation proceeds without disk
// logging rather than failing outright.
func openStageLog(issueNumber int, logDir, label string, stdout *bytes.Buffer) (io.Writer, *os.File) {
	if err := os.MkdirAll(logDir, 0700); err != nil {
		claudeLog(issueNumber, "warn", "could not create log dir: %v\n", err)
		return stdout, nil
	}
	if err := os.Chmod(logDir, 0700); err != nil {
		claudeLog(issueNumber, "warn", "could not set log dir permissions: %v\n", err)
		return stdout, nil
	}
	safeLabel := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "-").Replace(label)
	now := time.Now().UTC()
	logPath := filepath.Join(logDir, fmt.Sprintf("%s-%s-%d.log", safeLabel, now.Format("20060102-150405"), now.UnixNano()))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		claudeLog(issueNumber, "warn", "could not create log file %s: %v\n", logPath, err)
		return stdout, nil
	}
	return io.MultiWriter(stdout, logFile), logFile
}

// interpretClaudeResult classifies a completed Claude invocation's raw NDJSON
// output: it checks for a WaitDelay-related exit override, parses the
// stream-json result, extracts result text and token usage (falling back to
// scanning intermediate assistant turns when JSON parsing failed or the
// process was killed before emitting a result line), and classifies the
// completion/error status of the invocation from the parsed or extracted
// text.
func interpretClaudeResult(ctx context.Context, issueNumber int, rawOutput []byte, runErr error, wasTimedOut bool, sessFilePath, logDir string) (string, bool, TokenUsage, error) {
	if errors.Is(runErr, exec.ErrWaitDelay) && ctx.Err() == nil {
		claudeLog(issueNumber, "warn", "WaitDelay fired: Claude exited but grandchild processes held stdout pipe open; processing buffered output (%d bytes)\n", len(rawOutput))
		runErr = nil
	}

	resp, ok := parseClaudeJSON(bytes.TrimSpace(rawOutput))
	var text string
	var usage TokenUsage
	staleSessionDetected := false
	if ok {
		// Check for stale session ID error — delete the session file so the
		// next retry starts fresh instead of looping on the same expired ID.
		// Both the structural subtype and the errors[] substring are required:
		// the CLI echoes the dead session_id back on this response, so without
		// this detection saveSessionIDDirect below would immediately rewrite
		// the same dead pointer, making it self-renewing rather than merely stale.
		if resp.IsError && resp.Subtype == "error_during_execution" && len(resp.Errors) > 0 {
			for _, errMsg := range resp.Errors {
				if strings.Contains(errMsg, "No conversation found with session ID") {
					staleSessionDetected = true
					claudeLog(issueNumber, "warn", "session expired (stale session ID %q) — removing %s so the next invocation starts a fresh session\n", resp.SessionID, sessFilePath)
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
		if !staleSessionDetected {
			saveSessionIDDirect(issueNumber, sessFilePath, resp.SessionID)
		}
	} else if wasTimedOut {
		// Process was killed before emitting a result JSON line. Extract text from
		// intermediate assistant turns collected before the kill so we can detect
		// FABRIK_STAGE_COMPLETE and avoid a costly re-run of an already-done stage.
		text = extractTextFromAssistantTurns(rawOutput)
		claudeLog(issueNumber, "warn", "timeout: processing %d bytes of streamed output for markers\n", len(rawOutput))
	} else {
		claudeLog(issueNumber, "warn", "JSON parse failed (%d bytes); output not posted\n", len(rawOutput))
		text = fmt.Sprintf("⚠️ Claude output could not be parsed (raw output was %d bytes). Check logs at `%s` for details.", len(rawOutput), logDir)
	}

	if runErr != nil {
		// If the context was cancelled (engine shutdown) and this was NOT our own
		// timeout kill, treat as interrupted — the engine is going away, bookkeeping
		// would be partial.
		if ctx.Err() != nil && !wasTimedOut {
			return text, false, usage, fmt.Errorf("claude exited with error: %w", runErr)
		}
		// Check whether the agent emitted the completion marker before the error.
		// This handles: (a) normal completion followed by extra work that ends non-zero,
		// and (b) timeout kills where FABRIK_STAGE_COMPLETE appeared in streamed output.
		if stageCompleteRE.MatchString(text) {
			claudeLog(issueNumber, "warn", "stage completed (marker found) but Claude exited with error: %v\n", runErr)
			return text, true, usage, fmt.Errorf("claude exited with error: %w", runErr)
		}
		// Structural turn-cap classification from the CLI's own result object,
		// not inferred from turn counts (the CLI's own accounting can report a
		// turn count past the configured cap, e.g. num_turns: 51 against
		// max_turns: 50, so an inference built on >= would be fragile). This
		// check runs before classifyUsageLimitExit precisely because relying
		// on output-prose matching there previously caused this exact
		// condition to misclassify as a usage-limit exit — see #1183.
		if ok && resp.Subtype == "error_max_turns" {
			claudeLog(issueNumber, "claude", "turn limit reached (subtype=error_max_turns, terminal_reason=%q, num_turns=%d)\n", resp.TerminalReason, resp.NumTurns)
			return text, false, usage, &claudeTurnLimitError{TerminalReason: resp.TerminalReason, NumTurns: resp.NumTurns}
		}
		// Structural usage-limit classification from the CLI's own result
		// object only — never from output prose (#1183). Requires a parsed
		// result object (ok); an unparseable-JSON exit has no structured
		// payload to trust, so it falls through to the generic error below
		// rather than being classified by any means.
		if ok {
			if msg, detected := classifyUsageLimitExit(resp, usage); detected {
				claudeLog(issueNumber, "claude-limit", "usage-limit exit detected (turns=%d, cost=$%.4f): %s\n", usage.TurnsUsed, usage.CostUSD, msg)
				return text, false, usage, &claudeUsageLimitError{Message: msg}
			} else if _, detected := classifyAPIErrorExit(resp, usage); detected {
				claudeLog(issueNumber, "claude", "api_error exit detected (turns=%d, cost=$%.4f) — stage did not run, not charged against max_retries\n", usage.TurnsUsed, usage.CostUSD)
				return text, false, usage, &claudeAPIErrorExit{TerminalReason: resp.TerminalReason, NumTurns: resp.NumTurns, CostUSD: resp.CostUSD}
			} else if resp.TerminalReason != "" && resp.TerminalReason != usageLimitTerminalReason {
				// Diagnostic-only: records any other non-empty terminal_reason
				// seen on an error exit (e.g. "rapid_refill_breaker"), so a
				// real-world sighting is captured in logs for review rather
				// than silently dropped. Never used to trigger classification.
				claudeLog(issueNumber, "claude", "error exit with unmatched terminal_reason=%q (not classified as usage limit)\n", resp.TerminalReason)
			}
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

// stallCorrectiveHintText is injected into the prompt (via buildPrompt) when the
// engine detects a stall on this stage's previous incomplete attempt — a
// turn-capped run followed by an incomplete run using strictly fewer turns,
// which does not happen for a genuinely-progressing retry (#1146). It is
// deliberately hedged: detection is a heuristic, not a confirmed diagnosis, so
// the hint must never assert the cause with certainty.
const stallCorrectiveHintText = `**Note from Fabrik:** the previous attempt at this stage hit its turn limit without completing, and the retry after it used noticeably fewer turns without completing either — a pattern consistent with a stall, most often caused by backgrounding a long-running command (e.g. a dev server, build, or test run) and then waiting for a completion notification that never arrives in this headless environment. If that's what happened, run any long-running command in the foreground with an explicit timeout instead of backgrounding it. If something else caused the previous attempt to stop short, disregard this note and continue as planned.`

func buildPrompt(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, baseBranch, correctiveHint string) string {
	var b strings.Builder

	if correctiveHint != "" {
		b.WriteString(correctiveHint)
		b.WriteString("\n\n---\n\n")
	}

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
	if baseBranch != "" {
		b.WriteString(fmt.Sprintf("The repository's default base branch is `%s`.\n\n", baseBranch))
	}
	b.WriteString("Context files are available in `.fabrik-context/` in your working directory:\n")
	b.WriteString("- `.fabrik-context/issue.md` — the issue body (spec)\n")
	b.WriteString("- `.fabrik-context/stage-{Name}.md` — output from prior stages (e.g. `.fabrik-context/stage-Research.md`)\n")
	if baseBranch != "" {
		b.WriteString(fmt.Sprintf("- `.fabrik-context/codebase-changes.md` — files changed on `%s` since the last stage (if any)\n", baseBranch))
	} else {
		b.WriteString("- `.fabrik-context/codebase-changes.md` — files changed on the default branch since the last stage (if any)\n")
	}
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
	b.WriteString("\nWhen outputting FABRIK_BLOCKED_ON_INPUT, you MUST also emit a summary block containing the specific question you need answered (this is distinct from the stage-completion summary above, if any — here the block must describe the required input, not summarize completed work):\n\n")
	b.WriteString("FABRIK_SUMMARY_BEGIN\n")
	b.WriteString("(1–3 sentences stating exactly what input you need — direct and specific, no preamble; the user reads this on a small screen)\n")
	b.WriteString("FABRIK_SUMMARY_END\n")
	b.WriteString("\nIf your stage determines that no code or documentation changes are required — the issue\n")
	b.WriteString("is already resolved or the work is genuinely moot — you may signal this by emitting:\n")
	b.WriteString("FABRIK_NO_WORK_NEEDED\n")
	b.WriteString("This marker MUST co-occur with FABRIK_STAGE_COMPLETE on its own line. The engine will\n")
	b.WriteString("mark all remaining pipeline stages complete (with \"skipped\" comments) and move the issue\n")
	b.WriteString("directly to Done without creating a PR. It is mutually exclusive with FABRIK_BLOCKED_ON_INPUT.\n")

	return b.String()
}

func buildCommentReviewPrompt(stage *stages.Stage, item gh.ProjectItem, comments []gh.Comment, baseBranch string) string {
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
		if c.Path != "" {
			// Review thread comment: include file/line/hunk context so Claude
			// can navigate directly to the relevant location.
			header := fmt.Sprintf("**@%s** (%s)", c.Author, c.CreatedAt.Format("2006-01-02 15:04"))
			if c.ReviewThreadID != "" {
				header += fmt.Sprintf(" [Thread: %s]", c.ReviewThreadID)
			}
			b.WriteString(header + "\n")
			lineNum := c.Line
			if lineNum == 0 {
				lineNum = c.OriginalLine
			}
			if lineNum != 0 {
				b.WriteString(fmt.Sprintf("**File:** `%s` **Line:** %d\n", c.Path, lineNum))
			} else {
				b.WriteString(fmt.Sprintf("**File:** `%s`\n", c.Path))
			}
			if c.DiffHunk != "" {
				b.WriteString("**Diff context:**\n```diff\n")
				b.WriteString(c.DiffHunk)
				b.WriteString("\n```\n")
			}
			b.WriteString(c.Body + "\n\n")
		} else if gh.IsBotLogin(c.Author) {
			// #1045 requirement 4: a bot-authored comment with no inline
			// thread context is structurally distinct from a human's
			// comment: it's a finding to evaluate and address autonomously,
			// not a decision awaiting the model's interpretation. This
			// branch covers both delivery shapes the issue names — a plain
			// PR body/issue comment with no formal review submission at all
			// (the original report; c.ID has no reviewBodyIDPrefix) and a
			// synthetic review-body comment from dispatchReviewReinvoke
			// (c.ID does carry reviewBodyIDPrefix) — both render with
			// c.Path == "" (no inline thread), and the marker's job is the
			// same in either case: tell the skill "this is bot review
			// content," not "this came from a formal review." Marking it
			// here (rather than asking the model to pattern-match "@login"
			// suffixes itself) makes the distinction a testable
			// prompt-content assertion (AC1/AC5) — gh.IsBotLogin is the same
			// structural helper isBotServiceNotice already uses, so this
			// reuses an existing, if inherently incomplete
			// (suffix/prefix/literal allow-list), detector rather than
			// adding a new one.
			b.WriteString(fmt.Sprintf("**@%s** (%s) [Bot Review Finding]:\n%s\n\n", c.Author, c.CreatedAt.Format("2006-01-02 15:04"), c.Body))
		} else {
			b.WriteString(fmt.Sprintf("**@%s** (%s):\n%s\n\n", c.Author, c.CreatedAt.Format("2006-01-02 15:04"), c.Body))
		}
	}

	b.WriteString("---\n\n")
	if baseBranch != "" {
		b.WriteString(fmt.Sprintf("The repository's default base branch is `%s`.\n\n", baseBranch))
	}
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

// formatSpawnReceiptNote returns a deterministic "parse receipt" note when
// output contains N > 0 well-formed spawn blocks, as counted by
// ParseSpawnBlocks — the sole source of truth for block counting (#1338).
// Never derive N by string-matching the marker text: that is the exact
// regression #1263 guards against, since a Plan that merely mentions the
// marker in prose must not produce a note. Returns "" when there are no
// blocks, so a non-decomposing stage comment is byte-identical to before
// this note existed.
//
// The note deliberately avoids any literal FABRIK_* token so it can never be
// mistaken for a marker by this or any other marker-detection logic, and is
// self-delimited with its own "---" rule so it renders as a clearly separate
// block from the stats footer.
//
// Callers must only invoke this for the Plan stage's own output. preImplement
// (engine/spawn.go) only ever reads the comment literally named "Plan", so a
// note rendered on any other stage's comment would promise a spawn that
// mechanism never performs — e.g. if a later stage's context (which includes
// the Plan comment verbatim) leads it to quote a spawn block back into its
// own output. See the stage.Name == "Plan" gate at the call site in
// finalizeStageOutcome (engine/item.go).
func formatSpawnReceiptNote(output string) string {
	n := len(ParseSpawnBlocks(output))
	if n == 0 {
		return ""
	}
	if n == 1 {
		return "\n\n---\n1 sub-issue declared above. It does not exist yet — it will be created when this issue advances to the **Implement** stage."
	}
	return fmt.Sprintf("\n\n---\n%d sub-issues declared above. None exist yet — they will be created when this issue advances to the **Implement** stage.", n)
}

// scaleTokens renders a token count as a human-readable, k/M-scaled string:
// raw digits below 1,000; "Nk" below 1,000,000; "N.1M" at or above 1,000,000. No
// billion-scale unit is provided: a single invocation's token counts realistically
// stay well under 1B, so values at or above that render as a large-but-readable "M" figure.
func scaleTokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1000000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
}

// formatStatsFooter returns a one-line stats summary suitable for appending to a comment.
// Returns empty string when no stats are available (e.g. JSON parse fallback). Input is
// reported as an "effective input" total (raw + cached) since prompt caching means raw
// InputTokens alone is structurally near-zero once a conversation has any history. Cache
// reads and cache writes are broken out separately (not folded into one "cached" figure)
// because Anthropic prices them differently per token, and collapsing them would obscure
// that distinction from a reader trying to reason about actual spend from the footer alone.
func formatStatsFooter(usage TokenUsage, completed bool) string {
	if usage.TurnsUsed == 0 && usage.InputTokens == 0 && usage.OutputTokens == 0 &&
		usage.CacheReadTokens == 0 && usage.CacheCreationTokens == 0 {
		return ""
	}
	var completion string
	if !completed {
		completion = " Stage incomplete."
	}
	cached := usage.CacheReadTokens + usage.CacheCreationTokens
	effectiveInput := usage.InputTokens + cached
	inputStr := scaleTokens(effectiveInput) + " input"
	if cached > 0 {
		breakdown := scaleTokens(usage.InputTokens) + " raw"
		if usage.CacheReadTokens > 0 {
			breakdown += " + " + scaleTokens(usage.CacheReadTokens) + " cache-read"
		}
		if usage.CacheCreationTokens > 0 {
			breakdown += " + " + scaleTokens(usage.CacheCreationTokens) + " cache-write"
		}
		inputStr = fmt.Sprintf("%s (%s)", inputStr, breakdown)
	}
	if usage.MaxTurns > 0 {
		return fmt.Sprintf("\n\n---\nUsed %d/%d turns, %s / %s output tokens.%s",
			usage.TurnsUsed, usage.MaxTurns, inputStr, scaleTokens(usage.OutputTokens), completion)
	}
	return fmt.Sprintf("\n\n---\nUsed %d turns, %s / %s output tokens.%s",
		usage.TurnsUsed, inputStr, scaleTokens(usage.OutputTokens), completion)
}

// formatStatsLogLine returns a one-line, machine-greppable stats summary for operator logs,
// matching poll.go's cumulative "in: N | out: N | cache_read: N | cache_write: N" convention.
// Returns empty string when no stats are available so callers can suppress the log line.
func formatStatsLogLine(usage TokenUsage) string {
	if usage.TurnsUsed == 0 && usage.InputTokens == 0 && usage.OutputTokens == 0 &&
		usage.CacheReadTokens == 0 && usage.CacheCreationTokens == 0 {
		return ""
	}
	var turns string
	if usage.MaxTurns > 0 {
		turns = fmt.Sprintf("used %d/%d turns", usage.TurnsUsed, usage.MaxTurns)
	} else {
		turns = fmt.Sprintf("used %d turns", usage.TurnsUsed)
	}
	return fmt.Sprintf("%s | in: %d | out: %d | cache_read: %d | cache_write: %d",
		turns, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheCreationTokens)
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

// degenerateAtRefRE matches a bare "@some/path" reference with no other content —
// the pattern produced when a model writes its real output to a file and returns
// a dangling shell-style reference instead of the content itself.
var degenerateAtRefRE = regexp.MustCompile(`^@\S+$`)

// degenerateAbsPathRE matches a bare absolute filesystem path (at least two
// segments) with no other content, e.g. "/tmp/plan_comment.md". Relative paths
// (no leading "/") are deliberately NOT matched — they are far more likely to be
// legitimate short prose that merely contains a slash.
var degenerateAbsPathRE = regexp.MustCompile(`^/[^\s/]+(?:/[^\s/]+)+$`)

// isDegenerateOutput reports whether s — a trimmed stage output about to be posted
// as a comment — is nothing but a bare file reference: an "@file" attachment-style
// token, or a bare absolute filesystem path. Such output indicates the model wrote
// its real content to a file and returned a dangling reference instead of emitting
// it inline (see issue #1065). Deliberately conservative: only a single-line body
// that is entirely consumed by one of these two patterns qualifies — any other
// content on the line, or a multi-line body, is never flagged.
func isDegenerateOutput(s string) bool {
	s = strings.TrimSpace(strings.Trim(s, "\"`'"))
	if s == "" || strings.Contains(s, "\n") {
		return false
	}
	return degenerateAtRefRE.MatchString(s) || degenerateAbsPathRE.MatchString(s)
}

// extractSummary parses a brief summary from Claude's output.
func extractSummary(output string) string {
	return extractBetweenMarkers(output, "FABRIK_SUMMARY_BEGIN", "FABRIK_SUMMARY_END")
}

// forEachAssistantText scans raw NDJSON output line by line, and for each
// {"type":"assistant",...} message invokes fn with the concatenated text of
// its content blocks. Non-JSON, blank, or non-assistant lines are skipped.
func forEachAssistantText(rawOutput []byte, fn func(text string)) {
	scanner := bufio.NewScanner(bytes.NewReader(rawOutput))
	// A single NDJSON line (e.g. a large tool_use/tool_result block) can exceed
	// bufio.Scanner's default 64KB max token size; a line can never be longer
	// than the whole input, so len(rawOutput)+1 always fits without truncating.
	scanner.Buffer(make([]byte, 0, 64*1024), len(rawOutput)+1)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
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
		var sb strings.Builder
		for _, block := range envelope.Message.Content {
			if block.Type == "text" {
				sb.WriteString(block.Text)
			}
		}
		fn(sb.String())
	}
}

// extractIssueUpdateFromAssistantTurns scans raw NDJSON output for the last
// FABRIK_ISSUE_UPDATE_BEGIN/END block across all {"type":"assistant",...} messages.
// Returns the reconstructed "FABRIK_ISSUE_UPDATE_BEGIN\n<body>\nFABRIK_ISSUE_UPDATE_END"
// string if found, or empty string if not. Used as a fallback when the markers
// do not appear in the result field (emitted in an intermediate turn).
func extractIssueUpdateFromAssistantTurns(rawOutput []byte) string {
	var lastBlock string
	forEachAssistantText(rawOutput, func(text string) {
		if body := extractUpdatedBody(text); body != "" {
			lastBlock = "FABRIK_ISSUE_UPDATE_BEGIN\n" + body + "\nFABRIK_ISSUE_UPDATE_END"
		}
	})
	return lastBlock
}

// extractTextFromAssistantTurns scans raw NDJSON output and concatenates all text
// content from {"type":"assistant",...} messages. Used after a timeout kill to
// recover FABRIK_STAGE_COMPLETE and other markers from the streamed output when
// the Claude process was terminated before emitting a final "result" JSON line.
func extractTextFromAssistantTurns(rawOutput []byte) string {
	var sb strings.Builder
	forEachAssistantText(rawOutput, func(text string) {
		sb.WriteString(text)
	})
	return sb.String()
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
func saveSessionIDDirect(issueNumber int, path, sessionID string) {
	if sessionID == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		claudeLog(issueNumber, "warn", "failed to create session dir for %s: %v\n", path, err)
		return
	}
	if err := os.WriteFile(path, []byte(sessionID), 0600); err != nil {
		claudeLog(issueNumber, "warn", "failed to save session id to %s: %v\n", path, err)
	}
}
