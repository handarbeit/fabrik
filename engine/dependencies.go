package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/verveguy/fabrik/boardcache"
	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
	"github.com/verveguy/fabrik/tui"
)

// blockedLabelRetryDelay is the base delay for removeBlockedIfResolved retry backoff.
// Declared as a var so tests can set it to 0 to avoid sleeping.
var blockedLabelRetryDelay = 500 * time.Millisecond

// checkDependencies inspects item.BlockedBy and determines whether the issue
// is gated by unresolved dependencies.
//
// Returns true if the issue is blocked (one or more blocking issues are not
// CLOSED), false otherwise.
//
// Side effects when blocked:
//   - Logs a "blocked" message listing what is being waited for.
//   - If fabrik:blocked is not already on the issue, posts a comment and adds
//     the label (only on the first-time blocked → blocked transition).
//   - Emits an IssueBlockedEvent for the TUI.
//
// Side effects when unblocked:
//   - Removes the fabrik:blocked label if present (idempotent).
//
// Applies uniformly to every stage, including the first. When a user sets a
// blockedBy edge, they assert that the blocker's merged state matters to how
// this issue should be processed — even at specification time. (#473)
func (e *Engine) checkDependencies(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) bool {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	itemRepo := itemOwnerRepoString(item, e.defaultRepo())

	// Collect open (non-CLOSED) blocking dependencies.
	//
	// Prefer the engine Store's view of each blocker's IsClosed over dep.State
	// from the BlockedBy edge. The dep.State field comes from GitHub's GraphQL
	// blockedBy.nodes.state response and lags the actual issue close by seconds
	// (GitHub indexer lag). The Store's IsClosed flips immediately when the
	// per-poll Reconcile picks up the shallow board's updated state. Using the
	// Store as the source of truth here matches PushUnblockObserver.allBlockersClosed
	// and prevents the pull-path from re-blocking an issue that the push-path
	// has already correctly unblocked, just because the dep edge state is stale.
	var openDeps []gh.Dependency
	for _, dep := range item.BlockedBy {
		depRepo := dep.Repo
		if depRepo == "" {
			depRepo = itemRepo
		}
		// Store-preferred path: cache is authoritative for blocker IsClosed.
		if depSnap, snapErr := e.store.Get(depRepo, dep.Number); snapErr == nil {
			if depSnap.IsClosed() {
				continue // resolved per Store
			}
			openDeps = append(openDeps, dep)
			continue
		}
		// Fallback: no Store entry for this blocker — fall back to dep.State.
		if dep.State != "CLOSED" {
			openDeps = append(openDeps, dep)
		}
	}

	if len(openDeps) == 0 {
		// All dependencies resolved (or none exist) — remove fabrik:blocked if present.
		for _, l := range item.Labels {
			if l == "fabrik:blocked" {
				if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:blocked"); err != nil {
					if errors.Is(err, gh.ErrNotFound) {
						// Label already absent on GitHub — desired end state achieved; sync cache.
						if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
							cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:blocked")
						}
					} else {
						e.logf(item.Number, "warn", "could not remove fabrik:blocked label: %v\n", err)
					}
				} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
					cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:blocked")
				}
				break
			}
		}
		return false
	}

	// Build the "waiting for" list with appropriate formatting.
	waitingFor := make([]string, 0, len(openDeps))
	for _, dep := range openDeps {
		depRepo := dep.Repo
		if depRepo == "" || depRepo == itemRepo {
			waitingFor = append(waitingFor, fmt.Sprintf("#%d", dep.Number))
		} else {
			waitingFor = append(waitingFor, fmt.Sprintf("%s#%d", depRepo, dep.Number))
		}
	}

	e.logf(item.Number, "blocked", "waiting for %s to close\n", strings.Join(waitingFor, ", "))

	// Only post the comment and add the label on the first block transition.
	alreadyBlocked := false
	for _, l := range item.Labels {
		if l == "fabrik:blocked" {
			alreadyBlocked = true
			break
		}
	}
	if !alreadyBlocked {
		comment := fmt.Sprintf("🏭 **Fabrik — blocked on dependencies**\n\nWaiting for the following issues to close: %s", strings.Join(waitingFor, ", "))
		if dbID, err := e.client.AddComment(owner, repo, item.Number, comment); err != nil {
			e.logf(item.Number, "warn", "could not post blocked comment: %v\n", err)
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
		if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:blocked"); err != nil {
			e.logf(item.Number, "warn", "could not add fabrik:blocked label: %v\n", err)
		} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:blocked")
		}
	}

	e.emitStructural(tui.IssueBlockedEvent{
		IssueNumber: item.Number,
		Repo:        item.Repo,
		Title:       item.Title,
		StageName:   stage.Name,
		WaitingFor:  waitingFor,
	})

	return true
}

// removeBlockedIfResolved removes the fabrik:blocked label from the given issue
// when the push-based unblock path determines all blockers are resolved. It
// mirrors removeEditingLabel: 3 attempts, exponential backoff, ErrNotFound
// treated as success (idempotent), and cacheImpl write-through on success.
//
// Unlike checkDependencies this helper does NOT require a *stages.Stage and does
// NOT post a comment — it is a label-removal-only path for use from observers.
func (e *Engine) removeBlockedIfResolved(owner, repo string, issueNumber int) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := e.client.RemoveLabelFromIssue(owner, repo, issueNumber, "fabrik:blocked")
		if err == nil {
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, issueNumber), "fabrik:blocked")
			}
			return
		}
		if errors.Is(err, gh.ErrNotFound) {
			// Label already absent — treat as success and sync cache.
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, issueNumber), "fabrik:blocked")
			}
			return
		}
		if !isTransientError(err) {
			e.logf(issueNumber, "warn", "push-unblock: could not remove fabrik:blocked: %v\n", err)
			return
		}
		lastErr = err
		if attempt < maxAttempts-1 {
			delay := blockedLabelRetryDelay << attempt
			time.Sleep(delay)
		}
	}
	e.logf(issueNumber, "warn", "push-unblock: could not remove fabrik:blocked after %d attempts: %v\n", maxAttempts, lastErr)
}
