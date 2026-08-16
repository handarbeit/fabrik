package simclaude

import (
	"context"
	"fmt"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/claudeerr"
	"github.com/handarbeit/fabrik/stages"
)

// The constructors below each return a Script mirroring one of
// RealClaudeInvoker's documented failure-exit contracts (engine/claude.go),
// reachable from an external test package only because internal/claudeerr
// (#1449 Scope item 2) exports the four types the engine classifies via
// errors.As. Getting the (output, completed, usage, err) combination wrong
// for any of these would silently test the wrong engine branch — see
// invoker_test.go, which pins each shape against production's documented
// contract before any scenario test builds on top of it.

// TurnLimitExhausted returns a Script mirroring a turn-cap exit
// (engine/claude.go:1414): partial output with no completion marker,
// completed=false, non-zero usage (the run consumed real turns and cost),
// and a *claudeerr.TurnLimitError. finalizeStageOutcome's turnLimited branch
// requires exactly this shape to route the invocation through the
// "incomplete but resumable" path — StageAttempted, commitWIP, push,
// StageRetryIncremented, MaxRetries all still apply, unlike the
// did-not-run family below.
func TurnLimitExhausted(numTurns int) Script {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		output := fmt.Sprintf("working on %s...\n(turn limit reached before completion)\n", stage.Name)
		usage := engine.TokenUsage{TurnsUsed: numTurns, MaxTurns: numTurns, CostUSD: 1.23}
		return output, false, usage, &claudeerr.TurnLimitError{TerminalReason: "error_max_turns", NumTurns: numTurns}
	}
}

// UsageLimitExit returns a Script mirroring the CLI's structural
// account-usage-limit exit (engine/claude.go:1424): empty output,
// completed=false, zero usage (classifyUsageLimitExit's own structural
// signal is that the invocation is terminated immediately — 0 turns, $0
// cost), and a *claudeerr.UsageLimitError. handleUsageLimitExit requires
// exactly this shape to record StageAttempted without
// StageRetryIncremented and apply fabrik:claude-limit — never
// stage:<name>:failed, never fabrik:paused.
func UsageLimitExit() Script {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		usage := engine.TokenUsage{TurnsUsed: 0, CostUSD: 0}
		return "", false, usage, &claudeerr.UsageLimitError{Message: `terminal_reason="blocking_limit"`}
	}
}

// APIErrorExit returns a Script mirroring a transient Anthropic-side
// api_error exit (engine/claude.go:1427, #1458): empty output,
// completed=false, minimal usage, and a *claudeerr.APIErrorExit.
// handleAPIErrorExit requires exactly this shape to record StageAttempted
// without StageRetryIncremented — a deliberately more minimal response than
// UsageLimitExit's (log only, no label, no comment; see #1458 R4).
func APIErrorExit() Script {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		usage := engine.TokenUsage{TurnsUsed: 1, CostUSD: 0}
		return "", false, usage, &claudeerr.APIErrorExit{TerminalReason: "api_error", NumTurns: 1, CostUSD: 0}
	}
}

// PartialOutputThenKilled returns a Script mirroring the case where Claude's
// process exited non-zero but had already written FABRIK_STAGE_COMPLETE to
// its output beforehand (engine/claude.go:1392-1398's
// stageCompleteRE.MatchString(text) branch): completed=true, and a plain
// wrapped error — deliberately NOT one of the claudeerr types, since
// production returns a generic `fmt.Errorf("claude exited with error: %w",
// runErr)` here, not a classified type. finalizeStageOutcome treats this as
// a genuinely completed stage despite the trailing error: the commit this
// script makes is real, exactly like DefaultScript's, so the stage's actual
// work is not fictional even though the process technically failed.
func PartialOutputThenKilled() Script {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		if err := commitStageMarker(workDir, stage.Name, issue.Number); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		output := "doing final cleanup...\nFABRIK_STAGE_COMPLETE\n(extra output emitted after the marker, then a non-zero exit)\n"
		usage := engine.TokenUsage{TurnsUsed: 5, MaxTurns: 50, CostUSD: 0.08}
		return output, true, usage, fmt.Errorf("claude exited with error: exit status 1")
	}
}

// CommentReviewCompleted returns a CommentScript exercising the ordinary
// successful comment-review path (InvokeForComments): commits a real change
// responding to the comment and signals completion. Named distinctly from
// DefaultCommentScript so a scenario asserting AC5's "a comment-review
// invocation" case reads as an explicit choice, not an accidental fallback.
func CommentReviewCompleted() CommentScript {
	return DefaultCommentScript
}

// NoOpCommentReview returns a CommentScript that touches nothing in workDir
// and signals no progress: no commit (so headChanged stays false), no
// FABRIK_ISSUE_UPDATE_BEGIN/END body update, and completed=false — the
// exact shape processComments's own progress check (engine/comments.go:
// `progressed := headChanged || extractUpdatedBody(output) != "" ||
// completed`) treats as a no-op cycle, which is what both
// checkNoOpCommentCycle (engine/comment_noop_breaker.go, #1592 gap 4b) and
// dispatchReviewReinvoke's own no-commit refund path (engine/reviews.go,
// #1592 gap 4a) key off. Named distinctly from DefaultCommentScript/
// CommentReviewCompleted (both of which commit), mirroring this file's
// existing naming convention.
func NoOpCommentReview() CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		output := fmt.Sprintf("Reviewed the latest feedback on %s — nothing actionable, no changes made.\n", stage.Name)
		usage := engine.TokenUsage{TurnsUsed: 2, MaxTurns: 50, CostUSD: 0.03}
		return output, false, usage, nil
	}
}
