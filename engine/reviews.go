package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
	"github.com/verveguy/fabrik/tui"
)

// checkReviewGate inspects item.LinkedPRReviewRequests and item.LinkedPRReviews
// to determine whether the review gate is blocking review reinvoke or stage
// advancement.
//
// This function is only called from the catch-up loop (Phase 1) in poll.go,
// where item.LinkedPRReviewRequests and item.LinkedPRReviews contain fresh
// data from FetchItemDetails. handleStageComplete (Path 1) always applies
// fabrik:awaiting-review directly because reviewer assignment happens after
// MarkPRReady, so data would be stale.
//
// The gate clears under either of these conditions:
//
//  1. No outstanding requested reviewers AND at least one review has been
//     submitted. This handles both the "requested reviewers finished" case
//     and the "bot reviewer self-submitted" case. Bots like Copilot and
//     Gemini do not use the requested-reviewer mechanism — they self-trigger
//     via webhooks when the PR is marked ready and only appear in
//     LinkedPRReviews (not LinkedPRReviewRequests). Waiting on the reviews
//     array is the signal that catches them.
//
// The gate stays closed (returning blocked=true) when:
//
//   - There are outstanding requested reviewers who haven't submitted, OR
//   - No reviews have been submitted yet (even with no requested reviewers —
//     bot reviewers are typically 30s–10m behind PR-ready).
//
// Returns (blocked, timedOut):
//   - (true, false)  — gate is blocking; advance should not proceed
//   - (false, false) — gate cleared naturally; advance may proceed
//   - (false, true)  — gate cleared due to timeout; caller should pause the issue
//
// Side effects when blocking:
//   - Logs a message listing why we're waiting.
//   - Adds fabrik:awaiting-review label on first block transition (idempotent).
//
// Side effects when unblocking (naturally or by timeout):
//   - Removes fabrik:awaiting-review label if present (idempotent).
func (e *Engine) checkReviewGate(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) (blocked, timedOut bool) {
	// Gate is opt-in — only active when wait_for_reviews: true.
	if stage.WaitForReviews == nil || !*stage.WaitForReviews {
		return false, false
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Outstanding requested reviewers (humans or bots using the formal
	// request mechanism). A dismissed review puts the reviewer back here;
	// if they're not here, they've finished.
	outstanding := make([]string, 0, len(item.LinkedPRReviewRequests))
	for _, rr := range item.LinkedPRReviewRequests {
		if rr.Login != "" {
			outstanding = append(outstanding, rr.Login)
		}
	}
	hasReviews := len(item.LinkedPRReviews) > 0

	// Gate clears when all outstanding requested reviewers have responded
	// AND at least one review exists. This catches both human reviewers who
	// submit formally and bot reviewers (Copilot, Gemini) who self-submit
	// without ever appearing in reviewRequests.
	if len(outstanding) == 0 && hasReviews {
		e.removeAwaitingReviewLabel(owner, repo, item)
		return false, false
	}

	// Still waiting. Check timeout before continuing to block.
	timeout := e.cfg.ReviewWaitTimeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	for _, l := range item.Labels {
		if l == "fabrik:awaiting-review" {
			appliedAt, err := e.client.FetchLabelAppliedAt(owner, repo, item.Number, "fabrik:awaiting-review")
			if err != nil {
				e.logf(item.Number, "warn", "could not fetch awaiting-review label timestamp: %v\n", err)
			} else if !appliedAt.IsZero() && time.Since(appliedAt) >= timeout {
				var reason string
				if len(outstanding) > 0 {
					reason = "pending reviewers: " + strings.Join(outstanding, ", ")
				} else {
					reason = "no reviews submitted yet (bots may not have responded)"
				}
				e.logf(item.Number, "warn", "review wait timeout elapsed; pausing issue — %s\n", reason)
				e.removeAwaitingReviewLabel(owner, repo, item)
				return false, true
			}
			break
		}
	}

	if len(outstanding) > 0 {
		e.logf(item.Number, "awaiting-review", "waiting for reviewers: %s\n", strings.Join(outstanding, ", "))
	} else {
		e.logf(item.Number, "awaiting-review", "waiting for initial review submission (no reviewers requested; bot reviewers may still be processing)\n")
	}

	// Apply label on first block transition.
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
	}

	return true, false
}

// removeAwaitingReviewLabel removes fabrik:awaiting-review if present on the item.
func (e *Engine) removeAwaitingReviewLabel(owner, repo string, item gh.ProjectItem) {
	for _, l := range item.Labels {
		if l == "fabrik:awaiting-review" {
			if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:awaiting-review"); err != nil {
				e.logf(item.Number, "warn", "could not remove fabrik:awaiting-review label: %v\n", err)
			}
			return
		}
	}
}

// buildReviewThreadComments returns the inline per-line comments from
// unresolved review threads on the linked PR that have not yet been addressed
// (no 🚀 reaction and not already present in processedSet). These are real
// GitHub comments with real DatabaseIDs, so the 👀/🚀 reaction-based dedup
// mechanism works normally and each thread comment only triggers processing once.
//
// The top-level review body (if any) is not included — only thread comments,
// which are what reviewers use to flag specific code issues. Reviews that
// submit only a top-level body with no inline comments (e.g., bare APPROVED)
// have nothing actionable to address.
//
// processedSet is checked as defense-in-depth for within-session races: if a
// comment was processed this session (markCommentsProcessed wrote its key) but
// the ROCKET reaction hasn't propagated from the API yet, we still skip it.
func (e *Engine) buildReviewThreadComments(item gh.ProjectItem) []gh.Comment {
	iKey := issueKey(item, e.defaultRepo())
	out := make([]gh.Comment, 0, len(item.LinkedPRReviewThreadComments))
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range item.LinkedPRReviewThreadComments {
		if c.HasReaction("ROCKET") {
			continue
		}
		if _, seen := e.processedSet[iKey+"-comment-"+c.ID]; seen {
			continue
		}
		out = append(out, c)
	}
	return out
}

// pauseForReviewTimeout pauses the issue when the review wait timeout elapses.
// It applies fabrik:paused + fabrik:awaiting-input and posts an explanatory comment.
func (e *Engine) pauseForReviewTimeout(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	e.logf(item.Number, "review-timeout", "review wait timeout elapsed — pausing for human intervention\n")

	msg := fmt.Sprintf(
		"🏭 **Fabrik — review wait timeout**\n\nThe review gate for stage **%s** timed out waiting for outstanding reviewers.\n\n"+
			"Fabrik has paused this issue. Please check the PR for pending reviews, address any issues, and then remove the `fabrik:paused` label to resume.",
		stage.Name,
	)
	if dbID, err := e.client.AddComment(owner, repo, item.Number, msg); err != nil {
		e.logf(item.Number, "warn", "could not post review timeout comment: %v\n", err)
	} else if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
		e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:paused: %v\n", err)
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:awaiting-input: %v\n", err)
	}
}

// dispatchReviewReinvoke spawns a goroutine to re-invoke the stage agent via
// processComments with synthetic review comments. It marks the item in-flight,
// acquires the semaphore, calls processComments, then releases both.
// This allows the catch-up loop to remain non-blocking while the Claude
// invocation runs asynchronously.
func (e *Engine) dispatchReviewReinvoke(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	iKey := issueKey(item, e.defaultRepo())
	syntheticComments := e.buildReviewThreadComments(item)
	if len(syntheticComments) == 0 {
		e.logf(item.Number, "review-reinvoke", "no review bodies to process; skipping re-invocation\n")
		return
	}

	// Mark in-flight to prevent the next poll cycle's dispatch loop from
	// double-dispatching this item while the goroutine is running.
	e.inFlight.Store(iKey, item.IsPR)
	e.wg.Add(1)

	itemRepo := itemOwnerRepoString(item, e.defaultRepo())

	go func() {
		defer e.wg.Done()
		defer e.inFlight.Delete(iKey)

		// Acquire semaphore slot (respects MaxConcurrent; blocks until available).
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			e.logf(item.Number, "review-reinvoke", "context cancelled before semaphore acquired\n")
			return
		}
		defer func() { <-e.sem }()

		// Ensure the repo's WorktreeManager is registered before processComments
		// tries to use it. Phase 1 of the catch-up loop dispatches reinvokes
		// independently of the main dispatch loop, so this may be the first time
		// we touch this repo in the current session — processItem's ensureRepoReady
		// call cannot be relied on here. Without this, processComments panics at
		// worktreesFor() on a freshly-started Fabrik.
		if err := e.ensureRepoReady(ctx, item); err != nil {
			if errors.Is(err, ErrSkipItem) {
				e.logf(item.Number, "review-reinvoke", "repo not ready (clone failed elsewhere), skipping reinvoke\n")
				return
			}
			e.logf(item.Number, "warn", "review reinvoke: ensureRepoReady failed: %v\n", err)
			return
		}

		startTime := time.Now()
		e.emitStructural(tui.JobStartedEvent{
			IssueNumber: item.Number,
			Repo:        itemRepo,
			Title:       item.Title,
			StageName:   stage.Name,
			IsComment:   true,
			StartedAt:   startTime,
		})

		e.logf(item.Number, "review-reinvoke", "re-invoking stage %q via comment processing with %d synthetic review comment(s)\n",
			stage.Name, len(syntheticComments))
		err := e.processComments(ctx, board, item, stage, syntheticComments)

		e.mu.Lock()
		usage := e.lastUsage[iKey]
		completed := e.lastCompleted[iKey]
		blocked := e.lastBlocked[iKey]
		delete(e.lastUsage, iKey)
		delete(e.lastCompleted, iKey)
		delete(e.lastBlocked, iKey)
		e.mu.Unlock()
		e.emitStructural(tui.JobCompletedEvent{
			IssueNumber:    item.Number,
			Repo:           itemRepo,
			Title:          item.Title,
			StageName:      stage.Name,
			StageModel:     stage.Model,
			IsComment:      true,
			Success:        err == nil,
			Completed:      completed,
			BlockedOnInput: blocked,
			Duration:       time.Since(startTime),
			CompletedAt:    time.Now(),
			TurnsUsed:      usage.TurnsUsed,
			MaxTurns:       usage.MaxTurns,
			CostUSD:        usage.CostUSD,
		})

		if err != nil {
			if ctx.Err() != nil {
				return // context cancelled; normal shutdown
			}
			e.logf(item.Number, "warn", "review re-invocation failed: %v\n", err)
		}
	}()
}

// pauseForReviewCycleLimit pauses the issue when the maximum review re-invocation
// cycle count is reached. It applies fabrik:paused + fabrik:awaiting-input and
// posts an explanatory comment.
func (e *Engine) pauseForReviewCycleLimit(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, cycleCount, maxCycles int) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	e.logf(item.Number, "review-cycles", "review cycle limit %d reached — pausing for human intervention\n", maxCycles)

	msg := fmt.Sprintf(
		"🏭 **Fabrik — review cycle limit reached**\n\nThe stage **%s** has been re-invoked to address PR review feedback %d time(s), "+
			"which has reached the maximum configured limit (`FABRIK_MAX_REVIEW_CYCLES=%d`).\n\n"+
			"This usually means a reviewer (bot or human) is repeatedly requesting changes after each fix. "+
			"Fabrik has paused this issue for human review. Once the review situation is resolved, "+
			"remove the `fabrik:paused` label to resume.",
		stage.Name, cycleCount, maxCycles,
	)
	if dbID, err := e.client.AddComment(owner, repo, item.Number, msg); err != nil {
		e.logf(item.Number, "warn", "could not post review cycle limit comment: %v\n", err)
	} else if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
		e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:paused: %v\n", err)
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:awaiting-input: %v\n", err)
	}
}
