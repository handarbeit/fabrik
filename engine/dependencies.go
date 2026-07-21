package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// blockedLabelRetryDelay is the base delay for removeBlockedIfResolved retry backoff.
// Declared as a var so tests can set it to 0 to avoid sleeping.
var blockedLabelRetryDelay = 500 * time.Millisecond

const blockedCommentPrefix = "🏭 **Fabrik — blocked on dependencies**"

// buildBlockedComment constructs the full body of the blocked-on-dependencies comment.
func buildBlockedComment(waitingFor []string) string {
	return fmt.Sprintf("%s\n\nWaiting for the following issues to close: %s",
		blockedCommentPrefix, strings.Join(waitingFor, ", "))
}

// findBlockedComment returns the most recent comment in comments that starts
// with the blocked-on-dependencies prefix and was authored by fabrikUser.
// Returns nil if no such comment exists.
func findBlockedComment(comments []gh.Comment, fabrikUser string) *gh.Comment {
	var found *gh.Comment
	for i := range comments {
		c := &comments[i]
		if c.Author == fabrikUser && strings.HasPrefix(c.Body, blockedCommentPrefix) {
			found = c
		}
	}
	return found
}

// detectCycle performs a bounded BFS from the current item's open blockers,
// checking whether the current item (identified by itemRepo/itemNumber) appears
// as a transitive blocker of any of its own blockers — i.e., a cycle exists.
// maxHops limits how deeply the BFS traverses to avoid expensive full-graph scans.
func detectCycle(store *itemstate.Store, itemRepo string, itemNumber int, openDeps []gh.Dependency, maxHops int) bool {
	type key struct {
		repo   string
		number int
	}
	visited := make(map[key]bool)
	queue := make([]gh.Dependency, len(openDeps))
	copy(queue, openDeps)

	for hop := 0; hop < maxHops && len(queue) > 0; hop++ {
		var next []gh.Dependency
		for _, dep := range queue {
			depRepo := dep.Repo
			if depRepo == "" {
				depRepo = itemRepo
			}
			k := key{depRepo, dep.Number}
			if visited[k] {
				continue
			}
			visited[k] = true

			// Look up this dep's own blockers from the store.
			snap, err := store.Get(depRepo, dep.Number)
			if err != nil {
				continue // dep not in store; skip
			}
			for _, transitive := range snap.State().BlockedBy {
				tRepo := transitive.Repo
				if tRepo == "" {
					tRepo = depRepo // same-repo transitive dep
				}
				// Cycle detected: the dep is itself blocked by the current item.
				if tRepo == itemRepo && transitive.Number == itemNumber {
					return true
				}
				next = append(next, transitive)
			}
		}
		queue = next
	}
	return false
}

// checkDependencies inspects item.BlockedBy and determines whether the issue
// is gated by unresolved dependencies.
//
// Returns true if the issue is blocked (one or more blocking issues are not
// CLOSED), false otherwise.
//
// Side effects when blocked:
//   - Logs a "blocked" message listing what is being waited for.
//   - If fabrik:blocked is not already on the issue, posts a comment and adds
//     the label (first-time block transition).
//   - If fabrik:blocked is already on the issue and the waitingFor list changed,
//     edits the existing blocked comment in-place (FR-016).
//   - Detects cyclic blockedBy relationships and surfaces them as a paused error
//     rather than deadlocking (FR-017).
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
				e.applyLabelRemove(item, "fabrik:blocked", false)
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

	alreadyBlocked := false
	for _, l := range item.Labels {
		if l == "fabrik:blocked" {
			alreadyBlocked = true
			break
		}
	}

	newComment := buildBlockedComment(waitingFor)

	if !alreadyBlocked {
		// Cycle detection: before blocking, check that we're not creating a deadlock.
		// A bounded BFS (4 hops) is sufficient to catch the most common misconfigurations.
		if detectCycle(e.store, itemRepo, item.Number, openDeps, 4) {
			e.logf(item.Number, "warn", "cycle detected in blockedBy graph — pausing issue\n")
			cycleMsg := fmt.Sprintf("🏭 **Fabrik — cycle detected**\n\nIssue #%d has a cyclic `blockedBy` dependency: it is waiting for issues that are themselves (transitively) waiting for this issue. Fabrik cannot make progress. Remove the cycle manually and then remove `fabrik:paused` to continue.", item.Number)
			e.postComment(item, cycleMsg, false, false) //nolint:errcheck // failure already logged by postComment
			e.applyLabelAdd(item, "fabrik:paused", false)
			return false
		}

		// First-time block: post the comment and add the label.
		e.postItemComment(item, newComment, true)
		e.applyLabelAdd(item, "fabrik:blocked", false)
	} else {
		// Already blocked: edit the existing comment in-place if the dep list changed.
		existing := findBlockedComment(item.Comments, e.cfg.User)
		if existing != nil && existing.Body != newComment {
			if err := e.client.UpdateComment(owner, repo, existing.DatabaseID, newComment); err != nil {
				e.logf(item.Number, "warn", "could not update blocked comment: %v\n", err)
			} else if c := e.cache(); c != nil {
				c.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
					DatabaseID: existing.DatabaseID, Body: newComment, Author: e.cfg.User, CreatedAt: time.Now(),
				})
			}
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
			if c := e.cache(); c != nil {
				c.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, issueNumber), "fabrik:blocked")
			}
			return
		}
		if errors.Is(err, gh.ErrNotFound) {
			// Label already absent — treat as success and sync cache.
			if c := e.cache(); c != nil {
				c.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, issueNumber), "fabrik:blocked")
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
