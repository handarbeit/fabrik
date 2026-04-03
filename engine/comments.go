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

// processComments handles new user comments on an issue.
// Flow: 👀 reactions → editing label → invoke Claude → perform actions / update issue body → remove editing label → 🚀 reactions
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

	// Step 4: Invoke Claude with the comment review prompt
	modelOverride := e.extractModelOverride(item.Number, item.Labels)
	if modelOverride != "" {
		e.logf(item.Number, "model", "using model override %q\n", modelOverride)
	}
	output, _, usage, err := InvokeClaudeForComments(ctx, stage, item, comments, workDir, modelOverride)
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

	// Step 5: Parse the updated issue body from Claude's output and apply it
	if updatedBody := extractUpdatedBody(output); updatedBody != "" {
		e.logf(item.Number, "edit", "updating issue body\n")
		if err := e.client.UpdateIssueBody(e.cfg.Owner, e.cfg.Repo, item.Number, updatedBody); err != nil {
			e.logf(item.Number, "warn", "could not update issue body: %v\n", err)
		}
	} else {
		// No body update — post output as a comment instead
		if output != "" {
			comment := formatOutputComment(stage.Name+" (comment review)", output, "", branch, commit, timestamp)
			if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, comment); err != nil {
				e.logf(item.Number, "warn", "could not post comment: %v\n", err)
			}
		}
	}

	// Step 6: Remove editing label
	e.removeEditingLabel(item.Number)

	// Step 7: React with 🚀 to all processed comments
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
