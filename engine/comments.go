package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/verveguy/fabrik/boardcache"
	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/internal/itemstate"
	"github.com/verveguy/fabrik/stages"
)

func (e *Engine) findNewComments(item gh.ProjectItem) []gh.Comment {
	var newComments []gh.Comment
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	snap, _ := e.store.Get(repoStr, item.Number)
	for _, c := range item.Comments {
		// Skip comments we've already processed
		if !snap.CommentProcessed(c.ID).IsZero() {
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
func (e *Engine) processComments(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, comments []gh.Comment, onPIDReady ...func(int)) error {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

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
		repoStr := itemOwnerRepoString(item, e.defaultRepo())
		snap, _ := e.store.Get(repoStr, item.Number)
		for _, c := range item.LinkedPRReviewThreadComments {
			if existingIDs[c.ID] {
				continue
			}
			if c.HasReaction("ROCKET") {
				continue
			}
			if !snap.CommentProcessed(c.ID).IsZero() {
				continue
			}
			comments = append(comments, c)
		}
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
			// no write-through: excluded — AddPRReviewCommentReaction does not affect dispatch-relevant cache state
			if err := e.client.AddPRReviewCommentReaction(owner, repo, c.DatabaseID, "eyes"); err != nil {
				e.logf(item.Number, "warn", "could not add 👀 to review thread comment %s: %v\n", c.ID, err)
			}
		} else {
			// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
			if err := e.client.AddCommentReaction(owner, repo, c.DatabaseID, "eyes"); err != nil {
				e.logf(item.Number, "warn", "could not add 👀 to comment %s: %v\n", c.ID, err)
			}
		}
	}

	// Step 2: Add editing label
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:editing"); err != nil {
		return fmt.Errorf("adding editing label: %w", err)
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:editing")
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

	// If a PR exists and its base branch doesn't match the resolved base, update it.
	e.syncPRBase(item, baseBranch)

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
	invokeOpts := InvokeOptions{ModelOverride: modelOverride, EffortOverride: effortOverride, BaseBranch: baseBranch}
	if len(onPIDReady) > 0 && onPIDReady[0] != nil {
		invokeOpts.OnPIDReady = onPIDReady[0]
	}

	// Snapshot extend-turns label before loop (stable across any mid-loop FetchItemDetails re-fetch).
	hadExtendTurnsLabel := hasExtendTurnsLabel(item)

	// Determine initial turn budget. Label absent → MaxTurnsOverride=0 (InvokeClaudeForComments uses
	// commentMaxTurns naturally). Label present → 2× pre-granted budget (no progress check for first hit).
	base := commentMaxTurns(stage)
	firstBudget := 0
	totalMultiple := 1
	if hadExtendTurnsLabel && base > 0 {
		firstBudget = 2 * base
		totalMultiple = 2
	}
	baseline := snapshotBaseline(stage, item, workDir)

	// Extension loop: InvokeForComments resumes the existing session internally.
	// Hard cap is 3× commentMaxTurns across all invocations.
	var output string
	var usage TokenUsage
	currentBudget := firstBudget
	for {
		invokeOpts.MaxTurnsOverride = currentBudget
		var invOutput string
		var invCompleted bool
		var invUsage TokenUsage
		invOutput, invCompleted, invUsage, err = e.claude.InvokeForComments(ctx, stage, item, comments, workDir, invokeOpts)
		output += invOutput
		usage = addTokenUsage(usage, invUsage)

		// hitLimit uses currentBudget > 0 (not base > 0) so that extension only fires
		// when fabrik:extend-turns is present. Unlike the stage path, comment-review
		// extension is intentionally label-gated: no silent budget expansion without opt-in.
		hitLimit := !invCompleted && err == nil && currentBudget > 0 && invUsage.TurnsUsed >= currentBudget
		if !hitLimit || totalMultiple >= 3 {
			break
		}
		issueLogf := func(tag, format string, args ...any) {
			e.logf(item.Number, tag, format, args...)
		}
		hasProgress, progressErr := detectProgress(ctx, stage, &item, baseline, workDir, e.client, issueLogf)
		if progressErr != nil {
			e.logf(item.Number, "extend-turns", "comment progress check failed: %v\n", progressErr)
			break
		}
		if !hasProgress {
			break
		}
		totalMultiple++
		currentBudget = base
		e.logf(item.Number, "extend-turns", "extending comment review to %d× budget (%d turns used)\n", totalMultiple, usage.TurnsUsed)
	}
	// Report cumulative budget across all extensions.
	usage.MaxTurns = totalMultiple * base

	if usage.TurnsUsed > 0 || usage.InputTokens > 0 || usage.OutputTokens > 0 {
		if usage.MaxTurns > 0 {
			e.logf(item.Number, "stats", "used %d/%d turns, %dk input / %dk output tokens\n",
				usage.TurnsUsed, usage.MaxTurns, usage.InputTokens/1000, usage.OutputTokens/1000)
		} else {
			e.logf(item.Number, "stats", "used %d turns, %dk input / %dk output tokens\n",
				usage.TurnsUsed, usage.InputTokens/1000, usage.OutputTokens/1000)
		}
	}
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.totalTokens = addTokenUsage(e.totalTokens, usage)
	}()
	e.store.Apply(itemstate.InvocationRecorded{
		Repo:      itemOwnerRepoString(item, e.defaultRepo()),
		Number:    item.Number,
		Usage:     usage,
		IsComment: true,
	})
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

	// Step 6: Strip FABRIK_ISSUE_UPDATE block from output, then update issue body.
	if updatedBody := extractUpdatedBody(output); updatedBody != "" {
		e.logf(item.Number, "edit", "updating issue body\n")
		// no write-through: excluded — issue body is not read from cache for dispatch decisions
		if err := e.client.UpdateIssueBody(owner, repo, item.Number, updatedBody); err != nil {
			e.logf(item.Number, "warn", "could not update issue body: %v\n", err)
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
			} else {
				if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
					cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
						DatabaseID: dbID, Body: comment, Author: e.cfg.User, CreatedAt: time.Now(),
					})
				}
				// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
				if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
					e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
				}
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
				} else {
					if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
						cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
							DatabaseID: dbID, Body: stageComment, Author: e.cfg.User, CreatedAt: time.Now(),
						})
					}
					// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
					if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
						e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
					}
				}
			}
		}
	}

	// Step 8b: When this is a review-reinvoke (all comments are PR inline review
	// thread comments), also post a Fabrik-marked summary on the linked PR so
	// reviewers can see at a glance that their feedback was addressed. The
	// existing issue comment from Step 8 is unchanged (R4). Gate: output != ""
	// and a linked PR exists. No post_to_pr check — linked-PR existence is the
	// only gate (R5).
	if isReviewReinvoke(comments) && output != "" {
		prNumber, prErr := e.client.FindPRForIssue(owner, repo, item.Number)
		if prErr != nil {
			e.logf(item.Number, "warn", "review reinvoke: could not find PR for issue: %v\n", prErr)
		} else if prNumber > 0 {
			threads := buildThreadEntries(comments)
			prComment := formatReviewFeedbackComment(stage.Name, output, branch, commit, mainSHA, timestamp, threads, len(comments))
			// no write-through: excluded — posts to prNumber (PR comment thread, not issue cache)
			if _, err := e.client.AddComment(owner, repo, prNumber, prComment); err != nil {
				e.logf(item.Number, "warn", "could not post review feedback summary to PR #%d: %v\n", prNumber, err)
			} else {
				e.logf(item.Number, "post", "review feedback summary posted to PR #%d (%d thread(s))\n", prNumber, len(threads))
			}
		} else {
			e.logf(item.Number, "warn", "review reinvoke: no linked PR found — skipping PR summary comment\n")
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
			// no write-through: excluded — AddPRReviewCommentReaction does not affect dispatch-relevant cache state
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
			// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
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
		repoStr := itemOwnerRepoString(item, e.defaultRepo())
		e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		e.store.Apply(itemstate.EngineUnpaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		e.store.Apply(itemstate.InvocationRecorded{
			Repo:      itemOwnerRepoString(item, e.defaultRepo()),
			Number:    item.Number,
			Completed: true,
			Usage:     usage,
			IsComment: true,
		})
		var prNumber int
		if stage.CreateDraftPR {
			prNumber = e.ensureDraftPR(item, baseBranch)
			e.updatePRVerification(item, prNumber, summary)
		}
		if stage.MarkPRReadyOnComplete {
			e.markPRReady(item, prNumber)
		}
		e.handleStageComplete(ctx, board, item, stage)
	} else {
		e.logf(item.Number, "done", "comment processing complete\n")
	}

	return nil
}

// isReviewReinvoke reports whether this processComments invocation originated
// from a review-reinvoke dispatch (i.e., all comments are PR inline review
// thread comments). Returns false for an empty slice.
func isReviewReinvoke(comments []gh.Comment) bool {
	if len(comments) == 0 {
		return false
	}
	for _, c := range comments {
		if c.ReviewThreadID == "" {
			return false
		}
	}
	return true
}

// markCommentsSeenByStage adds a rocket reaction to any user comments that were
// present when a stage ran. item provides owner/repo/number context for API
// calls; preStageComments must be the snapshot captured before stage dispatch
// (item.Comments at dispatch time) — it must NOT be item.Comments from a
// re-fetch, as that would include comments that arrived during the run and were
// never processed by the stage.
func (e *Engine) markCommentsSeenByStage(item gh.ProjectItem, preStageComments []gh.Comment) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	for _, c := range preStageComments {
		if strings.HasPrefix(c.Body, "🏭 **Fabrik") {
			continue
		}
		if c.HasReaction("ROCKET") {
			continue
		}
		// This comment was seen by the stage — mark it so it won't trigger unblock
		// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
		if err := e.client.AddCommentReaction(owner, repo, c.DatabaseID, "rocket"); err != nil {
			e.logf(item.Number, "warn", "could not add rocket to seen comment %s: %v\n", c.ID, err)
		}
		e.store.Apply(itemstate.CommentProcessed{Repo: repoStr, Number: item.Number, CommentID: c.ID, At: time.Now()})
	}
}

// markCommentsProcessed records comments as processed so they won't be retried.
func (e *Engine) markCommentsProcessed(item gh.ProjectItem, comments []gh.Comment) {
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	for _, c := range comments {
		e.store.Apply(itemstate.CommentProcessed{Repo: repoStr, Number: item.Number, CommentID: c.ID, At: time.Now()})
	}
}
