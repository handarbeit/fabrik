package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Publication of a recorded stage outcome (#2394, ADR 2394): the per-poll scan that retries
// what finalizeStageOutcome could not finish, and the rules that discard a record that has
// gone stale.

// readyStageOutcome is the ready checkpoint: it fixes the comment payload and the head the
// publication was produced against (taken now, after any spec commit) in the record. Nil-safe.
func (e *Engine) readyStageOutcome(rec *stageOutcomeRecord, workDir, postOutput, footer, branch, commit, mainSHA, timestamp string, prNumber int) {
	if rec == nil {
		return
	}
	if head, err := gitRevParse(workDir, "HEAD"); err == nil && head != "" {
		rec.HeadSHA = head
	}
	rec.PostOutput, rec.Footer = postOutput, footer
	rec.Branch, rec.Commit, rec.MainSHA, rec.Timestamp = branch, commit, mainSHA, timestamp
	rec.PRNumber = prNumber
	rec.Phase = outcomePhaseReady
	e.persistOutcome(rec)
}

// outcomeDiscardReason returns why rec must be discarded rather than published, or "" when it
// may proceed (R3). head/headErr describe the worktree's current HEAD.
func outcomeDiscardReason(rec *stageOutcomeRecord, item gh.ProjectItem, stage *stages.Stage, head string, headErr error, freshStartGuard bool) string {
	switch {
	case item.IsClosed:
		return "the item is closed"
	case hasLabel(item.Labels, "fabrik:paused"):
		return "the item is paused"
	case freshStartGuard && rec.Stage == "Validate" && hasLabel(item.Labels, "fabrik:revalidate"):
		return "a forced Validate re-run was requested (fabrik:revalidate)"
	case freshStartGuard && rec.Stage == "Validate" && !rec.Rerun && hasLabel(item.Labels, validateRerunLabel):
		return "a forced Validate re-run is outstanding (" + validateRerunLabel + ")"
	case stage == nil || stage.Name != rec.Stage:
		return fmt.Sprintf("the item moved to column %q (recorded for stage %q)", item.Status, rec.Stage)
	case rec.Phase != outcomePhaseReady:
		return "the run was interrupted before its output was fixed for publication"
	case headErr != nil:
		return fmt.Sprintf("the worktree head cannot be read (%v)", headErr)
	case head != rec.HeadSHA:
		return fmt.Sprintf("the worktree head %s no longer matches the recorded %s", shortSHA(head), shortSHA(rec.HeadSHA))
	}
	return ""
}

// outcomeSignalPresent reports whether the stage's durable completion signal is already on
// the item: the completion label, or fabrik:awaiting-ci for a stage that waits for CI.
func outcomeSignalPresent(item gh.ProjectItem, stage *stages.Stage) bool {
	if hasLabel(item.Labels, fmt.Sprintf("stage:%s:complete", stage.Name)) {
		return true
	}
	return stage.WaitForCI != nil && *stage.WaitForCI && hasLabel(item.Labels, "fabrik:awaiting-ci")
}

// outcomeResult is how one publication attempt ended.
type outcomeResult int

const (
	outcomePublished outcomeResult = iota // every step has landed
	outcomeRetry                          // a step failed; try again on a later poll
	outcomeDiscarded                      // the item was paused (or similar); the record is moot
)

// settleStageOutcomes retries the publication of every live recorded outcome. It is sourced
// from the board snapshot (like settleNoWorkNeededScan) so it is independent of dispatch
// admission, which a live record deliberately refuses.
func (e *Engine) settleStageOutcomes(ctx context.Context, board *gh.ProjectBoard) {
	if e.stageOutcomes == nil || board == nil {
		return
	}
	recs := e.stageOutcomes.snapshot()
	if len(recs) == 0 {
		return
	}
	// An empty board is an unreadable one: discard nothing on it.
	if len(board.Items) == 0 {
		return
	}
	byKey := make(map[string]gh.ProjectItem, len(board.Items))
	for _, it := range board.Items {
		byKey[issueKey(it, e.defaultRepo())] = it
	}
	for i := range recs {
		if ctx.Err() != nil {
			return
		}
		rec := recs[i]
		item, ok := byKey[rec.key()]
		if !ok {
			e.dropStageOutcome(rec.key(), rec.Number, "the item is no longer on the board")
			continue
		}
		e.settleStageOutcome(ctx, board, item, &rec)
	}
}

func (e *Engine) settleStageOutcome(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, rec *stageOutcomeRecord) {
	// A worker or a comment pass for the item owns the record first: a finalizeStageOutcome
	// still running has not reached its ready checkpoint (the phase is still "recorded", the
	// head may still move with a spec commit), so judging the record now would discard a live
	// one. Skip before any discard rule is evaluated.
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	if snap, err := e.store.Get(repoStr, item.Number); err == nil && snap.Worker() != nil {
		return
	}
	if hasLabel(item.Labels, "fabrik:editing") {
		return
	}
	stage := stages.FindStage(e.cfg.Stages, item.Status)
	if stage != nil && stage.Name == rec.Stage && !item.IsClosed && outcomeSignalPresent(item, stage) {
		e.dropStageOutcome(rec.key(), rec.Number, "the completion signal is already on the item")
		return
	}
	wm := e.worktreesFor(item.Repo)
	var head string
	var headErr error
	if dir := wm.WorktreeDir(item.Number); dir == "" {
		headErr = errors.New("no worktree")
	} else if _, statErr := os.Stat(dir); statErr != nil {
		headErr = statErr
	} else {
		head, headErr = gitRevParse(dir, "HEAD")
	}
	if reason := outcomeDiscardReason(rec, item, stage, head, headErr, !e.stageOutcomeFreshStartOff.Load()); reason != "" {
		e.dropStageOutcome(rec.key(), rec.Number, reason)
		return
	}
	// Not while the forge is unreachable (the attempt would fail and teach nothing).
	if e.forgeHeld() {
		return
	}

	// live-read-audited: board-cache-backed deep fetch, applied to the Store on the next line via applyDeepFetched
	if err := e.readClient.FetchItemDetails(&item); err != nil {
		e.noteOutcomeFailure(item, rec, fmt.Errorf("reading the item: %w", err))
		return
	}
	e.applyDeepFetched(repoStr, &item)

	res, err := e.publishStageOutcome(ctx, board, item, stage, rec)
	switch res {
	case outcomePublished:
		e.stageOutcomes.remove(rec.key())
		e.logf(item.Number, "outcome", "published the recorded %s outcome of stage %q without re-running it\n", rec.Kind, rec.Stage)
	case outcomeDiscarded:
		e.dropStageOutcome(rec.key(), rec.Number, "publication paused the item")
	default:
		e.noteOutcomeFailure(item, rec, err)
	}
}

// countsAgainstBound reports whether a failed publication step counts against MaxRetries
// (R4). A transport failure or a rate limit (primary or secondary) is an outage, not a fault
// of the publication, and never counts: pausing for it would discard the record and re-run
// finished work. Both the typed gh.ErrRateLimited and the text classifier isTransientAPIError
// are consulted, since errors from git/exec and older call sites carry only text.
func countsAgainstBound(err error) bool {
	return err != nil && !gh.IsTransportError(err) && !errors.Is(err, gh.ErrRateLimited) && !isTransientAPIError(err)
}

// noteOutcomeFailure records a failed attempt on rec and escalates at MaxRetries.
func (e *Engine) noteOutcomeFailure(item gh.ProjectItem, rec *stageOutcomeRecord, err error) {
	if err == nil {
		err = errors.New("unknown failure")
	}
	rec.LastError = err.Error()
	counted := countsAgainstBound(err)
	if counted {
		rec.Attempts++
	}
	e.persistOutcome(rec)
	if e.stageOutcomes.get(rec.key()) == nil {
		// A fresh start or prune dropped the record while this attempt ran: nothing to escalate.
		return
	}
	e.logf(item.Number, "outcome", "publishing the recorded outcome of stage %q failed (attempt %d, %s): %v\n",
		rec.Stage, rec.Attempts, map[bool]string{true: "counted", false: "not counted — GitHub unreachable or rate limited"}[counted], err)
	if !counted || e.cfg.MaxRetries <= 0 || rec.Attempts < e.cfg.MaxRetries {
		return
	}
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	msg := fmt.Sprintf(
		"🏭 **Fabrik — publishing the %s outcome failed**\n\nStage **%s** finished, but publishing its result to GitHub failed %d times; the last error was: `%v`\n\n"+
			"Fabrik has paused this issue. Fix the cause (for example a missing permission), then comment or remove the `fabrik:paused` label to resume. "+
			"The recorded result is discarded, so the stage runs again on resume.",
		rec.Stage, rec.Stage, rec.Attempts, err)
	e.pauseIssue(item, msg, pauseOpts{awaitingInput: true, reactRocket: true})
	e.store.Apply(itemstate.EnginePaused{Repo: repoStr, Number: item.Number, StageName: rec.Stage})
	e.dropStageOutcome(rec.key(), rec.Number, fmt.Sprintf("publication failed %d times — item paused", rec.Attempts))
}

// publishStageOutcome performs the publication steps rec has not yet completed, in the
// order finalizeStageOutcome performs them: issue-body update, stage comment(s), push, draft
// PR, mark ready, completion signal. It never calls EnsureWorktree (which can rewrite HEAD)
// and never invokes Claude.
func (e *Engine) publishStageOutcome(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, rec *stageOutcomeRecord) (outcomeResult, error) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoStr := itemOwnerRepoString(item, e.defaultRepo())

	if rec.Kind == outcomeKindStage {
		// Issue body: never overwrite a human edit made while the outage lasted.
		if rec.BodyUpdate != "" && !rec.BodyDone {
			live, err := e.client.GetIssueBody(owner, repo, item.Number)
			if err != nil {
				return outcomeRetry, fmt.Errorf("reading the issue body: %w", err)
			}
			switch {
			case sameBody(live, rec.BodyUpdate):
			case sameBody(live, rec.BodyBefore):
				if err := e.updateIssueBody(item, owner, repo, rec.BodyUpdate); err != nil {
					return outcomeRetry, fmt.Errorf("updating the issue body: %w", err)
				}
			default:
				e.logf(item.Number, "outcome", "issue body was edited since the stage ran — not overwriting it with the recorded update\n")
			}
			rec.BodyDone = true
			e.persistOutcome(rec)
		}

		if rec.PostOutput != "" && !(rec.PRCommentDone && rec.IssueCommentDone) {
			if err := e.postStageOutput(item, stage.Name, stage.PostToPR, rec.PostOutput, rec.Footer, rec.Branch, rec.Commit, rec.MainSHA, rec.Timestamp, rec, true); err != nil {
				return outcomeRetry, fmt.Errorf("posting the stage comment: %w", err)
			}
		}

		if !rec.PushDone {
			pushErr := e.pushBranchUnlessQueued(item, e.worktreesFor(item.Repo))
			switch {
			case pushErr == nil, errors.Is(pushErr, ErrPushSkippedPRMerged):
			case errors.Is(pushErr, ErrPushRefused):
				return outcomeDiscarded, nil
			default:
				return outcomeRetry, fmt.Errorf("pushing the branch: %w", pushErr)
			}
			rec.PushDone = true
			e.persistOutcome(rec)
		}

		prNumber := rec.PRNumber
		if stage.CreateDraftPR {
			n, err := e.ensureDraftPR(item, rec.BaseBranch)
			if err != nil {
				return outcomeRetry, fmt.Errorf("ensuring the draft PR: %w", err)
			}
			prNumber = n
			e.updatePRVerification(item, prNumber, extractSummary(rec.Output))
			if !e.verifyAndHealLinkage(ctx, item, prNumber, stage, owner, repo, repoStr) {
				return outcomeDiscarded, nil
			}
		}
		e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		e.store.Apply(itemstate.EngineUnpaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		if stage.MarkPRReadyOnComplete && e.markPRReady(item, prNumber) {
			return outcomeDiscarded, nil
		}
	}

	if err := e.handleStageComplete(ctx, board, item, stage); err != nil {
		return outcomeRetry, err
	}
	return outcomePublished, nil
}

// sameBody compares two issue bodies ignoring line-ending and surrounding-space differences
// GitHub may introduce.
func sameBody(a, b string) bool {
	norm := func(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }
	return norm(a) == norm(b)
}
