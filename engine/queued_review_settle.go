package engine

import (
	gh "github.com/handarbeit/fabrik/github"
)

// settleQueuedReviewFindings is the sixth ADR-1270-pattern settle scan: the sole
// per-poll detector of unresolved review-thread feedback arriving on a Queued
// merge-train member's linked PR while it sits out of reach of the ordinary
// review-reinvoke path (#1208). Sourced directly from board.Items via
// groupQueuedByRepo — the same holding-stage-column filter and closed/fabrik:paused
// exclusion handleMergeTrainBatch's routeQueuedGroup already applies — rather than
// deepFetchCandidates: itemMayNeedWork unconditionally excludes HoldingStage items
// from that path, so a Queued member's reviewThreads are never populated by the
// normal poll pipeline at all. This scan calls FetchItemDetails itself, exactly as
// settleAwaitingCIScan does for the analogous fabrik:awaiting-ci gap (ADR-1270).
//
// Detection uses currentHeadReviewThreadComments (not raw buildReviewThreadComments)
// — the ADR-1207-canonical, current-head-scoped primitive — so a thread anchored to a
// commit the PR has since moved past never triggers a spurious ejection.
//
// A flagged member is ejected one of two ways, depending on whether a merge-train
// worker is currently in flight for its repo (mergeTrainWorkerActive):
//   - No worker in flight: nothing else could be touching the member's batch state,
//     so the scan ejects it directly via ejectQueuedMemberForReviewFindings.
//   - A worker IS in flight: the scan must not reach into that goroutine's own
//     in-memory batch slice — doing so would race its assemble/validate/land
//     sequence — so it leaves a pending-eject signal (markPendingReviewEject)
//     instead; the worker itself applies it at its own checkpoints
//     (applyPendingReviewEjects, called from runMergeTrainWorker's re-form loop and
//     landOneAtATime) before deciding a trial's fate. See docs/state-machine.md's
//     Queued Review-Finding Ejection section for the full sequencing.
//
// Native-merge-queue members (GitHub's own merge queue, not the internal train) are
// skipped: ejectMember/MaxMergeTrainEjections have no meaning for them, mirroring
// routeQueuedGroup's own FR-3 precedence rule.
func (e *Engine) settleQueuedReviewFindings(board *gh.ProjectBoard) {
	hs := holdingStage(e.cfg)
	if hs == nil {
		return
	}
	for _, g := range groupQueuedByRepo(board.Items, hs.Name, e.defaultRepo()) {
		workerActive := e.mergeTrainWorkerActive(g.repoKey)
		for _, item := range g.items {
			// Precedence rule 1 (FR-3, mirrors routeQueuedGroup): a native-merge-queue
			// member is not an internal-train member — ejectMember has no meaning here.
			if e.cfg.MergeQueue != "off" && item.LinkedPRIsMergeQueueEnabled {
				continue
			}
			if hasLabel(item.Labels, "fabrik:auto-merge-enabled") {
				continue
			}

			if err := e.readClient.FetchItemDetails(&item); err != nil {
				e.logf(item.Number, "queued-review-settle", "could not deep-fetch item details: %v — will retry next poll\n", err)
				continue
			}
			// A concurrent close/pause between the shallow board read (groupQueuedByRepo's
			// snapshot) and this deep-fetch must not act on now-stale state.
			if item.IsClosed || hasLabel(item.Labels, "fabrik:paused") {
				continue
			}

			findings := e.currentHeadReviewThreadComments(item)
			if len(findings) == 0 {
				continue
			}

			if workerActive {
				e.logf(item.Number, "queued-review-settle", "%d unresolved review-thread finding(s) on Queued member #%d — worker in flight for %s, flagging pending eject\n", len(findings), item.Number, g.repoKey)
				e.markPendingReviewEject(g.repoKey, item.Number, len(findings))
				continue
			}
			e.logf(item.Number, "queued-review-settle", "%d unresolved review-thread finding(s) on Queued member #%d — no worker in flight for %s, ejecting directly\n", len(findings), item.Number, g.repoKey)
			e.ejectQueuedMemberForReviewFindings(board.ProjectID, item, len(findings))
		}
	}
}
