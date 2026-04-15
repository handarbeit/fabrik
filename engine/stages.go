package engine

import (
	"errors"
	"fmt"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
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

func (e *Engine) handleStageComplete(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "done", "stage %q complete\n", stage.Name)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Clean up any failure label from a prior incomplete run.
	e.removeFailedLabel(owner, repo, item.Number, stage.Name)

	// Re-fetch labels so we see changes made while the stage was running
	// (e.g., fabrik:yolo added mid-run). The item snapshot from dispatch
	// time may be stale. On error, keep existing labels.
	if freshLabels, err := e.client.FetchLabels(e.cfg.Owner, e.cfg.Repo, item.Number); err == nil && len(freshLabels) > 0 {
		item.Labels = freshLabels
	}

	// yoloActive gates both PR merge and is the base for stage advancement.
	// stage.AutoAdvance overrides advancement only — not the merge decision.
	yoloActive := e.cfg.Yolo || hasYoloLabel(item)

	// cruiseActive mirrors yoloActive for advancement but skips the two
	// end-of-Validate actions (merge + Done advancement). When yolo is active,
	// cruise is suppressed so yolo always takes precedence.
	cruiseActive := !yoloActive && hasCruiseLabel(item)

	// Attempt PR merge after Validate when yolo is active.
	// This runs BEFORE adding the completion label so that on merge failure the
	// engine can retry Validate (itemNeedsWork skips stages with a complete label).
	if yoloActive && stage.Name == "Validate" {
		if err := e.attemptMergeOnValidate(item); err != nil {
			// Merge failed: post/pause already handled inside attemptMergeOnValidate.
			e.logf(item.Number, "warn", "PR not merged: %v\n", err)
			return
		}
	}

	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); err != nil {
		e.logf(item.Number, "warn", "could not add completion label: %v\n", err)
	}

	// fabrik:yolo or fabrik:cruise label overrides stage.AutoAdvance — if the user
	// explicitly labelled the issue, respect that over YAML config.
	shouldAdvance := yoloActive || cruiseActive
	if stage.AutoAdvance != nil && !hasYoloLabel(item) && !hasCruiseLabel(item) {
		shouldAdvance = *stage.AutoAdvance
	}

	// cruise stops at Validate: do not advance to Done or trigger any further
	// stage movement. The PR merge was already skipped (yoloActive is false).
	if cruiseActive && stage.Name == "Validate" {
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

// attemptMergeOnValidate finds the linked PR for item and merges it.
// Returns nil on success or when no PR exists (no-PR is logged and skipped).
// Returns an error that has been handled (comment+pause posted) on ErrNotMergeable.
func (e *Engine) attemptMergeOnValidate(item gh.ProjectItem) error {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	prNumber, err := e.client.FindPRForIssue(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "could not find PR for merge: %v\n", err)
		return nil
	}
	if prNumber == 0 {
		e.logf(item.Number, "warn", "no linked PR found at Validate completion; skipping auto-merge\n")
		return nil
	}

	if err := e.client.MergePR(owner, repo, prNumber); err != nil {
		if errors.Is(err, gh.ErrNotMergeable) {
			msg := fmt.Sprintf("🏭 **Fabrik — auto-merge skipped**\n\nAuto-merge skipped: PR #%d is not mergeable (GitHub reports a merge conflict or the mergeable status is not yet computed). Please resolve any conflicts and merge manually.", prNumber)
			if dbID, cerr := e.client.AddComment(owner, repo, item.Number, msg); cerr != nil {
				e.logf(item.Number, "warn", "could not post unmergeable comment: %v\n", cerr)
			} else if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
				e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
			}
			if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); lerr != nil {
				e.logf(item.Number, "warn", "could not add fabrik:paused label: %v\n", lerr)
			}
			return fmt.Errorf("PR #%d not mergeable", prNumber)
		}
		// Other API errors (transient 5xx, permissions, etc.): log and return an
		// error so the caller skips the completion label and stage advancement.
		// The engine will retry Validate on the next cooldown cycle.
		e.logf(item.Number, "warn", "auto-merge of PR #%d failed: %v\n", prNumber, err)
		return fmt.Errorf("auto-merge failed: %w", err)
	}

	e.logf(item.Number, "info", "auto-merged PR #%d\n", prNumber)
	return nil
}

// handleDecomposed is called when a stage outputs the FABRIK_DECOMPOSED marker.
// It adds the stage completion label and moves the parent issue directly to Done
// on the project board, bypassing all remaining pipeline stages.
// This is only expected from the Plan stage when it splits an issue into sub-issues.
func (e *Engine) handleDecomposed(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "done", "stage %q decomposed issue into sub-issues — moving to Done\n", stage.Name)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Add the stage completion label so the engine won't re-run this stage on restart.
	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); err != nil {
		e.logf(item.Number, "warn", "could not add completion label: %v\n", err)
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

	e.logf(item.Number, "advance", "moving decomposed issue to Done\n")
	if err := e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID); err != nil {
		e.logf(item.Number, "warn", "could not move issue to Done: %v\n", err)
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
	return e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID)
}
