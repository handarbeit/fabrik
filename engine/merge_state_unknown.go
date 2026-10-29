package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// The unknown-merge-state bound (#2302, ADR 2302).
//
// GitHub reports a PR's mergeability as null (or its merge state as "unknown")
// while it computes it, normally resolving within seconds to minutes. The merge
// gate claims every PRMergeUnsettled, so a persistent unknown state held an item
// with no bound of its own: the only bound was settleAwaitingCIScan's 4h
// CIBackstopTimeout, and only for items carrying fabrik:awaiting-ci. This file
// gives that one sub-reason its own short clock, kept per item and PR head in
// memory and sampled by handleMergeAndCIGates' mergeBlocked branch — the one
// place that sees every route into the gate.

// pauseCauseUnknownMergeStateMarker is the stable HTML marker the R4 pause
// comment carries so the attention classifier can derive the `unknown-merge-state`
// cause from an explicit recorded fact (ADR 2121), as it does for daemon-stop.
const pauseCauseUnknownMergeStateMarker = "<!-- fabrik:pause-cause=unknown-merge-state -->"

// unknownMergeClock is one item's continuous-unknown observation for one PR head.
type unknownMergeClock struct {
	head     string
	value    string // mergeable=null | mergeable_state=unknown
	since    time.Time
	lastSeen time.Time
}

// unknownMergeClocks is the in-memory clock store. A zero value is ready to use.
type unknownMergeClocks struct {
	mu sync.Mutex
	m  map[string]*unknownMergeClock
}

func (e *Engine) unknownMergeKey(item gh.ProjectItem) string {
	return ciBackstopKey(itemOwnerRepoString(item, e.defaultRepo()), item.Number)
}

// observeMergeState folds one settle result into the item's clock and returns a
// copy of the running clock when the result is an unknown merge state (nil
// otherwise).
//
//   - MergeObsUnknown starts a clock when there is none, the head differs, or the
//     item was unobserved for at least the merge-gate lapse (a pause, a stretch of
//     non-admission); otherwise it refreshes lastSeen.
//   - MergeObsComputed ends the clock.
//   - MergeObsNone (a read error, a terminal/queued/no-PR result) leaves it alone.
//
// Every timestamp is e.now(). A clock begins at the first observation in this
// process, so it never fires on the first evaluation after a start (ADR 2059 R2)
// and a restart can only lengthen the wait.
func (e *Engine) observeMergeState(item gh.ProjectItem, settle PRSettleResult) *unknownMergeClock {
	key := e.unknownMergeKey(item)
	c := &e.unknownMergeClocks
	c.mu.Lock()
	defer c.mu.Unlock()
	switch settle.MergeObs {
	case MergeObsComputed:
		delete(c.m, key)
		return nil
	case MergeObsUnknown:
		if settle.PR == nil {
			return nil
		}
		now := e.now()
		cur := c.m[key]
		if cur == nil || cur.head != settle.PR.HeadSHA || now.Sub(cur.lastSeen) >= e.wedgeLapseFor(gateMerge) {
			cur = &unknownMergeClock{head: settle.PR.HeadSHA, since: now}
			if c.m == nil {
				c.m = make(map[string]*unknownMergeClock)
			}
			c.m[key] = cur
		}
		cur.lastSeen = now
		cur.value = settle.MergeUnknownValue
		cp := *cur
		return &cp
	}
	return nil
}

// clearUnknownMergeClock drops the item's clock (pause, or a computed state).
func (e *Engine) clearUnknownMergeClock(item gh.ProjectItem) {
	c := &e.unknownMergeClocks
	c.mu.Lock()
	delete(c.m, e.unknownMergeKey(item))
	c.mu.Unlock()
}

// checkUnknownMergeBound is called from the merge gate's claim branch. It
// updates the clock from settle and, once the unknown state has lasted
// mergeStateUnknownTimeout for this head, pauses the item. It reports whether it
// paused (the caller claims the item either way).
func (e *Engine) checkUnknownMergeBound(pctx *phase1Ctx, settle PRSettleResult) bool {
	clock := e.observeMergeState(pctx.item, settle)
	if clock == nil {
		return false
	}
	bound := e.mergeStateUnknownTimeout()
	elapsed := e.now().Sub(clock.since)
	if elapsed < bound {
		return false
	}
	repo := itemOwnerRepoString(pctx.item, e.defaultRepo())
	if stageName, inFlight := e.ciBackstopWorkerInFlight(repo, pctx.item.Number); inFlight {
		// ADR 2059 R3: never race a running worker with a pause; the next poll
		// re-evaluates once it has exited.
		e.logf(pctx.item.Number, "merge-gate", "mergeability unknown for %s (bound %s) but a worker is in flight (stage=%s) — escalation deferred to a later poll\n",
			elapsed.Round(time.Second), bound, stageName)
		return false
	}
	e.logf(pctx.item.Number, "merge-gate", "mergeability unknown for PR #%d at %s for %s (bound %s) — escalating\n",
		settle.PR.Number, shortSHA(clock.head), elapsed.Round(time.Second), bound)
	e.pauseForUnknownMergeState(pctx.board, pctx.item, pctx.stage, settle, clock, elapsed)
	// The pause ends this episode: a human resume starts a fresh bound rather than
	// re-pausing at once (ADR 1813 / ADR 2059 R1).
	e.clearUnknownMergeClock(pctx.item)
	return true
}

// unknownMergeFragment is the stable prefix of the R4 pause comment's text. It
// carries the PR and head so an old comment for a different head can never
// suppress a legitimate fresh pause, and it cannot match either CI-gate fragment
// hasCIGatePauseComment looks for.
func unknownMergeFragment(prNumber int, head string) string {
	return fmt.Sprintf("GitHub has not reported a computed mergeability for PR #%d at head `%s`", prNumber, shortSHA(head))
}

// pauseForUnknownMergeState pauses the item with a comment naming what was
// observed (R4). Episode dedup mirrors pauseForCITimeout (#1408): an existing
// comment for this PR head is reused, only the pause labels are reapplied.
func (e *Engine) pauseForUnknownMergeState(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, settle PRSettleResult, clock *unknownMergeClock, elapsed time.Duration) (escalated bool) {
	frag := unknownMergeFragment(settle.PR.Number, clock.head)
	if hasPauseComment(item, frag) {
		e.logf(item.Number, "merge-gate", "unknown-merge-state pause comment already exists for this episode — reapplying pause without reposting\n")
		e.reapplyPauseLabels(item, pauseCauseCITimeout)
		return false
	}
	msg := fmt.Sprintf(
		"🏭 **Fabrik — merge state unknown**\n\n%s for %s: it kept returning `%s`.\n\n"+
			"The CI gate for stage **%s** was never reached because of it.\n\n"+
			"Check-run state at escalation: %s.\n\n"+
			"Fabrik has paused this issue. Open the PR to see whether GitHub now reports its mergeability; a push or a base-branch change usually makes GitHub recompute it. "+
			"Then comment or remove the `fabrik:paused` label to resume.\n\n%s",
		frag, elapsed.Round(time.Second), clock.value, stage.Name,
		e.checkRunStateNote(item, clock.head), pauseCauseUnknownMergeStateMarker,
	)
	e.pauseIssue(item, msg, pauseOpts{awaitingInput: true, reactRocket: true, cause: pauseCauseCITimeout})
	return true
}

// checkRunStateNote renders the PR head's check-run state at escalation, e.g.
// "20 of 20 checks passed" (R4/R5). One best-effort live read on the single
// escalation path, like suiteTimeoutNote: an error degrades to a note saying so
// and never blocks the pause.
func (e *Engine) checkRunStateNote(item gh.ProjectItem, headSHA string) string {
	counts, err := e.checkRunCounts(item, headSHA)
	if err != nil {
		return "check-run state could not be read"
	}
	return formatCheckRunCounts(counts)
}

// checkRunCounts is the one live read behind the escalation notes.
func (e *Engine) checkRunCounts(item gh.ProjectItem, headSHA string) (gh.CheckRunCounts, error) {
	if headSHA == "" {
		headSHA = item.LinkedPRHeadSHA
	}
	if headSHA == "" {
		return gh.CheckRunCounts{}, errors.New("no head SHA")
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	runs, err := e.readClient.FetchCheckRuns(owner, repo, headSHA)
	if err != nil {
		e.logf(item.Number, "merge-gate", "could not read check runs for the escalation note: %v\n", err)
		return gh.CheckRunCounts{}, err
	}
	return gh.SummarizeCheckRuns(runs), nil
}

func formatCheckRunCounts(c gh.CheckRunCounts) string {
	if c.Total == 0 {
		return "no check runs were reported for this head"
	}
	parts := []string{fmt.Sprintf("%d of %d checks passed", c.Passed, c.Total)}
	if c.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", c.Failed))
	}
	if c.Pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", c.Pending))
	}
	if c.NonPassing > 0 {
		parts = append(parts, fmt.Sprintf("%d cancelled or stale", c.NonPassing))
	}
	return strings.Join(parts, ", ")
}
