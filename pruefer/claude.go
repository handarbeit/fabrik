package pruefer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// reviewAllowedTools is the fixed, read-only tool allowlist for every review
// invocation. Unlike a Fabrik stage's configurable allowed_tools, this list
// is hardcoded and non-configurable: the reviewer must never mutate the
// working tree or the repo, and Claude never submits the review itself —
// Pruefer's Go code calls github.Client.SubmitPRReview directly once Claude
// has produced review text. Bash(gh:*) is deliberately excluded even though
// the issue's own illustrative example includes it: gh pr review --approve
// / gh pr merge are reachable through an unscoped grant, which would make
// "never approve" a prompt-level policy rather than a code-level guarantee
// (see adrs/1113-pruefer-v1-architecture.md).
var reviewAllowedTools = []string{
	"Read", "Grep", "Glob",
	"Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)",
	"Bash(git blame:*)", "Bash(git grep:*)", "Bash(git status:*)",
}

// reviewInactivityTimeout mirrors engine/claude.go's hardcoded 15-minute
// inactivity watchdog: if claude produces no stdout for this long, the
// process is presumed stuck and killed so it doesn't hold a concurrency
// slot forever. Var (not const) so tests can shrink it.
var reviewInactivityTimeout = 15 * time.Minute

// reviewWaitDelay mirrors engine/claude.go's claudeWaitDelay: how long to
// wait for stdout pipe drain after the process exits before giving up on
// recovering output held open by a grandchild process.
var reviewWaitDelay = 30 * time.Second

// reviewKillGrace is the SIGINT/SIGTERM grace window used by Review's kill
// escalation. Fixed (unlike Fabrik's per-stage configurable kill_grace)
// since Pruefer has no stage YAML to source an override from.
var reviewKillGrace = 10 * time.Second

// ReviewRequest bundles the inputs for a single Claude review invocation.
// The PR's head commit is expected to already be checked out at WorkDir
// (see worktree.go) — Claude inspects it directly rather than receiving the
// diff purely as prompt text, so it can use git/Read/Grep/Glob for
// surrounding-code context.
type ReviewRequest struct {
	Owner       string
	Repo        string
	PRNumber    int
	Title       string
	Body        string
	HeadSHA     string
	BaseBranch  string
	Model       string
	Effort      string
	WorkDir     string        // ephemeral clone directory; claude's cwd
	MaxWallTime time.Duration // 0 = no wall-time cap
}

// ClaudeInvoker defines the interface for invoking Claude Code to produce
// review text for a single PR. Deliberately narrower than
// engine.ClaudeInvoker — no stages.Stage, no gh.ProjectItem — since Pruefer
// has no board/stage concept.
type ClaudeInvoker interface {
	Review(ctx context.Context, req ReviewRequest) (ReviewResult, error)
}

// ReviewResult carries a completed review invocation's output text plus the
// turn/cost instrumentation the TUI's cost-visibility requirement depends
// on, mirroring engine/claude.go's TokenUsage capture (NumTurns/CostUSD
// parsed from claude's stream-json "result" envelope).
type ReviewResult struct {
	Text     string
	NumTurns int
	CostUSD  float64
}

// RealClaudeInvoker invokes the real claude CLI via exec.CommandContext,
// mirroring engine/claude.go's runClaude: non-interactive permission mode,
// a fixed read-only tool allowlist, an inactivity watchdog, an optional
// wall-time deadline, WaitDelay for buffered-stdout recovery, and graceful
// SIGINT→SIGTERM→SIGKILL process-group kill on cancellation.
type RealClaudeInvoker struct{}

// buildReviewPrompt constructs the prompt sent to claude via stdin.
func buildReviewPrompt(req ReviewRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are Pruefer, an automated code reviewer for pull request %s/%s#%d: %q.\n\n", req.Owner, req.Repo, req.PRNumber, req.Title)
	b.WriteString("The PR's head commit is already checked out in your working directory. Use git (diff, log, show, blame, grep, status), Read, Grep, and Glob to inspect the change and any surrounding code you need for context — you have no write access and no other tools.\n\n")
	if req.BaseBranch != "" {
		fmt.Fprintf(&b, "The PR's base branch is %q; compare against it (e.g. `git diff %s...HEAD`) to see only this PR's changes.\n\n", req.BaseBranch, req.BaseBranch)
	}
	if req.Body != "" {
		b.WriteString("## PR description\n\n")
		b.WriteString(req.Body)
		b.WriteString("\n\n")
	}
	b.WriteString("Write a code review as you would comment on the pull request: call out bugs, correctness issues, security concerns, and significant design problems. Skip nitpicks and style preferences unless they matter.\n\n")
	b.WriteString("You are NOT approving or requesting changes — this is a comment-only review. Do not use approval/rejection language such as \"LGTM\" or \"requesting changes\".\n\n")
	b.WriteString("Output ONLY the review text itself: no preamble, no meta-commentary about what you are about to do.\n")
	return b.String()
}

// buildReviewArgs constructs the claude CLI argument list for a review
// invocation. Always non-interactive (--permission-mode dontAsk) and always
// the fixed read-only allowlist — there is no unrestricted/label-driven
// escape hatch, unlike Fabrik stages.
func buildReviewArgs(req ReviewRequest) []string {
	args := []string{
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "dontAsk",
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	for _, tool := range reviewAllowedTools {
		args = append(args, "--allowedTools", tool)
	}
	return args
}

// buildReviewEnv returns the environment variable overrides applied on top
// of os.Environ() for a review invocation.
func buildReviewEnv(req ReviewRequest) []string {
	effort := req.Effort
	if effort == "" {
		effort = DefaultEffort
	}
	return []string{
		"CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1",
		"CLAUDE_CODE_EFFORT_LEVEL=" + effort,
	}
}

// mergeEnv builds a subprocess environment from base (typically
// os.Environ()), with overrides applied on top. Keys present in overrides
// are removed from base first so overrides take effect even when base
// already contains the same key — mirrors engine/claude.go's mergeEnv.
func mergeEnv(base, overrides []string) []string {
	if len(overrides) == 0 {
		return base
	}
	keys := make(map[string]bool, len(overrides))
	for _, kv := range overrides {
		if i := strings.IndexByte(kv, '='); i > 0 {
			keys[kv[:i]] = true
		}
	}
	result := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 && keys[kv[:i]] {
			continue
		}
		result = append(result, kv)
	}
	return append(result, overrides...)
}

// activityWriter wraps an io.Writer and updates a shared atomic timestamp on
// every Write call. Used by the inactivity watchdog — mirrors
// engine/claude.go's activityWriter.
type activityWriter struct {
	inner        io.Writer
	lastActivity *atomic.Int64
}

func (w *activityWriter) Write(p []byte) (int, error) {
	w.lastActivity.Store(time.Now().UnixNano())
	return w.inner.Write(p)
}

// claudeReviewResponse mirrors the subset of claude --output-format
// stream-json's final result object that Pruefer needs. NumTurns/CostUSD
// mirror engine/claude.go's claudeResponse fields of the same JSON keys.
type claudeReviewResponse struct {
	Result   string  `json:"result"`
	IsError  bool    `json:"is_error"`
	NumTurns int     `json:"num_turns"`
	CostUSD  float64 `json:"total_cost_usd"`
}

// parseClaudeReviewJSON parses claude's NDJSON stream output, handling both
// a single result object and a conversation array (only the last
// {"type":"result",...} line matters in the array case).
func parseClaudeReviewJSON(output []byte) (claudeReviewResponse, bool) {
	var resp claudeReviewResponse
	if err := json.Unmarshal(output, &resp); err == nil && resp.Result != "" {
		return resp, true
	}
	var lastResult *claudeReviewResponse
	for _, line := range bytes.Split(output, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var envelope struct {
			Type     string  `json:"type"`
			Result   string  `json:"result"`
			IsError  bool    `json:"is_error"`
			NumTurns int     `json:"num_turns"`
			CostUSD  float64 `json:"total_cost_usd"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil || envelope.Type != "result" {
			continue
		}
		r := claudeReviewResponse{Result: envelope.Result, IsError: envelope.IsError, NumTurns: envelope.NumTurns, CostUSD: envelope.CostUSD}
		lastResult = &r
	}
	if lastResult != nil {
		return *lastResult, true
	}
	return claudeReviewResponse{}, false
}

// Review invokes the claude CLI once, in req.WorkDir, and returns its review
// text plus turn/cost instrumentation. On any failure — process error,
// timeout, unparseable output, or an is_error response — it returns a
// non-nil error and a zero-value ReviewResult. Per the issue's "on
// invocation failure, post nothing" requirement, callers must not submit a
// review when err != nil.
func (r *RealClaudeInvoker) Review(ctx context.Context, req ReviewRequest) (ReviewResult, error) {
	logf(req.PRNumber, "claude", "reviewing %s/%s#%d (head %s) in %s\n", req.Owner, req.Repo, req.PRNumber, req.HeadSHA, req.WorkDir)

	stageCtx := ctx
	cancel := context.CancelFunc(func() {})
	if req.MaxWallTime > 0 {
		stageCtx, cancel = context.WithTimeout(ctx, req.MaxWallTime)
	}
	defer cancel()

	var stdout bytes.Buffer
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var stdoutWriter io.Writer = &activityWriter{inner: &stdout, lastActivity: &lastActivity}

	cmd := exec.CommandContext(stageCtx, "claude", buildReviewArgs(req)...)
	cmd.Dir = req.WorkDir
	cmd.Env = mergeEnv(os.Environ(), buildReviewEnv(req))
	cmd.Stdin = strings.NewReader(buildReviewPrompt(req))
	cmd.Stderr = os.Stderr
	cmd.Stdout = stdoutWriter
	waitDelay := reviewWaitDelay
	if waitDelay <= 0 {
		waitDelay = 30 * time.Second
	}
	cmd.WaitDelay = waitDelay
	setCmdProcAttr(cmd)

	watchdogCtx, watchdogCancel := context.WithCancel(context.Background())
	defer watchdogCancel()

	cmd.Cancel = func() error {
		if cmd.Process != nil {
			reason := "context_cancel"
			if req.MaxWallTime > 0 && stageCtx.Err() == context.DeadlineExceeded {
				reason = "max_wall_time"
			}
			killProcGroupGraceful(cmd.Process.Pid, req.PRNumber, "review", reason, reviewKillGrace, reviewKillGrace)
		}
		return nil
	}

	if err := cmd.Start(); err != nil {
		watchdogCancel()
		return ReviewResult{}, fmt.Errorf("starting claude: %w", err)
	}
	pid := cmd.Process.Pid

	go func(pid int) {
		timer := time.NewTimer(reviewInactivityTimeout)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				since := time.Since(time.Unix(0, lastActivity.Load()))
				if since >= reviewInactivityTimeout {
					logf(req.PRNumber, "warn", "review invocation idle for %s with no output — killing\n", reviewInactivityTimeout)
					killProcGroupGraceful(pid, req.PRNumber, "review", "inactivity_timeout", reviewKillGrace, reviewKillGrace)
					return
				}
				timer.Reset(reviewInactivityTimeout - since)
			case <-watchdogCtx.Done():
				return
			}
		}
	}(pid)

	runErr := cmd.Wait()
	watchdogCancel()
	killProcGroup(cmd, req.PRNumber, "review")

	if errors.Is(runErr, exec.ErrWaitDelay) && ctx.Err() == nil {
		logf(req.PRNumber, "warn", "WaitDelay fired: claude exited but grandchild processes held stdout pipe open; processing buffered output (%d bytes)\n", stdout.Len())
		runErr = nil
	}

	if runErr != nil {
		return ReviewResult{}, fmt.Errorf("claude exited with error: %w", runErr)
	}

	resp, ok := parseClaudeReviewJSON(bytes.TrimSpace(stdout.Bytes()))
	if !ok {
		return ReviewResult{}, fmt.Errorf("could not parse claude output (%d bytes)", stdout.Len())
	}
	if resp.IsError {
		return ReviewResult{}, fmt.Errorf("claude reported an error: %s", resp.Result)
	}
	if strings.TrimSpace(resp.Result) == "" {
		return ReviewResult{}, fmt.Errorf("claude produced an empty review")
	}
	return ReviewResult{Text: resp.Result, NumTurns: resp.NumTurns, CostUSD: resp.CostUSD}, nil
}
