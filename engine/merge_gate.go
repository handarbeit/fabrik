package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	case PRMergeQueued:
		// ADR-058 D4 FR-1: the PR is in GitHub's merge queue — a transient hand-off.
		// Block like PRMergeUnsettled (no conflict, no fabrik:rebase-needed churn) so a
		// human-enqueued non-yolo PR at a gate-checked stage simply waits for the queue.
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
			e.applyLabelAdd(item, "fabrik:rebase-needed", false)
		}
		return true, true
	}
	return false, false
}

// removeRebaseNeededLabel clears fabrik:rebase-needed if present.
func (e *Engine) removeRebaseNeededLabel(owner, repo string, item gh.ProjectItem) {
	for _, l := range item.Labels {
		if l == "fabrik:rebase-needed" {
			e.applyLabelRemove(item, "fabrik:rebase-needed", false)
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
		// the convergence flow (fabrik:auto-merge-enabled present at dispatch time) —
		// but NOT on a merge-queue repo (ADR-058 D4): there the recovery path is
		// re-enqueue, not native auto-merge, and the convergence monitor re-enqueues
		// the resolved PR once it re-derives clean. Re-enabling auto-merge here would
		// fight the queue model.
		if hasLabel(item, "fabrik:auto-merge-enabled") && !item.LinkedPRIsMergeQueueEnabled {
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
// priorInQueue is the item's previous-poll merge-queue membership, captured in
// poll.go before ItemDeepFetched overwrites the store (ADR-058 D4 OQ-3). It drives
// the poll-native "left the queue" edge used by the ejection-recovery classifier.
func (e *Engine) checkAutoMergeConvergence(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, settle PRSettleResult, priorInQueue bool) {
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

	// ADR-059 D6: under merge_train: on, fabrik:auto-merge-enabled is the expected anchor
	// for queue-enabled repos. handleMergeTrainBatch routes each Queued item to its landing
	// engine per repo: queue-enabled repos take the ADR-058 enqueue path (which applies this
	// label), and this convergence monitor then drains them Queued → Done. Non-queue repos go
	// to the internal train and never reach this path. So an item here under merge_train: on is
	// the intended 058-from-Queued convergence (or a pre-merge_train straggler); either way, let
	// convergence complete rather than interrupting mid-flight PR state. This is a debug trace,
	// not a warning — the interaction is now resolved by design.
	if e.cfg.MergeTrain == "on" {
		e.logf(item.Number, "auto-merge", "merge_train: on — item in auto-merge convergence (ADR-058 enqueue path from Queued handler); letting convergence complete\n")
	}

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

	// ① Terminal-first (the #913 merged-first invariant): a PR that merged via the
	// queue is terminal, never an ejection failure. settle.Status == PRMergeTerminal
	// already re-confirmed merged/closed via the authoritative single-PR endpoint
	// (pr_settle.go), catching the window where the REST list endpoint still reports
	// merged=false for several seconds after a queue merge. Evaluate it FIRST, before
	// any queue/ejection classification — a dequeue is never misread as a failure.
	if settle.Status == PRMergeTerminal || pr.Merged || pr.State == "closed" {
		e.advanceConvergedPRToDone(board, item, stage, pr.Number)
		return
	}

	// ② In-queue hand-off (ADR-058 D4 FR-1): the queue owns the PR — wait, no churn.
	// This is the single place queue membership is interpreted in the convergence
	// owner, replacing the inline settle.PR.IsInMergeQueue read #935 left here.
	// Record the SHA at which GitHub holds the PR so the clean re-enqueue gate below
	// can tell the post-enqueue consistency window (same SHA → suppress) apart from a
	// genuine post-resolution re-enqueue (SHA changed → enqueue fresh).
	if settle.Status == PRMergeQueued {
		if settle.PR != nil && settle.PR.HeadSHA != "" {
			e.store.Apply(itemstate.PREnqueueRecorded{Repo: repoStr, Number: item.Number, SHA: settle.PR.HeadSHA})
		}
		e.logf(item.Number, "auto-merge", "PR #%d in merge queue — waiting for queue to merge\n", pr.Number)
		// ②′ ADR-058 D5: stall detect-and-warn. When the PR has been in the queue
		// past CIWaitTimeout with no merge-group CI ever reporting (observable as
		// settle.Status remaining PRMergeQueued past the dwell), pause the issue with
		// an instructional comment. The dwell is anchored to fabrik:auto-merge-enabled
		// applied-at (set at first enqueue) so it survives restarts. Guard on
		// CIWaitTimeout > 0 so operators can disable the check.
		if e.cfg.CIWaitTimeout > 0 {
			appliedAt, faErr := e.client.FetchLabelAppliedAt(owner, repo, item.Number, "fabrik:auto-merge-enabled")
			if faErr != nil {
				e.logf(item.Number, "auto-merge", "could not fetch fabrik:auto-merge-enabled applied-at for stall check: %v\n", faErr)
			} else if !appliedAt.IsZero() && time.Since(appliedAt) >= e.cfg.CIWaitTimeout {
				e.logf(item.Number, "auto-merge", "PR #%d in merge queue past dwell (%s) with no merge-group CI — stall detected\n",
					pr.Number, e.cfg.CIWaitTimeout)
				e.pauseForMergeGroupStall(item, pr.Number)
				return
			}
		}
		return
	}

	// mergeQueueEnabled gates the ejection-recovery ladder (queue repos) against the
	// legacy GitHub-native auto-merge wait (non-queue repos). Dual-source for
	// cache-miss safety, mirroring the settle PRMergeQueued derivation.
	mergeQueueEnabled := item.LinkedPRIsMergeQueueEnabled || (settle.PR != nil && settle.PR.IsMergeQueueEnabled)
	// leftQueue is the poll-native ejection edge: in the queue last poll, not now.
	// settle.Status != PRMergeQueued is guaranteed here (handled at step ②).
	leftQueue := priorInQueue

	// ③ User manually disabled auto-merge: pause — but ONLY on non-queue repos. Every
	// enqueue-path PR has AutoMergeEnabled==false (EnqueuePullRequest never sets
	// auto_merge), so on a queue repo a false flag is the NORMAL state of an ejected
	// PR, not a user action — misreading it would pause every ejected PR (the
	// #913-adjacent trap). On queue repos the ejection-recovery ladder below owns it.
	if !pr.AutoMergeEnabled && !mergeQueueEnabled {
		e.logf(item.Number, "auto-merge", "PR #%d auto-merge disabled by user — pausing\n", pr.Number)
		msg := fmt.Sprintf("🏭 **Fabrik — auto-merge disabled**\n\n" +
			"Fabrik detected that GitHub auto-merge was disabled on the linked PR. " +
			"This issue has been paused (`fabrik:paused` + `fabrik:awaiting-input`) to prevent Fabrik from re-enabling auto-merge on the next poll cycle.\n\n" +
			"To resume:\n" +
			"- **Keep cruise behavior**: Remove `fabrik:paused` and `fabrik:yolo`. Fabrik will keep the branch up-to-date but leave merging to you.\n" +
			"- **Re-enable auto-merge**: Remove `fabrik:paused`. Fabrik will re-enable auto-merge on the next poll cycle.\n" +
			"- **Leave as-is**: Take no action. The PR remains open and unmerged until you act.")
		e.pauseIssue(item, msg, pauseOpts{
			awaitingInput:   true,
			removeAutoMerge: true,
		})
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

	// Confirmed conflict: dispatch a Claude rebase reinvoke, bounded by
	// MaxRebaseCycles. Mirrors the three-step pattern in handleMergeAndCIGates:
	// in-flight guard → cycle-limit check → dispatch or pauseForRebaseCycleLimit.
	// Shared by queue and non-queue repos. Past step ②, settle.Status ==
	// PRMergeConflicting guarantees the PR is NOT in the queue (an in-queue PR
	// returns PRMergeQueued), so #935's inline in-queue guard here is now dead and
	// removed — FR-1 consolidation. On a queue repo this is the D4 ejection→resolve
	// path: Claude resolves + force-pushes, and the clean re-enqueue below fires on
	// a later poll once the new SHA settles.
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

	// Ejection recovery for the remaining statuses — queue repos only (ADR-058 D4
	// FR-2/FR-3). A yolo PR that left the queue and re-derived its own verdict this
	// poll (settle ran the merge+CI gate against the current base) is recovered
	// poll-natively, no webhook:
	//   - PRMergeBlocked (CI failed) → Claude ci-fix reinvoke, bounded by MaxCiFixCycles.
	//   - PRMergeReady (clean) → re-enqueue fresh, gated to avoid the post-enqueue window.
	if mergeQueueEnabled {
		switch settle.Status {
		case PRMergeBlocked:
			var cycleCount int
			if snap, serr := e.store.Get(repoStr, item.Number); serr == nil {
				if snap.Worker() != nil {
					e.logf(item.Number, "auto-merge", "ci-fix already in-flight — skipping dispatch\n")
					return
				}
				cycleCount = snap.CIFixCycles(stage.Name)
			}
			maxCycles := e.cfg.MaxCiFixCycles
			if cycleCount >= maxCycles {
				e.pauseForCIFixCycleLimit(board, item, stage, cycleCount, maxCycles)
			} else {
				e.logf(item.Number, "auto-merge", "PR #%d ejected with failing CI — dispatching ci-fix reinvoke\n", pr.Number)
				e.store.Apply(itemstate.CIFixCycleIncremented{Repo: repoStr, Number: item.Number, StageName: stage.Name})
				e.dispatchCIFixReinvoke(ctx, board, item, stage, settle)
			}
			return
		case PRMergeReady:
			// Re-derived clean. Re-enqueue on the ejection edge (leftQueue — same SHA,
			// immediate) or after an off-queue resolution (head SHA changed since the
			// last enqueue). The post-enqueue consistency window is suppressed: there
			// priorInQueue==false AND the head SHA still equals LastEnqueuedSHA. The
			// LastEnqueuedSHA != "" guard avoids a spurious re-enqueue before the PR
			// has ever been observed in the queue (e.g. immediately after the initial
			// enqueue, or across a restart that lost the in-memory edge).
			var lastEnqueuedSHA string
			if snap, serr := e.store.Get(repoStr, item.Number); serr == nil {
				lastEnqueuedSHA = snap.LastEnqueuedSHA()
			}
			headSHA := ""
			if settle.PR != nil {
				headSHA = settle.PR.HeadSHA
			}
			if leftQueue || (lastEnqueuedSHA != "" && headSHA != lastEnqueuedSHA) {
				e.reEnqueueOrPause(board, item, stage, settle)
			} else {
				e.logf(item.Number, "auto-merge", "PR #%d clean, not ejected (post-enqueue window) — waiting for queue\n", pr.Number)
			}
			return
		}
	}

	// Non-queue fall-through (legacy GitHub-native auto-merge): GitHub is handling
	// it — auto-merge will fire when conditions are met.
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

	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput:   true,
		reactRocket:     true,
		removeAutoMerge: true,
	})
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
	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})
}

// advanceConvergedPRToDone removes the convergence labels and advances a merged
// (or closed) auto-merge PR to the next stage (Done). It is shared by
// checkAutoMergeConvergence's terminal-first guard and reEnqueueOrPause's
// merged-at-the-mutation-point guard so the #913 merged-on-dequeue case is
// handled identically wherever a merge is detected — a dequeue that is in fact a
// successful merge always advances to Done, never re-enqueues or pauses. There is
// no passive machinery that advances the board column once the label is removed,
// so advanceToNextStage must be called explicitly.
func (e *Engine) advanceConvergedPRToDone(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, prNumber int) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	e.logf(item.Number, "auto-merge", "PR #%d merged or closed — advancing to Done\n", prNumber)
	e.applyLabelRemove(item, "fabrik:auto-merge-enabled", false)
	e.removeRebaseNeededLabel(owner, repo, item)
	if err := e.advanceToNextStage(board, item, stage); err != nil {
		e.logf(item.Number, "warn", "could not advance to Done after PR merge: %v\n", err)
	}
}

// reEnqueueOrPause performs a fresh merge-queue enqueue for a yolo PR that left
// the queue and re-derived clean (PRMergeReady), bounded by MaxEnqueueCycles
// (ADR-058 D4 FR-2/FR-3). It is the single mutation point for D4 re-enqueue
// (ADR-056). Re-enqueue is always "fresh after off-queue resolution," never
// re-enqueue-in-place, so conflict-heavy PRs do not starve the queue.
//
// Order (the merged-first guard is deliberately first — the #913 trap):
//  1. FetchPRMerged (authoritative single-PR endpoint) — the REST list endpoint
//     reports merged=false for several seconds after a queue merge, so re-confirm
//     right at the mutation point. Merged → advance to Done; error → wait (never
//     re-enqueue on an unconfirmed state).
//  2. Worker-in-flight guard — skip if a reinvoke goroutine is already running.
//  3. EnqueueCycles >= MaxEnqueueCycles → pauseForEnqueueCycleLimit (queue-thrash).
//  4. else EnqueuePullRequest at the fresh head SHA (optimistic concurrency fails
//     safe on a stale SHA), then increment EnqueueCycles and record
//     LastEnqueuedSHA. Enqueue failure does NOT increment the cycle.
func (e *Engine) reEnqueueOrPause(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, settle PRSettleResult) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoStr := itemOwnerRepoString(item, e.defaultRepo())

	pr := settle.PR
	if pr == nil || pr.Number == 0 {
		e.logf(item.Number, "auto-merge", "re-enqueue skipped — no PR in settle result\n")
		return
	}

	// (1) Merged-first re-confirmation at the mutation point (#913 trap).
	merged, mErr := e.client.FetchPRMerged(owner, repo, pr.Number)
	if mErr != nil {
		e.logf(item.Number, "auto-merge", "could not confirm merged-state of PR #%d before re-enqueue: %v — waiting\n", pr.Number, mErr)
		return
	}
	if merged {
		e.advanceConvergedPRToDone(board, item, stage, pr.Number)
		return
	}

	// (2) Worker-in-flight guard + (3) cycle-limit check.
	var cycleCount int
	if snap, serr := e.store.Get(repoStr, item.Number); serr == nil {
		if snap.Worker() != nil {
			e.logf(item.Number, "auto-merge", "re-enqueue skipped — reinvoke already in-flight\n")
			return
		}
		cycleCount = snap.EnqueueCycles(stage.Name)
	}
	maxCycles := e.cfg.MaxEnqueueCycles
	if cycleCount >= maxCycles {
		e.pauseForEnqueueCycleLimit(board, item, stage, cycleCount, maxCycles)
		return
	}

	// (4) Fresh enqueue at the current head SHA. EnqueuePullRequest needs the SHA
	// for optimistic concurrency; skip (wait) if it is not yet known.
	if pr.HeadSHA == "" {
		e.logf(item.Number, "auto-merge", "re-enqueue skipped — PR #%d head SHA empty\n", pr.Number)
		return
	}
	if err := e.client.EnqueuePullRequest(owner, repo, pr.Number, pr.HeadSHA); err != nil {
		// Do NOT increment the cycle on enqueue failure — a stale-SHA optimistic
		// concurrency rejection or transient error should be retried next poll.
		e.logf(item.Number, "warn", "re-enqueue of PR #%d failed: %v\n", pr.Number, err)
		return
	}
	e.logf(item.Number, "auto-merge", "re-enqueued PR #%d into merge queue at %s (cycle %d/%d)\n",
		pr.Number, pr.HeadSHA[:min(8, len(pr.HeadSHA))], cycleCount+1, maxCycles)
	e.store.Apply(itemstate.EnqueueCycleIncremented{Repo: repoStr, Number: item.Number, StageName: stage.Name})
	e.store.Apply(itemstate.PREnqueueRecorded{Repo: repoStr, Number: item.Number, SHA: pr.HeadSHA})
}

// pauseForEnqueueCycleLimit pauses the issue when merge-queue re-enqueue trips
// have been attempted too many times — a queue-thrash loop (enqueue → eject →
// re-enqueue → eject) that no single sub-path cap (rebase / CI-fix) would catch.
// Mirrors pauseForRebaseCycleLimit: structured comment naming --max-enqueue-cycles,
// fabrik:paused + fabrik:awaiting-input (write-through). The EnqueueCycles counter
// is cleared by clearFailedStage (EngineCyclesCleared) when the user unpauses.
func (e *Engine) pauseForEnqueueCycleLimit(_ *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, cycleCount, maxCycles int) {
	e.logf(item.Number, "enqueue-cycles", "merge-queue re-enqueue limit %d reached — pausing for human intervention\n", maxCycles)

	msg := fmt.Sprintf(
		"🏭 **Fabrik — merge-queue re-enqueue limit reached**\n\nThe linked PR for stage **%s** has been re-enqueued into GitHub's merge queue %d time(s), "+
			"which has reached the configured limit of %d (override with `--max-enqueue-cycles` or `FABRIK_MAX_ENQUEUE_CYCLES`).\n\n"+
			"The PR keeps being ejected from the merge queue and re-enqueued without merging. This usually means the merge group repeatedly fails to build or test "+
			"against the current base — for example a flaky required check, a missing `merge_group` CI trigger, or a persistent semantic conflict with other queued PRs.\n\n"+
			"Fabrik has paused this issue. Investigate the merge-queue failures, then remove the `fabrik:paused` label to resume.",
		stage.Name, cycleCount, maxCycles,
	)
	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})
}

// pauseForMergeGroupStall pauses the issue when the linked PR has been in the
// merge queue past CIWaitTimeout with no merge-group CI ever reporting (ADR-058
// D5). Posts an instructional comment telling the operator to add on:merge_group
// to their required-check workflows, then applies fabrik:paused +
// fabrik:awaiting-input and removes fabrik:auto-merge-enabled.
//
// Removing fabrik:auto-merge-enabled resets the convergence flow so that after
// the operator fixes CI and re-queues, attemptMergeOnValidate re-applies the
// label with a fresh timestamp — preventing the stall check from re-firing
// immediately on resume (the dwell anchor would otherwise already be elapsed).
func (e *Engine) pauseForMergeGroupStall(item gh.ProjectItem, prNumber int) {
	dwellMinutes := int(e.cfg.CIWaitTimeout.Round(time.Minute).Minutes())
	e.logf(item.Number, "merge-queue", "stall detected on PR #%d after %d minutes — pausing with instructional comment\n", prNumber, dwellMinutes)

	msg := fmt.Sprintf(
		"🏭 **Fabrik — merge queue stall detected**\n\n"+
			"The merge queue is enabled but no CI check ever reported for the merge group after %d minutes. "+
			"This typically means no workflow has `on: merge_group` configured.\n\n"+
			"**Action required**: add `on: merge_group` to each workflow that must pass as a required check, "+
			"then re-queue the PR. Remove `fabrik:paused` to resume.",
		dwellMinutes,
	)
	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput:   true,
		reactRocket:     true,
		removeAutoMerge: true,
	})
}
