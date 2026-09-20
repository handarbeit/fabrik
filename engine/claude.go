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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/claudeerr"
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
//
// A second structural shape carries the same condition: terminal_reason
// "api_error" with api_error_status 429 (the HTTP status behind the failure).
// See apiErrorRateLimitStatus and ADR-1811.
const usageLimitTerminalReason = "blocking_limit"

// apiErrorRateLimitStatus is the HTTP status the CLI reports in
// api_error_status when an "api_error" exit is really a rate/usage-limit hit.
// Unlike a 5xx, this is an account-wide condition, so it is routed to
// claudeUsageLimitError rather than claudeAPIErrorExit. See ADR-1811.
const apiErrorRateLimitStatus = 429

// apiErrorStatus is the CLI's "api_error_status" result field (the HTTP status
// behind an api_error exit). It decodes tolerantly: parseClaudeJSON and
// tryParseResultMessage discard the entire result object on any unmarshal
// error, so a strict int would let one wrongly-typed value (e.g. the string
// "429") erase every classification, blocking_limit included. Only a JSON
// integer is accepted; absent, null, string, float, or anything else reads as
// 0, i.e. "not a 429" (fail-safe: under-detecting costs retries,
// over-detecting suspends a healthy account).
type apiErrorStatus int

// UnmarshalJSON implements json.Unmarshaler. It never returns an error.
func (s *apiErrorStatus) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		*s = 0
		return nil
	}
	*s = apiErrorStatus(n)
	return nil
}

// claudeUsageLimitError, claudeTurnLimitError, claudeAPIErrorExit, and
// claudeResumeFailureError are unexported aliases of the exported types in
// internal/claudeerr, where the definitions (and their doc comments) live.
// They are aliases, not new types (`type X = claudeerr.X`), so every existing
// construction site and errors.As assertion in this package (and its test
// files) keeps compiling and behaving unchanged — this extraction moved the
// types' location, not their name, identity, or behavior. See
// internal/claudeerr's package doc and ADR-1449.
type claudeUsageLimitError = claudeerr.UsageLimitError
type claudeTurnLimitError = claudeerr.TurnLimitError
type claudeAPIErrorExit = claudeerr.APIErrorExit
type claudeResumeFailureError = claudeerr.ResumeFailureError
type claudeToolsDeniedError = claudeerr.ToolsDeniedError

// apiErrorTerminalReason is the CLI's structural terminal_reason value for an
// Anthropic-side API error — observed live on 2026-08-08 (#1458) as eight
// exits across two issues, each at 1 turn and $0.0000. For a non-429
// api_error_status this is per-invocation and self-resolving: it says nothing
// about the account as a whole, so it must never trigger
// activateClaudeSuspension (see claudeAPIErrorExit's doc comment and #1458 R3).
// An api_error carrying api_error_status 429 is a session/usage limit, not a
// transient blip (ADR-1811): classifyUsageLimitExit claims it first, so only
// the remaining statuses reach classifyAPIErrorExit.
const apiErrorTerminalReason = "api_error"

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
//
// This function does not itself inspect api_error_status: the caller
// (interpretClaudeResult) tries classifyUsageLimitExit first, which claims the
// 429 subset (ADR-1811), leaving only non-429 api_errors for this path.
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
//
// A second structural trigger is terminal_reason "api_error" together with
// api_error_status 429 (ADR-1811): the CLI reports a session limit that way
// too. Only the structured fields are read — never resp.Result, whose text
// ("You've hit your session limit · resets ...") must not participate in
// classification (#1183). An absent, zero, or unparseable status is not a 429
// and falls through to classifyAPIErrorExit. The same exclusion gate applies
// to both triggers, so a mid-session 429 after real work (turns and cost both
// non-zero) is deliberately not detected. This function decides only *whether*
// the exit is a usage limit; the suspension's end instant is supplied separately
// by extractUsageLimitReset from the CLI's structured rate_limit_event line
// (ADR-1815) — never from the result text, which would let prose reach a
// behavioural decision.
func classifyUsageLimitExit(resp claudeResponse, usage TokenUsage) (msg string, detected bool) {
	switch {
	case resp.TerminalReason == usageLimitTerminalReason:
		msg = fmt.Sprintf("terminal_reason=%q", resp.TerminalReason)
	case resp.TerminalReason == apiErrorTerminalReason && resp.APIErrorStatus == apiErrorRateLimitStatus:
		msg = fmt.Sprintf("terminal_reason=%q api_error_status=%d", resp.TerminalReason, int(resp.APIErrorStatus))
	default:
		return "", false
	}
	if usage.TurnsUsed > 0 && usage.CostUSD > 0 {
		return "", false
	}
	return msg, true
}

// permissionDenial is one entry of the CLI's "permission_denials" array on
// the terminal result line. Named (rather than left as an anonymous inline
// struct) so tool_input can be decoded without forcing every construction
// site — including test fixtures — to redeclare the widened shape. ToolInput
// is decoded lazily and best-effort (see decodeToolCommand): its JSON shape
// is CLI-version-specific and undocumented (ADR-1523), so this type only
// declares what's needed to reach it, not its contents.
type permissionDenial struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// decodeToolCommand best-effort extracts a Bash tool_input's .command field.
// Returns "" for absent input, a decode failure, or a non-string/missing
// command field — never panics, never fabricates a value. Callers gate this
// on ToolName == "Bash" (see classifyToolsDenied): no captured evidence
// exists for other tools' tool_input shapes, so this is not attempted for
// them.
func decodeToolCommand(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v.Command
}

// toolDenial is engine's working alias for claudeerr.ToolDenial — one denied
// tool call, with command detail when decodable. See claudeerr.ToolDenial's
// doc comment.
type toolDenial = claudeerr.ToolDenial

// classifyToolsDenied determines whether a Claude invocation was blocked from
// making progress by the CLI's own permission layer denying one or more
// mutating tool calls, using only the CLI's own structured result object
// (resp) — never text the assistant itself wrote.
//
// resp.PermissionDenials (the CLI's "permission_denials" array on the
// terminal result line) is the sole structural signal: empirically captured
// against the installed CLI (2.1.227) using Fabrik's own invocation flags
// (--output-format stream-json --verbose --permission-mode dontAsk), a
// PreToolUse-hook-denied tool call populates this array on an otherwise
// ordinary clean exit (is_error=false, terminal_reason="completed") — there
// is no dedicated terminal_reason value the way there is for a usage-limit
// or api_error exit, so unlike classifyUsageLimitExit/classifyAPIErrorExit
// there is no adjacent field available as a belt-and-suspenders exclusion
// gate. See ADR-1523 for the full captured evidence and #1523.
//
// Callers are expected to gate this on !completed (see interpretClaudeResult)
// — a denial the model worked around and still finished the stage is
// ordinary success, not a condition to exempt from anything.
//
// toolNames is deduplicated (preserving first-seen order) so a tool denied
// repeatedly across multiple attempts within the same invocation is named
// once in the R4 explanatory comment, not once per denial. denials carries
// one entry per raw PermissionDenials entry (not deduplicated) with command
// detail decoded only for ToolName == "Bash" — see #1775.
func classifyToolsDenied(resp claudeResponse) (toolNames []string, denials []toolDenial, detected bool) {
	if len(resp.PermissionDenials) == 0 {
		return nil, nil, false
	}
	seen := make(map[string]bool, len(resp.PermissionDenials))
	for _, d := range resp.PermissionDenials {
		if d.ToolName == "" {
			continue
		}
		command := ""
		if d.ToolName == "Bash" {
			command = decodeToolCommand(d.ToolInput)
		}
		denials = append(denials, toolDenial{ToolName: d.ToolName, Command: command})
		if seen[d.ToolName] {
			continue
		}
		seen[d.ToolName] = true
		toolNames = append(toolNames, d.ToolName)
	}
	if len(toolNames) == 0 {
		return nil, nil, false
	}
	return toolNames, denials, true
}

// toolsDeniedCommandMaxLen/HeadLen/TailLen size sanitizeToolsDeniedCommand's
// truncation for a denied Bash command — a single logical line, realistically
// tens to a few hundred chars — distinct from merge_train.go's
// trainDiagPerCheck* constants, which are sized for multi-KB CI output
// blocks. Reuses truncateMiddle itself (the established in-repo convention
// for rendering untrusted/variable-length text in a comment), just not its
// CI-sized constants.
const (
	toolsDeniedCommandMaxLen  = 200
	toolsDeniedCommandHeadLen = 140
	toolsDeniedCommandTailLen = 40
)

// sanitizeToolsDeniedCommand renders a denied command safely for a
// single-line inline code span: embedded newlines are collapsed to spaces
// (a multi-line command would otherwise break a one-line comment sentence,
// or a log line), backticks are replaced with a straight quote (an embedded
// backtick would otherwise prematurely close the inline code span), and the
// result is truncated via truncateMiddle. truncateMiddle's own omission
// marker embeds newlines (correct for its fenced-block callers in
// merge_train.go) — those are collapsed too, so the final result is always
// single-line regardless of input. See R2/AC3.
func sanitizeToolsDeniedCommand(cmd string) string {
	if cmd == "" {
		return ""
	}
	replacer := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "`", "'")
	sanitized := strings.TrimSpace(replacer.Replace(cmd))
	if sanitized == "" {
		return ""
	}
	truncated := truncateMiddle(sanitized, toolsDeniedCommandMaxLen, toolsDeniedCommandHeadLen, toolsDeniedCommandTailLen)
	return strings.ReplaceAll(truncated, "\n", " ")
}

// firstToolsDeniedCommand returns the tool name and sanitized command of the
// first denial in denials that carries a non-empty command, and ok=true. When
// no denial carries a command (nil/empty denials, or every entry's Command is
// ""), ok is false and callers must degrade to tool-name-only wording — never
// an empty string standing in for a command (R2/AC2).
func firstToolsDeniedCommand(denials []toolDenial) (toolName, command string, ok bool) {
	for _, d := range denials {
		if d.Command == "" {
			continue
		}
		sanitized := sanitizeToolsDeniedCommand(d.Command)
		if sanitized == "" {
			continue
		}
		return d.ToolName, sanitized, true
	}
	return "", "", false
}

// toolsDeniedLogSummary renders the "tools=X" or "tools=X; first: `cmd`"
// fragment shared by both classifyToolsDenied log lines (R2/R9).
func toolsDeniedLogSummary(toolNames []string, denials []toolDenial) string {
	summary := fmt.Sprintf("tools=%s", strings.Join(toolNames, ", "))
	if _, command, ok := firstToolsDeniedCommand(denials); ok {
		summary += fmt.Sprintf("; first: `%s`", command)
	}
	return summary
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
	"Bash(date:*)",
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
	// Monitor promises to watch for a condition over time and report back
	// "on its own schedule" — a same-turn reply is explicitly not the
	// contract. A headless Fabrik stage has no future turn to receive that
	// report: the turn ends, the invocation is denied outright (a lost
	// invocation, not just a stall), and the denial spuriously exempts the
	// attempt from max_retries (see #1556). Confirmed reachable and denied
	// in production. See #1558.
	"Monitor",
	// CronCreate enqueues a prompt for a future time and fires "only while
	// the REPL is idle" — the same shape as ScheduleWakeup, recurring
	// instead of one-shot. A headless stage is never later idle to receive
	// the fire, so the promised delivery can never happen. See #1558.
	"CronCreate",
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

// claudeGHTokenOverrideFn, when non-nil, supplies the worker's GH_TOKEN/
// GITHUB_TOKEN value instead of claudeGHToken — set by Engine.New() only in
// GitHub App auth mode (#1713), where there is no static token to copy:
// AppID/AppPrivateKeyPath/AppInstallationID mint a ~1h installation token
// that a background refresh loop keeps current for the engine's own API
// calls. Reading it live here (typically Client.Token(), riding that same
// refresh loop) means a worker's `gh` invocations (e.g. fabrik-validate's
// Pre-Completion Gate) authenticate as the same installation the engine
// itself uses, without a second minting path. nil (the default, and always
// true in PAT mode) leaves buildClaudeEnv's claudeGHToken injection exactly
// as it was — R1/AC2 byte-identical behavior.
//
// Residual, accepted limitation: the value is read once, at this
// invocation's env-build time — a single invocation whose wall time exceeds
// the token's ~1h lifetime can still see a now-expired value for its
// remaining `gh` calls, since a running child process's environment can't
// be updated after the fact. See adrs/1713-engine-github-app-auth.md.
//
// Single-Engine-per-process assumption: like claudeGHToken/claudeGHHost/
// claudeAnthropicAPIKey below, this is a package-level global rather than an
// Engine field — two Engine instances running in the same process would
// stomp each other's value. That was already true for the plain-scalar vars
// above; this one is the first to close over per-instance state (a
// *gh.Client) rather than a plain string, so the same assumption is worth
// spelling out here explicitly rather than leaving it implicit.
var claudeGHTokenOverrideFn func() string

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
	output, completed, usage, err := runClaude(ctx, args, prompt, workDir, issue.Number, stage.Name, issue.Repo, sessFilePath, ld, extraEnv, wallTime, effectiveBudget, opts.OnPIDReady, sigIntGrace, sigTermGrace, resumeSessionID, opts.MaxResumeFailures)
	usage.MaxTurns = effectiveBudget
	if err != nil {
		return output, completed, usage, err
	}
	// AND, never overwrite: checkCompletion re-derives completion from the
	// stage's configured Completion.Type (a no-op re-match of the same marker
	// for the "claude"/"" type every real stage uses today, but `false` for
	// any other type — a config-driven signal orthogonal to what runClaude
	// just decided). completed already carries interpretClaudeResult's R3
	// guard (artifactMissingOnComplete) — blindly replacing it with
	// checkCompletion's marker-only regex would silently discard that guard
	// for this exact call path, since the bare FABRIK_STAGE_COMPLETE line is
	// never stripped from output before this point (#1782).
	return output, completed && checkCompletion(stage, output), usage, nil
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
	output, completed, usage, err := runClaude(ctx, args, prompt, workDir, issue.Number, stage.Name+"-comment-review", issue.Repo, sessFilePath, ld, extraEnv, wallTime, limit, opts.OnPIDReady, sigIntGrace, sigTermGrace, resumeSessionID, opts.MaxResumeFailures)
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
	ghToken := claudeGHToken
	if claudeGHTokenOverrideFn != nil {
		ghToken = claudeGHTokenOverrideFn()
	}
	if ghToken != "" {
		env = append(env, "GH_TOKEN="+ghToken, "GITHUB_TOKEN="+ghToken)
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
// given (repo, issueNumber, stageName). Originally purely observational (a
// human `ps | grep` aid), it is now also a liveness-verification signal
// (#1779): runWorkerDetectorScan (worker_liveness.go) and dispatchCandidates
// (poll.go) both probe the process table for this exact value and branch on
// whether it's found — see sentinel_probe.go and docs/state-machine.md §9.7.
// repo is expected to already be "owner/repo" (as populated from the GitHub
// GraphQL response on real board items); an empty repo falls back to the
// literal "unknown/repo" rather than producing a malformed sentinel.
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
	// APIErrorStatus is the HTTP status behind a terminal_reason "api_error"
	// exit; 429 marks a session/usage limit (ADR-1811). Decodes tolerantly —
	// see apiErrorStatus.
	APIErrorStatus apiErrorStatus `json:"api_error_status"`
	// PermissionDenials lists each tool call the CLI's permission layer
	// denied during this invocation (e.g. a mutating tool blocked by a
	// PreToolUse hook or an "ask" permission rule with no interactive prompt
	// available). Populated on an otherwise clean exit — see
	// classifyToolsDenied and ADR-1523.
	PermissionDenials []permissionDenial `json:"permission_denials"`
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

func runClaude(ctx context.Context, args []string, prompt string, workDir string, issueNumber int, label string, repo string, sessFilePath string, logDir string, extraEnv []string, maxWallTime time.Duration, maxTurns int, onPIDReady func(int), sigIntGrace, sigTermGrace time.Duration, resumeSessionID string, maxResumeFailures int) (string, bool, TokenUsage, error) {
	claudeLog(issueNumber, "claude", "invoking (%s) in %s\n", label, workDir)

	// Best-effort HEAD capture, before/after the invocation, so a tools-denied
	// classification below can report a measured commit count instead of
	// asserting "did not make progress" without having checked (#1743). Errors
	// are conventionally discarded here exactly as gitHeadSHA's other callers
	// already do (dispatchReviewReinvoke, engine/ci.go) — an empty headBefore
	// degrades interpretClaudeResult to the no-count wording, never a panic.
	headBefore, _ := gitHeadSHA(workDir)

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

	// watchdogWG lets the caller wait for every watchdogCtx-scoped goroutine
	// below to actually observe cancellation and return, before either
	// reaping descendants or letting runClaude return to a caller that may
	// immediately re-mutate a package-level test seam (e.g. a subsequent
	// test overwriting claudeInactivityTimeout). Without this wait, a
	// goroutine that hasn't yet been scheduled to run its first statement by
	// the time runClaude returns can still race a later test's write to the
	// same variable — confirmed via -race across repeated runs (2/8) on this
	// package, 0/20 on origin/main before this wait covered the inactivity
	// watchdog too.
	var watchdogWG sync.WaitGroup

	// Session-scoped descendant tracking (#1798 R1): observes the process tree
	// WHILE the invocation is live, recording (in the durable registry) every
	// process whose session ID equals this worker's own PID — including ones
	// that later detach (nohup/disown, backgrounding) and get reparented to
	// init on a sub-second timescale, well before a post-hoc walk at teardown
	// could see the transient ancestry. Stopped via watchdogCtx alongside the
	// inactivity watchdog below.
	//
	// Waiting for this goroutine to actually observe watchdogCtx cancellation
	// and return, before reapTrackedDescendants runs (below), matters
	// independently of the race described above: without it, a goroutine
	// mid-tick (e.g. blocked inside pidFingerprintFn for a just-discovered
	// descendant) could persist a new registry entry for this workerPID
	// *after* reapTrackedDescendants has already read and cleared them,
	// silently deferring that descendant's reap from "unconditional at
	// invocation end" (R2) to the next R3 backstop sweep — bounded by
	// JanitorIntervalHours, potentially hours.
	watchdogWG.Add(1)
	go func() {
		defer watchdogWG.Done()
		trackWorkerDescendants(watchdogCtx, pid, issueNumber, repo, label)
	}()

	// Durable worker record (#1814): written synchronously now, when pid
	// belongs to exactly one process, so the registry-independent session sweep
	// below (and the periodic proc-janitor) can find a descendant that
	// trackWorkerDescendants's tick never recorded. If the fingerprint capture
	// failed, retry it in the background so the record's live-worker phase stays
	// identifiable.
	workerRec := beginWorkerRecord(pid, issueNumber, repo, label)
	if workerRec.LStart == "" {
		watchdogWG.Add(1)
		go func() {
			defer watchdogWG.Done()
			backfillWorkerFingerprint(watchdogCtx, workerRec.ID, pid)
		}()
	}

	// Inactivity watchdog: kills the process group if no stdout is received for
	// claudeInactivityTimeout, indicating a stuck session regardless of wall time.
	// Stopped via watchdogCtx after cmd.Wait returns.
	var inactivityFired atomic.Bool
	watchdogWG.Add(1)
	go func(pid int) {
		defer watchdogWG.Done()
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
	watchdogCancel() // stop both watchdog goroutines promptly
	// Wait for both watchdogCtx-scoped goroutines above to actually observe
	// the cancellation and return — see watchdogWG's doc comment. Bounded by
	// pidFingerprintFn's own internal timeout (sentinelProbeTimeout) for the
	// tracker goroutine, and by an immediate ctx.Done() check for the
	// inactivity-watchdog goroutine, so this cannot hang.
	watchdogWG.Wait()
	killProcGroup(cmd, issueNumber, label)
	// R2: reap any session-scoped descendant that survived killProcGroup's
	// PGID-scoped kill (e.g. detached via nohup/disown, or otherwise no
	// longer a member of the worker's process group) — unconditional, on
	// every invocation end, clean exit or not.
	if reaped, _ := reapTrackedDescendants(pid, issueNumber); reaped > 0 {
		claudeLog(issueNumber, "kill", "reaped %d session-scoped descendant(s) at invocation end\n", reaped)
	}
	// #1814 R5: registry-independent sweep by the worker's session ID. Catches
	// a descendant spawned in the final seconds (never recorded by the tracker's
	// tick, hence invisible to reapTrackedDescendants) using a fresh process
	// scan, so correctness does not depend on how recently it was created.
	if reaped := sweepWorkerSessionAtInvocationEnd(workerRec); reaped > 0 {
		claudeLog(issueNumber, "kill", "reaped %d unrecorded session member(s) of worker PID %d at invocation end\n", reaped, pid)
	}
	rawOutput := stdout.Bytes()

	// wasTimedOut is true when this process was terminated by our own timeout
	// (wall-time deadline or inactivity watchdog) rather than an external engine
	// shutdown. When true, the ctx.Err() engine-shutdown guard is bypassed so
	// we still process whatever output was collected before the kill.
	wasTimedOut := inactivityFired.Load() || (stageCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil)

	// Best-effort commit-count measurement (#1743): -1 means "not measured,"
	// consumed only by interpretClaudeResult's tools-denied branch. Requires
	// both SHAs and a genuine change between them — a fresh worktree with no
	// prior commits, or a gitHeadSHA failure on either side, degrades cleanly
	// to -1 rather than a spurious "0 commit(s)".
	commitsPushed := -1
	headAfter, _ := gitHeadSHA(workDir)
	if headBefore != "" && headAfter != "" && headBefore != headAfter {
		if n, err := gitCommitCountBetween(workDir, headBefore, headAfter); err == nil {
			commitsPushed = n
		}
	}

	return interpretClaudeResult(ctx, issueNumber, rawOutput, runErr, wasTimedOut, sessFilePath, logDir, resumeSessionID, maxResumeFailures, commitsPushed)
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

// classifyResumeFailure implements the consecutive-resume-failure counter's
// increment/threshold/abandon logic for the generic-failure fallthrough in
// interpretClaudeResult (see #1414). It is only reached for a failure not
// already classified by a more specific check above it (stale session,
// turn-cap, usage-limit, api_error) — see interpretClaudeResult's final
// return point.
//
//   - maxResumeFailures <= 0 disables the mechanism entirely (mirrors
//     MaxRetries == 0's "unlimited" convention): the sidecar is left
//     untouched and the plain, unwrapped error is returned so normal
//     max_retries accounting applies exactly as it did before #1414.
//     resolveInt always resolves a positive default, so production never
//     exercises this branch.
//   - resumeSessionID == "" means this invocation was itself a cold start
//     (no --resume) that failed — not attributable to a resumed session, so
//     the count resets to 0 (clearing any stale leftover from a
//     since-abandoned or since-pruned prior session lineage) and the plain
//     error is returned unwrapped.
//   - Otherwise the count increments. At or past maxResumeFailures, the
//     session pointer is discarded (the same os.Remove used by the existing
//     stale-session path) and the sidecar reset to 0, so the very next
//     invocation on either invocation path cold-starts —
//     resolveResumeSessionID already treats an absent session file as a
//     cold start for both callers identically, so no separate "force cold
//     start" signal is needed. Every occurrence of this branch — not only
//     the one that crosses the threshold — returns *claudeResumeFailureError
//     so finalizeStageOutcome can exempt it from StageRetryIncremented; see
//     that type's doc comment for why.
func classifyResumeFailure(issueNumber int, sessFilePath, resumeSessionID string, maxResumeFailures int, lastErr error) error {
	plain := fmt.Errorf("claude exited with error: %w", lastErr)
	if maxResumeFailures <= 0 {
		return plain
	}
	if resumeSessionID == "" {
		resetResumeFailureCount(sessFilePath)
		return plain
	}
	count := readResumeFailureCount(sessFilePath) + 1
	abandoned := count >= maxResumeFailures
	if abandoned {
		claudeLog(issueNumber, "resume", "abandoning session %s after %d consecutive resume failures (threshold %d), last error: %v — branch work on disk is unaffected, only the resume pointer is discarded; next invocation will cold-start\n", resumeSessionID, count, maxResumeFailures, lastErr)
		os.Remove(sessFilePath)
		resetResumeFailureCount(sessFilePath)
	} else {
		writeResumeFailureCount(issueNumber, sessFilePath, count)
	}
	return &claudeResumeFailureError{
		Cause:               lastErr,
		SessionID:           resumeSessionID,
		ConsecutiveFailures: count,
		Threshold:           maxResumeFailures,
		Abandoned:           abandoned,
	}
}

// interpretClaudeResult classifies a completed Claude invocation's raw NDJSON
// output: it checks for a WaitDelay-related exit override, parses the
// stream-json result, extracts result text and token usage (falling back to
// scanning intermediate assistant turns when JSON parsing failed or the
// process was killed before emitting a result line), and classifies the
// completion/error status of the invocation from the parsed or extracted
// text.
//
// resumeSessionID is the --resume session ID this invocation attempted (""
// if this was a cold start) and maxResumeFailures is the effective
// MaxResumeFailures threshold — both threaded through from the caller's
// InvokeOptions purely to drive classifyResumeFailure; see #1414.
//
// commitsPushed is the number of commits the caller measured between the
// worktree's HEAD before and after this invocation (runClaude's
// headBefore/headAfter, via gitCommitCountBetween), or -1 when not
// measured/measurable. It is consulted only by the tools-denied branch below
// (#1743, R8), to report a measured "N commit(s) pushed" instead of
// asserting "did not make progress" — a claim interpretClaudeResult never
// actually checked.
func interpretClaudeResult(ctx context.Context, issueNumber int, rawOutput []byte, runErr error, wasTimedOut bool, sessFilePath, logDir string, resumeSessionID string, maxResumeFailures int, commitsPushed int) (string, bool, TokenUsage, error) {
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
		// General artifact-harvest fallback (#1782/R1/R2): the CLI's terminal
		// "result" field is whichever text the agent emitted in its very last
		// turn — if that turn was a wrap-up following one more tool call after
		// the real artifact, resp.Result carries the marker but no content.
		// artifactMissingOnComplete gates on !CheckNoWorkNeeded, so a
		// legitimate artifact-free completion is never scanned into. When it
		// fires, recover the last assistant turn that has real content beyond
		// the bare control markers — deliberately not "the turn containing the
		// marker," since the reported case's marker-bearing turn contains only
		// the marker itself (see ADR-1782).
		if artifactMissingOnComplete(text) {
			if artifact := extractLastSubstantialAssistantTurn(rawOutput); artifact != "" {
				claudeLog(issueNumber, "warn", "stage-complete marker present but terminal result carried no artifact — recovered %d bytes from an earlier assistant turn\n", len(artifact))
				text = artifact + "\n" + text
			} else {
				claudeLog(issueNumber, "warn", "stage-complete marker present but no artifact could be harvested from the terminal result or any assistant turn\n")
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
		// artifactMissingOnComplete (R3): a marker with no harvestable artifact
		// anywhere is not evidence of a healthy completion — fall through to
		// the classifiers below instead of returning completed=true.
		if stageCompleteRE.MatchString(text) && !artifactMissingOnComplete(text) {
			claudeLog(issueNumber, "warn", "stage completed (marker found) but Claude exited with error: %v\n", runErr)
			// Completed is completed — the strongest possible evidence the
			// session is healthy, regardless of the trailing error. Reset the
			// resume-failure counter (#1414).
			resetResumeFailureCount(sessFilePath)
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
			// A turn-cap exit consumed real turns and real cost — by
			// construction the strongest possible evidence the session is
			// healthy, not the poisoned-session symptom #1414 targets. Reset
			// the resume-failure counter.
			resetResumeFailureCount(sessFilePath)
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
				// The reset instant lives on a separate rate_limit_event stream line,
				// not on the result object (ADR-1815); a best-effort raw-stream scan
				// that can never affect the classification above.
				resetAt, resetReason := extractUsageLimitReset(rawOutput)
				return text, false, usage, &claudeUsageLimitError{Message: msg, ResetAt: resetAt, ResetFallbackReason: resetReason}
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
			// Diagnostic-only: classifyToolsDenied is only ever consulted on
			// the clean-exit path below (per the empirical evidence — see
			// classifyToolsDenied's doc comment), but if permission_denials
			// ever shows up alongside a non-zero exit too, log it for future
			// evidence rather than silently discarding it. Never classifies.
			if toolNames, denials, detected := classifyToolsDenied(resp); detected {
				claudeLog(issueNumber, "claude", "permission_denials present on non-clean exit (%s, terminal_reason=%q) — not classified here, evidence only\n", toolsDeniedLogSummary(toolNames, denials), resp.TerminalReason)
			}
		}
		// None of the classifiers above matched — the generic fallthrough.
		// classifyResumeFailure owns the consecutive-resume-failure counter
		// from here: it increments (and abandons the session at threshold)
		// when resumeSessionID != "", or resets when this was itself a
		// failed cold start. See its doc comment and #1414.
		return text, false, usage, classifyResumeFailure(issueNumber, sessFilePath, resumeSessionID, maxResumeFailures, runErr)
	}

	// A clean process exit (runErr == nil) is evidence the session loaded and
	// ran without a structural break, even if the stage itself didn't finish
	// (no FABRIK_STAGE_COMPLETE). Reset the resume-failure counter (#1414).
	resetResumeFailureCount(sessFilePath)
	// artifactMissingOnComplete (R3): never label a stage complete when the
	// marker is present but no artifact could be harvested anywhere (and this
	// isn't a legitimate FABRIK_NO_WORK_NEEDED completion) — that is the exact
	// #1632/#1782 defect, discovered only a stage later before this guard.
	completed := stageCompleteRE.MatchString(text) && !artifactMissingOnComplete(text)
	// Gated on !completed: a denial the model worked around and still
	// completed the stage is ordinary success — no exemption, no label. See
	// classifyToolsDenied's doc comment and ADR-1523.
	if !completed && ok {
		if toolNames, denials, detected := classifyToolsDenied(resp); detected {
			// "did not signal completion" is exactly what !completed
			// establishes — free to state, always true on this branch. Unlike
			// its predecessor's "did not make progress," it is never asserted
			// without having been measured (#1743): commitsPushed > 0 upgrades
			// this to the measured form when the caller could determine one
			// (see runClaude's headBefore/headAfter capture); -1 or 0 means
			// "not measured" or "no commits," and the wording degrades to the
			// completion-only phrasing rather than printing "0 commit(s)".
			progress := "stage did not signal completion"
			if commitsPushed > 0 {
				progress = fmt.Sprintf("%d commit(s) pushed, stage did not signal completion", commitsPushed)
			}
			claudeLog(issueNumber, "claude", "tool permission denial(s) detected (%s) — %s; not charged against max_retries\n", toolsDeniedLogSummary(toolNames, denials), progress)
			return text, false, usage, &claudeToolsDeniedError{ToolNames: toolNames, Denials: denials}
		}
	}
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
// engine detects a stall on this stage's previous incomplete attempt — a clean
// incomplete run (whether it hit its turn limit or stopped short on its own)
// followed by an incomplete run using strictly fewer turns, which does not
// happen for a genuinely-progressing retry (#1146, #1767). The predecessor is
// deliberately not described as having hit its turn limit: #1767 loosened
// detection to also catch a worker that recognizes it's waiting on a
// backgrounded command and ends its turn early, under budget — the cleanest and
// most common real-world shape of this stall, and one a turn-limit claim would
// misdescribe. It is deliberately hedged: detection is a heuristic, not a
// confirmed diagnosis, so the hint must never assert the cause with certainty.
const stallCorrectiveHintText = `**Note from Fabrik:** the previous attempt at this stage stopped without completing, and the retry after it used noticeably fewer turns without completing either — a pattern consistent with a stall, most often caused by backgrounding a long-running command (e.g. a dev server, build, or test run) and then waiting for a completion notification that never arrives in this headless environment. If that's what happened, run any long-running command in the foreground with an explicit timeout instead of backgrounding it. If something else caused the previous attempt to stop short, disregard this note and continue as planned.`

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

// formatMidflightSpawnReceiptNote returns a present-tense sibling of
// formatSpawnReceiptNote for the Review/Validate mid-flight spawn path
// (ADR-1419): unlike a Plan-stage declaration, these children already exist
// by the time this note is rendered — spawnChildren has already run and
// created, boarded, assigned, and linked them as blockers of this issue
// before finalizeStageOutcome prepends this note to the stage's output.
// spawned holds each child as "owner/repo#N" (spawnChildren's own format).
// Returns "" for an empty list so a stage output with no spawn is
// byte-identical to before this note existed.
func formatMidflightSpawnReceiptNote(spawned []string) string {
	n := len(spawned)
	if n == 0 {
		return ""
	}
	if n == 1 {
		return fmt.Sprintf("🏭 Spawned 1 sub-issue: %s. It has been registered, assigned, and linked as a blocker of this issue.\n\n---\n\n", spawned[0])
	}
	return fmt.Sprintf("🏭 Spawned %d sub-issues: %s. Each has been registered, assigned, and linked as a blocker of this issue.\n\n---\n\n", n, strings.Join(spawned, ", "))
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

// fabrikControlMarkerLines are the bare marker lines stripped by
// hasArtifactContent to decide whether text carries anything beyond Fabrik's
// own control vocabulary. Mirrors the marker set finalizeStageOutcome strips
// before posting (engine/item.go's postOutput computation) — kept in sync
// deliberately, since both are answering the same question ("is there
// anything here besides control markers?").
var fabrikControlMarkerLines = []string{
	"FABRIK_STAGE_COMPLETE",
	"FABRIK_BLOCKED_ON_INPUT",
	"FABRIK_NO_WORK_NEEDED",
	"FABRIK_SUMMARY_BEGIN",
	"FABRIK_SUMMARY_END",
	// FABRIK_ISSUE_UPDATE_BEGIN/END are structural delimiters, not content —
	// without stripping them too, an empty update block ("BEGIN\nEND" with no
	// body between) would survive as two bare lines and make
	// hasArtifactContent report content that isn't actually there.
	"FABRIK_ISSUE_UPDATE_BEGIN",
	"FABRIK_ISSUE_UPDATE_END",
}

// hasArtifactContent reports whether text carries anything beyond Fabrik's
// own bare control-marker lines (FABRIK_STAGE_COMPLETE and friends). Used by
// artifactMissingOnComplete (R3) and extractLastSubstantialAssistantTurn
// (R1/R2) to decide whether a given piece of text is "real content" or just
// control-plane chatter — see #1782.
func hasArtifactContent(text string) bool {
	for _, line := range fabrikControlMarkerLines {
		text = stripLine(text, line)
	}
	return strings.TrimSpace(text) != ""
}

// artifactMissingOnComplete reports whether text signals FABRIK_STAGE_COMPLETE
// but carries no harvestable artifact — the #1632/#1782 defect: a trailing
// tool call after the real content leaves the CLI's terminal result field
// (or, on a timeout/parse-failure path, the recovered assistant-turn text)
// holding nothing but the bare marker. Excludes the documented,
// legitimate artifact-free completion (FABRIK_NO_WORK_NEEDED co-occurring
// with FABRIK_STAGE_COMPLETE — R3's exclusion) so that path is never treated
// as this defect, and never triggers the assistant-turn scan below.
func artifactMissingOnComplete(text string) bool {
	return stageCompleteRE.MatchString(text) && !CheckNoWorkNeeded(text) && !hasArtifactContent(text)
}

// extractLastSubstantialAssistantTurn scans raw NDJSON output for the last
// assistant turn whose text survives hasArtifactContent's stripping
// non-empty — i.e. the last turn with real content, independent of which
// turn (if any) happens to carry the FABRIK_STAGE_COMPLETE marker itself.
// This is deliberate: in the reported shape, the marker-bearing turn
// contains only the marker, so anchoring the selection on "the turn with the
// marker" would still fail (R2, see ADR-1782). Returns "" if no assistant
// turn has any content beyond control markers.
func extractLastSubstantialAssistantTurn(rawOutput []byte) string {
	var last string
	forEachAssistantText(rawOutput, func(text string) {
		if hasArtifactContent(text) {
			last = text
		}
	})
	return last
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

// resumeFailureCountPath returns the sidecar file path that tracks the
// consecutive-resume-failure count for the session at sessFilePath. It is
// deliberately a plain sibling file (not itemstate, which is confirmed
// entirely in-memory — see #1414 Research) so the counter survives an engine
// restart, exactly as the spec requires. It is shared by both InvokeClaude
// and InvokeClaudeForComments (and merge_train.go's conflict-resolution
// path), since all three compute the identical sessFilePath stem — the
// counter is keyed to the session-pointer identity, not the invocation path.
func resumeFailureCountPath(sessFilePath string) string {
	return sessFilePath + ".resumefails"
}

// readResumeFailureCount reads the consecutive-resume-failure count for
// sessFilePath. A missing or unparseable sidecar is treated as 0 — the same
// "absence means zero/cold" convention resolveResumeSessionID already uses
// for the session file itself.
func readResumeFailureCount(sessFilePath string) int {
	data, err := os.ReadFile(resumeFailureCountPath(sessFilePath))
	if err != nil {
		return 0
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || count < 0 {
		return 0
	}
	return count
}

// writeResumeFailureCount persists count to the sidecar file for
// sessFilePath, mirroring saveSessionIDDirect's plain-text write idiom.
// Best-effort: a write failure is logged but does not fail the invocation.
func writeResumeFailureCount(issueNumber int, sessFilePath string, count int) {
	path := resumeFailureCountPath(sessFilePath)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		claudeLog(issueNumber, "warn", "failed to create session dir for %s: %v\n", path, err)
		return
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(count)), 0600); err != nil {
		claudeLog(issueNumber, "warn", "failed to save resume-failure count to %s: %v\n", path, err)
	}
}

// resetResumeFailureCount discards the sidecar file for sessFilePath,
// restoring the consecutive-failure count to its implicit zero. Best-effort:
// a missing file is not an error (os.Remove's own ENOENT is silently
// ignored, matching the existing stale-session os.Remove(sessFilePath) call
// this mirrors).
func resetResumeFailureCount(sessFilePath string) {
	os.Remove(resumeFailureCountPath(sessFilePath))
}
