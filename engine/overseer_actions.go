package engine

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// The overseer actions (#1969, ADR-1966-c): the four mutating requests a
// Claude Code session overseeing the pipeline can make over the local socket —
// promote, set_autonomy, revalidate, clear_claude_limit. Each performs an
// existing, documented transition through the engine's own client and
// write-through helpers, then posts a 🏭 audit comment naming the requester.
//
// This file is deliberately NOT named localapi*.go or channel_*.go: those are
// globbed by the read-path static scans (TestLocalAPISourceDoesNotReachGitHub,
// TestChannelEventsSourceDoesNotReachGitHub), which forbid the very identifiers
// an action needs (e.client). Keeping the writes here keeps the read guarantee
// of ADR-1966-a pinned and unweakened.
//
// There is intentionally no action that removes fabrik:paused /
// fabrik:awaiting-input and none that posts a free-form comment (R2): a pause
// is lifted only by a human comment (resumeAuthorised, ADR-1813).

// overseerActor implements localapi.Actor. mu serialises actions with each
// other; it does not take the per-item in-flight guard, so an action races the
// poll loop exactly as a human applying a label or dragging a card does.
type overseerActor struct {
	e  *Engine
	mu sync.Mutex
}

// overseerActor returns the Actor to wire into the local API server.
func (e *Engine) overseerActor() localapi.Actor { return &overseerActor{e: e} }

var _ localapi.Actor = (*overseerActor)(nil)

const (
	labelCruise      = "fabrik:cruise"
	labelYolo        = "fabrik:yolo"
	labelRevalidate  = "fabrik:revalidate"
	labelClearLimit  = "fabrik:clear-claude-limit"
	labelClaudeLimit = "fabrik:claude-limit"

	maxRequesterLen = 64
)

// sanitizeRequester validates the self-asserted subscriber name and reduces it
// to a form that is safe to render inline in a public comment: only
// [A-Za-z0-9._:/-] survive (anything else — spaces, @, backticks, newlines,
// markdown — becomes "_"), truncated to maxRequesterLen. The result can never
// ping a user or break out of the code span the audit comment wraps it in.
func sanitizeRequester(name string) (string, error) {
	if err := channelevents.ValidateSubscriberName(name); err != nil {
		return "", localapi.Errorf(localapi.CodeBadRequest, "actions need an attributable requester: %v", err)
	}
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == ':', r == '/', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= maxRequesterLen {
			break
		}
	}
	return b.String(), nil
}

// overseerAuditBody builds the audit comment. It must start with the exact
// "🏭 **Fabrik" prefix: under PAT mode the comment is authored by the
// operator's own non-bot login, so filterNewComments' prefix check is the only
// thing keeping it from being read as human steering (R4).
func overseerAuditBody(action, what, requester string) string {
	return fmt.Sprintf("🏭 **Fabrik — overseer action: %s**\n\n%s; requested by subscriber `%s` via MCP.", action, what, requester)
}

// overseerTarget is the resolved, guarded item an action operates on.
type overseerTarget struct {
	st   itemstate.ItemState
	ref  string
	item gh.ProjectItem // minimal item for the write helpers
}

func (a *overseerActor) resolve(issue string) (*overseerTarget, error) {
	repo, number, err := localAPIBackend{e: a.e}.resolveIssue(issue)
	if err != nil {
		return nil, err
	}
	snap, ok := a.e.store.Peek(repo, number)
	if !ok {
		return nil, localapi.Errorf(localapi.CodeNotFound, "%s is not an item this daemon manages", issueRef(repo, number))
	}
	st := snap.State()
	t := &overseerTarget{st: st, ref: issueRef(st.Repo, st.Number)}
	if st.IsPR {
		return nil, localapi.Errorf(localapi.CodeNotFound, "%s is a pull request, not a managed issue", t.ref)
	}
	t.item = gh.ProjectItem{ID: st.ID, ItemID: st.ItemID, Number: st.Number, Repo: st.Repo, Status: st.Status, Labels: st.Labels}
	if st.IsClosed {
		return nil, localapi.Refused(a.refusalState(&st), "%s is closed", t.ref)
	}
	return t, nil
}

// refusalState is the "current state" a refusal carries.
func (a *overseerActor) refusalState(st *itemstate.ItemState) localapi.RefusalState {
	var labels []string
	for _, l := range st.Labels {
		if strings.HasPrefix(l, "fabrik:") || strings.HasPrefix(l, "stage:") {
			labels = append(labels, l)
		}
	}
	sort.Strings(labels)
	return localapi.RefusalState{
		Issue:    issueRef(st.Repo, st.Number),
		Status:   st.Status,
		Labels:   labels,
		Autonomy: autonomyOf(st.Labels),
	}
}

func autonomyOf(labels []string) string {
	switch {
	case hasLabelStr(labels, labelCruise):
		return "cruise" // cruise wins when both are present
	case hasLabelStr(labels, labelYolo):
		return "yolo"
	}
	return "none"
}

// confirmLiveStatus re-reads the item's board column with one ID-keyed query
// (the #1871 liveLandingState precedent; not an ID lookup). The cache can lag
// GitHub, and a promote or revalidate acting on a stale column is the one
// failure that is expensive to undo (a backwards move), so it fails closed: a
// read error refuses the action.
func (a *overseerActor) confirmLiveStatus(t *overseerTarget) (string, error) {
	if t.st.ItemID == "" {
		return "", localapi.Refused(a.refusalState(&t.st), "%s has no known project item id, so its column cannot be confirmed", t.ref)
	}
	live, err := a.e.client.FetchProjectItemStatus(t.st.ItemID)
	if err != nil {
		return "", localapi.Refused(a.refusalState(&t.st), "could not confirm %s's current column live (%v); refusing rather than acting on the cache", t.ref, err)
	}
	return live, nil
}

// finish posts the audit comment after a successful write. A failed comment
// does not undo the action (a revert would be a second unaudited write): the
// result says so and it is logged by postComment.
func (a *overseerActor) finish(res *localapi.ActionResult, t *overseerTarget, what, requester string) *localapi.ActionResult {
	if err := a.postAudit(t, res.Action, what, requester); err != nil {
		res.AuditComment = localapi.AuditFailed
		res.Notes = append(res.Notes, fmt.Sprintf("the audit comment could not be posted (%v); the action itself was applied", err))
		return res
	}
	res.AuditComment = localapi.AuditPosted
	return res
}

// postAudit is the one place an overseer audit comment is posted (pinned by
// TestOverseerNoPauseLiftOrFreeFormCommentSurface).
func (a *overseerActor) postAudit(t *overseerTarget, action, what, requester string) error {
	_, err := a.e.postComment(t.item, overseerAuditBody(action, what, requester), false, true)
	return err
}

// promotableTargets lists the pipeline columns a promote may name: configured
// stages that are not unmanaged parking columns, holding stages or the cleanup
// stage (those moves are engine-managed) and that exist as a board option.
// statusOptions is nil when the board's status field is not loaded yet.
func (a *overseerActor) promotableTargets(statusOptions map[string]string) []string {
	var ss []string
	for _, s := range a.e.cfg.Stages {
		if s.Unmanaged || s.HoldingStage || s.CleanupWorktree {
			continue
		}
		if _, ok := statusOptions[s.Name]; !ok {
			continue
		}
		ss = append(ss, s.Name)
	}
	sort.SliceStable(ss, func(i, j int) bool {
		return a.e.stageByName(ss[i]).Order < a.e.stageByName(ss[j]).Order
	})
	return ss
}

// moveItemToStatus moves item's board Status to the named column through the
// engine's usual status-write idiom: GitHub mutation, cache write-through,
// staleness-baseline advance and webhook echo. It is the generic helper the
// four hand-copied moves (advanceToNextStage, advanceToQueued,
// rerouteQueuedMemberOffHolding, moveItemToValidate) never got; only new code
// uses it so those merge-train paths stay untouched.
func (e *Engine) moveItemToStatus(projectID string, item gh.ProjectItem, field *gh.StatusField, targetName string) error {
	if field == nil {
		return errors.New("status field metadata not available")
	}
	optionID, ok := field.Options[targetName]
	if !ok {
		return fmt.Errorf("no status option %q found on project board (available: %v)", targetName, mapKeys(field.Options))
	}
	if projectID == "" {
		return errors.New("project id not available")
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if err := e.client.UpdateProjectItemStatus(projectID, item.ItemID, field.FieldID, optionID); err != nil {
		return err
	}
	if c := e.cache(); c != nil {
		c.UpdateItemStatus(boardcache.ItemKey(owner+"/"+repo, item.Number), targetName)
	}
	e.store.Apply(itemstate.SelfWriteObserved{Repo: owner + "/" + repo, Number: item.Number})
	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEchoIfSubscribed("projects_v2_item", "edited", item.ItemID)
	}
	return nil
}

// Promote moves an item from an unmanaged parking column into a pipeline
// column, with every ID taken from state the engine already holds: the project
// id from the board cache, the item id from the store, the field and option ids
// from e.statusField. No FetchStatusField / FetchProjectBoard /
// LookupIssueProjectItem is made.
func (a *overseerActor) Promote(p localapi.PromoteParams) (*localapi.ActionResult, error) {
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

	e.mu.Lock()
	field := e.statusField
	e.mu.Unlock()
	var options map[string]string
	if field != nil {
		options = field.Options
	}
	targets := a.promotableTargets(options)
	refuse := func(format string, args ...any) error {
		rs := a.refusalState(&t.st)
		rs.ValidTargets = targets
		return localapi.Refused(rs, format, args...)
	}

	var projectID string
	if c := e.cache(); c != nil {
		projectID = c.ProjectID()
	}
	if field == nil || projectID == "" {
		return nil, refuse("the board metadata (project and status field) is not loaded yet; try again after the next poll")
	}

	to := strings.TrimSpace(p.To)
	if to == "" {
		return nil, refuse("`to` is required; valid targets: %s", strings.Join(targets, ", "))
	}
	if !slices.Contains(targets, to) {
		return nil, refuse("%q is not a valid promote target (valid: %s); holding, cleanup and unmanaged columns are engine-managed", to, strings.Join(targets, ", "))
	}
	if why := a.parkingRefusal(t.st.Status); why != "" {
		return nil, refuse("%s %s", t.ref, why)
	}

	live, err := a.confirmLiveStatus(t)
	if err != nil {
		return nil, err
	}
	if live != t.st.Status {
		// The cache lagged: judge against the live column, never the cached one.
		t.st.Status = live
	}
	if why := a.parkingRefusal(live); why != "" {
		return nil, refuse("%s %s (confirmed live)", t.ref, why)
	}

	if err := e.moveItemToStatus(projectID, t.item, field, to); err != nil {
		return nil, fmt.Errorf("moving %s to %s: %w", t.ref, to, err)
	}
	from := t.st.Status
	e.logf(t.st.Number, "overseer", "promoted %s from %s to %s (requested by %s)\n", t.ref, from, to, requester)

	res := &localapi.ActionResult{
		Action:  localapi.MethodPromote,
		Issue:   t.ref,
		Changed: true,
		Summary: fmt.Sprintf("moved %s from %s to %s", t.ref, from, to),
		Writes:  []string{fmt.Sprintf("board status %s -> %s", from, to)},
	}
	if mode := autonomyOf(t.st.Labels); mode != "none" {
		res.Notes = append(res.Notes, fmt.Sprintf("the item carries fabrik:%s, so it will auto-advance through the pipeline from %s", mode, to))
	}
	return a.finish(res, t, fmt.Sprintf("moved %s from %s to %s", t.ref, from, to), requester), nil
}

// parkingRefusal returns "" when status names an unmanaged parking column,
// otherwise the reason a promote is refused.
func (a *overseerActor) parkingRefusal(status string) string {
	if status == "" {
		return "has no known board column, so it is not established as parked"
	}
	s := a.e.stageByName(status)
	switch {
	case s == nil:
		return fmt.Sprintf("is in column %q, which has no configured stage", status)
	case !s.Unmanaged:
		return fmt.Sprintf("is already in the pipeline column %q; promote only moves items out of an unmanaged parking column", status)
	}
	return ""
}

// SetAutonomy makes the item's autonomy exactly the requested mode. Writes run
// add-before-remove (and yolo is removed before cruise for "none"), so a
// partial failure leaves the more conservative state: cruise beats yolo.
func (a *overseerActor) SetAutonomy(p localapi.SetAutonomyParams) (*localapi.ActionResult, error) {
	requester, err := sanitizeRequester(p.Subscriber)
	if err != nil {
		return nil, err
	}
	wantCruise, wantYolo := false, false
	switch p.Mode {
	case "cruise":
		wantCruise = true
	case "yolo":
		wantYolo = true
	case "none":
	default:
		return nil, localapi.Errorf(localapi.CodeBadRequest, "mode must be one of cruise, yolo, none (got %q)", p.Mode)
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	t, err := a.resolve(p.Issue)
	if err != nil {
		return nil, err
	}
	e := a.e
	hasCruise, hasYolo := hasLabelStr(t.st.Labels, labelCruise), hasLabelStr(t.st.Labels, labelYolo)
	res := &localapi.ActionResult{Action: localapi.MethodSetAutonomy, Issue: t.ref}
	if hasCruise == wantCruise && hasYolo == wantYolo {
		res.Summary = fmt.Sprintf("%s is already set to autonomy %q; nothing changed", t.ref, p.Mode)
		res.AuditComment = localapi.AuditNone
		return res, nil
	}

	type step struct {
		add   bool
		label string
	}
	var steps []step
	switch {
	case wantCruise:
		if !hasCruise {
			steps = append(steps, step{true, labelCruise})
		}
		if hasYolo {
			steps = append(steps, step{false, labelYolo})
		}
	case wantYolo:
		if !hasYolo {
			steps = append(steps, step{true, labelYolo})
		}
		if hasCruise {
			steps = append(steps, step{false, labelCruise})
		}
	default:
		if hasYolo {
			steps = append(steps, step{false, labelYolo})
		}
		if hasCruise {
			steps = append(steps, step{false, labelCruise})
		}
	}
	for _, s := range steps {
		var werr error
		verb, doing := "removed", "remove"
		if s.add {
			verb, doing = "added", "add"
			werr = e.addLabelChecked(t.item, s.label)
		} else {
			werr = e.removeLabelChecked(t.item, s.label)
		}
		if werr != nil {
			if len(res.Writes) == 0 {
				return nil, fmt.Errorf("setting autonomy %q on %s failed at %s %s (%v); no writes had landed; no audit comment posted", p.Mode, t.ref, doing, s.label, werr)
			}
			// An earlier step already changed a label, so the audit trail must
			// record it even though the action did not finish.
			applied := strings.Join(res.Writes, ", ")
			note := "audit comment posted"
			what := fmt.Sprintf("PARTIALLY applied autonomy `%s` to %s: %s, then failed at %s `%s`; the item may carry both autonomy labels (cruise wins)", p.Mode, t.ref, applied, doing, s.label)
			if cerr := a.postAudit(t, localapi.MethodSetAutonomy, what, requester); cerr != nil {
				note = fmt.Sprintf("the audit comment also failed (%v)", cerr)
			}
			e.logf(t.st.Number, "overseer", "autonomy of %s partially set to %s (%s; failed at %s %s: %v; requested by %s)\n", t.ref, p.Mode, applied, doing, s.label, werr, requester)
			return nil, fmt.Errorf("setting autonomy %q on %s failed at %s %s (%v); already applied: %s; %s", p.Mode, t.ref, doing, s.label, werr, applied, note)
		}
		res.Writes = append(res.Writes, verb+" "+s.label)
	}
	res.Changed = true
	res.Summary = fmt.Sprintf("set autonomy of %s to %q", t.ref, p.Mode)
	if wantYolo {
		res.Notes = append(res.Notes, "yolo lets the item auto-advance and auto-merge its PR when Validate completes; use cruise to stop short of the merge")
		if hasCruise {
			res.Notes = append(res.Notes, "this widened the item from cruise to yolo")
		}
	}
	e.logf(t.st.Number, "overseer", "autonomy of %s set to %s (%s; requested by %s)\n", t.ref, p.Mode, strings.Join(res.Writes, ", "), requester)
	return a.finish(res, t, fmt.Sprintf("set autonomy of %s to `%s` (%s)", t.ref, p.Mode, strings.Join(res.Writes, ", ")), requester), nil
}

// Revalidate applies fabrik:revalidate to an item sitting in Validate. The
// engine's handleRevalidateLabel clears fabrik:paused / fabrik:awaiting-input
// as part of re-entering Validate, so a paused item is refused here: letting a
// label lift a pause would be the label-based pause-lift R2 forbids. Only a
// human comment lifts a pause.
func (a *overseerActor) Revalidate(p localapi.RevalidateParams) (*localapi.ActionResult, error) {
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
	const validate = "Validate"
	if t.st.Status != validate {
		if t.st.Status == "" {
			return nil, refuse("%s has no known board column, so it is not established to be in %s", t.ref, validate)
		}
		return nil, refuse("%s is in %q, not %s; revalidate only applies to a Validate item (the engine would just strip the label)", t.ref, t.st.Status, validate)
	}
	if hasLabelStr(t.st.Labels, labelRevalidate) {
		return nil, refuse("%s already carries %s; the engine consumes it on a later poll", t.ref, labelRevalidate)
	}
	if hasLabelStr(t.st.Labels, "fabrik:paused") || hasLabelStr(t.st.Labels, "fabrik:awaiting-input") {
		return nil, refuse("%s is paused or awaiting input; revalidate would clear the pause, and only a human comment lifts a pause — comment on the issue instead", t.ref)
	}
	live, err := a.confirmLiveStatus(t)
	if err != nil {
		return nil, err
	}
	if live != validate {
		t.st.Status = live
		return nil, refuse("%s is in %q on GitHub, not %s (the daemon's cache was stale)", t.ref, live, validate)
	}

	if err := e.addLabelChecked(t.item, labelRevalidate); err != nil {
		return nil, fmt.Errorf("adding %s to %s: %w", labelRevalidate, t.ref, err)
	}
	e.logf(t.st.Number, "overseer", "revalidate requested for %s (requested by %s)\n", t.ref, requester)
	res := &localapi.ActionResult{
		Action:  localapi.MethodRevalidate,
		Issue:   t.ref,
		Changed: true,
		Summary: fmt.Sprintf("applied %s to %s; Validate re-runs on a following poll", labelRevalidate, t.ref),
		Writes:  []string{"added " + labelRevalidate},
	}
	if t.st.Worker != nil {
		res.Notes = append(res.Notes, "a Validate worker is in flight; the engine defers the revalidate until it exits")
	}
	return a.finish(res, t, fmt.Sprintf("requested a Validate re-run of %s (applied `%s`)", t.ref, labelRevalidate), requester), nil
}

// ClearClaudeLimit applies fabrik:clear-claude-limit to one managed open item.
// The suspension the engine's settle scan then clears is account-wide, but the
// label has to sit on some item, so the daemon picks one deterministically —
// one already carrying fabrik:claude-limit first, else the lowest-numbered —
// and names it in the result and the audit comment.
func (a *overseerActor) ClearClaudeLimit(p localapi.ClearClaudeLimitParams) (*localapi.ActionResult, error) {
	requester, err := sanitizeRequester(p.Subscriber)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.e

	if _, ok := e.claudeSuspendedUntilTime(e.now()); !ok {
		return nil, localapi.Refused(localapi.RefusalState{}, "no Claude usage-limit suspension is active in the daemon, so there is nothing to clear")
	}

	var pick *itemstate.ItemState
	var pending string
	better := func(c, cur *itemstate.ItemState) bool {
		cl, ul := hasLabelStr(c.Labels, labelClaudeLimit), hasLabelStr(cur.Labels, labelClaudeLimit)
		if cl != ul {
			return cl
		}
		if c.Number != cur.Number {
			return c.Number < cur.Number
		}
		return c.Repo < cur.Repo
	}
	e.store.Scan(func(it *itemstate.ItemState) {
		if it.IsPR || it.IsClosed || it.Repo == "" || it.ItemID == "" {
			return
		}
		if hasLabelStr(it.Labels, labelClearLimit) && pending == "" {
			pending = issueRef(it.Repo, it.Number)
		}
		if pick == nil || better(it, pick) {
			cp := *it
			pick = &cp
		}
	})
	if pending != "" {
		return &localapi.ActionResult{
			Action:       localapi.MethodClearClaudeLimit,
			Issue:        pending,
			Summary:      fmt.Sprintf("a clear request is already pending on %s; the engine consumes it on its next poll", pending),
			AuditComment: localapi.AuditNone,
		}, nil
	}
	if pick == nil {
		return nil, localapi.Refused(localapi.RefusalState{}, "no managed open issue is cached to carry %s", labelClearLimit)
	}

	t := &overseerTarget{st: *pick, ref: issueRef(pick.Repo, pick.Number)}
	t.item = gh.ProjectItem{ID: pick.ID, ItemID: pick.ItemID, Number: pick.Number, Repo: pick.Repo, Status: pick.Status, Labels: pick.Labels}
	if err := e.addLabelChecked(t.item, labelClearLimit); err != nil {
		return nil, fmt.Errorf("adding %s to %s: %w", labelClearLimit, t.ref, err)
	}
	e.logf(t.st.Number, "overseer", "clear-claude-limit requested via %s (requested by %s)\n", t.ref, requester)
	res := &localapi.ActionResult{
		Action:  localapi.MethodClearClaudeLimit,
		Issue:   t.ref,
		Changed: true,
		Summary: fmt.Sprintf("applied %s to %s; the engine clears the account-wide suspension on its next poll", labelClearLimit, t.ref),
		Writes:  []string{"added " + labelClearLimit},
		Notes:   []string{fmt.Sprintf("the label was placed on %s, chosen by the daemon; the suspension it clears is account-wide, not specific to that issue", t.ref)},
	}
	return a.finish(res, t, fmt.Sprintf("requested clearing the account-wide Claude usage-limit suspension (applied `%s` to %s, an item the daemon chose; the suspension is not specific to this issue)", labelClearLimit, t.ref), requester), nil
}
