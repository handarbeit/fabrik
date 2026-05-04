package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// botRepromptedLabel is the idempotency guard for Phase 1 of the bot-reviewer
// escalation ladder and the timing anchor for Phase 2. Fixed label, not per-login,
// because the guard is bot-agnostic: Phase 1 fires once per gate cycle for all
// outstanding bots in one round. Must stay ≤50 chars (GitHub REST API limit).
const botRepromptedLabel = "fabrik:bot-reprompted"

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
// Bot-aware escalation ladder (when all outstanding reviewers are bots and
// item.LinkedPRNumber > 0):
//
//   - Phase 1 (fires at 1× ReviewWaitTimeout from fabrik:awaiting-review): sends
//     a formal re-request (DELETE+POST) and an @mention comment on the PR for
//     each unresponsive bot; applies fabrik:bot-reprompted label; returns
//     (true, false) — still blocked.
//   - Phase 2 (fires at 1× ReviewWaitTimeout from fabrik:bot-reprompted):
//     removes fabrik:bot-reprompted and fabrik:awaiting-review, then returns
//     (false, true) so the caller fires pauseForReviewTimeout with a contextual
//     "re-prompt was already attempted" message.
//
// Mixed or pure-human reviewer paths are unchanged: pause at 1× ReviewWaitTimeout.
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
//   - Removes fabrik:bot-reprompted label if present (idempotent).
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

	// Determine if all outstanding reviewers are bots. Used by Phase 1/2 logic.
	allBots := len(outstanding) > 0
	for _, rr := range item.LinkedPRReviewRequests {
		if rr.Login != "" && !rr.IsBot {
			allBots = false
			break
		}
	}

	// Find the fabrik:bot-reprompted label (idempotency guard for Phase 1 and
	// timing anchor for Phase 2).
	var reprompted bool
	for _, l := range item.Labels {
		if l == botRepromptedLabel {
			reprompted = true
			break
		}
	}

	// Phase 2 check: if a re-prompt was already sent and another full timeout
	// window has elapsed without response, pause for human.
	if reprompted && allBots {
		repromptedAt, err := e.client.FetchLabelAppliedAt(owner, repo, item.Number, botRepromptedLabel)
		if err != nil {
			e.logf(item.Number, "warn", "could not fetch bot-reprompted label timestamp: %v\n", err)
		} else if !repromptedAt.IsZero() {
			timeout := e.cfg.ReviewWaitTimeout
			if timeout <= 0 {
				timeout = 15 * time.Minute
			}
			if time.Since(repromptedAt) >= timeout {
				e.logf(item.Number, "review-gate", "phase 2: bot(s) unresponsive after re-prompt — pausing for human\n")
				// Remove fabrik:bot-reprompted and fabrik:awaiting-review.
				// item.Labels is the pre-cleanup snapshot so pauseForReviewTimeout
				// can still detect Phase 2 context from it after we return.
				for _, l := range item.Labels {
					if l == botRepromptedLabel || l == "fabrik:awaiting-review" {
						if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, l); err != nil {
							e.logf(item.Number, "warn", "phase 2: could not remove %s: %v\n", l, err)
						} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
							cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), l)
						}
					}
				}
				return false, true
			}
		}
	}

	// Still waiting. Check the fabrik:awaiting-review timeout.
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
				// 1× timeout elapsed.
				if allBots && item.LinkedPRNumber > 0 && !reprompted {
					// Phase 1: re-prompt all outstanding bot reviewers.
					var repromptedLogins []string
					for _, rr := range item.LinkedPRReviewRequests {
						if rr.Login == "" {
							continue
						}
						login := rr.Login
						if err := e.client.DeleteReviewRequest(owner, repo, item.LinkedPRNumber, []string{login}); err != nil {
							e.logf(item.Number, "warn", "phase 1: could not delete review request for %s: %v\n", login, err)
						}
						if err := e.client.AddReviewRequest(owner, repo, item.LinkedPRNumber, []string{login}); err != nil {
							e.logf(item.Number, "warn", "phase 1: could not re-add review request for %s: %v\n", login, err)
						}
						msg := fmt.Sprintf("🏭 **Fabrik — review re-prompt**\n\n@%s just checking in — could you take a look at this PR?", botMentionHandle(login))
						// no write-through: excluded — posts to item.LinkedPRNumber (PR comment thread, not issue cache)
						if dbID, err := e.client.AddComment(owner, repo, item.LinkedPRNumber, msg); err != nil {
							e.logf(item.Number, "warn", "phase 1: could not post re-prompt comment for %s: %v\n", login, err)
							// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
						} else if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
							e.logf(item.Number, "warn", "phase 1: could not add 🚀 to re-prompt comment: %v\n", reactErr)
						}
						repromptedLogins = append(repromptedLogins, login)
					}
					if err := e.client.AddLabelToIssue(owner, repo, item.Number, botRepromptedLabel); err != nil {
						e.logf(item.Number, "warn", "phase 1: could not add %s: %v\n", botRepromptedLabel, err)
					} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
						cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), botRepromptedLabel)
					}
					e.logf(item.Number, "review-gate", "phase 1: re-prompted bot reviewer(s): %s\n", strings.Join(repromptedLogins, ", "))
					return true, false
				}

				// If Phase 1 already fired (reprompted label present) and Phase 2
				// hasn't timed out yet, stay blocked and let Phase 2 handle it.
				if allBots && reprompted {
					break
				}

				// Mixed/pure-human or no PR number: existing pause behavior.
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
		} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-review")
		}
	}

	return true, false
}

// removeAwaitingReviewLabel removes fabrik:awaiting-review and the
// fabrik:bot-reprompted label if present on the item (gate-cycle cleanup).
func (e *Engine) removeAwaitingReviewLabel(owner, repo string, item gh.ProjectItem) {
	for _, l := range item.Labels {
		if l == "fabrik:awaiting-review" {
			if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:awaiting-review"); err != nil {
				e.logf(item.Number, "warn", "could not remove fabrik:awaiting-review label: %v\n", err)
			} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-review")
			}
		}
		if l == botRepromptedLabel {
			if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, l); err != nil {
				e.logf(item.Number, "warn", "could not remove %s label: %v\n", l, err)
			} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), l)
			}
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
// If item.Labels contains the fabrik:bot-reprompted label (the pre-cleanup snapshot
// captured before checkReviewGate removed it), Phase 2 context is detected and a
// more specific "after re-prompt" message is posted.
func (e *Engine) pauseForReviewTimeout(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	e.logf(item.Number, "review-timeout", "review wait timeout elapsed — pausing for human intervention\n")

	// Build pending-reviewer list with bot/human tags for the pause comment.
	var reviewerParts []string
	for _, rr := range item.LinkedPRReviewRequests {
		if rr.Login == "" {
			continue
		}
		tag := "human"
		if rr.IsBot {
			tag = "bot"
		}
		reviewerParts = append(reviewerParts, fmt.Sprintf("`%s` (%s)", rr.Login, tag))
	}

	// Detect Phase 2 context: checkReviewGate removed the label from GitHub but
	// item.Labels is the pre-cleanup snapshot, so the label is still present here.
	// Derive bot logins from LinkedPRReviewRequests (bots haven't responded, so
	// they're still in the requests list).
	var repromptedLogins []string
	for _, l := range item.Labels {
		if l == botRepromptedLabel {
			for _, rr := range item.LinkedPRReviewRequests {
				if rr.IsBot && rr.Login != "" {
					repromptedLogins = append(repromptedLogins, rr.Login)
				}
			}
			break
		}
	}

	var msg string
	if len(repromptedLogins) > 0 {
		// Phase 2 message: re-prompt was already sent but bot didn't respond.
		botList := strings.Join(repromptedLogins, ", ")
		prRef := ""
		if item.LinkedPRNumber > 0 {
			prRef = fmt.Sprintf("PR #%d", item.LinkedPRNumber)
		} else {
			prRef = "the linked PR"
		}
		msg = fmt.Sprintf(
			"🏭 **Fabrik — review wait timeout (after bot re-prompt)**\n\n"+
				"The review gate for stage **%s** timed out waiting for %s (bot). "+
				"A re-prompt was sent, but no review was submitted in the additional waiting window.\n\n"+
				"Fabrik has paused this issue. To resume, either:\n"+
				"- (a) post a review on %s yourself,\n"+
				"- (b) remove `wait_for_reviews: true` from the %s stage YAML if bot reviews are unreliable on this repo,\n"+
				"- (c) merge %s manually, or\n"+
				"- (d) remove `fabrik:paused` to let the engine cycle through another re-prompt + wait.",
			stage.Name, botList, prRef, stage.Name, prRef,
		)
	} else {
		// Standard timeout message with named reviewers.
		pendingLine := ""
		if len(reviewerParts) > 0 {
			pendingLine = "\n\nPending reviewers: " + strings.Join(reviewerParts, ", ")
		}
		msg = fmt.Sprintf(
			"🏭 **Fabrik — review wait timeout**\n\nThe review gate for stage **%s** timed out waiting for outstanding reviewers.%s\n\n"+
				"Fabrik has paused this issue. Please check the PR for pending reviews, address any issues, and then remove the `fabrik:paused` label to resume.",
			stage.Name, pendingLine,
		)
	}

	if dbID, err := e.client.AddComment(owner, repo, item.Number, msg); err != nil {
		e.logf(item.Number, "warn", "could not post review timeout comment: %v\n", err)
	} else {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
				DatabaseID: dbID, Body: msg, Author: e.cfg.User, CreatedAt: time.Now(),
			})
		}
		// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
		if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
			e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
		}
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:paused: %v\n", err)
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:paused")
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:awaiting-input: %v\n", err)
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-input")
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

		var usage TokenUsage
		var completed, blocked bool
		if snap, snapErr := e.store.Get(itemRepo, item.Number); snapErr == nil {
			st := snap.State()
			usage = st.LastTokenUsage
			completed = st.LastInvocationCompleted
			blocked = st.LastInvocationBlocked
		}
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
	} else {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
				DatabaseID: dbID, Body: msg, Author: e.cfg.User, CreatedAt: time.Now(),
			})
		}
		// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
		if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
			e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
		}
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:paused: %v\n", err)
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:paused")
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:awaiting-input: %v\n", err)
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-input")
	}
}

// botMentionHandle maps copilot-* logins to "copilot" — GitHub's canonical mention surface for the reviewer bot.
func botMentionHandle(login string) string {
	if strings.HasPrefix(strings.ToLower(login), "copilot") {
		return "copilot"
	}
	return login
}
