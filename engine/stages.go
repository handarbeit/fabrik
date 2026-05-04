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
)

// errRebaseDispatched is returned by attemptMergeOnValidate when a rebase
// reinvoke has been dispatched instead of immediately pausing. handleStageComplete
// detects this sentinel and adds stage:Validate:complete so the catch-up loop's
// Phase 2 drives the merge retry rather than itemNeedsWork triggering a full
// Validate re-invocation.
var errRebaseDispatched = errors.New("rebase reinvoke dispatched")

// hasYoloLabel reports whether item has the "fabrik:yolo" label.
func hasYoloLabel(item gh.ProjectItem) bool {
	for _, l := range item.Labels {
		if l == "fabrik:yolo" {
			return true
		}
	}
	return false
}

// hasCruiseLabel reports whether item has the "fabrik:cruise" label.
// cruise auto-advances through all stages but stops before auto-merging
// the PR or advancing to Done at Validate completion.
func hasCruiseLabel(item gh.ProjectItem) bool {
	for _, l := range item.Labels {
		if l == "fabrik:cruise" {
			return true
		}
	}
	return false
}

// hasUnrestrictedLabel reports whether item has the "fabrik:unrestricted" label,
// which tells the engine to pass --dangerously-skip-permissions to Claude Code.
func hasUnrestrictedLabel(item gh.ProjectItem) bool {
	for _, l := range item.Labels {
		if l == "fabrik:unrestricted" {
			return true
		}
	}
	return false
}

func (e *Engine) handleStageComplete(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "done", "stage %q complete\n", stage.Name)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Clean up any failure label from a prior incomplete run.
	e.removeFailedLabel(owner, repo, item.Number, stage.Name)

	// Re-fetch labels so we see changes made while the stage was running
	// (e.g., fabrik:yolo added mid-run). Must bypass cache — the webhook for a
	// label added mid-run may not have been applied yet. On error, keep existing labels.
	if freshLabels, err := e.client.FetchLabels(e.cfg.Owner, e.cfg.Repo, item.Number); err == nil && len(freshLabels) > 0 {
		item.Labels = freshLabels
	}

	// yoloActive gates both PR merge and is the base for stage advancement.
	// stage.AutoAdvance overrides advancement only — not the merge decision.
	yoloActive := e.cfg.Yolo || hasYoloLabel(item)

	// cruiseActive mirrors yoloActive for advancement but skips the two
	// end-of-Validate actions (merge + Done advancement). When yolo is active,
	// cruise is suppressed so yolo always takes precedence.
	cruiseActive := !yoloActive && hasCruiseLabel(item)

	// Attempt PR merge after Validate when yolo is active and wait_for_ci is false.
	// When wait_for_ci: true the merge happens in the catch-up loop's Phase 2 after
	// the CI gate clears (see ADR 032). This runs BEFORE adding the completion label
	// so that on merge failure the engine can retry Validate (itemNeedsWork skips
	// stages with a complete label).
	waitForCI := stage.WaitForCI != nil && *stage.WaitForCI
	if yoloActive && stage.Name == "Validate" && !waitForCI {
		if err := e.attemptMergeOnValidate(ctx, board, item, stage); err != nil {
			if errors.Is(err, errRebaseDispatched) {
				// Rebase reinvoke dispatched — add completion label so the catch-up
				// loop retries the merge rather than itemNeedsWork triggering a full
				// Validate re-invocation.
				completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
				if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); lerr != nil {
					e.logf(item.Number, "warn", "could not add completion label: %v\n", lerr)
				}
				e.logf(item.Number, "rebase-reinvoke", "PR merge deferred — rebase dispatched\n")
			} else {
				// Merge failed: post/pause already handled inside attemptMergeOnValidate.
				e.logf(item.Number, "warn", "PR not merged: %v\n", err)
			}
			return
		}
	}

	// Conjunctive gate: for wait_for_ci stages, defer stage:X:complete until
	// the CI gate clears. Adding fabrik:awaiting-ci here (idempotent) keeps the
	// item durable in the "CI await" state so the dispatcher skips it and the
	// catch-up loop can evaluate CI on every poll (R1, R2, R3).
	if waitForCI {
		alreadyAwaitingCI := false
		for _, l := range item.Labels {
			if l == "fabrik:awaiting-ci" {
				alreadyAwaitingCI = true
				break
			}
		}
		if !alreadyAwaitingCI {
			if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-ci"); err != nil {
				e.logf(item.Number, "warn", "could not add fabrik:awaiting-ci label: %v\n", err)
			}
		}
		// Also seed fabrik:awaiting-review optimistically (Path 1 idempotent), so
		// the catch-up loop's review gate runs before the CI gate when both apply.
		if stage.WaitForReviews != nil && *stage.WaitForReviews {
			alreadyAwaitingReview := false
			for _, l := range item.Labels {
				if l == "fabrik:awaiting-review" {
					alreadyAwaitingReview = true
					break
				}
			}
			if !alreadyAwaitingReview {
				if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-review"); err != nil {
					e.logf(item.Number, "warn", "could not add fabrik:awaiting-review label: %v\n", err)
				}
			}
		}
		e.logf(item.Number, "awaiting-ci", "deferring stage:%s:complete until CI gate clears\n", stage.Name)
		return // catch-up loop adds stage:X:complete when checkCIGate clears
	}

	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); err != nil {
		e.logf(item.Number, "warn", "could not add completion label: %v\n", err)
	}

	// fabrik:yolo or fabrik:cruise label overrides stage.AutoAdvance — if the user
	// explicitly labelled the issue, respect that over YAML config.
	shouldAdvance := yoloActive || cruiseActive
	if stage.AutoAdvance != nil && !hasYoloLabel(item) && !hasCruiseLabel(item) {
		shouldAdvance = *stage.AutoAdvance
	}

	// cruise stops at Validate: do not advance to Done or trigger any further
	// stage movement. The PR merge was already skipped (yoloActive is false).
	if cruiseActive && stage.Name == "Validate" {
		shouldAdvance = false
	}

	if shouldAdvance {
		if e.checkDependencies(board, item, stage) {
			return // blocked; checkDependencies handled label + comment
		}
		// Path 1: handleStageComplete always has stale review data because
		// reviewers are added only after MarkPRReady (which runs inside the
		// stage). Rather than re-fetching, we optimistically apply
		// fabrik:awaiting-review and let the catch-up loop (Path 2) make
		// the real gate decision with fresh FetchItemDetails data.
		if stage.WaitForReviews != nil && *stage.WaitForReviews {
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
				e.logf(item.Number, "awaiting-review", "waiting for PR reviewers before advancing\n")
			}
			return // catch-up loop will advance once reviewers submit
		}
		if err := e.advanceToNextStage(board, item, stage); err != nil {
			e.logf(item.Number, "warn", "could not advance: %v\n", err)
		}
	} else {
		e.logf(item.Number, "wait", "waiting for human to advance\n")
	}
}

// attemptMergeOnValidate finds the linked PR for item, gates on CI status,
// and merges it. Returns nil on success or when no PR exists.
// On CI timeout, returns a handled error (pause already applied).
// On ErrNotMergeable (base-branch conflict): applies fabrik:rebase-needed
// idempotently, then dispatches dispatchRebaseReinvoke and returns the
// errRebaseDispatched sentinel (caller must add stage:Validate:complete so
// the catch-up loop retries the merge). At MaxRebaseCycles, falls back to
// pauseForRebaseCycleLimit (handled error, pause applied).
// Returns a retriable error (caller should retry) when CI is pending or a
// transient API error occurs.
//
// CI gate logic (R1–R6):
//   - No check runs → gate clears (R5: repo has no CI)
//   - Any pending/queued → block; track ciMergePendingSince; pause after CIWaitTimeout
//   - Any failed → add fabrik:awaiting-ci; return error
//   - All green → clear ciMergePendingSince; clear fabrik:awaiting-ci; proceed to merge
func (e *Engine) attemptMergeOnValidate(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) error {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	iKey := issueKey(item, e.defaultRepo())

	// Use FetchLinkedPR (REST) to get both the PR number and head SHA in one call.
	pr, err := e.readClient.FetchLinkedPR(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "could not fetch linked PR for merge: %v — will retry\n", err)
		return fmt.Errorf("fetch linked PR: %w", err)
	}
	if pr == nil {
		e.logf(item.Number, "warn", "no linked PR found at Validate completion; skipping auto-merge\n")
		return nil
	}

	// Trust GitHub's branch-protection-aware mergeable_state when it's
	// positive: "clean" = ready to merge per branch protection; "unstable" =
	// non-required checks failing but still mergeable. In both cases, skip
	// the raw check_runs gate below — that gate is over-aggressive, treating
	// any check_run failure (including non-required workflow jobs like
	// "Cleanup artifacts") as blocking, which contradicts GitHub's own
	// merge decision. mergeable_state is only available from the single-PR
	// endpoint, so this requires an extra REST call.
	bypassCheckRunsGate := false
	if pr.HeadSHA != "" {
		mergeableState, msErr := e.readClient.FetchPRMergeableState(owner, repo, pr.Number)
		if msErr != nil {
			e.logf(item.Number, "warn", "could not fetch mergeable_state: %v — falling back to check-runs gate\n", msErr)
		} else if gh.MergeableStateAccepted(mergeableState) {
			e.logf(item.Number, "ci-gate", "mergeable_state=%q — skipping check_runs gate, proceeding to merge\n", mergeableState)
			// Clear stale CI-await state so a stuck fabrik:awaiting-ci from
			// the prior over-aggressive gate doesn't survive past the merge.
			e.removeAwaitingCILabel(owner, repo, item)
			e.mu.Lock()
			delete(e.ciMergePendingSince, iKey)
			e.mu.Unlock()
			bypassCheckRunsGate = true
		}
	}

	// CI gate: fetch check runs and evaluate (R1-R6). Skipped when GitHub's
	// mergeable_state already says the PR is mergeable (above).
	if !bypassCheckRunsGate && pr.HeadSHA != "" {
		checkRuns, err := e.readClient.FetchCheckRuns(owner, repo, pr.HeadSHA)
		if err != nil {
			e.logf(item.Number, "warn", "could not fetch check runs for merge guard: %v — skipping merge until CI status can be fetched\n", err)
			return fmt.Errorf("merge guard: fetch check runs: %w", err)
		} else if len(checkRuns) > 0 {
			var pending, failed []gh.CheckRun
			for _, cr := range checkRuns {
				switch cr.Status {
				case "queued", "in_progress":
					pending = append(pending, cr)
				case "completed":
					switch cr.Conclusion {
					case "failure", "timed_out", "action_required":
						failed = append(failed, cr)
					}
				}
			}

			if len(failed) > 0 {
				// R3: CI failed — apply label and block merge.
				names := make([]string, 0, len(failed))
				for _, cr := range failed {
					names = append(names, fmt.Sprintf("%s (%s)", cr.Name, cr.Conclusion))
				}
				e.logf(item.Number, "ci-gate", "merge blocked — CI failed: %s\n", strings.Join(names, ", "))
				if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-ci"); lerr != nil {
					e.logf(item.Number, "warn", "could not add fabrik:awaiting-ci: %v\n", lerr)
				}
				// Clean up pending timer since we now have a definitive failure state.
				e.mu.Lock()
				delete(e.ciMergePendingSince, iKey)
				e.mu.Unlock()
				return fmt.Errorf("merge blocked: CI checks failed")
			}

			if len(pending) > 0 {
				// R2: CI still running — check timeout (R6).
				names := make([]string, 0, len(pending))
				for _, cr := range pending {
					names = append(names, cr.Name)
				}
				e.logf(item.Number, "ci-gate", "merge blocked — CI still running: %s\n", strings.Join(names, ", "))

				timeout := e.cfg.CIWaitTimeout
				if timeout <= 0 {
					timeout = 30 * time.Minute
				}
				e.mu.Lock()
				since, tracked := e.ciMergePendingSince[iKey]
				if !tracked {
					e.ciMergePendingSince[iKey] = time.Now()
					since = e.ciMergePendingSince[iKey]
				}
				e.mu.Unlock()

				if time.Since(since) >= timeout {
					// R6: timeout elapsed — pause issue.
					e.mu.Lock()
					delete(e.ciMergePendingSince, iKey)
					e.mu.Unlock()
					msg := fmt.Sprintf("🏭 **Fabrik — CI wait timeout (merge guard)**\n\n"+
						"Auto-merge blocked: CI checks for PR #%d have been in progress for longer than "+
						"the configured timeout (%s). Fabrik has paused this issue for human review.\n\n"+
						"Pending checks: %s\n\nOnce CI resolves, remove `fabrik:paused` to resume.",
						pr.Number, timeout, strings.Join(names, ", "))
					if dbID, cerr := e.client.AddComment(owner, repo, item.Number, msg); cerr != nil {
						e.logf(item.Number, "warn", "could not post CI timeout comment: %v\n", cerr)
					} else if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
						e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
					}
					if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); lerr != nil {
						e.logf(item.Number, "warn", "could not add fabrik:paused: %v\n", lerr)
					}
					if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-input"); lerr != nil {
						e.logf(item.Number, "warn", "could not add fabrik:awaiting-input: %v\n", lerr)
					}
					return fmt.Errorf("merge guard: CI wait timeout elapsed after %s", timeout)
				}
				return fmt.Errorf("merge blocked: CI checks still running")
			}

			// R4: All checks green — clear pending timer and awaiting-ci label.
			e.mu.Lock()
			delete(e.ciMergePendingSince, iKey)
			e.mu.Unlock()
			e.removeAwaitingCILabel(owner, repo, item)
		}
		// R5: len(checkRuns) == 0 — no CI configured; gate clears.
	}

	if err := e.client.MergePR(owner, repo, pr.Number); err != nil {
		if errors.Is(err, gh.ErrNotMergeable) {
			// Apply fabrik:rebase-needed idempotently.
			alreadyLabeled := false
			for _, l := range item.Labels {
				if l == "fabrik:rebase-needed" {
					alreadyLabeled = true
					break
				}
			}
			if !alreadyLabeled {
				if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:rebase-needed"); lerr != nil {
					e.logf(item.Number, "warn", "could not add fabrik:rebase-needed label: %v\n", lerr)
				}
			}
			// inFlight guard: if a rebase goroutine is already running, skip
			// dispatch and cycle-limit check to avoid counter drift.
			if _, ok := e.inFlight.Load(iKey); ok {
				e.logf(item.Number, "rebase-reinvoke", "skipping dispatch — rebase reinvoke already in-flight\n")
				return fmt.Errorf("PR #%d not mergeable (rebase in-flight)", pr.Number)
			}
			stageKey := iKey + "-" + stage.Name
			e.mu.Lock()
			cycleCount := e.rebaseCycleCount[stageKey]
			maxCycles := e.cfg.MaxRebaseCycles
			e.mu.Unlock()
			if cycleCount >= maxCycles {
				e.pauseForRebaseCycleLimit(board, item, stage, cycleCount, maxCycles)
				return fmt.Errorf("PR #%d not mergeable — rebase cycle limit reached", pr.Number)
			}
			e.mu.Lock()
			e.rebaseCycleCount[stageKey]++
			e.mu.Unlock()
			e.dispatchRebaseReinvoke(ctx, board, item, stage)
			return errRebaseDispatched
		}
		// Other API errors (transient 5xx, permissions, etc.): log and return an
		// error so the caller retries on the next cooldown cycle.
		e.logf(item.Number, "warn", "auto-merge of PR #%d failed: %v\n", pr.Number, err)
		return fmt.Errorf("auto-merge failed: %w", err)
	}

	e.removeRebaseNeededLabel(owner, repo, item)
	e.logf(item.Number, "info", "auto-merged PR #%d\n", pr.Number)
	return nil
}

// handleDecomposed is called when a stage outputs the FABRIK_DECOMPOSED marker.
// It adds the stage completion label and moves the parent issue directly to Done
// on the project board, bypassing all remaining pipeline stages.
// This is only expected from the Plan stage when it splits an issue into sub-issues.
func (e *Engine) handleDecomposed(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "done", "stage %q decomposed issue into sub-issues — moving to Done\n", stage.Name)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Add the stage completion label so the engine won't re-run this stage on restart.
	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); err != nil {
		e.logf(item.Number, "warn", "could not add completion label: %v\n", err)
	}

	if e.statusField == nil {
		e.logf(item.Number, "warn", "status field metadata not available; cannot move to Done\n")
		return
	}

	optionID, ok := e.statusField.Options["Done"]
	if !ok {
		e.logf(item.Number, "warn", "no status option %q found on project board (available: %v); cannot move to Done\n",
			"Done", mapKeys(e.statusField.Options))
		return
	}

	e.logf(item.Number, "advance", "moving decomposed issue to Done\n")
	if err := e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID); err != nil {
		e.logf(item.Number, "warn", "could not move issue to Done: %v\n", err)
	} else {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.UpdateItemStatus(boardcache.ItemKey(item.Repo, item.Number), "Done")
		}
	}
}

func (e *Engine) advanceToNextStage(board *gh.ProjectBoard, item gh.ProjectItem, currentStage *stages.Stage) error {
	next := stages.NextStage(e.cfg.Stages, currentStage.Name)
	if next == nil {
		e.logf(item.Number, "info", "completed all stages\n")
		return nil
	}

	if e.statusField == nil {
		return fmt.Errorf("status field metadata not available")
	}

	optionID, ok := e.statusField.Options[next.Name]
	if !ok {
		return fmt.Errorf("no status option %q found on project board (available: %v)",
			next.Name, mapKeys(e.statusField.Options))
	}

	e.logf(item.Number, "advance", "moving to stage %q\n", next.Name)
	err := e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID)
	if err == nil {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.UpdateItemStatus(boardcache.ItemKey(item.Repo, item.Number), next.Name)
		}
	}
	return err
}
