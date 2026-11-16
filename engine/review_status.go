package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/warnings"
)

// ReviewStatusEnabled reports whether the `fabrik/review` commit status (#2471,
// ADR 2471) is enabled anywhere: the repo-wide review_status config key, or any
// stage's own review_status: true. It is the one decision the startup
// permission check and `fabrik init --github-app` share, so they cannot
// disagree about when `statuses: write` is demanded. It deliberately ignores
// per-item labels and wait_for_reviews: a mis-set stage is reported separately,
// and over-asking for the grant is the safe direction.
func ReviewStatusEnabled(repoWide bool, stgs []*stages.Stage) bool {
	if repoWide {
		return true
	}
	for _, s := range stgs {
		if s != nil && s.ReviewStatus != nil && *s.ReviewStatus {
			return true
		}
	}
	return false
}

// reviewStatusState is a value of the fabrik/review commit status.
const (
	reviewStatusPending = "pending"
	reviewStatusSuccess = "success"
	reviewStatusFailure = "failure"
)

// reviewStatusResult is the status a head should carry.
type reviewStatusResult struct {
	State       string
	Description string
}

// reviewStatusVerdict is the fabrik/review status for head, derived from the
// per-reviewer standings (reviewerStandings, #2465) of the declared reviewers —
// never from a second collapse of the review history (FR-003). The caller
// supplies a known head; an empty head yields ok=false (no status is written:
// an unknown head never produces success).
//
// Per declared reviewer:
//
//	changes requested                       → failure
//	awaiting re-review (verdict on old head) → pending
//	clean                                   → success
//	no verdict, COMMENTED at head           → success (how a comment-only bot such
//	                                          as Pruefer expresses a clean pass; a
//	                                          review with findings makes Fabrik push
//	                                          a fix, i.e. a new head and a pending
//	                                          status)
//	no verdict, no comment at head          → pending
//	absent from the history                 → pending
//
// Overall: any failure wins, then any pending, otherwise success. Undeclared
// reviewers never affect the status.
func reviewStatusVerdict(standings []reviewerStanding, declared []string, head string) (reviewStatusResult, bool) {
	if head == "" || len(declared) == 0 {
		return reviewStatusResult{}, false
	}
	var failed, pending, clean []string
	for _, d := range declared {
		var st *reviewerStanding
		for i := range standings {
			if reviewerIdentityMatches(d, standings[i].Author) {
				st = &standings[i]
				break
			}
		}
		switch {
		case st == nil:
			pending = append(pending, d)
		case st.Kind == standingChangesRequested:
			failed = append(failed, d)
		case st.Kind == standingAwaitingReReview:
			pending = append(pending, d)
		case st.Kind == standingClean, st.Kind == standingNoVerdict && st.CommentAtHead:
			clean = append(clean, d)
		default:
			pending = append(pending, d)
		}
	}
	sha := standingSHA(head)
	switch {
	case len(failed) > 0:
		return reviewStatusResult{reviewStatusFailure, fitStatusDescription("Changes requested on " + sha + " by " + strings.Join(failed, ", "))}, true
	case len(pending) > 0:
		return reviewStatusResult{reviewStatusPending, fitStatusDescription("Waiting for review of " + sha + " from " + strings.Join(pending, ", "))}, true
	default:
		return reviewStatusResult{reviewStatusSuccess, fitStatusDescription("Reviewed " + sha + ": no changes requested by " + strings.Join(clean, ", "))}, true
	}
}

// fitStatusDescription keeps a description within GitHub's 140-character limit.
func fitStatusDescription(s string) string {
	if r := []rune(s); len(r) > 140 {
		return string(r[:139]) + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// The producer: settleReviewStatusScan (#2471, ADR 2471; docs/state-machine.md
// §6.6.7).
//
// A per-poll settle scan sourced from board.Items (independent of admission,
// like settlePRReadyScan). For each item whose stage has the feature enabled it
// reads the PR's review history and head live, derives the verdict through
// reviewStatusVerdict and writes the `fabrik/review` commit status only when
// (PR, head, state) differs from the last successful write. A push therefore
// produces a pending write for the new head within one poll, an unchanged state
// writes nothing, and a restart costs at most one redundant write per item.
//
// The status is display and CI-coordination only: a failed write is logged,
// reported once on the warnings panel and retried; it never pauses an item and
// never blocks a gate.
// ---------------------------------------------------------------------------

const (
	reviewStatusWriteWarnType  = "review_status_write"
	reviewStatusConfigWarnType = "review_status_config"
)

// reviewStatusRecord is the last successfully written status of one item.
type reviewStatusRecord struct {
	pr    int
	head  string
	state string
	// endedAt is when the record last left pending for its head (zero while it is
	// pending or never was): the review wait's end, which the CI dwell anchors
	// resume from.
	endedAt time.Time
}

// reviewStatusState is the engine's fabrik/review state (zero value ready).
type reviewStatusState struct {
	mu      sync.Mutex
	records map[string]*reviewStatusRecord // by issueKey

	warnMu      sync.Mutex
	writeWarned bool   // a review_status_write entry is currently recorded
	configSig   string // last review_status_config set reconciled
	configDone  bool   // configSig is meaningful (a reconcile has completed)

	off            atomic.Bool // SetReviewStatusDisabledForTest: write nothing
	headBlind      atomic.Bool // SetReviewStatusHeadBlindForTest: verdict ignores the head
	attributionOff atomic.Bool // SetReviewWaitAttributionDisabledForTest
}

// reviewStatusStageEnabled reports whether stage publishes the status: a stage
// that waits for reviews, with its own review_status key (when set) or else the
// repo-wide config key.
func (e *Engine) reviewStatusStageEnabled(stage *stages.Stage) bool {
	if stage == nil || stage.WaitForReviews == nil || !*stage.WaitForReviews {
		return false
	}
	if stage.ReviewStatus != nil {
		return *stage.ReviewStatus
	}
	return e.cfg.ReviewStatus
}

// settleReviewStatusScan is the per-poll scan; see the section comment above.
func (e *Engine) settleReviewStatusScan(board *gh.ProjectBoard) {
	if e.reviewStatus.off.Load() {
		return
	}
	e.reportReviewStatusConfig()
	if !ReviewStatusEnabled(e.cfg.ReviewStatus, e.cfg.Stages) {
		return
	}
	if e.forgeHeld() || e.isDraining() {
		return // retried next poll; nothing is lost
	}
	seen := map[string]bool{}
	for _, item := range board.Items {
		if item.IsPR || item.IsClosed || item.LinkedPRNumber == 0 {
			continue
		}
		stage := stages.FindStage(e.cfg.Stages, item.Status)
		if !e.reviewStatusStageEnabled(stage) {
			continue
		}
		declared := e.effectiveExpectedReviewers(item, stage)
		if declared == nil || len(*declared) == 0 {
			continue // nothing to wait on; reported as a config problem
		}
		key := issueKey(item, e.defaultRepo())
		seen[key] = true
		e.settleReviewStatus(item, key, *declared)
	}
	// An item that left an enabling stage (or closed) loses its record, so no
	// stale attribution survives it.
	e.reviewStatus.mu.Lock()
	for k := range e.reviewStatus.records {
		if !seen[k] {
			delete(e.reviewStatus.records, k)
		}
	}
	e.reviewStatus.mu.Unlock()
}

func (e *Engine) settleReviewStatus(item gh.ProjectItem, key string, declared []string) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	pr := item.LinkedPRNumber

	// live-read-audited: the status must follow the PR's head and state as GitHub has them now; a failed read writes nothing
	details, err := e.client.FetchPRDetails(owner, repo, pr)
	if err != nil {
		e.logf(item.Number, "review-status", "could not read PR #%d: %v\n", pr, err)
		return
	}
	if details == nil || details.HeadSHA == "" || details.State != "open" || details.Merged {
		return // closed, merged or no head: no status
	}
	head := details.HeadSHA
	// live-read-audited: the verdict is derived from the uncollapsed history at the head just read; the Store is not a decision input here
	history, err := e.readClient.FetchPRReviewHistory(owner, repo, pr)
	if err != nil {
		e.logf(item.Number, "review-status", "could not read PR #%d review history: %v\n", pr, err)
		return // never success on an unreadable history
	}
	if e.reviewStatus.headBlind.Load() {
		// Non-vacuity seam: pretend every review is of the current head.
		blind := make([]gh.PRReview, len(history))
		copy(blind, history)
		for i := range blind {
			blind[i].CommitID = head
		}
		history = blind
	}
	res, ok := reviewStatusVerdict(reviewerStandings(history, head, declared), declared, head)
	if !ok {
		return
	}

	e.reviewStatus.mu.Lock()
	if e.reviewStatus.records == nil {
		e.reviewStatus.records = map[string]*reviewStatusRecord{}
	}
	rec := e.reviewStatus.records[key]
	unchanged := rec != nil && rec.pr == pr && rec.head == head && rec.state == res.State
	e.reviewStatus.mu.Unlock()
	if unchanged {
		return
	}

	if err := e.client.CreateCommitStatus(owner, repo, head, res.State, gh.ReviewStatusContext, res.Description); err != nil {
		e.logf(item.Number, "review-status", "writing %s=%s on %s failed: %v\n", gh.ReviewStatusContext, res.State, standingSHA(head), err)
		e.reportReviewStatusWriteFailure(item, owner, repo, err)
		return // the record is not advanced: retried on a later poll
	}
	e.resolveReviewStatusWriteFailure()
	e.logf(item.Number, "review-status", "%s = %s on %s: %s\n", gh.ReviewStatusContext, res.State, standingSHA(head), res.Description)

	now := time.Now()
	e.reviewStatus.mu.Lock()
	next := &reviewStatusRecord{pr: pr, head: head, state: res.State}
	if rec != nil && rec.pr == pr && rec.head == head && rec.state == reviewStatusPending && res.State != reviewStatusPending {
		next.endedAt = now
	} else if rec != nil && rec.pr == pr && rec.head == head && res.State != reviewStatusPending {
		next.endedAt = rec.endedAt
	}
	e.reviewStatus.records[key] = next
	e.reviewStatus.mu.Unlock()
}

// reviewWaitFloor is the latest instant the item's review wait is known to have
// lasted until (#2471, R3): "now" while the status last written for its PR is
// pending, the moment it left pending afterwards, zero when there is no record.
// The CI wait timers measure a CI stall from max(anchor, floor), so time spent
// waiting for a reviewer never counts as CI time. Memory-only: after a restart
// the first scan re-establishes it. The instants are host-clock times because
// every CI dwell compares with time.Since (the sim drives these gates with
// backdated GitHub times, not the engine clock — ADR 2467).
func (e *Engine) reviewWaitFloor(item gh.ProjectItem) time.Time {
	if e.reviewStatus.attributionOff.Load() || e.reviewStatus.off.Load() || item.LinkedPRNumber == 0 {
		return time.Time{}
	}
	e.reviewStatus.mu.Lock()
	defer e.reviewStatus.mu.Unlock()
	rec := e.reviewStatus.records[issueKey(item, e.defaultRepo())]
	if rec == nil || rec.pr != item.LinkedPRNumber {
		return time.Time{}
	}
	if rec.state == reviewStatusPending {
		return time.Now()
	}
	return rec.endedAt
}

// reviewWaitPending reports whether the item's fabrik/review status is
// currently pending, and the reviewers/head it names (for pause wording).
func (e *Engine) reviewWaitPending(item gh.ProjectItem) (head string, ok bool) {
	if e.reviewStatus.attributionOff.Load() || e.reviewStatus.off.Load() || item.LinkedPRNumber == 0 {
		return "", false
	}
	e.reviewStatus.mu.Lock()
	defer e.reviewStatus.mu.Unlock()
	rec := e.reviewStatus.records[issueKey(item, e.defaultRepo())]
	if rec == nil || rec.pr != item.LinkedPRNumber || rec.state != reviewStatusPending {
		return "", false
	}
	return rec.head, true
}

// anchorAfterReviewWait returns anchor moved forward to the end of the item's
// review wait, when that is later.
func (e *Engine) anchorAfterReviewWait(item gh.ProjectItem, anchor time.Time) time.Time {
	if floor := e.reviewWaitFloor(item); floor.After(anchor) {
		return floor
	}
	return anchor
}

// isReviewStatusPermissionError reports a refusal that points at a missing
// statuses grant or scope.
func isReviewStatusPermissionError(err error) bool {
	return errors.Is(err, gh.ErrForbidden) || errors.Is(err, gh.ErrNotFound)
}

func (e *Engine) reportReviewStatusWriteFailure(item gh.ProjectItem, owner, repo string, err error) {
	if gh.IsTransportError(err) {
		return // an outage is the forge hold's business, not a configuration condition
	}
	e.reviewStatus.warnMu.Lock()
	defer e.reviewStatus.warnMu.Unlock()
	if e.reviewStatus.writeWarned {
		return
	}
	detail := fmt.Sprintf("Writing the `%s` commit status on %s/%s failed: %v\n\n", gh.ReviewStatusContext, owner, repo, err)
	if isReviewStatusPermissionError(err) {
		detail += "GitHub refused the write. Under GitHub App auth the installation needs `statuses: write`; with a personal access token it needs `repo` scope " +
			"(or fine-grained commit statuses: write). "
	}
	detail += "No item is paused and no gate is blocked; the write is retried every poll. To stop writing the status, remove `review_status` from the config and stages."
	c := warnings.Condition{
		Type:        reviewStatusWriteWarnType,
		Subject:     "write",
		Title:       "Cannot write the fabrik/review commit status",
		Detail:      detail,
		Fingerprint: "review-status-write",
	}
	if rerr := warnings.Report(e.warnWriter("review-status"), c); rerr != nil {
		e.logf(item.Number, "review-status", "recording warning: %v\n", rerr)
		return
	}
	e.reviewStatus.writeWarned = true
}

func (e *Engine) resolveReviewStatusWriteFailure() {
	e.reviewStatus.warnMu.Lock()
	defer e.reviewStatus.warnMu.Unlock()
	if !e.reviewStatus.writeWarned {
		return
	}
	if err := warnings.Resolve(reviewStatusWriteWarnType + ":write"); err != nil {
		return
	}
	e.reviewStatus.writeWarned = false
}

// reportReviewStatusConfig reconciles the review_status_config panel entries:
// a stage that enables the status but cannot publish it. Reconciled only when
// the set changed, so a poll that changes nothing does no panel I/O.
func (e *Engine) reportReviewStatusConfig() {
	var conds []warnings.Condition
	anyQualifies := false
	for _, s := range e.cfg.Stages {
		if s == nil {
			continue
		}
		explicit := s.ReviewStatus != nil && *s.ReviewStatus
		waits := s.WaitForReviews != nil && *s.WaitForReviews
		switch {
		case explicit && !waits:
			conds = append(conds, warnings.Condition{
				Subject: "stage:" + s.Name,
				Title:   fmt.Sprintf("Stage %q sets review_status but not wait_for_reviews", s.Name),
				Detail: fmt.Sprintf("Stage %q sets review_status: true, but the fabrik/review status is only written for stages with wait_for_reviews: true, so nothing is written for it.\n\n"+
					"Fix: set wait_for_reviews: true on the stage, or remove review_status.", s.Name),
				Fingerprint: "stage:" + s.Name + ":no-wait-for-reviews",
			})
		case waits && e.reviewStatusStageEnabled(s) && (s.ExpectedReviewers == nil || len(*s.ExpectedReviewers) == 0):
			conds = append(conds, warnings.Condition{
				Subject: "stage:" + s.Name,
				Title:   fmt.Sprintf("Stage %q enables the fabrik/review status but declares no expected_reviewers", s.Name),
				Detail: fmt.Sprintf("Stage %q enables the fabrik/review status, but it declares no expected_reviewers, so there is no review to wait on and no status is written "+
					"(a workflow waiting on it would otherwise wait for ever).\n\n"+
					"Fix: declare the review bot in expected_reviewers on the stage (for example `expected_reviewers: [pruefer]`), or remove review_status.", s.Name),
				Fingerprint: "stage:" + s.Name + ":no-expected-reviewers",
			})
		case waits && e.reviewStatusStageEnabled(s):
			anyQualifies = true
		}
	}
	if e.cfg.ReviewStatus && !anyQualifies && len(conds) == 0 {
		conds = append(conds, warnings.Condition{
			Subject:     "repo",
			Title:       "review_status is on but no stage can publish the fabrik/review status",
			Detail:      "The repo-wide review_status key is on, but no stage has wait_for_reviews: true.\n\nFix: set wait_for_reviews: true and expected_reviewers on the stage that reviews, or remove review_status.",
			Fingerprint: "repo:no-stage",
		})
	}
	sig := make([]string, 0, len(conds))
	for _, c := range conds {
		sig = append(sig, c.Subject+"="+c.Fingerprint)
	}
	sort.Strings(sig)
	joined := strings.Join(sig, ";")
	e.reviewStatus.warnMu.Lock()
	defer e.reviewStatus.warnMu.Unlock()
	if e.reviewStatus.configDone && joined == e.reviewStatus.configSig {
		return
	}
	if err := warnings.Reconcile(e.warnWriter("review-status"), reviewStatusConfigWarnType, conds); err != nil {
		e.logf(0, "review-status", "recording configuration warnings: %v\n", err)
		return
	}
	e.reviewStatus.configSig = joined
	e.reviewStatus.configDone = true
}

// pauseForReviewWaitBackstop is pauseForCIBackstop's branch for an item whose
// fabrik/review status is pending (#2471, R3, FR-006): the CIBackstopTimeout
// bound is unchanged, but the comment says the item was waiting for the review
// of the head — not for CI — and names who has not reviewed it.
func (e *Engine) pauseForReviewWaitBackstop(item gh.ProjectItem, stage *stages.Stage, appliedAt time.Time, head string) bool {
	e.logf(item.Number, "ci-timeout", "CIBackstopTimeout elapsed while the fabrik/review status is pending — pausing as a review wait, not a CI stall\n")
	who := "an expected reviewer"
	if declared := e.effectiveExpectedReviewers(item, stage); declared != nil && len(*declared) > 0 {
		who = "the expected reviewer(s) " + strings.Join(*declared, ", ")
	}
	total := ""
	if !appliedAt.IsZero() {
		total = fmt.Sprintf(" `fabrik:awaiting-ci` has been set for %s.", time.Since(appliedAt).Round(time.Minute))
	}
	msg := fmt.Sprintf(
		"🏭 **Fabrik — review wait timeout**\n\nThe review wait for stage **%s** timed out. This was a wait for the review of the PR head `%s`, not for CI: "+
			"the `%s` commit status is `pending` because %s has not yet reviewed that commit, so a CI job that depends on it has not run.%s\n\n"+
			"Fabrik has paused this issue. Check why the review has not arrived (is the review bot running and able to see this PR?), "+
			"then comment or remove the `fabrik:paused` label to resume.",
		stage.Name, standingSHA(head), gh.ReviewStatusContext, who, total,
	)
	e.pauseIssue(item, msg, pauseOpts{awaitingInput: true, reactRocket: true, cause: pauseCauseReviewTimeout})
	e.emitReviewTimeout(item, stage)
	return true
}
