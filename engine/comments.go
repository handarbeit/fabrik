package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func (e *Engine) findNewComments(item gh.ProjectItem) []gh.Comment {
	var newComments []gh.Comment
	e.mu.Lock()
	defer e.mu.Unlock()
	iKey := issueKey(item, e.defaultRepo())
	for _, c := range item.Comments {
		// Skip comments we've already processed
		key := iKey + "-comment-" + c.ID
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
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	iKey := issueKey(item, e.defaultRepo())

	// Merge any unresolved PR review thread comments into the working slice.
	// This ensures that when a user nudge arrives (e.g. "please address Copilot
	// feedback"), the review thread comments are processed alongside the
	// conversation comment without requiring a separate dispatchReviewReinvoke
	// cycle. For non-PR-backed items LinkedPRReviewThreadComments is empty, so
	// this is a no-op. For dispatchReviewReinvoke call sites the synthetic
	// comments were already filtered by buildReviewThreadComments, so the merge
	// adds nothing (ID dedup prevents duplicates).
	if len(item.LinkedPRReviewThreadComments) > 0 {
		existingIDs := make(map[string]bool, len(comments))
		for _, c := range comments {
			existingIDs[c.ID] = true
		}
		e.mu.Lock()
		for _, c := range item.LinkedPRReviewThreadComments {
			if existingIDs[c.ID] {
				continue
			}
			if c.HasReaction("ROCKET") {
				continue
			}
			if _, seen := e.processedSet[iKey+"-comment-"+c.ID]; seen {
				continue
			}
			comments = append(comments, c)
		}
		e.mu.Unlock()
	}

	e.logf(item.Number, "comments", "processing %d new comment(s) — stage: %s\n",
		len(comments), stage.Name)

	// Step 1: React with 👀 to all new comments. PR review thread (inline)
	// comments use a different REST endpoint than issue comments.
	for _, c := range comments {
		if c.DatabaseID == 0 {
			e.logf(item.Number, "debug", "skipping 👀 reaction for synthetic comment %s (no DatabaseID)\n", c.ID)
			continue
		}
		if c.ReviewThreadID != "" {
			if err := e.client.AddPRReviewCommentReaction(owner, repo, c.DatabaseID, "eyes"); err != nil {
				e.logf(item.Number, "warn", "could not add 👀 to review thread comment %s: %v\n", c.ID, err)
			}
		} else {
			if err := e.client.AddCommentReaction(owner, repo, c.DatabaseID, "eyes"); err != nil {
				e.logf(item.Number, "warn", "could not add 👀 to comment %s: %v\n", c.ID, err)
			}
		}
	}

	// Step 2: Add editing label
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:editing"); err != nil {
		return fmt.Errorf("adding editing label: %w", err)
	}

	// Step 3: Ensure worktree
	wm := e.worktreesFor(item.Repo)
	baseBranch, err := e.baseBranchForItem(item, wm)
	if err != nil {
		e.removeEditingLabel(owner, repo, item.Number)
		return fmt.Errorf("setting up worktree for %s/%s: %w", owner, repo, err)
	}
	workDir, err := wm.EnsureWorktree(item.Number, baseBranch, false)
	if err != nil {
		e.removeEditingLabel(owner, repo, item.Number)
		return fmt.Errorf("setting up worktree for %s/%s: %w", owner, repo, err)
	}

	// Write context files (all stages including current) before Claude runs.
	e.writeContextFiles(item, stage, workDir, true)

	// Step 4: Invoke Claude with the comment review prompt
	modelOverride := e.extractModelOverride(item.Number, item.Labels)
	if modelOverride != "" {
		e.logf(item.Number, "model", "using model override %q\n", modelOverride)
	}
	effortOverride := e.extractEffortOverride(item.Number, item.Labels)
	if effortOverride != "" {
		e.logf(item.Number, "effort", "using effort override %q\n", effortOverride)
	}
	output, _, usage, err := e.claude.InvokeForComments(ctx, stage, item, comments, workDir, InvokeOptions{ModelOverride: modelOverride, EffortOverride: effortOverride, BaseBranch: baseBranch})
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.totalTokens = e.totalTokens.add(usage)
		e.lastUsage[issueKey(item, e.defaultRepo())] = usage
	}()
	if err != nil {
		e.removeEditingLabel(owner, repo, item.Number)
		if ctx.Err() != nil {
			e.logf(item.Number, "skip", "cancelled during claude comment review\n")
			return nil
		}
		e.logf(item.Number, "warn", "claude comment review issue: %v\n", err)
		return err
	}

	// Capture git metadata for the comment header
	branch, commit, mainSHA, timestamp := captureGitMeta(workDir, baseBranch)

	// Step 5: Check for stage completion marker before stripping.
	// Capture summary before markers are stripped in-place below — once stripped,
	// extractSummary(output) returns "" and the Verification update is silently lost.
	completed := checkCompletion(stage, output)
	summary := extractSummary(output)

	// Step 6: Strip FABRIK_ISSUE_UPDATE block from output, then update issue body if allowed.
	if updatedBody := extractUpdatedBody(output); updatedBody != "" {
		if stage.UpdateIssueBody {
			e.logf(item.Number, "edit", "updating issue body\n")
			if err := e.client.UpdateIssueBody(owner, repo, item.Number, updatedBody); err != nil {
				e.logf(item.Number, "warn", "could not update issue body: %v\n", err)
			}
		} else {
			e.logf(item.Number, "warn", "stage %q comment processing produced FABRIK_ISSUE_UPDATE markers but is not allowed to update the issue body — ignoring\n", stage.Name)
		}
		output = stripMarkers(output, "FABRIK_ISSUE_UPDATE_BEGIN", "FABRIK_ISSUE_UPDATE_END")
	}

	// Step 7: Strip all Fabrik markers from output before posting.
	output = stripLine(output, "FABRIK_STAGE_COMPLETE")
	output = stripLine(output, "FABRIK_BLOCKED_ON_INPUT")
	output = stripLine(output, "FABRIK_DECOMPOSED")
	output = stripLine(output, "FABRIK_SUMMARY_BEGIN")
	output = stripLine(output, "FABRIK_SUMMARY_END")
	output = strings.TrimSpace(output)

	// Step 8: Rewrite or create the stage comment (unless post_to_pr).
	// For post_to_pr stages the stage output lives on the PR; comment processing
	// output on such stages is posted as a new comment on the issue as before.
	if output != "" {
		if stage.PostToPR {
			comment := formatOutputComment(stage.Name+" (comment review)", output, "", branch, commit, mainSHA, timestamp)
			if dbID, err := e.client.AddComment(owner, repo, item.Number, comment); err != nil {
				e.logf(item.Number, "warn", "could not post comment: %v\n", err)
			} else if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
				e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
			}
		} else {
			existing := findStageComment(item.Comments, stage.Name)
			stageComment := formatOutputComment(stage.Name, output, "", branch, commit, mainSHA, timestamp)
			if existing != nil {
				e.logf(item.Number, "edit", "rewriting stage comment for %s\n", stage.Name)
				if err := e.client.UpdateComment(owner, repo, existing.DatabaseID, stageComment); err != nil {
					e.logf(item.Number, "warn", "could not update stage comment: %v\n", err)
				}
			} else {
				if dbID, err := e.client.AddComment(owner, repo, item.Number, stageComment); err != nil {
					e.logf(item.Number, "warn", "could not post stage comment: %v\n", err)
				} else if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
					e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
				}
			}
		}
	}

	// Step 9: Remove editing label
	e.removeEditingLabel(owner, repo, item.Number)

	// Step 10: React with 🚀 to all processed comments and resolve any review
	// threads that were addressed.
	resolvedThreads := make(map[string]bool)
	for _, c := range comments {
		if c.DatabaseID == 0 {
			e.logf(item.Number, "debug", "skipping 🚀 reaction for synthetic comment %s (no DatabaseID)\n", c.ID)
			continue
		}
		if c.ReviewThreadID != "" {
			if err := e.client.AddPRReviewCommentReaction(owner, repo, c.DatabaseID, "rocket"); err != nil {
				e.logf(item.Number, "warn", "could not add 🚀 to review thread comment %s: %v\n", c.ID, err)
			}
			if !resolvedThreads[c.ReviewThreadID] {
				if err := e.client.ResolveReviewThread(c.ReviewThreadID); err != nil {
					e.logf(item.Number, "warn", "could not resolve review thread %s: %v\n", c.ReviewThreadID, err)
				} else {
					e.logf(item.Number, "review", "resolved review thread %s\n", c.ReviewThreadID)
				}
				resolvedThreads[c.ReviewThreadID] = true
			}
		} else {
			if err := e.client.AddCommentReaction(owner, repo, c.DatabaseID, "rocket"); err != nil {
				e.logf(item.Number, "warn", "could not add 🚀 to comment %s: %v\n", c.ID, err)
			}
		}
	}

	// Mark comments as processed only after everything succeeded
	e.markCommentsProcessed(item, comments)

	// Step 11: If comment processing resolved the stage, handle completion.
	// This avoids an unnecessary extra stage invocation after unblocking.
	if completed {
		e.logf(item.Number, "done", "comment processing completed stage %q\n", stage.Name)
		stageKey := iKey + "-" + stage.Name
		func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			delete(e.retryCount, stageKey)
			delete(e.pausedDueToRetries, stageKey)
			e.lastCompleted[iKey] = true
		}()
		var prNumber int
		if stage.CreateDraftPR {
			prNumber = e.ensureDraftPR(item, baseBranch)
			e.updatePRVerification(item, prNumber, summary)
		}
		if stage.MarkPRReadyOnComplete {
			e.markPRReady(item, prNumber)
		}
		e.handleStageComplete(board, item, stage)
	} else {
		e.logf(item.Number, "done", "comment processing complete\n")
	}

	return nil
}

// markCommentsSeenByStage adds a rocket reaction to any user comments that were
// present when a stage ran. item provides owner/repo/number context for API
// calls; preStageComments must be the snapshot captured before stage dispatch
// (item.Comments at dispatch time) — it must NOT be item.Comments from a
// re-fetch, as that would include comments that arrived during the run and were
// never processed by the stage.
func (e *Engine) markCommentsSeenByStage(item gh.ProjectItem, preStageComments []gh.Comment) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	iKey := issueKey(item, e.defaultRepo())
	for _, c := range preStageComments {
		if strings.HasPrefix(c.Body, "🏭 **Fabrik") {
			continue
		}
		if c.HasReaction("ROCKET") {
			continue
		}
		// This comment was seen by the stage — mark it so it won't trigger unblock
		if err := e.client.AddCommentReaction(owner, repo, c.DatabaseID, "rocket"); err != nil {
			e.logf(item.Number, "warn", "could not add rocket to seen comment %s: %v\n", c.ID, err)
		}
		e.mu.Lock()
		key := iKey + "-comment-" + c.ID
		e.processedSet[key] = time.Now()
		e.mu.Unlock()
	}
}

// markCommentsProcessed records comments as processed so they won't be retried.
func (e *Engine) markCommentsProcessed(item gh.ProjectItem, comments []gh.Comment) {
	iKey := issueKey(item, e.defaultRepo())
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range comments {
		key := iKey + "-comment-" + c.ID
		e.processedSet[key] = time.Now()
	}
}
