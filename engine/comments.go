package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
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

	itemRepo := itemOwnerRepoString(item, e.defaultRepo())
	startedAt := time.Now()
	e.emitStructural(tui.JobStartedEvent{
		IssueNumber: item.Number,
		Repo:        itemRepo,
		Title:       item.Title,
		StageName:   stage.Name,
		IsComment:   true,
		StartedAt:   startedAt,
	})
	defer e.emitStructural(tui.JobCompletedEvent{
		IssueNumber: item.Number,
		Repo:        itemRepo,
		Title:       item.Title,
		StageName:   stage.Name,
		IsComment:   true,
		Skipped:     true,
	})

	// Step 1: React with 👀 to all new comments.
	e.acknowledgeComments(owner, repo, item.Number, comments)

	// Step 2: Add editing label
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:editing"); err != nil {
		return fmt.Errorf("adding editing label: %w", err)
	} else {
		e.syncLabelAdd(item, "fabrik:editing", true)
	}

	// Step 3: Ensure worktree
	wm := e.worktreesFor(item.Repo)
	baseBranch, err := e.baseBranchForItem(item, wm)
	if err != nil {
		e.removeEditingLabel(owner, repo, item.Number)
		return fmt.Errorf("setting up worktree for %s/%s: %w", owner, repo, err)
	}
	// Merge-queue awareness (ADR-058 D3): skip the preemptive rebase when the PR is
	// in the queue (FR-1) or the repo is queue-enabled (FR-2). Both ProjectItem-sourced
	// signals are false-by-default, preserving legacy behavior on non-queue repos (FR-3).
	skipUpdate := prInMergeQueue(item) || e.suppressPreemptiveRebase(item)
	workDir, err := wm.EnsureWorktree(item.Number, baseBranch, skipUpdate)
	if err != nil {
		e.removeEditingLabel(owner, repo, item.Number)
		return fmt.Errorf("setting up worktree for %s/%s: %w", owner, repo, err)
	}

	// If a PR exists and its base branch doesn't match the resolved base, update it.
	e.syncPRBase(item, baseBranch)
	e.ensureEnvExcluded(item.Number, workDir)
	e.symlinkEnvIfEnabled(item.Number, workDir)

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

	output, usage, invCompleted, err := e.runCommentExtensionLoop(ctx, stage, &item, comments, workDir, invokeOpts, hadExtendTurnsLabel)

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
	// Honor FABRIK_STAGE_COMPLETE consistently with stage runs: invCompleted is the
	// invoke layer's marker-based completion (engine/claude.go), which already treats
	// the marker as authoritative even when the process exits non-zero (e.g. a timeout
	// kill after the stage finished) and withholds completion on engine shutdown. A
	// non-zero exit does NOT veto completion; it is recorded separately via Errored so
	// the error is still visible in history (JobCompletedEvent.Success=false).
	completed := invCompleted
	e.store.Apply(itemstate.InvocationRecorded{
		Repo:      itemOwnerRepoString(item, e.defaultRepo()),
		Number:    item.Number,
		Completed: completed,
		Errored:   err != nil,
		Usage:     usage,
		IsComment: true,
		Duration:  time.Since(startedAt),
	})
	// Bail early ONLY if the stage did not complete. If FABRIK_STAGE_COMPLETE was
	// emitted before the process exited non-zero (e.g. a timeout kill after the stage
	// finished, or trailing work that ended non-zero), proceed with the completion path
	// exactly like a stage run — a non-zero exit must not silently swallow a real
	// completion. The error is already recorded via Errored above. On engine shutdown,
	// invCompleted is false (see engine/claude.go), so that case still bails here.
	if err != nil && !completed {
		e.removeEditingLabel(owner, repo, item.Number)
		if ctx.Err() != nil {
			e.logf(item.Number, "skip", "cancelled during claude comment review\n")
			return nil
		}
		e.logf(item.Number, "warn", "claude comment review issue: %v\n", err)
		return err
	}
	if err != nil {
		e.logf(item.Number, "warn", "claude comment review exited with error but stage completed (marker found) — proceeding: %v\n", err)
	}

	summary := e.publishCommentOutput(owner, repo, item, stage, comments, output, workDir, baseBranch)

	e.finalizeComments(ctx, board, item, stage, comments, owner, repo, baseBranch, completed, summary)

	return nil
}

// acknowledgeComments reacts with 👀 to all new comments. PR review thread
// (inline) comments use a different REST endpoint than issue comments.
func (e *Engine) acknowledgeComments(owner, repo string, itemNumber int, comments []gh.Comment) {
	for _, c := range comments {
		if c.DatabaseID == 0 {
			e.logf(itemNumber, "debug", "skipping 👀 reaction for synthetic comment %s (no DatabaseID)\n", c.ID)
			continue
		}
		if c.ReviewThreadID != "" {
			// no write-through: excluded — AddPRReviewCommentReaction does not affect dispatch-relevant cache state
			if err := e.client.AddPRReviewCommentReaction(owner, repo, c.DatabaseID, "eyes"); err != nil {
				e.logf(itemNumber, "warn", "could not add 👀 to review thread comment %s: %v\n", c.ID, err)
			}
		} else {
			// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
			if err := e.client.AddCommentReaction(owner, repo, c.DatabaseID, "eyes"); err != nil {
				e.logf(itemNumber, "warn", "could not add 👀 to comment %s: %v\n", c.ID, err)
			}
		}
	}
}

// runCommentExtensionLoop determines the initial turn budget (label absent →
// MaxTurnsOverride=0, using commentMaxTurns naturally; label present → 2× the
// pre-granted budget, no progress check for the first hit), then invokes
// Claude for comment review, extending the turn budget while fabrik:extend-turns
// is present and progress is detected, up to a hard cap of 3× commentMaxTurns
// across all invocations. InvokeForComments resumes the existing session
// internally on each extension. Unlike the stage path, comment-review
// extension is intentionally label-gated: no silent budget expansion without
// opt-in. item is a pointer because a mid-loop progress check may re-fetch it
// (see detectProgress) — the caller observes the refreshed item afterward.
func (e *Engine) runCommentExtensionLoop(ctx context.Context, stage *stages.Stage, item *gh.ProjectItem, comments []gh.Comment, workDir string, invokeOpts InvokeOptions, hadExtendTurnsLabel bool) (output string, usage TokenUsage, completed bool, err error) {
	base := commentMaxTurns(stage)
	firstBudget := 0
	totalMultiple := 1
	if hadExtendTurnsLabel && base > 0 {
		firstBudget = 2 * base
		totalMultiple = 2
	}
	baseline := snapshotBaseline(stage, *item, workDir)

	currentBudget := firstBudget
	for {
		invokeOpts.MaxTurnsOverride = currentBudget
		var invOutput string
		var invUsage TokenUsage
		invOutput, completed, invUsage, err = e.claude.InvokeForComments(ctx, stage, *item, comments, workDir, invokeOpts)
		output += invOutput
		usage = addTokenUsage(usage, invUsage)

		// hitLimit uses currentBudget > 0 (not base > 0) so that extension only fires
		// when fabrik:extend-turns is present.
		hitLimit := !completed && err == nil && currentBudget > 0 && invUsage.TurnsUsed >= currentBudget
		if !hitLimit || totalMultiple >= 3 {
			break
		}
		issueLogf := func(tag, format string, args ...any) {
			e.logf(item.Number, tag, format, args...)
		}
		hasProgress, progressErr := detectProgress(ctx, stage, item, baseline, workDir, e.client, issueLogf)
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
	return output, usage, completed, err
}

// publishCommentOutput captures the summary from output (before markers are
// stripped in-place below — once stripped, extractSummary(output) returns ""
// and the Verification update would be silently lost), applies any
// FABRIK_ISSUE_UPDATE_BEGIN/END issue-body update, strips Fabrik markers, and
// posts the stage comment — plus, for a review-reinvoke, a Fabrik-marked
// summary comment on the linked PR so reviewers can see at a glance that their
// feedback was addressed. Returns the extracted summary for the caller to pass
// to updatePRVerification on stage completion.
func (e *Engine) publishCommentOutput(owner, repo string, item gh.ProjectItem, stage *stages.Stage, comments []gh.Comment, output, workDir, baseBranch string) string {
	branch, commit, mainSHA, timestamp := captureGitMeta(workDir, baseBranch)

	summary := extractSummary(output)

	// Strip FABRIK_ISSUE_UPDATE block from output, then update issue body.
	if updatedBody := extractUpdatedBody(output); updatedBody != "" {
		e.logf(item.Number, "edit", "updating issue body\n")
		// no write-through: excluded — issue body is not read from cache for dispatch decisions
		if err := e.client.UpdateIssueBody(owner, repo, item.Number, updatedBody); err != nil {
			e.logf(item.Number, "warn", "could not update issue body: %v\n", err)
		} else if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "edited", boardcache.ItemKey(owner+"/"+repo, item.Number))
		}
		output = stripMarkers(output, "FABRIK_ISSUE_UPDATE_BEGIN", "FABRIK_ISSUE_UPDATE_END")
	}

	// Strip all Fabrik markers from output before posting.
	output = stripLine(output, "FABRIK_STAGE_COMPLETE")
	output = stripLine(output, "FABRIK_BLOCKED_ON_INPUT")
	output = stripLine(output, "FABRIK_NO_WORK_NEEDED")
	output = stripLine(output, "FABRIK_SUMMARY_BEGIN")
	output = stripLine(output, "FABRIK_SUMMARY_END")
	output = strings.TrimSpace(output)

	// Rewrite or create the stage comment (unless post_to_pr). For post_to_pr
	// stages the stage output lives on the PR; comment processing output on
	// such stages is posted as a new comment on the issue as before.
	if output != "" {
		if stage.PostToPR {
			comment := formatOutputComment(stage.Name+" (comment review)", output, "", branch, commit, mainSHA, timestamp)
			e.postItemComment(item, comment, true)
		} else {
			existing := findStageComment(item.Comments, stage.Name)
			stageComment := formatOutputComment(stage.Name, output, "", branch, commit, mainSHA, timestamp)
			if existing != nil {
				e.logf(item.Number, "edit", "rewriting stage comment for %s\n", stage.Name)
				if err := e.client.UpdateComment(owner, repo, existing.DatabaseID, stageComment); err != nil {
					e.logf(item.Number, "warn", "could not update stage comment: %v\n", err)
				}
			} else {
				e.postItemComment(item, stageComment, true)
			}
		}
	}

	// When this is a review-reinvoke (all comments are PR inline review thread
	// comments), also post a Fabrik-marked summary on the linked PR. The
	// existing issue comment above is unchanged (R4). Gate: output != "" and a
	// linked PR exists. No post_to_pr check — linked-PR existence is the only
	// gate (R5).
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
				if e.webhookMgr != nil {
					e.webhookMgr.RegisterEcho("issue_comment", "created", boardcache.ItemKey(owner+"/"+repo, prNumber))
				}
				e.logf(item.Number, "post", "review feedback summary posted to PR #%d (%d thread(s))\n", prNumber, len(threads))
			}
		} else {
			e.logf(item.Number, "warn", "review reinvoke: no linked PR found — skipping PR summary comment\n")
		}
	}

	return summary
}

// finalizeComments removes the editing label, reacts with 🚀 to all processed
// comments (resolving any addressed review threads), marks the comments as
// processed so they won't be retried, and — if comment processing resolved
// the stage — creates/marks-ready the draft PR and advances to the next
// stage. This avoids an unnecessary extra stage invocation after unblocking.
func (e *Engine) finalizeComments(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, comments []gh.Comment, owner, repo, baseBranch string, completed bool, summary string) {
	e.removeEditingLabel(owner, repo, item.Number)

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

	if completed {
		e.logf(item.Number, "done", "comment processing completed stage %q\n", stage.Name)
		repoStr := itemOwnerRepoString(item, e.defaultRepo())
		e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		e.store.Apply(itemstate.EngineUnpaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		var prNumber int
		if stage.CreateDraftPR {
			// Error is intentionally ignored here — comment processing implies the stage
			// already advanced; a PR creation failure here is non-fatal for this path.
			prNumber, _ = e.ensureDraftPR(item, baseBranch)
			e.updatePRVerification(item, prNumber, summary)
		}
		if stage.MarkPRReadyOnComplete {
			e.markPRReady(item, prNumber)
		}
		e.handleStageComplete(ctx, board, item, stage)
	} else {
		e.logf(item.Number, "done", "comment processing complete\n")
	}
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
