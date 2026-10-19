package engine

import (
	"fmt"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
)

// The overseer's advance action (#2126, ADR 2126): move an item whose current
// stage is complete exactly one configured stage forward, through the engine's
// own advanceToNextStage (the transition auto-advance uses). It lives beside
// overseer_actions.go, not in localapi*.go / channel_*.go, for the same reason
// that file does (the read-path static scans).
//
// A refused advance writes nothing to GitHub: every gate the engine applies to
// its own advance is evaluated through a read-only core (openDependencies,
// unprocessedFeedback via feedbackGateBlocks, reviewGateHold). The one side
// effect of the refusal path is feedbackGateBlocks' in-memory feedback-pending
// cooldown, which is what the engine's own held advance records too.
//
// The review wait uses reviewGateHold, which shares the engine's pure verdict
// functions (reviewGateOutstanding, reviewGateFastAdvance, reviewGateAuthorityVerdict,
// effectiveExpectedReviewers/effectiveReviewAuthority) with checkReviewGate but is
// not checkReviewGate: it has no timeout, bot re-prompt ladder or pause. Those stay
// with the engine's own catch-up gate, which keeps evaluating the item while
// stage:<X>:complete stands; this action only ever fails closed on a hold.

// engineWaitLabels are labels the engine's own phase-1 handlers own: while one is
// present the engine itself would hold the item, so an overseer advance must not
// skip past it. Each entry is paired with the wording of the refusal.
var engineWaitLabels = []struct{ label, why string }{
	{"fabrik:awaiting-ci", "a CI wait is in progress"},
	{"fabrik:awaiting-review", "a review wait is in progress"},
	{"fabrik:rebase-needed", "the PR needs a rebase and the engine is handling it"},
	{"fabrik:auto-merge-enabled", "auto-merge is enabled and the engine is waiting for the PR to land"},
	{"fabrik:awaiting-done", "the engine is finishing a no-work-needed completion"},
}

// advanceColumn judges the item's column (status) against the stage
// configuration. It returns the current and next stage, or a refusal reason
// ("" means the column can be advanced from).
func (a *overseerActor) advanceColumn(status string) (cur, next *stages.Stage, why string) {
	if status == "" {
		return nil, nil, "has no known board column, so it is not established to be in a pipeline stage"
	}
	if stages.IsParkingColumn(a.e.cfg.Stages, status) {
		return nil, nil, fmt.Sprintf("is in the parking column %q; use fabrik_promote to move it into the pipeline", status)
	}
	cur = a.e.stageByName(status)
	if cur == nil {
		return nil, nil, fmt.Sprintf("is in column %q, which has no configured stage", status)
	}
	if cur.CleanupWorktree {
		return nil, nil, fmt.Sprintf("is in %q, the final column; there is no later stage to advance to", status)
	}
	if cur.HoldingStage {
		return nil, nil, fmt.Sprintf("is in the holding stage %q, which the engine's merge train owns; an overseer cannot move it", status)
	}
	// The stage that follows in configured order, skipping parking columns
	// exactly as stages.NextStage does — but looking at holding and cleanup
	// stages too, since moving into them is the engine's own landing.
	var following *stages.Stage
	seen := false
	for _, s := range a.e.cfg.Stages {
		if s == cur {
			seen = true
			continue
		}
		if seen && !s.Unmanaged {
			following = s
			break
		}
	}
	if following == nil || following.HoldingStage || following.CleanupWorktree {
		to := "the end of the pipeline"
		if following != nil {
			to = fmt.Sprintf("%q", following.Name)
		}
		return nil, nil, fmt.Sprintf("is in %s: %s → use the PR merge; the move out of %s (to %s) is the engine's landing and the human's merge decision, not an overseer move", status, status, status, to)
	}
	return cur, following, ""
}

// advanceLabelRefusal judges the labels (live, at the call site) and the
// worker handle, in the documented refusal order after the column check. It
// returns "" when nothing stands in the way.
func (a *overseerActor) advanceLabelRefusal(t *overseerTarget, cur *stages.Stage, labels []string) string {
	completeLabel := fmt.Sprintf("stage:%s:complete", cur.Name)
	complete := hasLabelStr(labels, completeLabel)

	inFlight := t.st.Worker != nil || hasLabelStr(labels, "fabrik:editing") || hasLabelStr(labels, fmt.Sprintf("stage:%s:in_progress", cur.Name))
	if !inFlight {
		for _, l := range labels {
			if strings.HasPrefix(l, "fabrik:locked:") {
				inFlight = true
				break
			}
		}
	}
	if inFlight {
		if complete {
			return fmt.Sprintf("a worker is in flight for %s even though %s is present (a review, CI-fix or rebase dispatch), so it is not safe to advance; wait for it to finish", t.ref, completeLabel)
		}
		return fmt.Sprintf("stage not complete — a worker is running for %s (%s is absent); wait for it to finish", t.ref, completeLabel)
	}
	if !complete {
		return fmt.Sprintf("stage not complete — %s does not carry %s; the engine runs or retries the stage on its own", t.ref, completeLabel)
	}
	if hasLabelStr(labels, "fabrik:paused") || hasLabelStr(labels, "fabrik:awaiting-input") {
		return fmt.Sprintf("paused — %s is paused or awaiting input; a human comment resumes it, and this tool never lifts a pause", t.ref)
	}
	if hasLabelStr(labels, "fabrik:blocked") {
		return fmt.Sprintf("blocked — %s carries fabrik:blocked; it moves again once its blocking issues close", t.ref)
	}
	for _, w := range engineWaitLabels {
		if hasLabelStr(labels, w.label) {
			return fmt.Sprintf("%s carries %s: %s; the engine advances it when that clears", t.ref, w.label, w.why)
		}
	}
	return ""
}

// Advance moves the item one stage forward. See the file comment for the
// refusal order; each refusal carries the item's current state and writes
// nothing.
func (a *overseerActor) Advance(p localapi.AdvanceParams) (*localapi.ActionResult, error) {
	requester, err := sanitizeRequester(p.Subscriber)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	t, err := a.resolve(p.Issue)
	if err != nil {
		return nil, err
	}
	e := a.e
	refuse := func(format string, args ...any) error {
		return localapi.Refused(a.refusalState(&t.st), format, args...)
	}

	e.mu.Lock()
	field := e.statusField
	projectID := e.boardProjectID
	e.mu.Unlock()
	if c := e.cache(); c != nil && c.ProjectID() != "" {
		projectID = c.ProjectID()
	}
	if field == nil || projectID == "" {
		return nil, refuse("the board metadata (project and status field) is not loaded yet; try again after the next poll")
	}

	// 1. The column, judged on the cache first (config-only, no API call).
	cur, next, why := a.advanceColumn(t.st.Status)
	if why != "" {
		return nil, refuse("%s %s", t.ref, why)
	}
	if _, ok := field.Options[next.Name]; !ok {
		return nil, refuse("%s cannot advance to %q: the board has no %q column (available: %s)", t.ref, next.Name, next.Name, strings.Join(mapKeys(field.Options), ", "))
	}
	// The worker handle is in memory and always current, so it is checked before
	// any live read.
	if t.st.Worker != nil {
		return nil, refuse("%s", a.advanceLabelRefusal(t, cur, t.st.Labels))
	}

	// 2. Live confirmation of the column, then judge on the live column.
	live, err := a.confirmLiveStatus(t)
	if err != nil {
		return nil, err
	}
	if live != t.st.Status {
		// The cache lagged: the live column decides which stage is current.
		t.st.Status = live
		t.item.Status = live
		var liveWhy string
		cur, next, liveWhy = a.advanceColumn(live)
		if liveWhy != "" {
			return nil, refuse("%s %s (confirmed live)", t.ref, liveWhy)
		}
		if _, ok := field.Options[next.Name]; !ok {
			return nil, refuse("%s cannot advance to %q: the board has no %q column (available: %s)", t.ref, next.Name, next.Name, strings.Join(mapKeys(field.Options), ", "))
		}
	}

	// 3. Live labels: completion, pause, block and engine-owned waits.
	owner, repo := itemOwnerRepo(t.item, e.defaultRepo())
	// live-read-audited: overseer advance gate reads live to refuse; a refused action writes nothing (ADR 2126)
	labels, err := e.client.FetchLabels(owner, repo, t.st.Number)
	if err != nil {
		return nil, refuse("could not confirm %s's labels live (%v); refusing rather than advance from a possibly stale cache", t.ref, err)
	}
	t.st.Labels = labels
	t.item.Labels = labels
	if why := a.advanceLabelRefusal(t, cur, labels); why != "" {
		return nil, refuse("%s", why)
	}

	// 4. The gates the engine applies to its own advance, read-only. One live
	// details read feeds the dependency and feedback checks.
	gateItem := t.item
	if t.st.LinkedPR != nil {
		gateItem.LinkedPRNumber = t.st.LinkedPR.Number
	}
	// live-read-audited: overseer advance gate reads live to refuse; a refused action writes nothing (ADR 2126)
	if err := e.client.FetchItemDetails(&gateItem); err != nil {
		return nil, refuse("could not read %s's dependencies and comments live (%v); refusing rather than skip a gate the engine applies", t.ref, err)
	}
	gateItem.Status = live
	if open := e.openDependencies(itemOwnerRepoString(gateItem, e.defaultRepo()), gateItem.BlockedBy); len(open) > 0 {
		var names []string
		for _, d := range open {
			if d.Repo == "" || d.Repo == itemOwnerRepoString(gateItem, e.defaultRepo()) {
				names = append(names, fmt.Sprintf("#%d", d.Number))
			} else {
				names = append(names, fmt.Sprintf("%s#%d", d.Repo, d.Number))
			}
		}
		return nil, refuse("blocked — %s is waiting for %s to close; it moves again once they do", t.ref, strings.Join(names, ", "))
	}
	if e.feedbackGateBlocks(gateItem, true, "overseer advance") {
		return nil, refuse("%s has unprocessed comments or review feedback (or the feedback read failed); the engine addresses them before advancing", t.ref)
	}
	if h := e.reviewGateHold(gateItem, cur, owner, repo); h.Blocked {
		return nil, refuse("a review wait stands between %s and the next stage: %s; the engine's review gate owns this wait and its timeout (which pauses the item for a human), and this tool never skips it", t.ref, strings.TrimSpace(h.Msg))
	}

	// 5. The move: the engine's own advance. advanceToNextStage returns nil when
	// there is no next stage, which advanceColumn has already ruled out.
	from := cur.Name
	if err := e.advanceToNextStage(&gh.ProjectBoard{ProjectID: projectID}, t.item, cur); err != nil {
		return nil, fmt.Errorf("advancing %s from %s to %s: %w", t.ref, from, next.Name, err)
	}
	e.logf(t.st.Number, "overseer", "advanced %s from %s to %s (requested by %s)\n", t.ref, from, next.Name, requester)

	res := &localapi.ActionResult{
		Action:  localapi.MethodAdvance,
		Issue:   t.ref,
		Changed: true,
		Summary: fmt.Sprintf("advanced %s from %s to %s; the engine dispatches %s on a following poll", t.ref, from, next.Name, next.Name),
		Writes:  []string{fmt.Sprintf("board status %s -> %s", from, next.Name)},
	}
	if mode := autonomyOf(labels); mode != "none" {
		res.Notes = append(res.Notes, fmt.Sprintf("the item carries fabrik:%s, so the engine would also auto-advance it; the autonomy label is unchanged", mode))
	}
	what := fmt.Sprintf("advanced %s from %s to %s (stage `%s` was complete)", t.ref, from, next.Name, from)
	return a.finish(res, t, what, requester), nil
}
