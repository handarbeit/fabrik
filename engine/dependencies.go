package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/verveguy/fabrik/boardcache"
	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
	"github.com/verveguy/fabrik/tui"
)

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
// No-op (returns false immediately) for the first stage of the pipeline.
func (e *Engine) checkDependencies(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) bool {
	// First stage never blocked — it must always run to produce the spec.
	if stages.IsFirstStage(e.cfg.Stages, stage.Name) {
		return false
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	itemRepo := itemOwnerRepoString(item, e.defaultRepo())

	// Collect open (non-CLOSED) blocking dependencies.
	var openDeps []gh.Dependency
	for _, dep := range item.BlockedBy {
		if dep.State != "CLOSED" {
			openDeps = append(openDeps, dep)
		}
	}

	if len(openDeps) == 0 {
		// All dependencies resolved (or none exist) — remove fabrik:blocked if present.
		for _, l := range item.Labels {
			if l == "fabrik:blocked" {
				if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:blocked"); err != nil {
					e.logf(item.Number, "warn", "could not remove fabrik:blocked label: %v\n", err)
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
