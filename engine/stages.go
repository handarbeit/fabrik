package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

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

// refreshAutonomyLabels re-reads item.Labels live from GitHub, in place, so
// that a fabrik:yolo/fabrik:cruise change made while a stage was running (an
// operator adding OR removing either label mid-run) is observed at the
// moment the advance decision is actually made, rather than at the next full
// board refresh (#1769). It is the single shared helper both handleStageComplete
// (D1) and runCatchUpPhase2 (D2) must call, before either reads
// hasYoloLabel/hasCruiseLabel — mirroring effectiveReviewAuthority/
// effectiveExpectedReviewers' "one shared helper, every gate" precedent
// (ADR-1250, ADR-1283, engine/reviews.go), except — unlike those two, which
// only resolve a label already present on the snapshot — this helper
// performs the live fetch itself.
//
// Uses the item's own resolved owner/repo (itemOwnerRepo), never
// e.cfg.Owner/e.cfg.Repo, which is empty in multi-repo mode and, on a
// single-repo instance, would target whichever issue happens to hold this
// item's number in the configured repo rather than the item's actual repo
// (R1). Must bypass any cache — a label change made moments ago may not
// have an applied webhook delta yet — so this always calls e.client (the
// raw GitHubClient), never e.readClient.
//
// On a fetch error, item.Labels is left untouched and a warning naming the
// item and the error is logged (R2) — silent fallback is what hid #1769,
// and must not survive the fix. On success, item.Labels is replaced
// unconditionally: a fetch that legitimately returns zero labels is real
// data, not a failure, and must not be discarded (R3).
//
// Because attemptMergeOnValidate receives item by value from both callers,
// and both callers invoke this helper before attemptMergeOnValidate is ever
// called, one fetch per handleStageComplete/runCatchUpPhase2 invocation
// covers every downstream hasYoloLabel/hasCruiseLabel read, including the
// one inside attemptMergeOnValidate — satisfying R5's "must not fetch
// twice."
//
// It reports whether the labels are now live. The yolo/cruise reads keep their
// historical fall-back-to-snapshot behaviour on a failure, but the pause guard
// (#2341) must not: a stale snapshot can predate a human's fabrik:paused, so
// callers pass the result to pausedOrUnread and fail closed.
func (e *Engine) refreshAutonomyLabels(item *gh.ProjectItem) (live bool) {
	owner, repo := itemOwnerRepo(*item, e.defaultRepo())
	freshLabels, err := e.client.FetchLabels(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "refreshAutonomyLabels: could not re-fetch labels for %s/%s#%d: %v\n", owner, repo, item.Number, err)
		return false
	}
	item.Labels = freshLabels
	return true
}

func (e *Engine) handleStageComplete(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "done", "stage %q complete\n", stage.Name)

	// Circuit breaker (#1089): a stage:*:complete transition is forward progress —
	// reset before any of the completion side effects below so a breaker check
	// triggered by the same cycle that produced this completion never trips.
	e.resetCommentBreaker(item)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// A completed Validate ends the merge-train auto-repair flow (#2045): the pending
	// repair context is kept across retries of that Validate and dropped here.
	if stage.Name == "Validate" {
		e.dropPendingRepair(autoRepairKey(owner, repo, item.Number))
	}

	// Clean up any failure label from a prior incomplete run.
	e.removeFailedLabel(owner, repo, item.Number, stage.Name)

	// Re-fetch labels so we see changes made while the stage was running
	// (e.g., fabrik:yolo/fabrik:cruise added or removed mid-run) before any
	// autonomy-label read below (#1769). See refreshAutonomyLabels' own doc
	// comment for the full rationale.
	labelsLive := e.refreshAutonomyLabels(&item)

	// Clear any orphaned fabrik:awaiting-input label. It is added by blockOnInput
	// when Claude emits FABRIK_BLOCKED_ON_INPUT; if the user manually removes
	// fabrik:paused (bypassing unblockAwaitingInput), the label can survive through
	// subsequent stage runs. Clean it up on every completion path.
	if hasLabel(item.Labels, "fabrik:awaiting-input") {
		if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil &&
			!errors.Is(err, gh.ErrNotFound) {
			e.logf(item.Number, "warn", "could not remove awaiting-input label: %v\n", err)
		} else if err == nil {
			if c := e.cache(); c != nil {
				c.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, item.Number), "fabrik:awaiting-input")
			}
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("issues", "unlabeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:awaiting-input")
			}
		}
	}

	// yoloActive gates both PR merge and is the base for stage advancement.
	// stage.AutoAdvance overrides advancement only — not the merge decision.
	yoloActive := e.cfg.Yolo || hasYoloLabel(item)

	// cruiseActive mirrors yoloActive for advancement but skips the two
	// end-of-Validate actions (merge + Done advancement).
	//
	// It is deliberately false when yolo is also present, but that does NOT mean
	// yolo takes precedence: both end-of-Validate guards below test the raw
	// hasCruiseLabel, not cruiseActive, so cruise still wins the merge decision
	// and the stop-at-Validate decision when both labels are set. Suppressing
	// cruiseActive under yolo only avoids double-counting in the shouldAdvance
	// expression, where yoloActive already contributes the same true value at
	// every non-Validate stage.
	cruiseActive := !yoloActive && hasCruiseLabel(item)

	// autoMergeEnabled is true when attemptMergeOnValidate successfully called
	// EnablePullRequestAutoMerge. Done advancement is deferred to the convergence
	// monitor (checkAutoMergeConvergence in the catch-up loop) in that case.
	autoMergeEnabled := false

	// landingDeferred is true when attemptMergeOnValidate declined to
	// advance because an unresolved review thread exists on the current head
	// (#1207 guard 1). Advancement must be suppressed the same way it is for
	// autoMergeEnabled — the catch-up loop's Phase 2 Validate branch retries
	// attemptMergeOnValidate on every poll and will proceed once the thread
	// is resolved by the review-reinvoke loop (§2.9).
	landingDeferred := false

	// Enable GitHub native auto-merge after Validate when yolo is active and
	// wait_for_ci is false. When wait_for_ci: true the merge path is handled by
	// checkCIGate in the catch-up loop (see ADR 032). This runs BEFORE adding the
	// completion label so that on failure the engine retries Validate (itemNeedsWork
	// skips stages with a complete label).
	waitForCI := stage.WaitForCI != nil && *stage.WaitForCI
	// Pause guard (#2341): item.Labels is live as of refreshAutonomyLabels above, so a
	// fabrik:paused applied while this stage ran is visible here. A paused item is not
	// merged or advanced; it keeps its completion label and lands once the pause lifts.
	// Fail closed: when that refresh failed, item.Labels is the pre-run snapshot, which
	// may predate the pause, so the decision waits for a later poll too.
	paused := e.pausedOrUnread(item.Labels, labelsLive)
	if yoloActive && stage.Name == "Validate" && !waitForCI {
		if paused {
			e.refuseIfPausedOrUnread(item, labelsLive, "landing")
			landingDeferred = true
		} else {
			var mergeErr error
			autoMergeEnabled, landingDeferred, mergeErr = e.attemptMergeOnValidate(ctx, board, item, stage)
			if mergeErr != nil {
				e.logf(item.Number, "warn", "PR not merged: %v\n", mergeErr)
				return
			}
		}
	}

	// Conjunctive gate: for wait_for_ci stages, defer stage:X:complete until
	// the CI gate clears. Adding fabrik:awaiting-ci here (idempotent) keeps the
	// item durable in the "CI await" state so the dispatcher skips it and the
	// catch-up loop can evaluate CI on every poll (R1, R2, R3).
	if waitForCI {
		// #2299 R1-R3: the PR may have reached a terminal state while this worker
		// ran (item.Labels is live as of refreshAutonomyLabels above). Merged → the
		// terminal path; closed unmerged → the pause; a read error arms as before.
		if e.completionTerminalCheck(board, item, stage) {
			return
		}
		alreadyAwaitingCI := hasLabel(item.Labels, "fabrik:awaiting-ci")
		awaitingCIArmed := alreadyAwaitingCI
		// awaiting-ci is this stage's durable completion state (#2395): only once it
		// stands does the dispatch count of the episode end.
		completionDurable := alreadyAwaitingCI
		if !alreadyAwaitingCI {
			if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-ci"); err != nil {
				e.logf(item.Number, "warn", "could not add fabrik:awaiting-ci label: %v\n", err)
			} else {
				awaitingCIArmed = true
				if c := e.cache(); c != nil {
					c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), "fabrik:awaiting-ci")
				}
				if e.webhookMgr != nil {
					e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:awaiting-ci")
				}
				e.recordLabelAppliedAtNow(item, "fabrik:awaiting-ci")
				completionDurable = true
				e.setStatusLine(item, statusLineAwaitingCI(item.LinkedPRNumber)) // display-only (#2048)
			}
		}
		if completionDurable {
			e.resetStageDispatches(item, stage.Name)
		}
		// fabrik:awaiting-review is NOT seeded here when wait_for_ci: true.
		// Path 2 (checkReviewGate in the catch-up loop) handles the review gate
		// after the CI gate clears and stage:X:complete is added (#617). Review
		// *feedback* is a separate matter: settleAwaitingCIScan's handler chain
		// detects and dispatches it during the CI wait (#1953 R1), without
		// writing that label.
		// fabrik:awaiting-ci is the durable completion signal of this Validate stage
		// run, so the forced re-run marker (#2378) can go. A comment pass never gets
		// here while it is set (finalizeComments), so this is a real stage run.
		if stage.Name == "Validate" && awaitingCIArmed {
			e.clearValidateRerun(item, labelsLive)
		}
		e.logf(item.Number, "awaiting-ci", "deferring stage:%s:complete until CI gate clears\n", stage.Name)
		return // catch-up loop adds stage:X:complete when checkCIGate clears
	}

	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); err != nil {
		e.logf(item.Number, "warn", "could not add completion label: %v\n", err)
	} else {
		// The completion is durable: the dispatch count of the episode ends (#2395).
		e.resetStageDispatches(item, stage.Name)
		if c := e.cache(); c != nil {
			c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), completeLabel)
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+completeLabel)
		}
		if stage.Name == "Validate" {
			// The completion label is the durable signal of this Validate stage
			// run: the forced re-run marker (#2378) can go.
			e.clearValidateRerun(item, labelsLive)
			repoStr := itemOwnerRepoString(item, e.defaultRepo())
			wm := e.worktreesFor(item.Repo)
			if sha, shaErr := gitRevParse(wm.WorktreeDir(item.Number), "HEAD"); shaErr == nil && sha != "" {
				e.store.Apply(itemstate.ValidateCompletedAtSHA{Repo: repoStr, Number: item.Number, SHA: sha})
				e.logf(item.Number, "validate-sha", "recorded completion SHA %s\n", sha)
			} else {
				e.logf(item.Number, "warn", "could not record completion SHA: %v\n", shaErr)
			}
		}
	}

	// fabrik:yolo or fabrik:cruise label overrides stage.AutoAdvance — if the user
	// explicitly labelled the issue, respect that over YAML config.
	shouldAdvance := yoloActive || cruiseActive
	if stage.AutoAdvance != nil && !hasYoloLabel(item) && !hasCruiseLabel(item) {
		shouldAdvance = *stage.AutoAdvance
	}

	// cruise stops at Validate: do not advance to Done or trigger any further
	// stage movement. cruise wins over yolo — when both labels are present,
	// the PR is left for human merge (FR-003, FR-015).
	if hasCruiseLabel(item) && stage.Name == "Validate" {
		shouldAdvance = false
	}

	// If GitHub auto-merge was just enabled, defer Done advancement to
	// checkAutoMergeConvergence in the catch-up loop. Done cleanup must not
	// run until the PR is actually merged by GitHub.
	if autoMergeEnabled {
		shouldAdvance = false
	}

	// #1207 guard 1: an unresolved review thread on the current head deferred
	// attemptMergeOnValidate — do not advance. poll.go's Phase 2 Validate
	// branch retries attemptMergeOnValidate on every subsequent poll, so this
	// resolves itself once the review-reinvoke loop (§2.9) clears the thread.
	// The same "landing deferred" return carries the #1862 comment gate: an
	// unmerged item with a pending comment must never fall through to
	// advanceToNextStage (Done) here.
	if landingDeferred {
		shouldAdvance = false
	}

	if shouldAdvance {
		// Pause guard (#2341): refuse before any gate that writes a label or records a
		// cooldown, so the refusal leaves every label as it was.
		if paused {
			e.refuseIfPausedOrUnread(item, labelsLive, "advancing")
			return
		}
		if e.checkDependencies(board, item, stage) {
			return // blocked; checkDependencies handled label + comment
		}
		// Feedback gate (#1953 R2/R3): `item` is the pre-run snapshot, so feedback
		// that landed while this invocation ran is invisible to it. The run is
		// allowed to finish (nothing is injected mid-run); before the completion
		// is treated as final and the item advances, consult the live predicate.
		// On a hold the stage keeps stage:<X>:complete and does not advance —
		// "final" means advance or landing, not the label — and Phase 1's
		// handleReviewGate / the comment path dispatches the feedback on the next
		// poll, under the ordinary reinvoke bounds, before the catch-up loop
		// advances the item again.
		if e.feedbackGateBlocks(item, false, "advance") {
			return
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
				} else {
					if c := e.cache(); c != nil {
						c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), "fabrik:awaiting-review")
					}
					if e.webhookMgr != nil {
						e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:awaiting-review")
					}
					e.recordLabelAppliedAtNow(item, "fabrik:awaiting-review")
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

// attemptMergeOnValidate enables GitHub native auto-merge for the linked PR
// of a yolo issue at Validate completion. Returns (true, false, nil) when
// auto-merge was enabled (or is already enabled as an idempotency guard),
// (false, false, nil) when no action is needed (cruise label, review gate
// blocking, no linked PR), (false, true, nil) when landing is deferred — an
// unresolved review thread exists on the current head (#1207 guard 1) or an
// unprocessed feedback is pending (#1862/#1953, feedbackGateBlocks) — and
// (false, false, err) on failure. A deferred return tells handleStageComplete
// to stop rather than fall through to advanceToNextStage. The fabrik:auto-merge-enabled label serves
// as both the idempotency guard and the budget-start anchor read by
// checkAutoMergeConvergence.
//
// This function is the single landing-decision owner for both merge_train modes
// (ADR-058/ADR-059 "invoke, don't relocate"), which is why the wait_for_reviews
// gate is enforced here rather than at either call site — see
// reviewGateBlocksLanding and ADR-1216.
func (e *Engine) attemptMergeOnValidate(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) (enabled bool, deferred bool, err error) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// cruise > yolo: when cruise is present, auto-merge is suppressed regardless of yolo.
	// cruise auto-advances through stages but leaves the PR for human merge at Validate.
	//
	// Freshness invariant (#1769): this check relies on item.Labels already being
	// live. Both callers (handleStageComplete, runCatchUpPhase2) call
	// e.refreshAutonomyLabels upstream of every attemptMergeOnValidate call site,
	// so item.Labels is guaranteed fresh by the time execution reaches here. A
	// future edit to either caller must not reorder a new label check ahead of
	// that refresh, or this check goes stale again.
	//
	// Note: the dependency guard below (FetchItemDetails, ADR-1419) performs its
	// own live re-read for an unrelated purpose (BlockedBy), and as a side effect
	// of its GraphQL query also resets item.Labels — overwriting the REST-fresh
	// snapshot refreshAutonomyLabels just produced. That's harmless today because
	// this check and the fabrik:auto-merge-enabled check just below it both run
	// before that overwrite, and nothing in this function reads
	// hasYoloLabel/hasCruiseLabel afterward — but a future label check added
	// after the dependency guard must not assume it's reading the REST-fresh
	// snapshot; it would actually see the GraphQL-refetched one instead.
	if hasCruiseLabel(item) {
		return false, false, nil
	}

	// Idempotency: auto-merge was already enabled on a prior run.
	if hasLabel(item.Labels, "fabrik:auto-merge-enabled") {
		return true, false, nil
	}

	// Pause guard (#2341): a fabrik:paused applied while the stage ran (or before the
	// listing showed it) wins over the landing. Checked here, ahead of the re-read
	// below, against the REST-fresh labels refreshAutonomyLabels produced — the
	// re-read replaces them with GraphQL ones that can lag a REST write. Returns the
	// deferred shape so handleStageComplete never falls through to an advance.
	if e.refuseIfPaused(item, "landing") {
		return false, true, nil
	}

	// Dependency guard (ADR-1419): re-verify BlockedBy live, immediately
	// before any landing decision. A Review/Validate mid-flight spawn
	// (finalizeStageOutcome, engine/item.go) can create a brand-new
	// blockedBy edge within this very dispatch — after Claude already
	// emitted FABRIK_STAGE_COMPLETE alongside the spawn block — so the
	// `item` snapshot passed in here predates that edge entirely.
	// checkDependencies's own live-reread (engine/dependencies.go) only
	// fires when the item is *already* fabrik:blocked, which it never is
	// yet on a same-dispatch edge, so it cannot catch this on its own.
	// Without this guard, a Validate agent that (against its own skill
	// instructions — see "If You Discover a Blocking Issue" in
	// the validate skill/SKILL.md) emits a spawn block together with
	// FABRIK_STAGE_COMPLETE on a yolo issue could reach MergePR/auto-merge
	// before fabrik:blocked is ever applied — reproducing, one dispatch
	// earlier, the exact silent-merge-past-an-undeclared-blocker danger
	// this issue exists to close. Both callers of this function
	// (handleStageComplete and poll.go's catch-up loop) already gate on
	// yolo and return above once fabrik:auto-merge-enabled is set, so this
	// live fetch is bounded to the same narrow per-Validate-completion
	// window guard 1 below already re-fetches live data in.
	fresh := item
	liveRead := true
	if ferr := e.client.FetchItemDetails(&fresh); ferr != nil {
		e.logf(item.Number, "warn", "attemptMergeOnValidate: live re-read of dependencies failed (%v) — using possibly-stale snapshot\n", ferr)
		liveRead = false
	} else {
		item = fresh
		// Second pause check (#2341), against the labels the re-read just returned.
		if e.refuseIfPaused(item, "landing") {
			return false, true, nil
		}
	}
	if e.checkDependencies(board, item, stage) {
		e.logf(item.Number, "yolo-merge-guard", "not merging: dependencies are open (live re-check)\n")
		return false, false, nil
	}

	// Guard 1 (#1207): do not advance out of Validate or enable auto-merge/
	// merge-queue while an unresolved review thread exists on the current
	// head. Scoped to the current head via currentHeadReviewThreadComments
	// (excludes GitHub-marked isOutdated threads) so a thread against a
	// superseded commit never blocks indefinitely. The existing catch-up
	// loop (poll.go's Phase 2 Validate branch) retries this function on
	// every poll while fabrik:auto-merge-enabled is absent, so once the
	// thread is resolved by the review-reinvoke loop (§2.9), the very next
	// poll re-enters here and proceeds — no separate retry plumbing needed.
	//
	// Freshness note: at the primary call site (handleStageComplete, invoked
	// synchronously right after a Claude invocation completes), item is the
	// same pre-invocation snapshot Path 1 already treats as stale everywhere
	// else in this function (see the wait_for_reviews comment below) — it can
	// be tens of minutes old by the time a long Validate run finishes, not
	// just a few seconds. A thread posted during that run is invisible to
	// this check. This is deliberately not solved by re-fetching here (that
	// would re-add the per-stage-completion GraphQL round-trip Path 1 exists
	// to avoid); instead it is closed by guard 2 on the very next poll — by
	// then fabrik:auto-merge-enabled is set, poll.go's deep-fetch has
	// refreshed the item, and handleReviewGate's guard 2 disables auto-merge
	// before GitHub can act on it. The exposure this leaves is the same
	// PollSeconds-bounded residual race already accepted for guard 2's
	// convergence-window disable, not a new unbounded gap. poll.go's Phase 2
	// Validate retry (the other caller of this function) always sees
	// deep-fetched, fresh data, so it is not subject to this note.
	if blocking := e.currentHeadReviewThreadComments(item); len(blocking) > 0 {
		e.logf(item.Number, "yolo-merge-guard", "not advancing: %d unresolved review thread(s) on %s\n",
			len(blocking), item.LinkedPRHeadSHA)
		return false, true, nil
	}

	// Review gate (#1216): a wait_for_reviews stage must not reach any landing
	// action while reviewer requests are outstanding. Sits ahead of the
	// merge_train fork so both modes are gated identically (FR-3), and re-reads
	// review state live so a reviewer requested during the CI-await window still
	// blocks (FR-2).
	if e.reviewGateBlocksLanding(item, stage, owner, repo) {
		return false, false, nil
	}

	// Feedback gate (#1862 comments, #1953 review threads and bodies): unprocessed
	// feedback must not be merged past or advanced to Queued — dispatch processes
	// it after this poll's catch-up loop, by which point the merge would already
	// have happened and the rework would be committed to a dead branch. Reads the
	// live item the re-read above just refreshed (a failed re-read holds), and
	// sits ahead of the merge_train fork so all three landing paths are gated
	// identically. Returns deferred=true so handleStageComplete never falls
	// through to advanceToNextStage on an unmerged item.
	if e.feedbackGateBlocks(item, liveRead, "landing decision") {
		return false, true, nil
	}

	// Observation only (#1968 R2, R10): every landing gate has passed — the
	// engine is about to queue or merge. Recorded here because a yolo item lands
	// inside this call and never reaches runCatchUpPhase2. Changes nothing.
	// The judged head (#2342): the PR head the review, thread and feedback gates
	// just evaluated, taken once from the live re-read above. It is handed to
	// landValidated, which pins every merge, auto-merge enablement and enqueue
	// to it — the landing never reads a head of its own. When the CI gate
	// recorded the head it cleared (wait_for_ci stages), it must be this same
	// commit; a different one means the CI verdict is for a superseded head.
	judgedHead := item.LinkedPRHeadSHA
	if ciHead, ok := e.takeCIJudgedHead(item); ok && judgedHead != "" && ciHead != judgedHead {
		e.deferLandingHeadMoved(item, ciHead, judgedHead)
		return false, true, nil
	}

	// Land only the head Validate verified (#2342): the gates judge the head as it is
	// now, which is not necessarily the head Validate completed on. If a commit was
	// pushed after Validate completed, the settle scan that would strip the completion
	// runs after this landing decision in the poll, so run its rule here, against the
	// head just read live. An invalidation re-runs Validate on the new head and defers
	// this landing silently, like a moved head.
	if !e.landingValidateSHACheckOff.Load() && e.checkValidateSHAInvalidationAt(item, judgedHead) {
		e.logf(item.Number, "landing", "not landing: head %s is not the head Validate completed on — Validate re-runs on it, landing deferred\n", judgedHead)
		return false, true, nil
	}

	landing := e.noteValidateLanding(item, stage)
	enabled, deferred, err = e.landValidated(ctx, board, item, owner, repo, judgedHead)
	// Announced only once the merge or enqueue went through (#1968 R2): a failed
	// landing, e.g. a conflict leading to fabrik:rebase-needed, announces nothing.
	landing.finish(err == nil && !deferred && (enabled || e.cfg.MergeTrain == "on"))
	return enabled, deferred, err
}

// landValidated performs the landing action once every gate has passed: advance
// to Queued under the merge train, otherwise enqueue, auto-merge or merge directly.
//
// judgedHead is the PR head the gates evaluated for this decision (#2342). Every
// write below names it, so GitHub itself refuses a head that moved: the refusal
// (gh.ErrHeadMoved) is a silent deferral — no label, comment, pause or counter —
// and the next poll judges the new head. An empty judgedHead is never papered
// over with a head read here (FR-008): the landing defers.
func (e *Engine) landValidated(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, owner, repo, judgedHead string) (enabled bool, deferred bool, err error) {
	// Merge-train gate: when merge_train: on, advance to Queued instead of enabling auto-merge.
	// Cruise items always bypass this (handled above). New items never reach fabrik:auto-merge-enabled
	// when merge_train: on, so this gate fires exactly once per qualifying Validate completion.
	// Pause guard (#2341): entry check so this site is safe on its own.
	if e.refuseIfPaused(item, "landing") {
		return false, true, nil
	}
	if e.cfg.MergeTrain == "on" {
		advanceErr := e.advanceToQueued(ctx, board, item, owner, repo)
		if errors.Is(advanceErr, errLandingPaused) {
			return false, true, nil
		}
		return false, false, advanceErr
	}

	pr, err := e.readClient.FetchLinkedPR(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "could not fetch linked PR for auto-merge: %v — will retry\n", err)
		return false, false, fmt.Errorf("fetch linked PR: %w", err)
	}
	if pr == nil {
		e.logf(item.Number, "warn", "no linked PR found at Validate completion; skipping auto-merge\n")
		return false, false, nil
	}

	// The PR read above supplies the number and the merge-queue flag only; its
	// head may be up to linkedPRCacheTTL old and is never used to pin (#2342).
	if judgedHead == "" {
		e.logf(item.Number, "landing", "PR #%d: no judged head for this landing decision — deferring, not issuing an unpinned merge\n", pr.Number)
		return false, true, nil
	}

	// Enqueue path: when the repo requires a merge queue and merge-queue routing is not disabled.
	//
	// Merge-queue awareness audit (ADR-058 D3 FR-1): this early-return IS the guard for
	// the direct-merge fallback below. On a queue-enabled repo (default config),
	// the enqueue path returns here before MergePR is ever reached — a queued PR is never
	// directly merged (a direct merge on a queue-required branch returns HTTP 405). The
	// only way to reach the direct-merge fallback on a queue repo is merge_queue: off (the
	// kill-switch), an operator's explicit choice. No separate guard is required at the
	// merge site; this early-return is the audit evidence.
	if e.cfg.MergeQueue != "off" && pr.IsMergeQueueEnabled {
		queued, queueErr := e.enqueueForQueue(owner, repo, item, pr.Number, judgedHead)
		if errors.Is(queueErr, gh.ErrHeadMoved) {
			e.deferLandingHeadMoved(item, judgedHead, "")
			return false, true, nil
		}
		return queued, false, queueErr
	}

	strategy := e.cfg.AutoMergeStrategy
	if strategy == "" {
		strategy = "MERGE"
	}
	if err := e.client.EnablePullRequestAutoMerge(owner, repo, pr.Number, strategy, judgedHead); err != nil {
		if errors.Is(err, gh.ErrHeadMoved) {
			e.deferLandingHeadMoved(item, judgedHead, "")
			return false, true, nil
		}
		if errors.Is(err, gh.ErrAutoMergeNotEnabled) {
			e.logf(item.Number, "warn", "auto-merge is not enabled for this repository — "+
				"enable it in Settings → General → Allow auto-merge; Fabrik will retry on the next poll\n")
			return false, false, fmt.Errorf("enabling auto-merge on PR #%d: %w", pr.Number, err)
		}
		// Any error other than ErrAutoMergeNotEnabled and ErrHeadMoved means the PR is in a terminal
		// GitHub state (CLEAN, UNSTABLE, or any future variant) where auto-merge cannot
		// be queued. Fall back to a direct merge call. If that also fails (e.g. DIRTY),
		// surface the MergePR error so existing rebase/CI-fix gates can act on it.
		e.logf(item.Number, "info", "PR #%d: enable auto-merge failed (%v) — falling back to direct merge\n", pr.Number, err)

		// #1207 guard 1 supplement: this direct-merge fallback is the one place
		// in this function where "proceed now" and "no more guards will ever
		// run" happen at the same instant. Everywhere else in this function,
		// a miss on guard 1's stale item snapshot (see the freshness note
		// above) is closed by guard 2 on the next poll, because enabling
		// auto-merge/enqueuing opens a convergence window that guard 2
		// monitors. MergePR below merges synchronously — there is no window,
		// no next poll, nothing for guard 2 to catch. So re-check live,
		// immediately before the point of no return, instead of trusting the
		// stale snapshot guard 1 already checked.
		fresh := item
		if ferr := e.client.FetchItemDetails(&fresh); ferr != nil {
			return false, false, fmt.Errorf("direct-merge fallback: live re-read of review threads for PR #%d: %w", pr.Number, ferr)
		}
		// Pause guard (#2341): the last live read before the irreversible merge.
		if e.refuseIfPaused(fresh, "merging (direct-merge fallback)") {
			return false, true, nil
		}
		// #2342: this re-read is a second head observation. A different head
		// means the gates never judged what is there now — defer rather than
		// merge it (and the pinned merge below would refuse it anyway).
		if fresh.LinkedPRHeadSHA != "" && fresh.LinkedPRHeadSHA != judgedHead {
			e.deferLandingHeadMoved(item, judgedHead, fresh.LinkedPRHeadSHA)
			return false, true, nil
		}
		if blocking := e.currentHeadReviewThreadComments(fresh); len(blocking) > 0 {
			e.logf(item.Number, "yolo-merge-guard", "not advancing: %d unresolved review thread(s) on %s (direct-merge fallback)\n",
				len(blocking), fresh.LinkedPRHeadSHA)
			return false, true, nil
		}
		// #1862/#1953: same reason — narrow the check-then-merge window for
		// feedback that arrived after the feedback gate's read above.
		if e.feedbackGateBlocks(fresh, true, "direct merge") {
			return false, true, nil
		}
		if mergeErr := e.client.MergePRAtHeadSHA(owner, repo, pr.Number, judgedHead); mergeErr != nil {
			if errors.Is(mergeErr, gh.ErrHeadMoved) {
				e.deferLandingHeadMoved(item, judgedHead, "")
				return false, true, nil
			}
			return false, false, fmt.Errorf("direct merge fallback on PR #%d: %w", pr.Number, mergeErr)
		}
		// #2123: announce the landing at the merge itself, before GitHub's close
		// of the linked issue reaches the store and before any close tracking.
		e.emitLanded(item, pr.Number)
		// Apply idempotency guard and convergence anchor after successful direct merge.
		if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:auto-merge-enabled"); lerr != nil {
			e.logf(item.Number, "warn", "could not add fabrik:auto-merge-enabled label: %v\n", lerr)
		} else {
			if c := e.cache(); c != nil {
				c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), "fabrik:auto-merge-enabled")
			}
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:auto-merge-enabled")
			}
			e.recordLabelAppliedAtNow(item, "fabrik:auto-merge-enabled")
		}
		e.logf(item.Number, "info", "PR #%d merged directly (auto-merge unavailable fallback)\n", pr.Number)
		return true, false, nil
	}

	// Apply fabrik:auto-merge-enabled as idempotency guard and budget-start anchor.
	if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:auto-merge-enabled"); lerr != nil {
		e.logf(item.Number, "warn", "could not add fabrik:auto-merge-enabled label: %v\n", lerr)
	} else {
		if c := e.cache(); c != nil {
			c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), "fabrik:auto-merge-enabled")
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:auto-merge-enabled")
		}
		e.recordLabelAppliedAtNow(item, "fabrik:auto-merge-enabled")
	}

	e.logf(item.Number, "info", "GitHub auto-merge enabled on PR #%d (%s) — awaiting GitHub atomic merge\n", pr.Number, strategy)
	return true, false, nil
}

// deferLandingHeadMoved is the silent deferral for a landing whose PR head is no
// longer the one the gates judged (#2342): one log line and an admission
// cooldown so the next poll re-evaluates the new head. It writes no label or
// comment, pauses nothing and touches no retry, cycle or ejection counter.
// observed names the head seen instead; empty when GitHub refused the pin.
func (e *Engine) deferLandingHeadMoved(item gh.ProjectItem, judged, observed string) {
	short := func(sha string) string { return sha[:min(8, len(sha))] }
	if observed != "" {
		e.logf(item.Number, "landing", "PR head moved (judged %s, now %s) — deferring; the new head is evaluated on the next poll\n", short(judged), short(observed))
	} else {
		e.logf(item.Number, "landing", "PR head moved since it was judged (%s) — deferring; the new head is evaluated on the next poll\n", short(judged))
	}
	e.store.Apply(itemstate.CooldownRecorded{
		Repo:   itemOwnerRepoString(item, e.defaultRepo()),
		Number: item.Number,
		Reason: mergeUnsettledCooldownReason,
		Until:  e.now().Add(e.mergeGateRecheckInterval()),
	})
}

// enqueueForQueue enqueues a linked PR into the repository's native merge queue
// (ADR-058) and applies the fabrik:auto-merge-enabled label as both idempotency
// guard and the convergence anchor read by checkAutoMergeConvergence. It performs
// the boardcache write-through and webhook echo so the label is visible without a
// GitHub round-trip. Returns (true, nil) on successful enqueue, (false, err) if the
// enqueue mutation fails (a label-add failure is non-fatal: logged and swallowed).
//
// This is the ADR-058 enqueue path, extracted from attemptMergeOnValidate so it can
// be invoked from two convergence-owner call sites (ADR-059 D6 "invoke, don't
// relocate"): (1) attemptMergeOnValidate on the merge_train: off precedence path,
// and (2) handleMergeTrainBatch's per-repo engine selection for queue-enabled repos
// when merge_train: on. Both callers apply the MergeQueue != "off" && merge-queue-
// enabled guard before calling; the guard is deliberately kept at the call sites (it
// doubles as the HTTP-405 direct-merge audit evidence noted in attemptMergeOnValidate).
//
// The head SHA is passed by the caller from poll-native state (pr.HeadSHA or the
// GraphQL-populated item.LinkedPRHeadSHA) — never from a REST re-fetch of the queue
// flag, per ADR-058/ADR-059 FR-1.
func (e *Engine) enqueueForQueue(owner, repo string, item gh.ProjectItem, prNumber int, headSHA string) (bool, error) {
	if err := e.client.EnqueuePullRequest(owner, repo, prNumber, headSHA); err != nil {
		return false, fmt.Errorf("enqueue PR #%d: %w", prNumber, err)
	}
	if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:auto-merge-enabled"); lerr != nil {
		e.logf(item.Number, "warn", "could not add fabrik:auto-merge-enabled label: %v\n", lerr)
	} else {
		if c := e.cache(); c != nil {
			// Use the resolved owner/repo (not item.Repo, which may be empty when it
			// defaults to the default repo) so the cache key matches the stored entry
			// and the write-through actually lands on the right item — consistent with
			// the webhook-echo key below.
			c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), "fabrik:auto-merge-enabled")
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:auto-merge-enabled")
		}
		e.recordLabelAppliedAtNow(item, "fabrik:auto-merge-enabled")
	}
	e.logf(item.Number, "info", "PR #%d enqueued into merge queue — awaiting GitHub merge\n", prNumber)
	return true, nil
}

// handleNoWorkNeeded is called when a stage outputs both FABRIK_STAGE_COMPLETE and
// FABRIK_NO_WORK_NEEDED. It durably records the no-work-needed decision (the
// fabrik:awaiting-done marker) as its very first mutation — before any other API
// call — so a rate-limited or otherwise-failed invocation still leaves a trace that
// this issue's fate was decided, and does not silently fall back into the normal
// pipeline on a later poll (see adrs/ for the durable-marker rationale). The rest of
// the work (completion labels, skip comments, the Done move, and the issue close) is
// delegated to settleNoWorkNeeded, which is idempotent and safe to retry.
func (e *Engine) handleNoWorkNeeded(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "done", "stage %q signaled no work needed — skipping remaining stages and moving to Done\n", stage.Name)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Feedback gate (#2220 FR-004): the no-work decision ends in a Done move, an
	// advance that must not outrun an unprocessed comment. Held before the
	// awaiting-done marker is written, so there is nothing to retract; the stage
	// keeps stage:<X>:complete, the comment path handles the comment next poll
	// (stage has run), and its own completion advances the item normally.
	if !hasLabel(item.Labels, "fabrik:awaiting-done") && e.feedbackGateBlocks(item, false, "no-work-needed") {
		completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
		if !hasLabel(item.Labels, completeLabel) {
			if err := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); err != nil {
				e.logf(item.Number, "warn", "could not add completion label: %v\n", err)
			} else {
				e.resetStageDispatches(item, stage.Name) // durable completion ends the episode (#2395)
				if c := e.cache(); c != nil {
					c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), completeLabel)
				}
				if e.webhookMgr != nil {
					e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+completeLabel)
				}
			}
		}
		return
	}

	if !hasLabel(item.Labels, "fabrik:awaiting-done") {
		if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-done"); err != nil {
			e.logf(item.Number, "warn", "could not add awaiting-done marker: %v\n", err)
		} else {
			e.resetStageDispatches(item, stage.Name) // durable completion ends the episode (#2395)
			if c := e.cache(); c != nil {
				c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), "fabrik:awaiting-done")
			}
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:awaiting-done")
			}
		}
	}

	e.settleNoWorkNeeded(board, item, stage)
}

// holdingStage returns the first stage in cfg with HoldingStage: true, or nil if
// none. A stage also marked Unmanaged is never returned — Unmanaged stages are
// never a valid resolution target (see the Unmanaged field doc in stages/stages.go).
func holdingStage(cfg Config) *stages.Stage {
	for _, s := range cfg.Stages {
		if s.HoldingStage && !s.Unmanaged {
			return s
		}
	}
	return nil
}

// cleanupStage returns the lowest-Order stage in cfg with CleanupWorktree: true,
// or nil if none is configured. Used by settleClosedItemsToDone to locate the
// Done column by its behavioral flag rather than a hardcoded stage name. A stage
// also marked Unmanaged is never returned, for the same reason as holdingStage.
func cleanupStage(cfg Config) *stages.Stage {
	var cleanup *stages.Stage
	for _, s := range cfg.Stages {
		if s.CleanupWorktree && !s.Unmanaged && (cleanup == nil || s.Order < cleanup.Order) {
			cleanup = s
		}
	}
	return cleanup
}

// advanceToQueued moves an item from Validate to the configured holding stage column
// when merge_train: on. It sets the board status and adds stage:Validate:complete
// so the Phase 2 catch-up loop does not re-dispatch Validate on the next poll.
func (e *Engine) advanceToQueued(_ context.Context, board *gh.ProjectBoard, item gh.ProjectItem, owner, repo string) error {
	// Pause guard (#2341): a paused item is not moved to Queued and gets no label.
	if e.refuseIfPaused(item, "moving to the holding stage") {
		return errLandingPaused
	}
	hs := holdingStage(e.cfg)
	if hs == nil {
		return fmt.Errorf("merge_train: on but no holding stage (HoldingStage: true) configured in stages")
	}
	if e.statusField == nil {
		return fmt.Errorf("status field metadata not available; cannot advance to %s", hs.Name)
	}

	optionID, ok := e.statusField.Options[hs.Name]
	if !ok {
		return fmt.Errorf("no status option %q found on project board (available: %v); is the %s column missing?",
			hs.Name, mapKeys(e.statusField.Options), hs.Name)
	}

	projectID, pidErr := e.writeProjectID(board)
	if pidErr != nil {
		return fmt.Errorf("move to %s: %w", hs.Name, pidErr)
	}
	e.logf(item.Number, "merge-train", "advancing to %s for merge train batching\n", hs.Name)
	if err := e.client.UpdateProjectItemStatus(projectID, item.ItemID, e.statusField.FieldID, optionID); err != nil {
		return fmt.Errorf("move to %s: %w", hs.Name, err)
	}
	if c := e.cache(); c != nil {
		c.UpdateItemStatus(boardcache.ItemKey(owner+"/"+repo, item.Number), hs.Name)
	}
	// Advances the probe staleness baseline (#1090) — the status move above just
	// bumped the item's real GitHub updatedAt via the project-item mutation.
	e.store.Apply(itemstate.SelfWriteObserved{Repo: owner + "/" + repo, Number: item.Number})
	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEchoIfSubscribed("projects_v2_item", "edited", item.ItemID)
	}

	// Add stage:Validate:complete so the Phase 2 catch-up loop does not re-dispatch Validate.
	completeLabel := "stage:Validate:complete"
	if lerr := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); lerr != nil {
		e.logf(item.Number, "warn", "advanced to %s but could not add %s label: %v — will retry on next poll\n", hs.Name, completeLabel, lerr)
		return fmt.Errorf("add %s label: %w", completeLabel, lerr)
	}
	if c := e.cache(); c != nil {
		c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), completeLabel)
	}
	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+completeLabel)
	}

	e.logf(item.Number, "merge-train", "holding in %s — waiting for batch\n", hs.Name)
	e.setStatusLine(item, statusLineQueuedWaiting)
	return nil
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

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	projectID, pidErr := e.writeProjectID(board)
	if pidErr != nil {
		return fmt.Errorf("moving to %s: %w", next.Name, pidErr)
	}
	e.logf(item.Number, "advance", "moving to stage %q\n", next.Name)
	// write-through: already covered by c.UpdateItemStatus call in the else block below
	err := e.client.UpdateProjectItemStatus(projectID, item.ItemID, e.statusField.FieldID, optionID)
	if err == nil {
		if c := e.cache(); c != nil {
			c.UpdateItemStatus(boardcache.ItemKey(owner+"/"+repo, item.Number), next.Name)
		}
		// Advances the probe staleness baseline (#1090) — the status move above
		// just bumped the item's real GitHub updatedAt via the project-item mutation.
		e.store.Apply(itemstate.SelfWriteObserved{Repo: owner + "/" + repo, Number: item.Number})
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEchoIfSubscribed("projects_v2_item", "edited", item.ItemID)
		}
		// The item left its stage: whatever line described the old one is stale.
		// This is the single funnel for Done moves (merge-train landings, the
		// ordinary merge, no-work-needed, closed-item advance), so it is also the
		// "clear at Done" rule (#2048); a following stage dispatch writes its own
		// line.
		e.clearStatusLine(item)
	}
	return err
}
