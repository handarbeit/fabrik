package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// hasYoloLabel reports whether item has the "fabrik:yolo" label.
func hasYoloLabel(item gh.ProjectItem) bool {
	for _, l := range item.Labels {
		if l == "fabrik:yolo" {
			return true
		}
	}
	return false
}

// hasCruiseLabel reports whether item has the "fabrik:cruise" label.
// cruise auto-advances through all stages but stops before auto-merging
// the PR or advancing to Done at Validate completion.
func hasCruiseLabel(item gh.ProjectItem) bool {
	for _, l := range item.Labels {
		if l == "fabrik:cruise" {
			return true
		}
	}
	return false
}

// hasUnrestrictedLabel reports whether item has the "fabrik:unrestricted" label,
// which tells the engine to pass --dangerously-skip-permissions to Claude Code.
func hasUnrestrictedLabel(item gh.ProjectItem) bool {
	for _, l := range item.Labels {
		if l == "fabrik:unrestricted" {
			return true
		}
	}
	return false
}

// stageIsGateChecked reports whether a stage gates completion on CI or reviews
// (wait_for_ci / wait_for_reviews). The Validate stage is the gate-checked stage
// in the default pipeline; the settle-owner (runValidatePRTerminalAdvance) and
// the closed-issue admit gates key on this property so a merged PR at a
// gate-checked stage is advanced/healed regardless of which gate label is set.
func stageIsGateChecked(stage *stages.Stage) bool {
	if stage == nil {
		return false
	}
	return (stage.WaitForCI != nil && *stage.WaitForCI) || (stage.WaitForReviews != nil && *stage.WaitForReviews)
}

func (e *Engine) handleStageComplete(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "done", "stage %q complete\n", stage.Name)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Clean up any failure label from a prior incomplete run.
	e.removeFailedLabel(owner, repo, item.Number, stage.Name)

	// Re-fetch labels so we see changes made while the stage was running
	// (e.g., fabrik:yolo added mid-run). Must bypass cache — the webhook for a
	// label added mid-run may not have been applied yet. On error, keep existing labels.
	if freshLabels, err := e.client.FetchLabels(e.cfg.Owner, e.cfg.Repo, item.Number); err == nil && len(freshLabels) > 0 {
		item.Labels = freshLabels
	}

	// Clear any orphaned fabrik:awaiting-input label. It is added by blockOnInput
	// when Claude emits FABRIK_BLOCKED_ON_INPUT; if the user manually removes
	// fabrik:paused (bypassing unblockAwaitingInput), the label can survive through
	// subsequent stage runs. Clean it up on every completion path.
	if hasLabel(item, "fabrik:awaiting-input") {
		if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil &&
			!errors.Is(err, gh.ErrNotFound) {
			e.logf(item.Number, "warn", "could not remove awaiting-input label: %v\n", err)
		} else if err == nil {
			if c := e.cache(); c != nil {
				c.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-input")
			}
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("issues", "unlabeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:awaiting-input")
			}
		}
	}

	// yoloActive gates both PR merge and is the base for stage advancement.
	// stage.AutoAdvance overrides advancement only — not the merge decision.
	yoloActive := e.cfg.Yolo || hasYoloLabel(item)

	// cruiseActive mirrors yoloActive for advancement but skips the two
	// end-of-Validate actions (merge + Done advancement). When yolo is active,
	// cruise is suppressed so yolo always takes precedence.
	cruiseActive := !yoloActive && hasCruiseLabel(item)

	// autoMergeEnabled is true when attemptMergeOnValidate successfully called
	// EnablePullRequestAutoMerge. Done advancement is deferred to the convergence
	// monitor (checkAutoMergeConvergence in the catch-up loop) in that case.
	autoMergeEnabled := false

	// Enable GitHub native auto-merge after Validate when yolo is active and
	// wait_for_ci is false. When wait_for_ci: true the merge path is handled by
	// checkCIGate in the catch-up loop (see ADR 032). This runs BEFORE adding the
	// completion label so that on failure the engine retries Validate (itemNeedsWork
	// skips stages with a complete label).
	waitForCI := stage.WaitForCI != nil && *stage.WaitForCI
	if yoloActive && stage.Name == "Validate" && !waitForCI {
		var mergeErr error
		autoMergeEnabled, mergeErr = e.attemptMergeOnValidate(ctx, board, item, stage)
		if mergeErr != nil {
			e.logf(item.Number, "warn", "PR not merged: %v\n", mergeErr)
			return
		}
	}

	// Conjunctive gate: for wait_for_ci stages, defer stage:X:complete until
	// the CI gate clears. Adding fabrik:awaiting-ci here (idempotent) keeps the
	// item durable in the "CI await" state so the dispatcher skips it and the
	// catch-up loop can evaluate CI on every poll (R1, R2, R3).
	if waitForCI {
		alreadyAwaitingCI := false
		for _, l := range item.Labels {
			if l == "fabrik:awaiting-ci" {
				alreadyAwaitingCI = true
				break
			}
		}
		if !alreadyAwaitingCI {
			if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-ci"); err != nil {
				e.logf(item.Number, "warn", "could not add fabrik:awaiting-ci label: %v\n", err)
			} else {
				if c := e.cache(); c != nil {
					c.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-ci")
				}
				if e.webhookMgr != nil {
					e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:awaiting-ci")
				}
			}
		}
		// fabrik:awaiting-review is NOT seeded here when wait_for_ci: true.
		// Path 2 (checkReviewGate in the catch-up loop) handles the review gate
		// after the CI gate clears and stage:X:complete is added (#617).
		e.logf(item.Number, "awaiting-ci", "deferring stage:%s:complete until CI gate clears\n", stage.Name)
		return // catch-up loop adds stage:X:complete when checkCIGate clears
	}

	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); err != nil {
		e.logf(item.Number, "warn", "could not add completion label: %v\n", err)
	} else {
		if c := e.cache(); c != nil {
			c.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), completeLabel)
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+completeLabel)
		}
		if stage.Name == "Validate" {
			repoStr := itemOwnerRepoString(item, e.defaultRepo())
			wm := e.worktreesFor(item.Repo)
			if sha, shaErr := gitRevParse(wm.WorktreeDir(item.Number), "HEAD"); shaErr == nil && sha != "" {
				e.store.Apply(itemstate.ValidateCompletedAtSHA{Repo: repoStr, Number: item.Number, SHA: sha})
				e.logf(item.Number, "validate-sha", "recorded completion SHA %s\n", sha)
			} else {
				e.logf(item.Number, "warn", "could not record completion SHA: %v\n", shaErr)
			}
		}
	}

	// fabrik:yolo or fabrik:cruise label overrides stage.AutoAdvance — if the user
	// explicitly labelled the issue, respect that over YAML config.
	shouldAdvance := yoloActive || cruiseActive
	if stage.AutoAdvance != nil && !hasYoloLabel(item) && !hasCruiseLabel(item) {
		shouldAdvance = *stage.AutoAdvance
	}

	// cruise stops at Validate: do not advance to Done or trigger any further
	// stage movement. cruise wins over yolo — when both labels are present,
	// the PR is left for human merge (FR-003, FR-015).
	if hasCruiseLabel(item) && stage.Name == "Validate" {
		shouldAdvance = false
	}

	// If GitHub auto-merge was just enabled, defer Done advancement to
	// checkAutoMergeConvergence in the catch-up loop. Done cleanup must not
	// run until the PR is actually merged by GitHub.
	if autoMergeEnabled {
		shouldAdvance = false
	}

	if shouldAdvance {
		if e.checkDependencies(board, item, stage) {
			return // blocked; checkDependencies handled label + comment
		}
		// Path 1: handleStageComplete always has stale review data because
		// reviewers are added only after MarkPRReady (which runs inside the
		// stage). Rather than re-fetching, we optimistically apply
		// fabrik:awaiting-review and let the catch-up loop (Path 2) make
		// the real gate decision with fresh FetchItemDetails data.
		if stage.WaitForReviews != nil && *stage.WaitForReviews {
			alreadyWaiting := false
			for _, l := range item.Labels {
				if l == "fabrik:awaiting-review" {
					alreadyWaiting = true
					break
				}
			}
			if !alreadyWaiting {
				if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-review"); err != nil {
					e.logf(item.Number, "warn", "could not add fabrik:awaiting-review label: %v\n", err)
				} else {
					if c := e.cache(); c != nil {
						c.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-review")
					}
					if e.webhookMgr != nil {
						e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:awaiting-review")
					}
				}
				e.logf(item.Number, "awaiting-review", "waiting for PR reviewers before advancing\n")
			}
			return // catch-up loop will advance once reviewers submit
		}
		if err := e.advanceToNextStage(board, item, stage); err != nil {
			e.logf(item.Number, "warn", "could not advance: %v\n", err)
		}
	} else {
		e.logf(item.Number, "wait", "waiting for human to advance\n")
	}
}

// attemptMergeOnValidate enables GitHub native auto-merge for the linked PR
// of a yolo issue at Validate completion. Returns (true, nil) when auto-merge
// was enabled (or is already enabled as an idempotency guard), (false, nil)
// when no action is needed (cruise label, no linked PR), and (false, err) on
// failure. The fabrik:auto-merge-enabled label serves as both the idempotency
// guard and the budget-start anchor read by checkAutoMergeConvergence.
func (e *Engine) attemptMergeOnValidate(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, _ *stages.Stage) (bool, error) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// cruise > yolo: when cruise is present, auto-merge is suppressed regardless of yolo.
	// cruise auto-advances through stages but leaves the PR for human merge at Validate.
	if hasCruiseLabel(item) {
		return false, nil
	}

	// Idempotency: auto-merge was already enabled on a prior run.
	if hasLabel(item, "fabrik:auto-merge-enabled") {
		return true, nil
	}

	// Merge-train gate: when merge_train: on, advance to Queued instead of enabling auto-merge.
	// Cruise items always bypass this (handled above). New items never reach fabrik:auto-merge-enabled
	// when merge_train: on, so this gate fires exactly once per qualifying Validate completion.
	if e.cfg.MergeTrain == "on" {
		return false, e.advanceToQueued(ctx, board, item, owner, repo)
	}

	pr, err := e.readClient.FetchLinkedPR(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "could not fetch linked PR for auto-merge: %v — will retry\n", err)
		return false, fmt.Errorf("fetch linked PR: %w", err)
	}
	if pr == nil {
		e.logf(item.Number, "warn", "no linked PR found at Validate completion; skipping auto-merge\n")
		return false, nil
	}

	// Enqueue path: when the repo requires a merge queue and merge-queue routing is not disabled.
	//
	// Merge-queue awareness audit (ADR-058 D3 FR-1): this early-return IS the guard for
	// the MergePR direct-merge fallback below. On a queue-enabled repo (default config),
	// the enqueue path returns here before MergePR is ever reached — a queued PR is never
	// directly merged (a direct merge on a queue-required branch returns HTTP 405). The
	// only way to reach the MergePR fallback on a queue repo is merge_queue: off (the
	// kill-switch), an operator's explicit choice. No separate guard is required at the
	// MergePR site; this early-return is the audit evidence.
	if e.cfg.MergeQueue != "off" && pr.IsMergeQueueEnabled {
		return e.enqueueForQueue(owner, repo, item, pr.Number, pr.HeadSHA)
	}

	strategy := e.cfg.AutoMergeStrategy
	if strategy == "" {
		strategy = "MERGE"
	}
	if err := e.client.EnablePullRequestAutoMerge(owner, repo, pr.Number, strategy); err != nil {
		if errors.Is(err, gh.ErrAutoMergeNotEnabled) {
			e.logf(item.Number, "warn", "auto-merge is not enabled for this repository — "+
				"enable it in Settings → General → Allow auto-merge; Fabrik will retry on the next poll\n")
			return false, fmt.Errorf("enabling auto-merge on PR #%d: %w", pr.Number, err)
		}
		// Any error other than ErrAutoMergeNotEnabled means the PR is in a terminal
		// GitHub state (CLEAN, UNSTABLE, or any future variant) where auto-merge cannot
		// be queued. Fall back to a direct merge call. If that also fails (e.g. DIRTY),
		// surface the MergePR error so existing rebase/CI-fix gates can act on it.
		e.logf(item.Number, "info", "PR #%d: enable auto-merge failed (%v) — falling back to direct merge\n", pr.Number, err)
		if mergeErr := e.client.MergePR(owner, repo, pr.Number); mergeErr != nil {
			return false, fmt.Errorf("direct merge fallback on PR #%d: %w", pr.Number, mergeErr)
		}
		// Apply idempotency guard and convergence anchor after successful direct merge.
		if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:auto-merge-enabled"); lerr != nil {
			e.logf(item.Number, "warn", "could not add fabrik:auto-merge-enabled label: %v\n", lerr)
		} else {
			if c := e.cache(); c != nil {
				c.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:auto-merge-enabled")
			}
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:auto-merge-enabled")
			}
		}
		e.logf(item.Number, "info", "PR #%d merged directly (auto-merge unavailable fallback)\n", pr.Number)
		return true, nil
	}

	// Apply fabrik:auto-merge-enabled as idempotency guard and budget-start anchor.
	if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:auto-merge-enabled"); lerr != nil {
		e.logf(item.Number, "warn", "could not add fabrik:auto-merge-enabled label: %v\n", lerr)
	} else {
		if c := e.cache(); c != nil {
			c.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:auto-merge-enabled")
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:auto-merge-enabled")
		}
	}

	e.logf(item.Number, "info", "GitHub auto-merge enabled on PR #%d (%s) — awaiting GitHub atomic merge\n", pr.Number, strategy)
	return true, nil
}

// enqueueForQueue enqueues a linked PR into the repository's native merge queue
// (ADR-058) and applies the fabrik:auto-merge-enabled label as both idempotency
// guard and the convergence anchor read by checkAutoMergeConvergence. It performs
// the boardcache write-through and webhook echo so the label is visible without a
// GitHub round-trip. Returns (true, nil) on successful enqueue, (false, err) if the
// enqueue mutation fails (a label-add failure is non-fatal: logged and swallowed).
//
// This is the ADR-058 enqueue path, extracted from attemptMergeOnValidate so it can
// be invoked from two convergence-owner call sites (ADR-059 D6 "invoke, don't
// relocate"): (1) attemptMergeOnValidate on the merge_train: off precedence path,
// and (2) handleMergeTrainBatch's per-repo engine selection for queue-enabled repos
// when merge_train: on. Both callers apply the MergeQueue != "off" && merge-queue-
// enabled guard before calling; the guard is deliberately kept at the call sites (it
// doubles as the HTTP-405 direct-merge audit evidence noted in attemptMergeOnValidate).
//
// The head SHA is passed by the caller from poll-native state (pr.HeadSHA or the
// GraphQL-populated item.LinkedPRHeadSHA) — never from a REST re-fetch of the queue
// flag, per ADR-058/ADR-059 FR-1.
func (e *Engine) enqueueForQueue(owner, repo string, item gh.ProjectItem, prNumber int, headSHA string) (bool, error) {
	if err := e.client.EnqueuePullRequest(owner, repo, prNumber, headSHA); err != nil {
		return false, fmt.Errorf("enqueue PR #%d: %w", prNumber, err)
	}
	if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:auto-merge-enabled"); lerr != nil {
		e.logf(item.Number, "warn", "could not add fabrik:auto-merge-enabled label: %v\n", lerr)
	} else {
		if c := e.cache(); c != nil {
			// Use the resolved owner/repo (not item.Repo, which may be empty when it
			// defaults to the default repo) so the cache key matches the stored entry
			// and the write-through actually lands on the right item — consistent with
			// the webhook-echo key below.
			c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), "fabrik:auto-merge-enabled")
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:auto-merge-enabled")
		}
	}
	e.logf(item.Number, "info", "PR #%d enqueued into merge queue — awaiting GitHub merge\n", prNumber)
	return true, nil
}

// handleNoWorkNeeded is called when a stage outputs both FABRIK_STAGE_COMPLETE and
// FABRIK_NO_WORK_NEEDED. It durably records the no-work-needed decision (the
// fabrik:awaiting-done marker) as its very first mutation — before any other API
// call — so a rate-limited or otherwise-failed invocation still leaves a trace that
// this issue's fate was decided, and does not silently fall back into the normal
// pipeline on a later poll (see adrs/ for the durable-marker rationale). The rest of
// the work (completion labels, skip comments, the Done move, and the issue close) is
// delegated to settleNoWorkNeeded, which is idempotent and safe to retry.
func (e *Engine) handleNoWorkNeeded(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "done", "stage %q signaled no work needed — skipping remaining stages and moving to Done\n", stage.Name)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	if !hasLabel(item, "fabrik:awaiting-done") {
		if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-done"); err != nil {
			e.logf(item.Number, "warn", "could not add awaiting-done marker: %v\n", err)
		} else {
			if c := e.cache(); c != nil {
				c.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-done")
			}
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:awaiting-done")
			}
		}
	}

	e.settleNoWorkNeeded(board, item, stage)
}

// holdingStage returns the first stage in cfg with HoldingStage: true, or nil if none.
func holdingStage(cfg Config) *stages.Stage {
	for _, s := range cfg.Stages {
		if s.HoldingStage {
			return s
		}
	}
	return nil
}

// cleanupStage returns the lowest-Order stage in cfg with CleanupWorktree: true,
// or nil if none is configured. Used by settleClosedItemsToDone to locate the
// Done column by its behavioral flag rather than a hardcoded stage name.
func cleanupStage(cfg Config) *stages.Stage {
	var cleanup *stages.Stage
	for _, s := range cfg.Stages {
		if s.CleanupWorktree && (cleanup == nil || s.Order < cleanup.Order) {
			cleanup = s
		}
	}
	return cleanup
}

// advanceToQueued moves an item from Validate to the configured holding stage column
// when merge_train: on. It sets the board status and adds stage:Validate:complete
// so the Phase 2 catch-up loop does not re-dispatch Validate on the next poll.
func (e *Engine) advanceToQueued(_ context.Context, board *gh.ProjectBoard, item gh.ProjectItem, owner, repo string) error {
	hs := holdingStage(e.cfg)
	if hs == nil {
		return fmt.Errorf("merge_train: on but no holding stage (HoldingStage: true) configured in stages")
	}
	if e.statusField == nil {
		return fmt.Errorf("status field metadata not available; cannot advance to %s", hs.Name)
	}

	optionID, ok := e.statusField.Options[hs.Name]
	if !ok {
		return fmt.Errorf("no status option %q found on project board (available: %v); is the %s column missing?",
			hs.Name, mapKeys(e.statusField.Options), hs.Name)
	}

	e.logf(item.Number, "merge-train", "advancing to %s for merge train batching\n", hs.Name)
	if err := e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID); err != nil {
		return fmt.Errorf("move to %s: %w", hs.Name, err)
	}
	if c := e.cache(); c != nil {
		c.UpdateItemStatus(boardcache.ItemKey(item.Repo, item.Number), hs.Name)
	}
	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEchoIfSubscribed("projects_v2_item", "edited", item.ItemID)
	}

	// Add stage:Validate:complete so the Phase 2 catch-up loop does not re-dispatch Validate.
	completeLabel := "stage:Validate:complete"
	if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); lerr != nil {
		e.logf(item.Number, "warn", "advanced to %s but could not add %s label: %v — will retry on next poll\n", hs.Name, completeLabel, lerr)
		return fmt.Errorf("add %s label: %w", completeLabel, lerr)
	}
	if c := e.cache(); c != nil {
		c.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), completeLabel)
	}
	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+completeLabel)
	}

	e.logf(item.Number, "merge-train", "holding in %s — waiting for batch\n", hs.Name)
	return nil
}

func (e *Engine) advanceToNextStage(board *gh.ProjectBoard, item gh.ProjectItem, currentStage *stages.Stage) error {
	next := stages.NextStage(e.cfg.Stages, currentStage.Name)
	if next == nil {
		e.logf(item.Number, "info", "completed all stages\n")
		return nil
	}

	if e.statusField == nil {
		return fmt.Errorf("status field metadata not available")
	}

	optionID, ok := e.statusField.Options[next.Name]
	if !ok {
		return fmt.Errorf("no status option %q found on project board (available: %v)",
			next.Name, mapKeys(e.statusField.Options))
	}

	e.logf(item.Number, "advance", "moving to stage %q\n", next.Name)
	// write-through: already covered by c.UpdateItemStatus call in the else block below
	err := e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID)
	if err == nil {
		if c := e.cache(); c != nil {
			c.UpdateItemStatus(boardcache.ItemKey(item.Repo, item.Number), next.Name)
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEchoIfSubscribed("projects_v2_item", "edited", item.ItemID)
		}
	}
	return err
}
