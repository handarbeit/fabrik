package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// reinvokeOpts parameterizes dispatchReinvoke's per-call-site divergences.
// The three reinvoke dispatchers (CI-fix, review, rebase) share an identical
// goroutine scaffold (queued WorkerEntered -> semaphore -> WorkerSlotAcquired -> ensureRepoReady -> build
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
	// kind is the worker kind recorded on the Worker handle (itemstate.WorkerKind*,
	// #2121): ci-fix, review or rebase reinvoke.
	kind string
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
	didNotRunAccountUsageLimited didNotRunKind = "account-usage-limited"
	didNotRunUsageLimit          didNotRunKind = "usage-limit"
	didNotRunAPIError            didNotRunKind = "api-error"
	didNotRunAPIKeyHelper        didNotRunKind = "api-key-helper"
	didNotRunPostMerge           didNotRunKind = "post-merge"
	// didNotRunSkillUnresolved: the stage's skill reference resolved to no
	// loaded plugin (ADR 2137), so no worker was spawned.
	didNotRunSkillUnresolved didNotRunKind = "skill-unresolved"
	// didNotRunSkillLoadFailed: the stage's skill failed to load in the worker
	// (#2138, ADR-2138), so the run did none of the requested work.
	didNotRunSkillLoadFailed didNotRunKind = "skill-load-failed"
)

// commentRunBlockedOnInput is the sentinel kind processCommentsClassified returns
// when a comment-path run emitted FABRIK_BLOCKED_ON_INPUT and paused the item
// (#2280, ADR 2280). It is deliberately not a did-not-run: the invocation ran, so
// it must not feed the never-ran tally. dispatchReinvoke gives it its own branch:
// the pre-charged cycle is refunded (a question is not a failed convergence
// attempt) and opts.after is skipped.
const commentRunBlockedOnInput didNotRunKind = "blocked-on-input"

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
	var skillErr *skillUnresolvedError
	if errors.As(err, &skillErr) {
		return didNotRunSkillUnresolved
	}
	var skillLoadErr *claudeSkillLoadError
	if errors.As(err, &skillLoadErr) {
		return didNotRunSkillLoadFailed
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

// commentIDsTrigger joins the ids of a synthetic comment batch for the Worker
// trigger (#2121): "," separated, "" for an empty batch.
func commentIDsTrigger(comments []gh.Comment) string {
	ids := make([]string, 0, len(comments))
	for _, c := range comments {
		if c.ID != "" {
			ids = append(ids, c.ID)
		}
	}
	return strings.Join(ids, ",")
}

// dispatchReinvoke is the shared goroutine scaffold for all three reinvoke
// dispatchers (dispatchCIFixReinvoke, dispatchReviewReinvoke,
// dispatchRebaseReinvoke). It performs: optional precheck -> queued WorkerEntered ->
// semaphore acquire -> WorkerSlotAcquired -> ensureRepoReady (ErrSkipItem skips
// silently) -> opts.build -> WorkerTriggerSet -> optional opts.stageVariant -> heartbeat +
// onPIDReady -> processCommentsClassified -> (did-not-run refund | optional opts.after) -> error logging
// (ctx.Err() short-circuit) -> deferred WorkerExited.
func (e *Engine) dispatchReinvoke(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, opts reinvokeOpts) {
	// #2259: a SIGHUP drain starts nothing new; the next process re-dispatches.
	if e.isDraining() {
		return
	}
	if opts.precheck != nil && !opts.precheck() {
		e.logf(item.Number, opts.tag, "precheck failed; skipping re-invocation\n")
		return
	}

	itemRepo := itemOwnerRepoString(item, e.defaultRepo())

	// Claim the item via the Store so the dispatch guard (snap.Worker() != nil) blocks
	// double-dispatch before the goroutine starts — as a QUEUED worker: it holds no
	// worker slot until the goroutine acquires e.sem, and until then status and
	// health must not report it as running (#2121, F16). WorkerExited is deferred
	// inside the goroutine so any early exit also clears it.
	queuedAt := e.now()
	e.store.Apply(itemstate.WorkerEntered{
		Repo:        itemRepo,
		Number:      item.Number,
		StageName:   stage.Name,
		StartedAt:   queuedAt,
		Kind:        opts.kind,
		Queued:      true,
		QueuedSince: queuedAt,
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
		case <-e.drainGateCh():
			// #2259: queued but not started — abandon; the new process re-dispatches.
			e.logf(item.Number, opts.tag, "SIGHUP drain: abandoning queued re-invocation\n")
			return
		}
		defer func() { <-e.sem }()
		if e.isDraining() {
			// The drain began between the slot being taken and this check.
			e.logf(item.Number, opts.tag, "SIGHUP drain: abandoning queued re-invocation\n")
			return
		}
		// Runs first on exit, ahead of the WorkerExited wake (FR-001b, #2232).
		defer e.postRunFeedbackAdmission(ctx, item, e.now())

		// The slot is held: the worker is running from here, and the PID<=0
		// stale-worker detector measures from now rather than from dispatch. A
		// reinvoke deliberately claims no store Lock: Lock mirrors the
		// fabrik:locked label, which a reinvoke never applies (its cross-instance
		// marker is fabrik:editing), so there is nothing to release (#2179, ADR 2179).
		e.store.Apply(itemstate.WorkerSlotAcquired{Repo: itemRepo, Number: item.Number, At: e.now()})

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
		if trigger := commentIDsTrigger(comments); trigger != "" {
			e.store.Apply(itemstate.WorkerTriggerSet{Repo: itemRepo, Number: item.Number, Trigger: trigger})
		}

		reinvokeStage := stage
		if opts.stageVariant != nil {
			reinvokeStage = opts.stageVariant(stage)
		}

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
		if kind == commentRunBlockedOnInput {
			// The run executed and paused the item for a human answer (#2280).
			// Refund the cycle so the bound is free once the human replies, record
			// no did-not-run state, and skip opts.after: its HEAD-based hooks (CI
			// no-op-SHA debounce, rebase auto-merge re-enable, review refund) are
			// wrong on a just-paused item.
			if opts.cycle.refund != nil {
				for _, m := range opts.cycle.refund(itemRepo, item.Number, stage.Name) {
					e.store.Apply(m)
				}
			}
			e.logf(item.Number, opts.tag, "invocation blocked on input (%s) — refunding %s cycle counter for stage %q (#2280)\n", kind, opts.cycle.label, stage.Name)
		} else if kind != "" {
			e.refundDidNotRunCycle(item, itemRepo, stage, opts.tag, kind, opts.cycle)
		} else {
			// The reinvoke stays charged (it ran, or is ambiguous): a genuine
			// cycle ends any streak of never-ran reinvokes, so the tally the
			// pause messages read never blames an old outage for a loop
			// that is really failing to converge.
			e.store.Apply(itemstate.DidNotRunReinvokesReset{Repo: itemRepo, Number: item.Number, StageName: stage.Name})
			if opts.after != nil {
				opts.after(workDir, err)
			}
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
// is looking at. The tally is reset by every reinvoke that stays charged, so it
// is the streak of never-ran reinvokes since the last genuine cycle — an old
// outage cannot be blamed for a loop that is really failing to converge.
// Returns dominant when that streak is at least cycleCount, in which case
// callers replace their "reviewer keeps requesting changes" style diagnosis
// with note; a smaller nonzero streak yields an addendum only; a zero tally
// yields "" so the message is byte-identical to before.
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
