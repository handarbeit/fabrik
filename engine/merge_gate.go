package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// checkMergeabilityGate inspects GitHub's mergeable flag on the linked PR to
// detect a base-branch conflict before the CI gate polls checks. Runs only
// when stage.WaitForCI is true — the same opt-in signal used by the CI gate,
// since a PR that cannot merge has no reason to proceed to CI-await.
//
// Returns (blocked, conflict):
//
//   - (false, false) — gate clears. No PR, mergeable==true, or wait_for_ci
//     disabled. Caller falls through to the CI gate on the same poll.
//
//   - (true, false)  — blocked but no confirmed conflict. Mergeable is nil
//     (GitHub has not yet computed it), or a transient API error was seen
//     while fetching. Caller should skip to next item; the next poll
//     re-evaluates. The fabrik:rebase-needed label is not touched.
//
//   - (true, true)   — confirmed conflict. fabrik:rebase-needed applied
//     (idempotent). Caller should dispatch a rebase reinvoke.
func (e *Engine) checkMergeabilityGate(item gh.ProjectItem, stage *stages.Stage) (blocked, conflict bool) {
	if stage.WaitForCI == nil || !*stage.WaitForCI {
		return false, false
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	pr, err := e.readClient.FetchLinkedPR(owner, repo, item.Number)
	if err != nil {
		// Transient API error. Block this item for the rest of Phase 1 so we
		// don't run the CI gate on stale data (it would make its own REST
		// call against an unknown mergeability state). Mirrors checkCIGate's
		// transient-error handling.
		e.logf(item.Number, "merge-gate", "could not fetch linked PR: %v — blocking until API recovers\n", err)
		return true, false
	}
	if pr == nil || pr.Number == 0 {
		return false, false
	}

	mergeable, err := e.readClient.FetchPRMergeable(owner, repo, pr.Number)
	if err != nil {
		e.logf(item.Number, "merge-gate", "could not fetch mergeable: %v — blocking until API recovers\n", err)
		return true, false
	}

	if mergeable == nil {
		// GitHub has not yet computed mergeability (null). Asking again triggers
		// the computation; the next poll will see a definite answer.
		e.logf(item.Number, "merge-gate", "PR #%d mergeable=null — GitHub still computing, re-checking next poll\n", pr.Number)
		return true, false
	}

	if *mergeable {
		// Clear a stale label if one is present from an earlier conflict that
		// has since been resolved.
		e.removeRebaseNeededLabel(owner, repo, item)
		return false, false
	}

	// Confirmed conflict. Apply fabrik:rebase-needed (idempotent).
	e.logf(item.Number, "merge-gate", "PR #%d is not mergeable (base conflict) — rebase required\n", pr.Number)
	alreadyLabeled := false
	for _, l := range item.Labels {
		if l == "fabrik:rebase-needed" {
			alreadyLabeled = true
			break
		}
	}
	if !alreadyLabeled {
		if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:rebase-needed"); err != nil {
			e.logf(item.Number, "warn", "could not add fabrik:rebase-needed label: %v\n", err)
		} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:rebase-needed")
		}
	}
	return true, true
}

// removeRebaseNeededLabel clears fabrik:rebase-needed if present.
func (e *Engine) removeRebaseNeededLabel(owner, repo string, item gh.ProjectItem) {
	for _, l := range item.Labels {
		if l == "fabrik:rebase-needed" {
			if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:rebase-needed"); err != nil {
				e.logf(item.Number, "warn", "could not remove fabrik:rebase-needed label: %v\n", err)
			} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:rebase-needed")
			}
			return
		}
	}
}

// buildRebaseComment constructs a synthetic comment instructing the stage
// agent to rebase onto the base branch and resolve conflicts. It intentionally
// leaves the resolution to Claude rather than running `git rebase` engine-side,
// because semantic conflicts (two PRs adding the same ADR number, two PRs
// choosing the same migration ID) require judgment that a plain rebase would
// silently mishandle.
//
// The stage name is embedded in the body so the agent knows which stage is
// being re-invoked (the comment lands through processComments, which strips
// stage-level framing).
func (e *Engine) buildRebaseComment(item gh.ProjectItem, stage *stages.Stage, baseBranch string) gh.Comment {
	branchName := fmt.Sprintf("fabrik/issue-%d", item.Number)
	if baseBranch == "" {
		baseBranch = "main"
	}
	body := fmt.Sprintf(
		"🏭 **Fabrik — Rebase required** (re-invoking stage: %s)\n\n"+
			"GitHub reports PR for issue #%d as not mergeable — the base branch `%s` has moved "+
			"since this branch was last rebased, and a conflict exists.\n\n"+
			"**Instructions:**\n"+
			"1. `git status` and commit any uncommitted work on `%s`.\n"+
			"2. `git fetch origin %s && git rebase origin/%s`.\n"+
			"3. Resolve every conflict **conservatively** — never drop code from `%s`. Your branch adds to the base, it does not replace it.\n"+
			"4. Watch for **semantic collisions** (two files picked the same number, two ADRs chose the same identifier, two migrations claimed the same slot). Rename your side, update any references, and keep both contributions.\n"+
			"5. Run the project's build + test commands. If either fails, the resolution is wrong — fix it before pushing.\n"+
			"6. `git push --force-with-lease` once clean.\n"+
			"7. **Do NOT emit `FABRIK_STAGE_COMPLETE`.** The engine re-evaluates mergeability on the next poll and resumes the CI gate once the push lands.\n\n"+
			"If a conflict cannot be resolved safely (the required judgment is beyond an automated fix), `git rebase --abort`, leave the branch untouched, and explain the situation so a human can take over.\n",
		stage.Name, item.Number, baseBranch, branchName, baseBranch, baseBranch, baseBranch,
	)
	return gh.Comment{
		ID:         "rebase-synthetic",
		DatabaseID: 0, // synthetic — no GitHub comment exists to react to
		Body:       body,
		Author:     "fabrik",
	}
}

// dispatchRebaseReinvoke spawns a goroutine that re-invokes the stage agent
// with a synthetic rebase-required comment. Mirrors dispatchCIFixReinvoke /
// dispatchReviewReinvoke: marks inFlight, acquires the semaphore, calls
// processComments, then releases both.
func (e *Engine) dispatchRebaseReinvoke(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	iKey := issueKey(item, e.defaultRepo())

	e.inFlight.Store(iKey, item.IsPR)
	e.wg.Add(1)

	itemRepo := itemOwnerRepoString(item, e.defaultRepo())

	go func() {
		defer e.wg.Done()
		defer e.inFlight.Delete(iKey)

		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			e.logf(item.Number, "rebase-reinvoke", "context cancelled before semaphore acquired\n")
			return
		}
		defer func() { <-e.sem }()

		if err := e.ensureRepoReady(ctx, item); err != nil {
			if errors.Is(err, ErrSkipItem) {
				e.logf(item.Number, "rebase-reinvoke", "repo not ready, skipping reinvoke\n")
				return
			}
			e.logf(item.Number, "warn", "rebase reinvoke: ensureRepoReady failed: %v\n", err)
			return
		}

		// Resolve the base branch for the rebase instructions. Failure here is
		// not fatal — the synthetic comment falls back to "main".
		wm := e.worktreesFor(item.Repo)
		baseBranch, _ := e.baseBranchForItem(item, wm)

		syntheticComment := e.buildRebaseComment(item, stage, baseBranch)

		rebaseStage := *stage
		if stage.RebaseSkill != "" {
			rebaseStage.CommentSkill = stage.RebaseSkill
			rebaseStage.CommentPrompt = ""
		}

		// Register WorkerHandle so the heartbeat/liveness system tracks this goroutine.
		now := time.Now()
		e.store.Apply(itemstate.LocalLockAcquired{
			Repo:       itemRepo,
			Number:     item.Number,
			User:       e.cfg.User,
			AcquiredAt: now,
			Worker:     &itemstate.WorkerHandle{StageName: stage.Name, StartedAt: now},
		})
		done := make(chan struct{})
		defer close(done)
		e.startHeartbeat(ctx, itemRepo, item.Number, done)
		defer e.store.Apply(itemstate.WorkerExited{Repo: itemRepo, Number: item.Number})
		onPIDReady := func(pid int) {
			e.store.Apply(itemstate.WorkerPIDSet{Repo: itemRepo, Number: item.Number, PID: pid})
		}

		startTime := time.Now()
		e.emitStructural(tui.JobStartedEvent{
			IssueNumber: item.Number,
			Repo:        itemRepo,
			Title:       item.Title,
			StageName:   stage.Name,
			IsComment:   true,
			StartedAt:   startTime,
		})

		e.logf(item.Number, "rebase-reinvoke", "re-invoking stage %q via comment processing with rebase context\n", stage.Name)
		err := e.processComments(ctx, board, item, &rebaseStage, []gh.Comment{syntheticComment}, onPIDReady)

		var usage TokenUsage
		var completed, blocked bool
		if snap, snapErr := e.store.Get(itemRepo, item.Number); snapErr == nil {
			st := snap.State()
			usage = st.LastTokenUsage
			completed = st.LastInvocationCompleted
			blocked = st.LastInvocationBlocked
		}
		e.emitStructural(tui.JobCompletedEvent{
			IssueNumber:    item.Number,
			Repo:           itemRepo,
			Title:          item.Title,
			StageName:      stage.Name,
			StageModel:     stage.Model,
			IsComment:      true,
			Success:        err == nil,
			Completed:      completed,
			BlockedOnInput: blocked,
			Duration:       time.Since(startTime),
			CompletedAt:    time.Now(),
			TurnsUsed:      usage.TurnsUsed,
			MaxTurns:       usage.MaxTurns,
			CostUSD:        usage.CostUSD,
		})

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.logf(item.Number, "warn", "rebase re-invocation failed: %v\n", err)
		}
	}()
}

// pauseForRebaseCycleLimit pauses the issue when rebase re-invocations have
// been attempted too many times — usually a signal that the conflict needs
// human judgment.
func (e *Engine) pauseForRebaseCycleLimit(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, cycleCount, maxCycles int) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	e.logf(item.Number, "rebase-cycles", "rebase cycle limit %d reached — pausing for human intervention\n", maxCycles)

	msg := fmt.Sprintf(
		"🏭 **Fabrik — rebase cycle limit reached**\n\nThe stage **%s** has been re-invoked to rebase onto the base branch %d time(s), "+
			"which has reached the configured limit of %d (override with `--max-rebase-cycles` or `FABRIK_MAX_REBASE_CYCLES`).\n\n"+
			"GitHub still reports the PR as not mergeable. This usually means the conflict requires human judgment "+
			"(for example: two PRs picked the same ADR number or migration slot, or a semantic overlap that cannot be "+
			"resolved by automated rebase).\n\n"+
			"Fabrik has paused this issue. Resolve the conflict manually, then remove the `fabrik:paused` and "+
			"`fabrik:rebase-needed` labels to resume.",
		stage.Name, cycleCount, maxCycles,
	)
	if dbID, err := e.client.AddComment(owner, repo, item.Number, msg); err != nil {
		e.logf(item.Number, "warn", "could not post rebase cycle limit comment: %v\n", err)
	} else {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
				DatabaseID: dbID, Body: msg, Author: e.cfg.User, CreatedAt: time.Now(),
			})
		}
		// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
		if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
			e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
		}
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:paused: %v\n", err)
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:paused")
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:awaiting-input: %v\n", err)
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-input")
	}
}
