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
)

// checkMergeabilityGate interprets the pre-fetched settle result to detect a
// base-branch conflict before the CI gate acts. Runs only when stage.WaitForCI
// is true — the same opt-in signal used by the CI gate, since a PR that cannot
// merge has no reason to proceed to CI-await.
//
// Returns (blocked, conflict):
//
//   - (false, false) — gate clears. No PR, mergeable==true, CI failed (deferred
//     to checkCIGate), or wait_for_ci disabled. Caller falls through to the CI gate.
//
//   - (true, false)  — blocked but no confirmed conflict. GitHub has not yet
//     computed mergeability (PRMergeUnsettled). The fabrik:rebase-needed label
//     is not touched.
//
//   - (true, true)   — confirmed conflict. fabrik:rebase-needed applied
//     (idempotent). Caller should dispatch a rebase reinvoke.
func (e *Engine) checkMergeabilityGate(item gh.ProjectItem, stage *stages.Stage, settle PRSettleResult) (blocked, conflict bool) {
	if stage.WaitForCI == nil || !*stage.WaitForCI {
		return false, false
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	switch settle.Status {
	case PRMergeNoPR, PRMergeTerminal:
		return false, false
	case PRMergeUnsettled:
		return true, false
	case PRMergeBlocked:
		// CI has failed but there is no base-branch conflict. Clear the merge
		// gate so checkCIGate can classify the failure and dispatch CI-fix.
		e.removeRebaseNeededLabel(owner, repo, item)
		return false, false
	case PRMergeReady:
		// Clear a stale label if one is present from an earlier conflict that
		// has since been resolved.
		e.removeRebaseNeededLabel(owner, repo, item)
		return false, false
	case PRMergeConflicting:
		prNum := 0
		if settle.PR != nil {
			prNum = settle.PR.Number
		}
		// Confirmed conflict. Apply fabrik:rebase-needed (idempotent).
		e.logf(item.Number, "merge-gate", "PR #%d is not mergeable (base conflict) — rebase required\n", prNum)
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
	return false, false
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
// dispatchReviewReinvoke: marks the item in-flight via WorkerEntered, acquires
// the semaphore, calls processComments, then releases both.
func (e *Engine) dispatchRebaseReinvoke(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
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
			Worker:     &itemstate.WorkerHandle{StageName: stage.Name, StartedAt: now, LastSignAt: now},
		})
		done := make(chan struct{})
		defer close(done)
		e.startHeartbeat(ctx, itemRepo, item.Number, done)
		onPIDReady := func(pid int) {
			e.store.Apply(itemstate.WorkerPIDSet{Repo: itemRepo, Number: item.Number, PID: pid})
		}

		e.logf(item.Number, "rebase-reinvoke", "re-invoking stage %q via comment processing with rebase context\n", stage.Name)
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		err := e.processComments(ctx, board, item, &rebaseStage, []gh.Comment{syntheticComment}, onPIDReady)

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.logf(item.Number, "warn", "rebase re-invocation failed: %v\n", err)
			return
		}

		// GitHub disables auto-merge on every push. Re-enable it if this issue is in
		// the convergence flow (fabrik:auto-merge-enabled present at dispatch time).
		if hasLabel(item, "fabrik:auto-merge-enabled") {
			pr, prErr := e.client.FetchLinkedPR(owner, repo, item.Number)
			if prErr != nil || pr == nil || pr.Number == 0 {
				e.logf(item.Number, "warn", "rebase reinvoke: could not fetch linked PR for auto-merge re-enable: %v\n", prErr)
			} else {
				strategy := e.cfg.AutoMergeStrategy
				if strategy == "" {
					strategy = "MERGE"
				}
				if rerr := e.client.EnablePullRequestAutoMerge(owner, repo, pr.Number, strategy); rerr != nil {
					if errors.Is(rerr, gh.ErrAutoMergeAlreadyClean) {
						// PR is already CLEAN after the rebase push — merge directly.
						// Without this, checkAutoMergeConvergence sees AutoMergeEnabled=false
						// and incorrectly pauses the issue as "user disabled auto-merge".
						e.logf(item.Number, "info", "PR #%d is already in clean status after rebase — falling back to direct merge\n", pr.Number)
						if mergeErr := e.client.MergePR(owner, repo, pr.Number); mergeErr != nil {
							e.logf(item.Number, "warn", "direct merge fallback after rebase failed: %v\n", mergeErr)
						} else {
							e.logf(item.Number, "info", "PR #%d merged directly after rebase push (already-clean fallback)\n", pr.Number)
						}
					} else {
						e.logf(item.Number, "warn", "auto-merge re-enable after rebase failed: %v\n", rerr)
					}
				} else {
					e.logf(item.Number, "auto-merge", "re-enabled auto-merge on PR #%d after rebase push\n", pr.Number)
				}
			}
		}
	}()
}

// checkAutoMergeConvergence monitors a yolo issue that has entered the GitHub
// native auto-merge convergence flow (fabrik:auto-merge-enabled is present).
// Called from Phase 1 of the catch-up loop; replaces checkMergeabilityGate and
// checkCIGate for these items. settle carries the pre-fetched PR/CI state from
// settlePRMergeState; it drives the unsettled/conflict branch decisions so that
// the convergence path no longer independently interprets mergeable_state.
// Returns after completing any dispatch or pause.
func (e *Engine) checkAutoMergeConvergence(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, settle PRSettleResult) {
	// Phase 1 of the catch-up loop calls this before processItem has had a chance
	// to register the WorktreeManager for item.Repo. Without this guard,
	// pauseForConvergenceFailed → worktreesFor would panic on the first poll
	// cycle after restart whenever fabrik:auto-merge-enabled is present and the
	// convergence budget has already elapsed. Mirrors the guard used by the
	// other reinvoke dispatchers (ci-fix, rebase, review).
	if err := e.ensureRepoReady(ctx, item); err != nil {
		if errors.Is(err, ErrSkipItem) {
			return
		}
		e.logf(item.Number, "warn", "auto-merge convergence: ensureRepoReady failed: %v\n", err)
		return
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoStr := itemOwnerRepoString(item, e.defaultRepo())

	// Use e.client (direct GitHub API) rather than e.readClient (boardcache) so
	// that pr.AutoMergeEnabled reflects the live GitHub state. The boardcache
	// LinkedPRState does not persist AutoMergeEnabled; a cache hit would return
	// false and incorrectly trigger the "user disabled auto-merge" path on every poll.
	pr, err := e.client.FetchLinkedPR(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "auto-merge", "could not fetch linked PR: %v — skipping convergence check\n", err)
		return
	}
	if pr == nil || pr.Number == 0 {
		return
	}

	// PR merged or closed: remove label and advance to the next stage (Done).
	// There is no passive machinery that would advance the board column after the
	// label is removed; advanceToNextStage must be called explicitly here.
	if pr.Merged || pr.State == "closed" {
		e.logf(item.Number, "auto-merge", "PR #%d merged or closed — advancing to Done\n", pr.Number)
		if rerr := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:auto-merge-enabled"); rerr != nil {
			e.logf(item.Number, "warn", "could not remove fabrik:auto-merge-enabled: %v\n", rerr)
		} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:auto-merge-enabled")
		}
		e.removeRebaseNeededLabel(owner, repo, item)
		if err := e.advanceToNextStage(board, item, stage); err != nil {
			e.logf(item.Number, "warn", "could not advance to Done after PR merge: %v\n", err)
		}
		return
	}

	// User manually disabled auto-merge: pause the issue to prevent the next poll
	// from re-enabling auto-merge via Phase 2 (yoloActive=true + no label → attemptMergeOnValidate).
	if !pr.AutoMergeEnabled {
		e.logf(item.Number, "auto-merge", "PR #%d auto-merge disabled by user — pausing\n", pr.Number)
		msg := fmt.Sprintf("🏭 **Fabrik — auto-merge disabled**\n\n" +
			"Fabrik detected that GitHub auto-merge was disabled on the linked PR. " +
			"This issue has been paused (`fabrik:paused` + `fabrik:awaiting-input`) to prevent Fabrik from re-enabling auto-merge on the next poll cycle.\n\n" +
			"To resume:\n" +
			"- **Keep cruise behavior**: Remove `fabrik:paused` and `fabrik:yolo`. Fabrik will keep the branch up-to-date but leave merging to you.\n" +
			"- **Re-enable auto-merge**: Remove `fabrik:paused`. Fabrik will re-enable auto-merge on the next poll cycle.\n" +
			"- **Leave as-is**: Take no action. The PR remains open and unmerged until you act.")
		if dbID, cerr := e.client.AddComment(owner, repo, item.Number, msg); cerr != nil {
			e.logf(item.Number, "warn", "could not post auto-merge disabled comment: %v\n", cerr)
		} else {
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
					DatabaseID: dbID, Body: msg, Author: e.cfg.User, CreatedAt: time.Now(),
				})
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
		if rerr := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:auto-merge-enabled"); rerr != nil {
			e.logf(item.Number, "warn", "could not remove fabrik:auto-merge-enabled: %v\n", rerr)
		} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:auto-merge-enabled")
		}
		return
	}

	// Check convergence budget.
	if e.cfg.ConvergenceBudget > 0 {
		budgetStart, berr := e.client.FetchLabelAppliedAt(owner, repo, item.Number, "fabrik:auto-merge-enabled")
		if berr != nil {
			e.logf(item.Number, "auto-merge", "could not fetch fabrik:auto-merge-enabled applied-at: %v\n", berr)
		} else if !budgetStart.IsZero() {
			elapsed := time.Since(budgetStart)
			if elapsed > e.cfg.ConvergenceBudget {
				e.logf(item.Number, "auto-merge", "convergence budget exhausted (%.0fs / %.0fs) — pausing\n",
					elapsed.Seconds(), e.cfg.ConvergenceBudget.Seconds())
				e.pauseForConvergenceFailed(ctx, board, item, stage, settle, elapsed)
				return
			}
		}
	}

	// GitHub has not yet computed mergeability: wait.
	if settle.Status == PRMergeUnsettled {
		e.logf(item.Number, "auto-merge", "PR #%d settle=unsettled (%s) — waiting for GitHub to compute\n", pr.Number, settle.Reason)
		return
	}

	// Confirmed conflict: dispatch rebase reinvoke, bounded by MaxRebaseCycles.
	// Mirrors the three-step pattern in handleMergeAndCIGates: in-flight guard →
	// cycle-limit check → dispatch or pauseForRebaseCycleLimit.
	if settle.Status == PRMergeConflicting {
		var cycleCount int
		if snap, serr := e.store.Get(repoStr, item.Number); serr == nil {
			if snap.Worker() != nil {
				e.logf(item.Number, "auto-merge", "rebase already in-flight — skipping dispatch\n")
				return
			}
			cycleCount = snap.RebaseCycles(stage.Name)
		}
		maxCycles := e.cfg.MaxRebaseCycles
		if cycleCount >= maxCycles {
			e.pauseForRebaseCycleLimit(board, item, stage, cycleCount, maxCycles)
		} else {
			e.logf(item.Number, "auto-merge", "PR #%d merge conflict — dispatching rebase reinvoke\n", pr.Number)
			e.store.Apply(itemstate.RebaseCycleIncremented{Repo: repoStr, Number: item.Number, StageName: stage.Name})
			e.dispatchRebaseReinvoke(ctx, board, item, stage)
		}
		return
	}

	// All other settle statuses (PRMergeReady, PRMergeBlocked, PRMergeTerminal, PRMergeNoPR):
	// GitHub is handling it — auto-merge will fire when conditions are met.
	e.logf(item.Number, "auto-merge", "PR #%d settle=%s — waiting for GitHub auto-merge\n", pr.Number, settle.Reason)
}

// pauseForConvergenceFailed is called when the convergence budget exhausts before
// the yolo PR merges. Posts a structured comment naming the actual current state,
// applies fabrik:paused + fabrik:awaiting-input, and removes fabrik:auto-merge-enabled.
// settle carries the pre-fetched PR/CI state from settlePRMergeState; PR diagnostic
// fields (mergeableState, headSHA) and check run summary come from settle rather
// than a fresh FetchLinkedPR / FetchCheckRuns call, eliminating the split-brain.
func (e *Engine) pauseForConvergenceFailed(_ context.Context, _ *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, settle PRSettleResult, elapsed time.Duration) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoStr := itemOwnerRepoString(item, e.defaultRepo())

	pr := settle.PR
	var mergeableState, headSHA, latestCI string
	var commitsBehind int
	if pr != nil && pr.Number != 0 {
		mergeableState = pr.MergeableState
		headSHA = pr.HeadSHA
		// Determine base branch for commits-behind count.
		wm := e.worktreesFor(item.Repo)
		baseBranch, _ := e.baseBranchForItem(item, wm)
		if baseBranch == "" {
			baseBranch = "main"
		}
		commitsBehind, _ = e.client.FetchCommitsBehind(owner, repo, baseBranch, headSHA)
		latestCI = summarizeCIRuns(settle.CheckRuns)
	}

	var rebaseCycles int
	if snap, serr := e.store.Get(repoStr, item.Number); serr == nil {
		rebaseCycles = snap.RebaseCycles(stage.Name)
	}

	budgetStr := e.cfg.ConvergenceBudget.String()
	elapsedStr := elapsed.Round(time.Second).String()

	msg := fmt.Sprintf(
		"🏭 **Fabrik — convergence budget exhausted**\n\n"+
			"Fabrik attempted to merge the linked PR via GitHub native auto-merge for **%s** "+
			"(budget: %s) without converging. Current state:\n\n"+
			"| | |\n"+
			"|---|---|\n"+
			"| Elapsed | %s |\n"+
			"| Rebase reinvokes dispatched | %d |\n"+
			"| Commits behind base | %d |\n"+
			"| PR `mergeable_state` | `%s` |\n"+
			"| Latest CI (SHA `%s`) | %s |\n\n"+
			"**Next steps — choose one:**\n"+
			"1. **Manual rebase + re-yolo**: Resolve conflicts manually, push, then remove `fabrik:paused` and ensure `fabrik:yolo` is present. Fabrik will re-enable auto-merge and restart the convergence budget.\n"+
			"2. **Switch to cruise**: Remove `fabrik:paused` + `fabrik:yolo`, add `fabrik:cruise`. Fabrik will keep the branch rebased against main but leave merging to you.\n"+
			"3. **Leave as-is**: Remove `fabrik:paused` when ready. Fabrik will retry auto-merge from the current state.",
		elapsedStr, budgetStr,
		elapsedStr, rebaseCycles, commitsBehind,
		mergeableState, headSHA, latestCI,
	)

	if dbID, err := e.client.AddComment(owner, repo, item.Number, msg); err != nil {
		e.logf(item.Number, "warn", "could not post convergence-failed pause comment: %v\n", err)
	} else {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
				DatabaseID: dbID, Body: msg, Author: e.cfg.User, CreatedAt: time.Now(),
			})
		}
		if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
			e.logf(item.Number, "warn", "could not add 🚀 to convergence-failed comment: %v\n", reactErr)
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
	if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:auto-merge-enabled"); err != nil {
		e.logf(item.Number, "warn", "could not remove fabrik:auto-merge-enabled: %v\n", err)
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:auto-merge-enabled")
	}
}

// summarizeCIRuns produces a brief human-readable summary of check run results.
func summarizeCIRuns(runs []gh.CheckRun) string {
	if len(runs) == 0 {
		return "none"
	}
	var failed, pending, passed int
	for _, r := range runs {
		switch {
		case r.Status != "completed":
			pending++
		case r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped":
			passed++
		default:
			failed++
		}
	}
	if failed > 0 {
		return fmt.Sprintf("❌ %d failed, %d passed, %d pending", failed, passed, pending)
	}
	if pending > 0 {
		return fmt.Sprintf("⏳ %d pending, %d passed", pending, passed)
	}
	return fmt.Sprintf("✅ %d passed", passed)
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
