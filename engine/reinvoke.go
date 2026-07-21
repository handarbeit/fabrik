package engine

import (
	"context"
	"errors"
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
}

// dispatchReinvoke is the shared goroutine scaffold for all three reinvoke
// dispatchers (dispatchCIFixReinvoke, dispatchReviewReinvoke,
// dispatchRebaseReinvoke). It performs: optional precheck -> WorkerEntered ->
// semaphore acquire -> ensureRepoReady (ErrSkipItem skips silently) ->
// opts.build -> optional opts.stageVariant -> LocalLockAcquired + heartbeat +
// onPIDReady -> processComments -> optional opts.after -> error logging
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

		e.logf(item.Number, opts.tag, "re-invoking stage %q via comment processing\n", stage.Name)
		err := e.processComments(ctx, board, item, reinvokeStage, comments, onPIDReady)

		if opts.after != nil {
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
