package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

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
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-input")
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
				if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
					cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-ci")
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
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), completeLabel)
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
					if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
						cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-review")
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
func (e *Engine) attemptMergeOnValidate(_ context.Context, _ *gh.ProjectBoard, item gh.ProjectItem, _ *stages.Stage) (bool, error) {
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

	pr, err := e.readClient.FetchLinkedPR(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "could not fetch linked PR for auto-merge: %v — will retry\n", err)
		return false, fmt.Errorf("fetch linked PR: %w", err)
	}
	if pr == nil {
		e.logf(item.Number, "warn", "no linked PR found at Validate completion; skipping auto-merge\n")
		return false, nil
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
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:auto-merge-enabled")
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
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:auto-merge-enabled")
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:auto-merge-enabled")
		}
	}

	e.logf(item.Number, "info", "GitHub auto-merge enabled on PR #%d (%s) — awaiting GitHub atomic merge\n", pr.Number, strategy)
	return true, nil
}

// handleNoWorkNeeded is called when a stage outputs both FABRIK_STAGE_COMPLETE and
// FABRIK_NO_WORK_NEEDED. It marks the emitting stage complete, adds dummy
// stage:<name>:complete labels for all subsequent non-cleanup stages (with a one-line
// "skipped" comment per stage), and moves the issue directly to Done without creating
// a PR. This is the canonical path when Plan (or any stage) determines that no code
// or documentation changes are required.
func (e *Engine) handleNoWorkNeeded(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "done", "stage %q signaled no work needed — skipping remaining stages and moving to Done\n", stage.Name)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Clear any orphaned fabrik:awaiting-input label (same rationale as handleStageComplete).
	if hasLabel(item, "fabrik:awaiting-input") {
		if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil &&
			!errors.Is(err, gh.ErrNotFound) {
			e.logf(item.Number, "warn", "could not remove awaiting-input label: %v\n", err)
		} else if err == nil {
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-input")
			}
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("issues", "unlabeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:awaiting-input")
			}
		}
	}

	// Mark the emitting stage complete so the engine doesn't re-run it on restart.
	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); err != nil {
		e.logf(item.Number, "warn", "could not add completion label for stage %q: %v\n", stage.Name, err)
	} else {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), completeLabel)
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

	// Find the order boundary for the cleanup (Done) stage.
	doneOrder := math.MaxInt
	for _, s := range e.cfg.Stages {
		if s.CleanupWorktree && s.Order < doneOrder {
			doneOrder = s.Order
		}
	}

	// Add dummy completion labels and "skipped" comments for all subsequent non-cleanup stages.
	// The comment body must start with the canonical "🏭 **Fabrik" prefix so findNewComments
	// dedup prevents Fabrik from processing its own output on the next poll.
	skippedComment := fmt.Sprintf("🏭 **Fabrik — skipped: no work needed**\n\n_Skipped: no work needed (FABRIK_NO_WORK_NEEDED emitted by %s)._", stage.Name)
	for _, s := range e.cfg.Stages {
		if s.Order <= stage.Order || s.Order >= doneOrder {
			continue
		}
		skipLabel := fmt.Sprintf("stage:%s:complete", s.Name)
		if err := e.client.AddLabelToIssue(owner, repo, item.Number, skipLabel); err != nil {
			e.logf(item.Number, "warn", "could not add skip label for stage %q: %v\n", s.Name, err)
		} else {
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), skipLabel)
			}
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+skipLabel)
			}
		}
		// Post the "skipped" comment — no rocket reaction, this is engine-generated metadata.
		if dbID, err := e.client.AddComment(owner, repo, item.Number, skippedComment); err != nil {
			e.logf(item.Number, "warn", "could not post skipped comment for stage %q: %v\n", s.Name, err)
		} else {
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
					DatabaseID: dbID, Body: skippedComment, Author: e.cfg.User, CreatedAt: time.Now(),
				})
			}
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("issue_comment", "created", boardcache.ItemKey(owner+"/"+repo, item.Number))
			}
		}
	}

	if e.statusField == nil {
		e.logf(item.Number, "warn", "status field metadata not available; cannot move to Done\n")
		return
	}

	optionID, ok := e.statusField.Options["Done"]
	if !ok {
		e.logf(item.Number, "warn", "no status option %q found on project board (available: %v); cannot move to Done\n",
			"Done", mapKeys(e.statusField.Options))
		return
	}

	e.logf(item.Number, "advance", "moving no-work-needed issue to Done\n")
	if err := e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID); err != nil {
		e.logf(item.Number, "warn", "could not move issue to Done: %v\n", err)
	} else {
		e.store.Apply(itemstate.StatusUpdateRecorded{Repo: item.Repo, Number: item.Number, At: time.Now()})
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.UpdateItemStatus(boardcache.ItemKey(item.Repo, item.Number), "Done")
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEchoIfSubscribed("projects_v2_item", "edited", item.ItemID)
		}
		// Close the GitHub issue so it mirrors the normal pipeline close-on-merge path.
		// No webhook echo registered: applyIssuesDelta's "closed" case never calls
		// matchEchoFn, so any registered echo would expire unused. The ApplyIssueClosed
		// write-through handles cache coherence immediately.
		if err := e.client.CloseIssue(owner, repo, item.Number); err != nil {
			e.logf(item.Number, "warn", "could not close issue (no work needed): %v\n", err)
		} else {
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyIssueClosed(boardcache.ItemKey(item.Repo, item.Number))
			}
			e.logf(item.Number, "done", "closed issue (no work needed)\n")
		}
	}
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
	// write-through: already covered by cacheImpl.UpdateItemStatus call in the else block below
	err := e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID)
	if err == nil {
		e.store.Apply(itemstate.StatusUpdateRecorded{Repo: item.Repo, Number: item.Number, At: time.Now()})
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.UpdateItemStatus(boardcache.ItemKey(item.Repo, item.Number), next.Name)
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEchoIfSubscribed("projects_v2_item", "edited", item.ItemID)
		}
	}
	return err
}
