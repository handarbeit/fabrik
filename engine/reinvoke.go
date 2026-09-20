package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// reinvokeOpts parameterizes dispatchReinvoke's per-call-site divergences.
// The three reinvoke dispatchers (CI-fix, review, rebase) share an identical
// goroutine scaffold (WorkerEntered -> semaphore -> ensureRepoReady -> build
// synthetic comment(s) -> optional stage variant -> heartbeat/lock ->
// processComments -> optional post-processing); only these fields differ
// per call site.
type reinvokeOpts struct {
	// tag is the log-message prefix (e.g. "ci-fix-reinvoke", "review-reinvoke",
	// "rebase-reinvoke").
	tag string
	// precheck, if non-nil, runs synchronously before WorkerEntered/goroutine
	// dispatch; returning false skips the reinvoke entirely with no worker
	// bookkeeping. Used by review's pre-dispatch emptiness check, which has no
	// workDir dependency and must not incur ensureRepoReady/WorkerEntered churn
	// for a same-poll no-op.
	precheck func() bool
	// build constructs the synthetic comment(s) to feed into processComments.
	// Runs inside the goroutine, after ensureRepoReady, so it may use workDir.
	build func(workDir string) []gh.Comment
	// stageVariant, if non-nil, returns a modified stage to pass to
	// processComments (e.g. swapping in CIFixSkill/RebaseSkill over
	// CommentSkill). nil means use the stage unmodified.
	stageVariant func(*stages.Stage) *stages.Stage
	// after, if non-nil, runs after processComments completes, receiving the
	// worktree dir and the processComments error. Used for CI's no-op-SHA
	// recording and rebase's auto-merge re-enablement.
	after func(workDir string, err error)
	// cycle names the cycle counter(s) charged synchronously before this
	// dispatch, so dispatchReinvoke can refund them when the invocation
	// provably never ran (#1812). The zero value refunds nothing.
	cycle cycleCharge
}

// didNotRunKind classifies why a processComments call provably never executed a
// Claude invocation (#1812). "" means it ran, or that it is ambiguous — the
// reinvoke cycle stays charged in that case (R3/R4): under-refunding costs an
// unnecessary pause, over-refunding removes the only bound on a genuine
// non-convergence loop.
type didNotRunKind string

const (
	didNotRunSuspended    didNotRunKind = "account-suspended"
	didNotRunUsageLimit   didNotRunKind = "usage-limit"
	didNotRunAPIError     didNotRunKind = "api-error"
	didNotRunAPIKeyHelper didNotRunKind = "api-key-helper"
)

// classifyDidNotRun maps a processComments error to the did-not-run set the
// stage-dispatch path already exempts from max_retries: claudeUsageLimitError,
// claudeAPIErrorExit (whose classifier already refuses a run that consumed
// turns and incurred cost, R4) and apiKeyHelperDetectedError. Every other error
// — turn limit, resume failure, tools denied, a mid-run crash, an output
// publication failure — may have done real work and returns "" (R3).
// apiKeyHelperDetectedError is produced only by the stage-dispatch path today,
// so its arm here is forward-compatibility only.
func classifyDidNotRun(err error) didNotRunKind {
	if err == nil {
		return ""
	}
	var limitErr *claudeUsageLimitError
	if errors.As(err, &limitErr) {
		return didNotRunUsageLimit
	}
	var apiErr *claudeAPIErrorExit
	if errors.As(err, &apiErr) {
		return didNotRunAPIError
	}
	var keyHelperErr *apiKeyHelperDetectedError
	if errors.As(err, &keyHelperErr) {
		return didNotRunAPIKeyHelper
	}
	return ""
}

// cycleCharge describes the cycle counter(s) a reinvoke dispatch charged before
// starting, and the compensating mutations that undo the charge.
type cycleCharge struct {
	// label names the counter for the refund log line ("review", "rebase",
	// "ci-fix").
	label string
	// refund builds the compensating mutations for this item and stage. Each
	// is floored at zero by the store, so a double application is harmless.
	refund func(repo string, number int, stageName string) []itemstate.Mutation
}

// refundDidNotRunCycle compensates the pre-dispatch cycle charge for an
// invocation that never ran, records the never-refunded did-not-run tally the
// cycle-limit pause messages draw on (R7), and logs the refund (R5).
func (e *Engine) refundDidNotRunCycle(item gh.ProjectItem, itemRepo string, stage *stages.Stage, tag string, kind didNotRunKind, c cycleCharge) {
	if c.refund != nil {
		for _, m := range c.refund(itemRepo, item.Number, stage.Name) {
			e.store.Apply(m)
		}
	}
	e.store.Apply(itemstate.DidNotRunReinvokeRecorded{Repo: itemRepo, Number: item.Number, StageName: stage.Name})
	e.logf(item.Number, tag, "invocation did not run (%s) — refunding %s cycle counter for stage %q (#1812)\n", kind, c.label, stage.Name)
}

// commentIDsForLog joins the IDs of a dispatched comment batch for inclusion
// in dispatchReinvoke's log line, so a log scraper (e.g. the e2e suite) can
// distinguish which review/comment a given reinvoke was dispatched for,
// rather than relying on a count- or timing-based proxy. "|" is used as the
// separator: GraphQL node IDs are base64url (comma-safe already, but "|"
// avoids any ambiguity), and review-body synthetic IDs (reviewBodyCommentID,
// "review-body:<DatabaseID>") never contain it either. Returns "" for an
// empty batch.
func commentIDsForLog(comments []gh.Comment) string {
	ids := make([]string, len(comments))
	for i, c := range comments {
		ids[i] = c.ID
	}
	return strings.Join(ids, "|")
}

// dispatchReinvoke is the shared goroutine scaffold for all three reinvoke
// dispatchers (dispatchCIFixReinvoke, dispatchReviewReinvoke,
// dispatchRebaseReinvoke). It performs: optional precheck -> WorkerEntered ->
// semaphore acquire -> ensureRepoReady (ErrSkipItem skips silently) ->
// opts.build -> optional opts.stageVariant -> LocalLockAcquired + heartbeat +
// onPIDReady -> processCommentsClassified -> (did-not-run refund | optional opts.after) -> error logging
// (ctx.Err() short-circuit) -> deferred WorkerExited.
func (e *Engine) dispatchReinvoke(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, opts reinvokeOpts) {
	if opts.precheck != nil && !opts.precheck() {
		e.logf(item.Number, opts.tag, "precheck failed; skipping re-invocation\n")
		return
	}

	itemRepo := itemOwnerRepoString(item, e.defaultRepo())

	// Mark in-flight via the Store so the dispatch guard (snap.Worker() != nil) blocks
	// double-dispatch before the goroutine starts. WorkerExited is deferred inside the
	// goroutine so any early exit also clears it.
	e.store.Apply(itemstate.WorkerEntered{
		Repo:      itemRepo,
		Number:    item.Number,
		StageName: stage.Name,
		StartedAt: time.Now(),
	})
	e.wg.Add(1)

	go func() {
		defer e.wg.Done()
		defer e.store.Apply(itemstate.WorkerExited{Repo: itemRepo, Number: item.Number})

		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			e.logf(item.Number, opts.tag, "context cancelled before semaphore acquired\n")
			return
		}
		defer func() { <-e.sem }()

		if err := e.ensureRepoReady(ctx, item); err != nil {
			if errors.Is(err, ErrSkipItem) {
				e.logf(item.Number, opts.tag, "repo not ready, skipping reinvoke\n")
				return
			}
			e.logf(item.Number, "warn", "%s: ensureRepoReady failed: %v\n", opts.tag, err)
			return
		}

		wm := e.worktreesFor(item.Repo)
		workDir := wm.WorktreeDir(item.Number)

		comments := opts.build(workDir)

		reinvokeStage := stage
		if opts.stageVariant != nil {
			reinvokeStage = opts.stageVariant(stage)
		}

		// Register WorkerHandle so the heartbeat/liveness system tracks this goroutine.
		now := time.Now()
		e.store.Apply(itemstate.LocalLockAcquired{
			Repo:       itemRepo,
			Number:     item.Number,
			User:       e.cfg.User,
			AcquiredAt: now,
			Worker:     &itemstate.WorkerHandle{StageName: stage.Name, StartedAt: now, LastSignAt: now},
		})
		done := make(chan struct{})
		defer close(done)
		e.startHeartbeat(ctx, itemRepo, item.Number, done)
		onPIDReady := func(pid int) {
			e.store.Apply(itemstate.WorkerPIDSet{Repo: itemRepo, Number: item.Number, PID: pid})
		}

		e.logf(item.Number, opts.tag, "re-invoking stage %q via comment processing (comments: %s)\n", stage.Name, commentIDsForLog(comments))
		kind, err := e.processCommentsClassified(ctx, board, item, reinvokeStage, comments, onPIDReady)

		// An invocation that provably never ran was not an attempt to
		// converge: refund the pre-dispatch cycle charge and skip opts.after,
		// whose HEAD-based logic (CI no-op-SHA debounce, rebase auto-merge
		// re-enable, #1045 no-op refund) is meaningless when nothing ran.
		if kind != "" {
			e.refundDidNotRunCycle(item, itemRepo, stage, opts.tag, kind, opts.cycle)
		} else if opts.after != nil {
			opts.after(workDir, err)
		}

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.logf(item.Number, "warn", "%s re-invocation failed: %v\n", opts.tag, err)
		}
	}()
}

// didNotRunPauseNote builds the cycle-limit pause message's did-not-run
// evidence (#1812, R7). Refunded did-not-run cycles no longer show in the cycle
// counters, so the never-refunded DidNotRunReinvokes tally is the only signal
// that the outage — not a non-converging reviewer or CI — is what an operator
// is looking at. Returns dominant when the tally is at least cycleCount (the
// invocations that reached the limit were largely accompanied by ones that never
// ran), in which case callers replace their "reviewer keeps requesting changes"
// style diagnosis with note; a smaller nonzero tally yields an addendum only;
// a zero tally yields "" so the message is byte-identical to before.
func (e *Engine) didNotRunPauseNote(repoStr string, number int, stageName string, cycleCount int) (note string, dominant bool) {
	snap, err := e.store.Get(repoStr, number)
	if err != nil {
		return "", false
	}
	n := snap.DidNotRunReinvokes(stageName)
	switch {
	case n <= 0:
		return "", false
	case n >= cycleCount:
		return fmt.Sprintf("**%d re-invocation(s) never ran** — Claude exited before doing any work "+
			"(an API error such as a 429 session limit, or a usage limit), so this is most likely a Claude "+
			"availability problem rather than a reviewer or CI that keeps failing. Real cycles take minutes each; "+
			"a burst of them within seconds means the invocations never ran. Check the `is_error`/`api_error` "+
			"result lines in `.fabrik/logs/<repo>/issue-%d/`.", n, number), true
	default:
		return fmt.Sprintf("Note: %d re-invocation(s) for this stage never ran (Claude API error or usage limit) "+
			"and were not counted toward this limit.", n), false
	}
}
