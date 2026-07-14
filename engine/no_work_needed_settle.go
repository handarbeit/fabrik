package engine

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// noWorkNeededRetryStage is a dedicated, non-real stage name used to key the
// existing StageRetryIncremented/StageRetryCleared/Attempts counter for retries of
// a stalled no-work-needed Done-move/close. The double-underscore wrapping makes it
// unrepresentable as a real YAML stage `name:`, so it can never collide with a
// configured stage's retry count.
const noWorkNeededRetryStage = "__no_work_needed__"

// hasSkippedComment reports whether item already carries the "skipped: no work
// needed" comment for stageName, so a retried settleNoWorkNeeded pass does not
// re-post it.
func hasSkippedComment(item gh.ProjectItem, stageName string) bool {
	marker := fmt.Sprintf("FABRIK_NO_WORK_NEEDED emitted by %s", stageName)
	for _, c := range item.Comments {
		if strings.Contains(c.Body, marker) {
			return true
		}
	}
	return false
}

// settleNoWorkNeeded performs the work of the no-work-needed decision: marking the
// emitting stage (and all subsequent non-cleanup stages) complete with a "skipped"
// comment trail, moving the issue to Done, and closing it. Every sub-step checks
// current state (hasLabel / hasSkippedComment / item.Status / item.IsClosed) before
// mutating, so the function is idempotent and safe to call repeatedly — the
// fabrik:awaiting-done marker (written by handleNoWorkNeeded before this is ever
// called) keeps the issue out of normal dispatch while retries land here on later
// polls. Called both from handleNoWorkNeeded (first attempt) and from the settle
// scan in poll.go (retries).
func (e *Engine) settleNoWorkNeeded(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	allStepsOK := true

	// The label/comment marking steps below are only meaningful before the board
	// move to Done succeeds. Once item.Status == "Done", these steps are
	// guaranteed to have already completed successfully in an earlier pass — the
	// move to Done is only ever attempted after allStepsOK, below, so item.Status
	// cannot be "Done" unless every step here already landed. Skipping them here
	// also sidesteps a stage-resolution hazard on retry: the settle scan in
	// poll.go re-derives `stage` via stages.FindStage(item.Status), which once the
	// board has moved to Done resolves to the cleanup stage itself rather than the
	// original emitting stage — using that resolved stage below would spuriously
	// add a stage:Done:complete label before the real Done-stage cleanup
	// (worktree removal) has ever run, permanently short-circuiting normal
	// cleanup dispatch for the item (see itemNeedsWork's CleanupWorktree branch).
	if item.Status != "Done" {
		// Clear any orphaned fabrik:awaiting-input label (same rationale as handleStageComplete).
		if hasLabel(item, "fabrik:awaiting-input") {
			if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil &&
				!errors.Is(err, gh.ErrNotFound) {
				e.logf(item.Number, "warn", "could not remove awaiting-input label: %v\n", err)
				allStepsOK = false
			} else if err == nil {
				if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
					cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-input")
				}
				if e.webhookMgr != nil {
					e.webhookMgr.RegisterEcho("issues", "unlabeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:awaiting-input")
				}
			}
		}

		// Mark the emitting stage complete so the engine doesn't re-run it on restart.
		completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
		if !hasLabel(item, completeLabel) {
			if err := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); err != nil {
				e.logf(item.Number, "warn", "could not add completion label for stage %q: %v\n", stage.Name, err)
				allStepsOK = false
			} else {
				if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
					cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), completeLabel)
				}
				if e.webhookMgr != nil {
					e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+completeLabel)
				}
				if stage.Name == "Validate" {
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
		}

		// Find the order boundary for the cleanup (Done) stage.
		doneOrder := math.MaxInt
		for _, s := range e.cfg.Stages {
			if s.CleanupWorktree && s.Order < doneOrder {
				doneOrder = s.Order
			}
		}

		// Add dummy completion labels and "skipped" comments for all subsequent non-cleanup stages.
		// The comment body must start with the canonical "🏭 **Fabrik" prefix so findNewComments
		// dedup prevents Fabrik from processing its own output on the next poll.
		//
		// Every skip comment for this decision carries identical text (it names the emitting
		// stage, not the individual skipped stage — see the Sprintf below), so they are
		// indistinguishable from each other in item.Comments. hasSkippedComment is therefore
		// checked once, before the loop, as a per-decision idempotency guard: if any skip
		// comment for this emitting stage already exists, assume the full comment set was
		// already posted and don't re-post. Skip labels remain independently idempotent
		// per-stage via hasLabel.
		skippedComment := fmt.Sprintf("🏭 **Fabrik — skipped: no work needed**\n\n_Skipped: no work needed (FABRIK_NO_WORK_NEEDED emitted by %s)._", stage.Name)
		alreadyPostedSkipComments := hasSkippedComment(item, stage.Name)
		for _, s := range e.cfg.Stages {
			if s.Order <= stage.Order || s.Order >= doneOrder {
				continue
			}
			skipLabel := fmt.Sprintf("stage:%s:complete", s.Name)
			if !hasLabel(item, skipLabel) {
				if err := e.client.AddLabelToIssue(owner, repo, item.Number, skipLabel); err != nil {
					e.logf(item.Number, "warn", "could not add skip label for stage %q: %v\n", s.Name, err)
					allStepsOK = false
				} else {
					if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
						cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), skipLabel)
					}
					if e.webhookMgr != nil {
						e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+skipLabel)
					}
				}
			}
			// Post the "skipped" comment — no rocket reaction, this is engine-generated metadata.
			if !alreadyPostedSkipComments {
				if dbID, err := e.client.AddComment(owner, repo, item.Number, skippedComment); err != nil {
					e.logf(item.Number, "warn", "could not post skipped comment for stage %q: %v\n", s.Name, err)
					allStepsOK = false
				} else {
					if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
						cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
							DatabaseID: dbID, Body: skippedComment, Author: e.cfg.User, CreatedAt: time.Now(),
						})
					}
					if e.webhookMgr != nil {
						e.webhookMgr.RegisterEcho("issue_comment", "created", boardcache.ItemKey(owner+"/"+repo, item.Number))
					}
				}
			}
		}
	}

	if !allStepsOK {
		e.logf(item.Number, "warn", "no-work-needed label/comment sub-steps incomplete — will retry next poll\n")
		e.recordNoWorkNeededRetry(item)
		return
	}

	// Move to Done, unless a previous pass already succeeded (item refetched at Done).
	if item.Status != "Done" {
		if e.statusField == nil {
			e.logf(item.Number, "warn", "status field metadata not available; cannot move to Done\n")
			e.recordNoWorkNeededRetry(item)
			return
		}
		optionID, ok := e.statusField.Options["Done"]
		if !ok {
			e.logf(item.Number, "warn", "no status option %q found on project board (available: %v); cannot move to Done\n",
				"Done", mapKeys(e.statusField.Options))
			e.recordNoWorkNeededRetry(item)
			return
		}
		e.logf(item.Number, "advance", "moving no-work-needed issue to Done\n")
		if err := e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID); err != nil {
			e.logf(item.Number, "warn", "could not move issue to Done: %v\n", err)
			e.recordNoWorkNeededRetry(item)
			return
		}
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.UpdateItemStatus(boardcache.ItemKey(item.Repo, item.Number), "Done")
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEchoIfSubscribed("projects_v2_item", "edited", item.ItemID)
		}
	}

	// Close the GitHub issue so it mirrors the normal pipeline close-on-merge path.
	// No webhook echo registered: applyIssuesDelta's "closed" case never calls
	// matchEchoFn, so any registered echo would expire unused. The ApplyIssueClosed
	// write-through handles cache coherence immediately.
	if !item.IsClosed {
		if err := e.client.CloseIssue(owner, repo, item.Number); err != nil {
			e.logf(item.Number, "warn", "could not close issue (no work needed): %v\n", err)
			e.recordNoWorkNeededRetry(item)
			return
		}
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyIssueClosed(boardcache.ItemKey(item.Repo, item.Number))
		}
		e.logf(item.Number, "done", "closed issue (no work needed)\n")
	}

	// Fully settled — clear the durable marker and the retry counter.
	e.clearNoWorkNeededMarker(item, owner, repo)
}

// recordNoWorkNeededRetry increments the in-memory retry counter for a stalled
// no-work-needed Done-move/close, keyed by the dedicated noWorkNeededRetryStage
// constant (not the emitting stage's real name — see the ADR for rationale).
// Escalates via escalateNoWorkNeededFailure once e.cfg.MaxRetries is reached.
func (e *Engine) recordNoWorkNeededRetry(item gh.ProjectItem) {
	if e.cfg.MaxRetries <= 0 {
		return
	}
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.StageRetryIncremented{Repo: repoStr, Number: item.Number, StageName: noWorkNeededRetryStage})
	var count int
	if snap, err := e.store.Get(repoStr, item.Number); err == nil {
		count = snap.Attempts(noWorkNeededRetryStage)
	}
	if count >= e.cfg.MaxRetries {
		e.escalateNoWorkNeededFailure(item)
	}
}

// escalateNoWorkNeededFailure is called when the outstanding no-work-needed
// Done-move/close has failed MaxRetries times. It pauses the issue (fabrik:paused)
// so it stops being retried silently forever, removes the fabrik:awaiting-done
// marker (dispatch suppression is no longer needed once fabrik:paused takes over),
// and posts an explanatory comment with the manual recovery steps — mirroring
// escalatePRCreationFailure/escalateFailedStage.
func (e *Engine) escalateNoWorkNeededFailure(item gh.ProjectItem) {
	e.logf(item.Number, "escalate", "no-work-needed Done-move/close failed %d time(s) — pausing issue\n", e.cfg.MaxRetries)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
		e.logf(item.Number, "warn", "could not add paused label: %v\n", err)
	} else {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:paused")
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:paused")
		}
	}

	if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:awaiting-done"); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "could not remove awaiting-done marker: %v\n", err)
	} else if err == nil {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-done")
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "unlabeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:awaiting-done")
		}
	}

	comment := fmt.Sprintf(
		"🏭 **Fabrik — no-work-needed completion failed**\n\nThis issue was determined to need no code or documentation changes, but the board move to Done and/or the issue close could not be completed after %d attempt(s). The issue has been paused.\n\nManual fix:\n```\ngh issue close %d --repo %s/%s\n```\nThen move the issue's project-board column to Done yourself (no `gh` one-liner exists for Projects-v2 status fields).\n\nThen remove the `fabrik:paused` label. Do not remove it without completing the above — removing the `fabrik:paused` label without completing these steps may cause the issue to re-enter the normal pipeline unexpectedly.",
		e.cfg.MaxRetries, item.Number, owner, repo,
	)
	if dbID, err := e.client.AddComment(owner, repo, item.Number, comment); err != nil {
		e.logf(item.Number, "warn", "could not post no-work-needed escalation comment: %v\n", err)
	} else {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
				DatabaseID: dbID, Body: comment, Author: e.cfg.User, CreatedAt: time.Now(),
			})
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issue_comment", "created", boardcache.ItemKey(owner+"/"+repo, item.Number))
		}
		// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
		if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
			e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
		}
	}

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.EnginePaused{Repo: repoStr, Number: item.Number, StageName: noWorkNeededRetryStage})
}

// clearNoWorkNeededMarker removes the fabrik:awaiting-done marker and clears the
// retry counter once settleNoWorkNeeded has fully succeeded (status moved to Done
// and the issue closed).
func (e *Engine) clearNoWorkNeededMarker(item gh.ProjectItem, owner, repo string) {
	if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:awaiting-done"); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "could not remove awaiting-done marker: %v\n", err)
		return
	} else if err == nil {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-done")
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "unlabeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+"fabrik:awaiting-done")
		}
	}

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: noWorkNeededRetryStage})
}
