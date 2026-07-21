package engine

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// childPlacementLabel is the durable marker recording that a spawned child
// issue's initial project-board Status placement did not succeed and is
// still outstanding. Written on the CHILD issue (not the parent) at the
// UpdateProjectItemStatus call site in spawnChildren, covering all three
// failure branches there (call error, nil statusField, no suitable option).
// The child, board item, and blockedBy link already exist by the time this
// marker is written — only the board column is wrong — so this is a
// recoverable-in-place condition, unlike spawnChildren's other (fatal,
// parent-pausing) failure branches.
const childPlacementLabel = "fabrik:awaiting-placement"

// childPlacementRetryStage is a dedicated, non-real stage name used to key
// the existing StageRetryIncremented/StageRetryCleared/Attempts counter for
// retries of a stalled child board-placement, mirroring
// noWorkNeededRetryStage's double-underscore convention. Unrepresentable as
// a real YAML stage `name:`, so it can never collide with a configured
// stage's own retry count (or with noWorkNeededRetryStage).
const childPlacementRetryStage = "__child_placement__"

// childParentFooterRe extracts the parent owner/repo#number back-reference
// written into a spawned child's body by childFooter.
var childParentFooterRe = regexp.MustCompile(`Spawned by Fabrik from parent issue (\S+)/(\S+)#(\d+)`)

// recordChildPlacementFailure adds the durable childPlacementLabel marker to
// a spawned child issue whose initial project Status placement failed. The
// marker is retried independent of stage dispatch by the settle scan in
// poll.go: a stranded child sits in a column (typically Backlog) that
// resolves to no configured stage, so itemMayNeedWork/itemNeedsWork would
// never revisit it via the ordinary dispatch path.
func (e *Engine) recordChildPlacementFailure(childOwner, childRepo string, childNumber int) {
	e.addLabel(gh.ProjectItem{Repo: childOwner + "/" + childRepo, Number: childNumber}, childPlacementLabel)
}

// settleChildPlacement retries a spawned child's outstanding project Status
// placement. Sourced from board.Items directly by the poll.go settle scan,
// NOT deepFetchCandidates: a stranded child sits in a column with no
// matching configured stage, so it never passes itemMayNeedWork's
// stage == nil guard and never reaches deepFetchCandidates — the same
// sourcing spawnChildren's original (buggy) call site itself never needed,
// since it always runs inline right after creating the child.
func (e *Engine) settleChildPlacement(board *gh.ProjectBoard, item gh.ProjectItem) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	e.mu.Lock()
	sf := e.statusField
	e.mu.Unlock()

	optionID := resolveSpecifyOptionID(sf)
	if optionID == "" {
		e.logf(item.Number, "warn", "child placement settle: no Specify/processing status option available on %s/%s#%d — will retry next poll\n", owner, repo, item.Number)
		e.recordChildPlacementRetry(item)
		return
	}

	if err := e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, sf.FieldID, optionID); err != nil {
		e.logf(item.Number, "warn", "child placement settle: could not set project status on %s/%s#%d: %v\n", owner, repo, item.Number, err)
		e.recordChildPlacementRetry(item)
		return
	}

	e.logf(item.Number, "spawn", "child placement settle: successfully placed %s/%s#%d\n", owner, repo, item.Number)
	e.clearChildPlacementMarker(item, owner, repo)
}

// recordChildPlacementRetry increments the retry counter for a stalled child
// board-placement, keyed by the dedicated childPlacementRetryStage constant
// (not any real stage name — see the ADR for rationale). Escalates via
// escalateChildPlacementFailure once e.cfg.MaxRetries is reached.
func (e *Engine) recordChildPlacementRetry(item gh.ProjectItem) {
	if e.cfg.MaxRetries <= 0 {
		return
	}
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.StageRetryIncremented{Repo: repoStr, Number: item.Number, StageName: childPlacementRetryStage})
	var count int
	if snap, err := e.store.Get(repoStr, item.Number); err == nil {
		count = snap.Attempts(childPlacementRetryStage)
	}
	if count >= e.cfg.MaxRetries {
		e.escalateChildPlacementFailure(item)
	}
}

// escalateChildPlacementFailure is called when a spawned child's outstanding
// board placement has failed MaxRetries times. It pauses the child
// (fabrik:paused) so it stops being silently retried forever, removes the
// childPlacementLabel marker (dispatch suppression is no longer relevant
// once the item is simply left wherever it is — unlike fabrik:awaiting-done,
// this marker never suppressed normal dispatch in the first place, since the
// child's column never matched a configured stage to begin with), posts an
// explanatory comment on the child with manual recovery steps, and makes a
// best-effort attempt to notify the parent issue — mirroring
// escalateNoWorkNeededFailure/escalateFailedStage.
func (e *Engine) escalateChildPlacementFailure(item gh.ProjectItem) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	e.logf(item.Number, "escalate", "child board placement on %s/%s#%d failed %d time(s) — pausing issue\n", owner, repo, item.Number, e.cfg.MaxRetries)

	e.addPausedLabelToItem(owner, repo, item)

	e.applyLabelRemove(item, childPlacementLabel, true)

	comment := fmt.Sprintf(
		"🏭 **Fabrik — spawned child board placement failed**\n\nThis issue was spawned as a sub-issue, but Fabrik could not place it into the `Specify` (or first processing) column on the project board after %d attempt(s). It may still be sitting in `Backlog` or wherever GitHub defaulted it. The issue has been paused.\n\nManual fix:\nMove this issue's project-board column to `Specify` (or the first processing stage) yourself, then remove the `fabrik:paused` label to let Fabrik pick it up.\n\nIf no suitable column exists on the board, this may indicate a board-configuration problem — check that a `Specify` (or equivalent first-stage) column exists.",
		e.cfg.MaxRetries,
	)
	// NOTE: this comment-post's cache write-through deliberately omits CreatedAt
	// (unlike postComment, which always stamps time.Now()) to preserve this
	// site's pre-existing behavior exactly; using e.cache() directly here
	// (rather than postComment/postItemComment) keeps that omission intact.
	if dbID, err := e.client.AddComment(owner, repo, item.Number, comment); err != nil {
		e.logf(item.Number, "warn", "could not post child-placement escalation comment: %v\n", err)
	} else {
		if c := e.cache(); c != nil {
			c.ApplyCommentAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), gh.Comment{
				DatabaseID: dbID, Body: comment, Author: e.cfg.User,
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
	e.store.Apply(itemstate.EnginePaused{Repo: repoStr, Number: item.Number, StageName: childPlacementRetryStage})

	e.notifyParentOfStalledChild(owner, repo, item)
}

// clearChildPlacementMarker removes the childPlacementLabel marker and clears
// the retry counter once settleChildPlacement has succeeded (or the closed-
// child short-circuit in the poll.go settle scan applies).
func (e *Engine) clearChildPlacementMarker(item gh.ProjectItem, owner, repo string) {
	if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, childPlacementLabel); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "could not remove %s marker: %v\n", childPlacementLabel, err)
		return
	} else if err == nil {
		e.syncLabelRemoval(item, childPlacementLabel, true)
	}

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: childPlacementRetryStage})
}

// notifyParentOfStalledChild makes a best-effort attempt to post a comment on
// the parent issue that spawned childItem, explaining that it has stalled on
// board placement and been paused. The parent link is recovered by lazily
// deep-fetching the child's Body (not needed for ordinary settle passes) and
// regex-parsing the childFooter back-reference out of it — there is no
// structured link from child to parent. Every failure along this path is
// logged and swallowed: the child's own escalation (pause + comment) has
// already completed by the time this is called and must not be affected by
// this function's outcome.
func (e *Engine) notifyParentOfStalledChild(childOwner, childRepo string, childItem gh.ProjectItem) {
	if err := e.readClient.FetchItemDetails(&childItem); err != nil {
		e.logf(childItem.Number, "warn", "could not fetch child body to notify parent: %v\n", err)
		return
	}

	parentOwner, parentRepo, parentNumber, ok := parseParentFromChildBody(childItem.Body)
	if !ok {
		e.logf(childItem.Number, "warn", "could not recover parent link from child body — skipping parent notification\n")
		return
	}

	comment := fmt.Sprintf(
		"🏭 **Fabrik — spawned child stalled**\n\nSpawned child `%s/%s#%d` could not be placed on the project board and has been paused (`fabrik:paused`). This issue remains blocked on the child's closure until the child is manually recovered — see the comment on the child issue for recovery steps.",
		childOwner, childRepo, childItem.Number,
	)
	if _, err := e.client.AddComment(parentOwner, parentRepo, parentNumber, comment); err != nil {
		e.logf(childItem.Number, "warn", "could not post stalled-child notification on parent %s/%s#%d: %v\n", parentOwner, parentRepo, parentNumber, err)
	}
}

// parseParentFromChildBody extracts the parent owner/repo#number back-
// reference from a spawned child's body (written by childFooter at spawn
// time). Returns ok=false if the body carries no such footer, e.g. a human
// edited it out.
func parseParentFromChildBody(body string) (owner, repo string, number int, ok bool) {
	m := childParentFooterRe.FindStringSubmatch(body)
	if m == nil {
		return "", "", 0, false
	}
	var n int
	if _, err := fmt.Sscanf(m[3], "%d", &n); err != nil {
		return "", "", 0, false
	}
	return m[1], m[2], n, true
}
