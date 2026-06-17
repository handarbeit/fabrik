package engine

import (
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// driftScanResult holds accounting counters from a single detectAndRepairBoardDrift run.
type driftScanResult struct {
	scanned            int
	repaired           int
	skipped            int // guard-based skip (Invariants 2-5)
	skippedPRNotMerged int // Invariant 6: PR not merged or no PR
	skippedRaceLost    int // Invariant 1: TryLocalLockAcquired returned false
}

// labelIndicatesDrift reports whether item has a cleanup-stage :complete label
// whose name does not match item.Status (i.e., board drift exists).
//
// Returns the highest-order cleanup stage whose label is present (EC-2: when
// multiple cleanup stages match, the highest order wins). Returns (nil, false)
// when no drift is detected.
func labelIndicatesDrift(item gh.ProjectItem, stgs []*stages.Stage) (*stages.Stage, bool) {
	cleanupByName := make(map[string]*stages.Stage, len(stgs))
	for _, s := range stgs {
		if s.CleanupWorktree {
			cleanupByName[s.Name] = s
		}
	}

	var target *stages.Stage
	for _, label := range item.Labels {
		if !strings.HasPrefix(label, "stage:") || !strings.HasSuffix(label, ":complete") {
			continue
		}
		stageName := strings.TrimSuffix(strings.TrimPrefix(label, "stage:"), ":complete")
		s, ok := cleanupByName[stageName]
		if !ok {
			continue
		}
		if target == nil || s.Order > target.Order {
			target = s
		}
	}
	if target == nil || item.Status == target.Name {
		return nil, false
	}
	return target, true
}

// shouldSkipDriftRepair returns true if any of Invariants 2–5 would be violated
// for this item. Makes a single store.Get call to read both Worker and
// LastStatusUpdateAt, minimising lock acquisitions.
//
// repoStr must be the normalized "owner/repo" string (not item.Repo, which may
// be empty for items in the default repo).
//
// Invariant 2: in-flight worker registered in the Store.
// Invariant 3: any fabrik:locked:<user> label (including self-lock).
// Invariant 4: fabrik:paused label present.
// Invariant 5: LastStatusUpdateAt is within cfg.RepairDwell (dwell gate).
func shouldSkipDriftRepair(item gh.ProjectItem, repoStr string, store *itemstate.Store, cfg Config) bool {
	snap, err := store.Get(repoStr, item.Number)
	hasSnap := err == nil

	// Invariant 2: in-flight worker is sacrosanct.
	if hasSnap && snap.Worker() != nil {
		return true
	}

	// Invariant 3: any fabrik:locked:<user> label (self OR other).
	for _, l := range item.Labels {
		if strings.HasPrefix(l, "fabrik:locked:") {
			return true
		}
	}

	// Invariant 4: operator-pause is sacrosanct.
	for _, l := range item.Labels {
		if l == "fabrik:paused" {
			return true
		}
	}

	// Invariant 5: recent advance must settle (anti-flap dwell gate).
	// Zero RepairDwell disables the gate. Zero LastStatusUpdateAt (EC-5:
	// bootstrap window) is treated as "never updated" — do not skip.
	if hasSnap && cfg.RepairDwell > 0 {
		if lastUpdate := snap.LastStatusUpdateAt(); !lastUpdate.IsZero() && time.Since(lastUpdate) < cfg.RepairDwell {
			return true
		}
	}

	return false
}

// detectAndRepairBoardDrift scans items for label-vs-column drift and repairs
// by calling advanceToNextStage when ALL guards pass (Invariants 1–7).
//
// Called from both startup (once, post-bootstrap) and per-poll (once per poll
// cycle, after R4 paused-item recovery and R4b convergence-paused recovery).
//
// When e.cfg.AutoRepairDrift is false, the scan emits the warn-only log line
// introduced in PR #880 and performs no board mutations (SC-8, SC-11).
//
// Must NEVER dispatch workers or acquire e.sem.
func (e *Engine) detectAndRepairBoardDrift(board *gh.ProjectBoard, items []gh.ProjectItem, advancedItems map[string]bool) driftScanResult {
	var res driftScanResult

	for _, item := range items {
		// Detect: item must have stage:X:complete for a cleanup stage with board
		// column != X.
		targetStage, hasDrift := labelIndicatesDrift(item, e.cfg.Stages)
		if !hasDrift {
			continue
		}
		res.scanned++

		// Pre-compute normalized repo string; used by store lookups, CAS, and lock release.
		repoStr := itemOwnerRepoString(item, e.defaultRepo())

		if !e.cfg.AutoRepairDrift {
			// Warn-only mode: emit identical log line to PR #880 startup scan.
			e.logf(item.Number, "startup", "warning: item #%d has label stage:%s:complete but board column is %q — board drift detected\n",
				item.Number, targetStage.Name, item.Status)
			continue
		}

		// Invariants 2–5: guard checks.
		if shouldSkipDriftRepair(item, repoStr, e.store, e.cfg) {
			e.logf(item.Number, "board-drift", "skipping repair (invariant 2-5 guard fired)\n")
			res.skipped++
			continue
		}

		// Invariant 6: linked PR must be terminally merged (EC-3, EC-4).
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		pr, prErr := e.client.FetchLinkedPR(owner, repo, item.Number)
		if prErr != nil {
			e.logf(item.Number, "board-drift", "could not verify linked PR: %v — skipping\n", prErr)
			res.skippedPRNotMerged++
			continue
		}
		if pr == nil || !pr.Merged {
			// EC-3 (closed-unmerged) or EC-4 (no linked PR): not a drift case.
			res.skippedPRNotMerged++
			continue
		}

		// Find the stage for the current board column to use as currentStage in
		// advanceToNextStage. advanceToNextStage advances exactly one step; for the
		// common case (one step behind the cleanup stage) this lands on the target.
		currentColumnStage := stages.FindStage(e.cfg.Stages, item.Status)
		if currentColumnStage == nil {
			e.logf(item.Number, "board-drift", "current column %q matches no configured stage — skipping\n", item.Status)
			res.skipped++
			continue
		}

		// Invariant 1: single-writer guarantee via CAS.
		// TryLocalLockAcquired holds the Store mutex for check-and-set; returns
		// false if another path already holds a WorkerHandle for this item.
		worker := &itemstate.WorkerHandle{StageName: "drift-repair", StartedAt: time.Now(), LastSignAt: time.Now()}
		if !e.store.TryLocalLockAcquired(repoStr, item.Number, e.cfg.User, worker, time.Now()) {
			e.logf(item.Number, "board-drift", "skipping repair (invariant-1: CAS lock lost to concurrent path)\n")
			res.skippedRaceLost++
			continue
		}

		e.logf(item.Number, "board-drift", "repairing drift: stage %s:complete present, column is %q — advancing\n",
			targetStage.Name, item.Status)

		if err := e.advanceToNextStage(board, item, currentColumnStage); err != nil {
			e.logf(item.Number, "warn", "board-drift: could not advance: %v\n", err)
		} else {
			res.repaired++
			if advancedItems != nil {
				advancedItems[issueKey(item, e.defaultRepo())] = true
			}
		}

		// Explicit lock release — no defer inside for-loop (would defer until
		// function return, holding all locks simultaneously).
		// WorkerExited must follow LocalLockReleased: TryLocalLockAcquired sets
		// item.Worker for the CAS, but LocalLockReleased only clears item.Lock.
		// Without WorkerExited, the stale WorkerHandle would cause all downstream
		// snap.Worker()!=nil guards (dispatch loop, revalidate, R4b) to skip this
		// item permanently until the engine restarts.
		e.store.Apply(itemstate.LocalLockReleased{Repo: repoStr, Number: item.Number})
		e.store.Apply(itemstate.WorkerExited{Repo: repoStr, Number: item.Number})
	}

	if res.scanned > 0 {
		e.logf(0, "board-drift", "scan complete: %d with drift, %d repaired, %d guards, %d no-merged-PR, %d race-lost\n",
			res.scanned, res.repaired, res.skipped, res.skippedPRNotMerged, res.skippedRaceLost)
	}

	return res
}
