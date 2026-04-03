package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

func (e *Engine) findNewComments(item gh.ProjectItem) []gh.Comment {
	var newComments []gh.Comment
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range item.Comments {
		// Only process comments from the configured user
		if c.Author != e.cfg.User {
			continue
		}
		// Skip comments we've already processed
		key := fmt.Sprintf("%d-comment-%s", item.Number, c.ID)
		if _, seen := e.processedSet[key]; seen {
			continue
		}
		// Skip comments that look like Fabrik output
		if strings.HasPrefix(c.Body, "🏭 **Fabrik") {
			continue
		}
		// Skip comments already processed (marked with 🚀 reaction)
		if c.HasReaction("ROCKET") {
			continue
		}
		newComments = append(newComments, c)
	}
	return newComments
}

// findStageComment finds the most recent comment for a given stage in the
// provided comments slice. Matches on the stage comment header prefix so it
// works for both issue comments and PR comments (FromPR > 0).
func findStageComment(comments []gh.Comment, stageName string) *gh.Comment {
	header := fmt.Sprintf("🏭 **Fabrik — stage: %s**", stageName)
	var found *gh.Comment
	for i := range comments {
		if strings.HasPrefix(comments[i].Body, header) {
			c := comments[i]
			found = &c
		}
	}
	return found
}

// collectStageComments gathers stage comment bodies for context files.
// When includeCurrent is false, only stages prior to currentStage are included.
// When includeCurrent is true, the current stage is also included.
func (e *Engine) collectStageComments(item gh.ProjectItem, currentStage *stages.Stage, includeCurrent bool) map[string]string {
	result := make(map[string]string)
	for _, s := range e.cfg.Stages {
		if s.CleanupWorktree {
			continue
		}
		var include bool
		if includeCurrent {
			include = s.Order <= currentStage.Order
		} else {
			include = s.Order < currentStage.Order
		}
		if !include {
			continue
		}
		if c := findStageComment(item.Comments, s.Name); c != nil {
			result[s.Name] = c.Body
		}
	}
	return result
}

// processComments handles new user comments on an issue.
// Flow: 👀 reactions → editing label → write context files → invoke Claude →
// rewrite stage comment (UpdateComment or fallback AddComment) → post acknowledgement
// → remove editing label → 🚀 reactions
func (e *Engine) processComments(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, comments []gh.Comment) error {
	e.logf(item.Number, "comments", "processing %d new comment(s) — stage: %s\n",
		len(comments), stage.Name)

	// Step 1: React with 👀 to all new comments
	for _, c := range comments {
		if err := e.client.AddCommentReaction(e.cfg.Owner, e.cfg.Repo, c.DatabaseID, "eyes"); err != nil {
			e.logf(item.Number, "warn", "could not add 👀 to comment %s: %v\n", c.ID, err)
		}
	}

	// Step 2: Add editing label
	if err := e.client.AddLabelToIssue(e.cfg.Owner, e.cfg.Repo, item.Number, "fabrik:editing"); err != nil {
		return fmt.Errorf("adding editing label: %w", err)
	}

	// Step 3: Ensure worktree
	baseBranch := e.worktrees.DefaultBaseBranch()
	workDir, err := e.worktrees.EnsureWorktree(item.Number, baseBranch, false)
	if err != nil {
		e.removeEditingLabel(item.Number)
		return fmt.Errorf("setting up worktree: %w", err)
	}

	// Step 4: Write context files (prior stages + current stage)
	stageComments := e.collectStageComments(item, stage, true)
	prBody := ""
	if stage.PostToPR {
		if prNum, prErr := e.client.FindPRForIssue(e.cfg.Owner, e.cfg.Repo, item.Number); prErr == nil && prNum > 0 {
			prBody, _ = e.client.GetIssueBody(e.cfg.Owner, e.cfg.Repo, prNum)
		}
	}
	if err := writeContextFiles(workDir, item.Body, stageComments, prBody); err != nil {
		e.logf(item.Number, "warn", "could not write context files: %v\n", err)
	}

	// Step 5: Invoke Claude with the comment review prompt
	modelOverride := e.extractModelOverride(item.Number, item.Labels)
	if modelOverride != "" {
		e.logf(item.Number, "model", "using model override %q\n", modelOverride)
	}
	output, _, usage, err := InvokeClaudeForComments(ctx, stage, item, comments, workDir, modelOverride, e.cfg.NoTmux || stage.NoTmux)
	if e.cfg.DebugOutput {
		saveDebugLog(item.Number, stage.Name+"-comment-review", output)
	}
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.totalTokens = e.totalTokens.add(usage)
	}()
	if err != nil {
		e.removeEditingLabel(item.Number)
		if ctx.Err() != nil {
			e.logf(item.Number, "skip", "cancelled during claude comment review\n")
			return nil
		}
		e.logf(item.Number, "warn", "claude comment review issue: %v\n", err)
		return err
	}

	// Capture git metadata for the comment header
	branch, commit, timestamp := captureGitMeta(workDir)

	// Step 6: Rewrite the stage comment with Claude's output
	if output != "" {
		footer := formatStatsFooter(usage, false)
		newCommentBody := formatOutputComment(stage.Name, output, footer, branch, commit, timestamp)
		existingComment := findStageComment(item.Comments, stage.Name)
		if existingComment != nil {
			e.logf(item.Number, "edit", "updating stage comment for %q\n", stage.Name)
			if err := e.client.UpdateComment(e.cfg.Owner, e.cfg.Repo, existingComment.DatabaseID, newCommentBody); err != nil {
				e.logf(item.Number, "warn", "could not update stage comment: %v\n", err)
			}
		} else {
			// No existing stage comment — create one
			targetNumber := item.Number
			if stage.PostToPR {
				if prNum, prErr := e.client.FindPRForIssue(e.cfg.Owner, e.cfg.Repo, item.Number); prErr == nil && prNum > 0 {
					targetNumber = prNum
				}
			}
			e.logf(item.Number, "edit", "no existing stage comment for %q, creating new\n", stage.Name)
			if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, targetNumber, newCommentBody); err != nil {
				e.logf(item.Number, "warn", "could not post stage comment: %v\n", err)
			}
		}
	}

	// Step 7: Post acknowledgement comment on the issue
	ackComment := fmt.Sprintf("🏭 **Fabrik**\n\nComment processed during **%s** stage. The stage comment has been updated.", stage.Name)
	if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, ackComment); err != nil {
		e.logf(item.Number, "warn", "could not post acknowledgement: %v\n", err)
	}

	// Step 8: Remove editing label
	e.removeEditingLabel(item.Number)

	// Step 9: React with 🚀 to all processed comments
	for _, c := range comments {
		if err := e.client.AddCommentReaction(e.cfg.Owner, e.cfg.Repo, c.DatabaseID, "rocket"); err != nil {
			e.logf(item.Number, "warn", "could not add 🚀 to comment %s: %v\n", c.ID, err)
		}
	}

	// Mark comments as processed only after everything succeeded
	e.markCommentsProcessed(item, comments)

	e.logf(item.Number, "done", "comment processing complete\n")
	return nil
}

// markCommentsProcessed records comments as processed so they won't be retried.
func (e *Engine) markCommentsProcessed(item gh.ProjectItem, comments []gh.Comment) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range comments {
		key := fmt.Sprintf("%d-comment-%s", item.Number, c.ID)
		e.processedSet[key] = time.Now()
	}
}
