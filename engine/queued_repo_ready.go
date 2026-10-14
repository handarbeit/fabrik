package engine

import (
	"context"

	gh "github.com/handarbeit/fabrik/github"
)

// ensureQueuedBaseReposReady registers the WorktreeManager for every repo that has a
// base:-labelled Queued member and none registered yet (#2250). groupQueuedByRepoAndBase
// resolves such a member's base through the repo's WorktreeManager, and managers live
// only in memory: they are registered by ensureRepoReady when an item in the repo is
// dispatched for a stage. After a restart, a repo whose only open items are Queued has
// nothing to register it, so without this its base:-labelled members were skipped every
// poll for ever.
//
// It uses ensureRepoReady, the stage-dispatch path, so a missing bare clone is cloned and
// a clone failure pauses the member with the same comment. At most one call per repo per
// poll; an error is logged and the member stays excluded until the next poll.
func (e *Engine) ensureQueuedBaseReposReady(ctx context.Context, items []gh.ProjectItem, holdingStatus string) {
	for _, rg := range groupQueuedItemsByRepoUnordered(items, holdingStatus, e.defaultRepo()) {
		e.mu.Lock()
		_, registered := e.worktreeManagers[rg.repoKey]
		e.mu.Unlock()
		if registered {
			continue
		}
		for _, item := range rg.items {
			if !itemHasBaseLabel(item) {
				continue
			}
			if err := e.ensureRepoReady(ctx, item); err != nil {
				e.logf(item.Number, "merge-train", "could not register %s to resolve base: labels: %v — retrying next poll\n", rg.repoKey, err)
			}
			break
		}
	}
}
