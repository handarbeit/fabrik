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

// maxArchiveLabelFetchesPerPoll bounds how many FetchLabelAppliedAt calls
// (cache misses) settleArchiveDoneItems will issue within a single poll. Without
// a cap, the first poll after a restart — when the CooldownAt cache is empty —
// would fire one synchronous FetchLabelAppliedAt (a full issue-events REST
// page-through) per waiting Done item; on a bloated board (the exact scenario
// this feature targets) that's a burst of sequential REST calls blocking the
// poll loop, working against the API-cost goal this feature exists to serve.
// Items beyond the budget are simply left uncached and retried on the next
// poll — identical, safe fallback to the existing "FetchLabelAppliedAt returned
// zero" path, so this never causes early, stuck, or duplicate archival, only a
// spread-out one. Var, not const, so tests can lower it. See ADR-068.
var maxArchiveLabelFetchesPerPoll = 20

// settleArchiveDoneItems is the per-poll settle scan that archives board items
// once they have been visibly settled in the Done (cleanup) column for at least
// ArchiveAfter (default 168h = 1 week). It re-implements the deliberately-disabled
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
//
// cleanupStage(e.cfg) resolves only the single lowest-Order CleanupWorktree
// stage (same helper settleClosedItemsToDone uses) — a board configured with a
// second cleanup-marked column would have items sitting there silently never
// evaluated for archival. This mirrors an existing, accepted assumption shared
// with settleClosedItemsToDone; Fabrik's stage config convention is a single
// terminal Done/cleanup stage.
func (e *Engine) settleArchiveDoneItems(board *gh.ProjectBoard) {
	if e.cfg.ArchiveDone == "off" {
		return
	}
	cleanup := cleanupStage(e.cfg)
	if cleanup == nil {
		return
	}
	completeLabel := fmt.Sprintf("stage:%s:complete", cleanup.Name)
	fetchBudget := maxArchiveLabelFetchesPerPoll

	for _, item := range board.Items {
		if item.Status != cleanup.Name {
			continue
		}
		if !hasLabel(item.Labels, completeLabel) {
			continue
		}
		e.maybeArchiveDoneItem(board, item, completeLabel, &fetchBudget)
	}
}

// maybeArchiveDoneItem evaluates and, if the grace period has elapsed, archives
// a single Done+complete item. The eligible-at time (label-applied-at +
// ArchiveAfter) is computed once via FetchLabelAppliedAt and cached in
// itemstate.CooldownAt; subsequent polls just compare time.Now() against the
// cached value until it expires, bounding the REST cost of the timing check.
// fetchBudget bounds the number of FetchLabelAppliedAt calls this poll will
// make across all items (see maxArchiveLabelFetchesPerPoll).
func (e *Engine) maybeArchiveDoneItem(board *gh.ProjectBoard, item gh.ProjectItem, completeLabel string, fetchBudget *int) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoStr := itemOwnerRepoString(item, e.defaultRepo())

	eligibleAt, cached := e.archiveEligibleAt(owner, repo, repoStr, item, completeLabel, fetchBudget)
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
	// No webhook echo registered: applyProjectsV2ItemDelta's "deleted"/"archived"
	// case (boardcache/delta.go) never calls matchEchoFn — only "edited" does. An
	// echo registered here would simply expire unmatched, and enough concurrent
	// archivals would trip doEchoSweep's WebhookStreamUnhealthy threshold for no
	// reason. The RemoveItem write-through above already gives cache coherence
	// immediately; there is nothing for an echo to protect. Mirrors the identical
	// no-echo rationale for issue-close in engine/no_work_needed_settle.go.
}

// archiveEligibleAt returns the cached or freshly-fetched "archive eligible at"
// time for item, and whether one is available yet. On a CooldownAt cache miss,
// it calls FetchLabelAppliedAt once — unless fetchBudget is exhausted for this
// poll, in which case it defers to the next poll exactly like the "not yet
// known" fail-open path below (see maxArchiveLabelFetchesPerPoll). A zero
// result (label-applied timestamp not found — FetchLabelAppliedAt's deliberate
// fail-open contract) is treated as "not yet known" and is not cached, so the
// next poll retries rather than permanently skipping the item or archiving it
// prematurely (requirement 7).
func (e *Engine) archiveEligibleAt(owner, repo, repoStr string, item gh.ProjectItem, completeLabel string, fetchBudget *int) (time.Time, bool) {
	if snap, err := e.store.Get(repoStr, item.Number); err == nil {
		if at := snap.CooldownAt(archiveEligibleAtCooldownReason); !at.IsZero() {
			return at, true
		}
	}

	if *fetchBudget <= 0 {
		// Budget exhausted for this poll — retry next poll rather than blocking
		// the poll loop with an unbounded burst of REST calls.
		return time.Time{}, false
	}
	*fetchBudget--

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
