package engine

import (
	"fmt"
	"time"

	"github.com/handarbeit/fabrik/internal/itemstate"

	gh "github.com/handarbeit/fabrik/github"
)

// archiveEligibleAtCooldownReason is the itemstate.CooldownAt reason key used to
// cache the computed "archive eligible at" time (stage-complete label's
// FetchLabelAppliedAt timestamp + ArchiveAfter) for a Done item, so
// FetchLabelAppliedAt — a full issue-events REST page-through, not cheap — is
// called at most once per item per engine lifetime (twice across a restart),
// rather than once per poll for every item still waiting out its grace period.
const archiveEligibleAtCooldownReason = "archive-eligible-at"

// settleArchiveDoneItems is the per-poll settle scan that archives board items
// once they have been visibly settled in the Done (cleanup) column for at least
// ArchiveAfter (default 24h). It re-implements the deliberately-disabled
// archiveDoneCompleteItems (removed dead by #1025) with corrected timing: the
// original archived Done items immediately, so work appeared to vanish the
// moment it completed; this version anchors the grace period to the GitHub-side,
// restart-safe stage:<Done>:complete label-applied timestamp (FetchLabelAppliedAt),
// following the same idiom as the CI-wait, review-wait, and convergence-budget
// gates (engine/ci.go, engine/reviews.go, engine/merge_gate.go). See ADR-068.
//
// Sourced directly from board.Items, not deepFetchCandidates, for the same
// reason as settleClosedItemsToDone and the other settle scans: once a Done
// item's worktree is removed, it never passes itemMayNeedWork's dispatch guard
// and is never deep-fetched again (engine/item.go worktreeExistsForItem). Done
// items must stay cheap to skip (requirement 9 / ADR-021's housekeeping-mutation
// exemption test), so this scan never deep-fetches and never requires a durable
// marker beyond the CooldownAt cache described above.
func (e *Engine) settleArchiveDoneItems(board *gh.ProjectBoard) {
	if e.cfg.ArchiveDone == "off" {
		return
	}
	cleanup := cleanupStage(e.cfg)
	if cleanup == nil {
		return
	}
	completeLabel := fmt.Sprintf("stage:%s:complete", cleanup.Name)

	for _, item := range board.Items {
		if item.Status != cleanup.Name {
			continue
		}
		if !hasLabel(item.Labels, completeLabel) {
			continue
		}
		e.maybeArchiveDoneItem(board, item, completeLabel)
	}
}

// maybeArchiveDoneItem evaluates and, if the grace period has elapsed, archives
// a single Done+complete item. The eligible-at time (label-applied-at +
// ArchiveAfter) is computed once via FetchLabelAppliedAt and cached in
// itemstate.CooldownAt; subsequent polls just compare time.Now() against the
// cached value until it expires, bounding the REST cost of the timing check.
func (e *Engine) maybeArchiveDoneItem(board *gh.ProjectBoard, item gh.ProjectItem, completeLabel string) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoStr := itemOwnerRepoString(item, e.defaultRepo())

	eligibleAt, cached := e.archiveEligibleAt(owner, repo, repoStr, item, completeLabel)
	if !cached {
		return
	}
	if time.Now().Before(eligibleAt) {
		return
	}

	e.logf(item.Number, "archive", "Done item settled since %s — archiving\n", completeLabel)
	if err := e.client.ArchiveProjectItem(board.ProjectID, item.ItemID); err != nil {
		e.logf(item.Number, "warn", "archive-done: could not archive item: %v — will retry next poll\n", err)
		return
	}
	if c := e.cache(); c != nil {
		c.RemoveItem(item.ItemID)
	}
	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEchoIfSubscribed("projects_v2_item", "archived", item.ItemID)
	}
}

// archiveEligibleAt returns the cached or freshly-fetched "archive eligible at"
// time for item, and whether one is available yet. On a CooldownAt cache miss,
// it calls FetchLabelAppliedAt once; a zero result (label-applied timestamp not
// found — FetchLabelAppliedAt's deliberate fail-open contract) is treated as
// "not yet known" and is not cached, so the next poll retries rather than
// permanently skipping the item or archiving it prematurely (requirement 7).
func (e *Engine) archiveEligibleAt(owner, repo, repoStr string, item gh.ProjectItem, completeLabel string) (time.Time, bool) {
	if snap, err := e.store.Get(repoStr, item.Number); err == nil {
		if at := snap.CooldownAt(archiveEligibleAtCooldownReason); !at.IsZero() {
			return at, true
		}
	}

	appliedAt, err := e.client.FetchLabelAppliedAt(owner, repo, item.Number, completeLabel)
	if err != nil {
		e.logf(item.Number, "warn", "archive-done: could not fetch %s label timestamp: %v\n", completeLabel, err)
		return time.Time{}, false
	}
	if appliedAt.IsZero() {
		// Not found (yet) — retry next poll without caching a bogus eligible-at time.
		return time.Time{}, false
	}

	eligibleAt := appliedAt.Add(e.cfg.ArchiveAfter)
	e.store.Apply(itemstate.CooldownRecorded{
		Repo:   repoStr,
		Number: item.Number,
		Reason: archiveEligibleAtCooldownReason,
		Until:  eligibleAt,
	})
	return eligibleAt, true
}
