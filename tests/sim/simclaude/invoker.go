// Package simclaude implements a scripted engine.ClaudeInvoker for
// tests/sim's harness (#1449).
//
// It exists because substituting engine.GitHubClient (tests/sim/simgh) is
// only half of what makes an Engine.Run() cheap to drive: the other live
// dependency is the Claude worker itself, and a Claude worker does not
// merely return a string. It runs inside a real worktree, writes files,
// commits, and returns marker-bearing output the engine parses
// (FABRIK_STAGE_COMPLETE, FABRIK_BLOCKED_ON_INPUT, and friends). A scripted
// invoker that returned marker text without touching the worktree would
// leave the entire commit/push/PR-diff half of the pipeline unexercised and
// would silently pass scenarios that cannot work in reality — see AC10's
// non-vacuity requirement and the guard test in invoker_test.go.
//
// Invoker implements engine.ClaudeInvoker by dispatching each call to a
// Script (or CommentScript for InvokeForComments), selected by stage name
// and invocation count (R1). A scenario registers only the scripts it cares
// about via ForStage/ForStageComments; every other invocation runs
// DefaultScript/DefaultCommentScript, which performs the minimum plausible
// stage work: writes a per-stage marker file inside the real workDir it is
// handed, commits it for real, and signals completion.
package simclaude

import (
	"context"
	"sync"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Script implements one scripted Claude invocation for a stage, matching
// engine.ClaudeInvoker.Invoke's exact signature so a Script can be handed
// directly to Invoker without adaptation.
type Script func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (output string, completed bool, usage engine.TokenUsage, err error)

// CommentScript implements one scripted Claude comment-review invocation,
// matching engine.ClaudeInvoker.InvokeForComments's exact signature.
type CommentScript func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (output string, completed bool, usage engine.TokenUsage, err error)

// Invoker is a scripted engine.ClaudeInvoker. The zero value is not usable;
// construct one with New.
type Invoker struct {
	mu             sync.Mutex
	stageScripts   map[string][]Script
	commentScripts map[string][]CommentScript
	stageCalls     map[string]int
	commentCalls   map[string]int
}

var _ engine.ClaudeInvoker = (*Invoker)(nil)

// New constructs an Invoker with no registered scripts — every invocation
// runs the default script until ForStage/ForStageComments registers
// something more specific.
func New() *Invoker {
	return &Invoker{
		stageScripts:   make(map[string][]Script),
		commentScripts: make(map[string][]CommentScript),
		stageCalls:     make(map[string]int),
		commentCalls:   make(map[string]int),
	}
}

// ForStage registers the sequence of Scripts to run for stageName's
// successive Invoke calls, in order: the 1st Invoke call for this stage
// (across every issue — the engine dispatches one stage per issue at a time,
// so this is equivalent to "per issue" for a single-issue scenario) uses
// scripts[0], the 2nd uses scripts[1], and so on. Once the list is
// exhausted, the last script repeats for every further call — so a scenario
// that only cares about the first attempt can register one script and let
// retries fall through to it again, or register a final "success" script to
// end a retry sequence. Returns inv for chaining.
func (inv *Invoker) ForStage(stageName string, scripts ...Script) *Invoker {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.stageScripts[stageName] = append([]Script(nil), scripts...)
	return inv
}

// ForStageComments registers the sequence of CommentScripts to run for
// stageName's successive InvokeForComments calls. Same selection rule as
// ForStage.
func (inv *Invoker) ForStageComments(stageName string, scripts ...CommentScript) *Invoker {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.commentScripts[stageName] = append([]CommentScript(nil), scripts...)
	return inv
}

// Invoke implements engine.ClaudeInvoker.
func (inv *Invoker) Invoke(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
	script := inv.pickStageScript(stage.Name)
	if script == nil {
		script = DefaultScript
	}
	return script(ctx, stage, issue, newComments, resume, workDir, opts)
}

// InvokeForComments implements engine.ClaudeInvoker.
func (inv *Invoker) InvokeForComments(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
	script := inv.pickCommentScript(stage.Name)
	if script == nil {
		script = DefaultCommentScript
	}
	return script(ctx, stage, issue, comments, workDir, opts)
}

func (inv *Invoker) pickStageScript(stageName string) Script {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.stageCalls[stageName]++
	n := inv.stageCalls[stageName]
	scripts := inv.stageScripts[stageName]
	if len(scripts) == 0 {
		return nil
	}
	idx := n - 1
	if idx >= len(scripts) {
		idx = len(scripts) - 1
	}
	return scripts[idx]
}

func (inv *Invoker) pickCommentScript(stageName string) CommentScript {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.commentCalls[stageName]++
	n := inv.commentCalls[stageName]
	scripts := inv.commentScripts[stageName]
	if len(scripts) == 0 {
		return nil
	}
	idx := n - 1
	if idx >= len(scripts) {
		idx = len(scripts) - 1
	}
	return scripts[idx]
}

// StageCallCount returns how many times Invoke has run for stageName so far
// — useful for a scenario asserting retry counts without threading its own
// counter.
func (inv *Invoker) StageCallCount(stageName string) int {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.stageCalls[stageName]
}

// CommentCallCount returns how many times InvokeForComments has run for
// stageName so far.
func (inv *Invoker) CommentCallCount(stageName string) int {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.commentCalls[stageName]
}

// DefaultScript is used for any Invoke call with no registered ForStage
// script (R1's "a scenario only scripts what it cares about"). It performs
// the minimum plausible stage work: writes a per-stage marker file inside
// the real workDir, commits it for real via the git binary, and signals
// completion. This is deliberately the shape AC10's non-vacuity guard
// targets — see TestDefaultScript_CommitIsReal_NotJustMarkerText in
// invoker_test.go.
func DefaultScript(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
	if err := commitStageMarker(workDir, stage.Name, issue.Number); err != nil {
		return "", false, engine.TokenUsage{}, err
	}
	return "FABRIK_STAGE_COMPLETE\n", true, defaultUsage(), nil
}

// DefaultCommentScript is the InvokeForComments counterpart of DefaultScript.
func DefaultCommentScript(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
	if err := commitStageMarker(workDir, stage.Name+"-comment", issue.Number); err != nil {
		return "", false, engine.TokenUsage{}, err
	}
	return "FABRIK_STAGE_COMPLETE\n", true, defaultUsage(), nil
}

// defaultUsage is a plausible, non-zero TokenUsage for a scripted invocation
// that actually "ran" — distinguishing it at a glance in logs/assertions
// from the deliberately-zero usage the did-not-run failure shapes return
// (scripts.go).
func defaultUsage() engine.TokenUsage {
	return engine.TokenUsage{
		InputTokens:  1000,
		OutputTokens: 200,
		CostUSD:      0.05,
		TurnsUsed:    3,
		MaxTurns:     50,
	}
}
