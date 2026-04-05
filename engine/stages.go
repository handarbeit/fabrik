package engine

import (
	"errors"
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
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

func (e *Engine) handleStageComplete(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "done", "stage %q complete\n", stage.Name)

	// Clean up any failure label from a prior incomplete run.
	e.removeFailedLabel(item.Number, stage.Name)

	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	if err := e.client.AddLabelToIssue(e.cfg.Owner, e.cfg.Repo, item.Number, completeLabel); err != nil {
		e.logf(item.Number, "warn", "could not add completion label: %v\n", err)
	}

	// yoloActive gates both PR merge and is the base for stage advancement.
	// stage.AutoAdvance overrides advancement only — not the merge decision.
	yoloActive := e.cfg.Yolo || hasYoloLabel(item)

	// Attempt PR merge after Validate when yolo is active.
	if yoloActive && stage.Name == "Validate" {
		if err := e.attemptMergeOnValidate(item); err != nil {
			// ErrNotMergeable: post comment and pause; no advance.
			e.logf(item.Number, "warn", "PR not merged: %v\n", err)
			return
		}
	}

	shouldAdvance := yoloActive
	if stage.AutoAdvance != nil {
		shouldAdvance = *stage.AutoAdvance
	}

	if shouldAdvance {
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
	prNumber, err := e.client.FindPRForIssue(e.cfg.Owner, e.cfg.Repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "could not find PR for merge: %v\n", err)
		return nil
	}
	if prNumber == 0 {
		e.logf(item.Number, "warn", "no linked PR found at Validate completion; skipping auto-merge\n")
		return nil
	}

	if err := e.client.MergePR(e.cfg.Owner, e.cfg.Repo, prNumber); err != nil {
		if errors.Is(err, gh.ErrNotMergeable) {
			msg := fmt.Sprintf("Auto-merge skipped: PR #%d is not mergeable (GitHub reports a merge conflict or the mergeable status is not yet computed). Please resolve any conflicts and merge manually.", prNumber)
			if cerr := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, msg); cerr != nil {
				e.logf(item.Number, "warn", "could not post unmergeable comment: %v\n", cerr)
			}
			if lerr := e.client.AddLabelToIssue(e.cfg.Owner, e.cfg.Repo, item.Number, "fabrik:paused"); lerr != nil {
				e.logf(item.Number, "warn", "could not add fabrik:paused label: %v\n", lerr)
			}
			return fmt.Errorf("PR #%d not mergeable", prNumber)
		}
		// Non-mergeable API error: log and continue (advance normally).
		e.logf(item.Number, "warn", "auto-merge of PR #%d failed: %v\n", prNumber, err)
		return nil
	}

	e.logf(item.Number, "info", "auto-merged PR #%d\n", prNumber)
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
	return e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID)
}
