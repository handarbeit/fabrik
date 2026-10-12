package engine

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
)

// The overseer's move_parked action (#2188, ADR 2188): move an item from one
// unmanaged parking column to another (the "accepted, not yet" triage decision,
// e.g. Triage -> Backlog). It starts no work — the engine never dispatches for a
// parking column — which is why it is its own tool rather than a widened
// promote. It lives beside overseer_actions.go, not in localapi*.go /
// channel_*.go, for the same reason that file does (the read-path static scans).
//
// The write is the existing moveItemToStatus; the comment is the existing
// finish/postAudit path. There is no label write of any kind, so a paused,
// blocked, cruise or yolo item moves like any other and keeps its labels.

// parkingDestinations lists the columns a move_parked may name: configured
// unmanaged stages, plus the legacy Backlog column when no Backlog stage is
// configured, each only when it exists as a board option. Membership is decided
// by stages.IsParkingColumn so the legacy-Backlog rule is never redefined here.
// statusOptions is nil when the board's status field is not loaded yet.
func (a *overseerActor) parkingDestinations(statusOptions map[string]string) []string {
	var ss []string
	for _, s := range a.e.cfg.Stages {
		if !stages.IsParkingColumn(a.e.cfg.Stages, s.Name) {
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
	const legacy = "Backlog"
	if !slices.Contains(ss, legacy) && stages.IsParkingColumn(a.e.cfg.Stages, legacy) {
		if _, ok := statusOptions[legacy]; ok {
			ss = append(ss, legacy)
		}
	}
	return ss
}

// without returns list minus every entry equal to drop.
func without(list []string, drop string) []string {
	var out []string
	for _, s := range list {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// MoveParked moves an item between two parking columns. Like Promote, every ID
// comes from state the engine already holds, and the item's column is confirmed
// with one live ID-keyed read before anything is written.
func (a *overseerActor) MoveParked(p localapi.MoveParkedParams) (*localapi.ActionResult, error) {
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
	dests := a.parkingDestinations(options)
	// withTargets is true only while the item is established as parked, so a
	// refusal never offers destinations for a move that cannot be made from the
	// item's actual column.
	withTargets := false
	refuse := func(format string, args ...any) error {
		rs := a.refusalState(&t.st)
		if withTargets {
			rs.ValidTargets = without(dests, t.st.Status)
		}
		return localapi.Refused(rs, format, args...)
	}

	var projectID string
	if c := e.cache(); c != nil {
		projectID = c.ProjectID()
	}
	if field == nil || projectID == "" {
		return nil, refuse("the board metadata (project and status field) is not loaded yet; try again after the next poll")
	}

	const pipelineHint = "fabrik_move_parked only moves an item between parking columns (it never pulls work out of the pipeline); use fabrik_advance to move a completed stage forward"
	if why := a.parkedSourceRefusal(t.st.Status, pipelineHint); why != "" {
		return nil, refuse("%s %s", t.ref, why)
	}
	withTargets = true

	to := strings.TrimSpace(p.To)
	valid := without(dests, t.st.Status)
	if to == "" {
		if len(valid) == 0 {
			return nil, refuse("there is no other parking column on the board to move %s to", t.ref)
		}
		return nil, refuse("`to` is required; valid parking destinations: %s", strings.Join(valid, ", "))
	}
	if !slices.Contains(dests, to) {
		if stages.IsParkingColumn(e.cfg.Stages, to) {
			return nil, refuse("%q is a parking column but has no matching option on the board, so the move cannot be written (valid parking destinations: %s)", to, strings.Join(valid, ", "))
		}
		if len(valid) == 0 {
			return nil, refuse("%q is not a valid parking destination, and there is no other parking column on the board to move %s to", to, t.ref)
		}
		return nil, refuse("%q is not a valid parking destination (valid: %s); pipeline, holding and cleanup columns are not parking columns", to, strings.Join(valid, ", "))
	}
	if to == t.st.Status {
		return nil, refuse("%s is already in %q", t.ref, to)
	}

	live, err := a.confirmLiveStatus(t)
	if err != nil {
		return nil, err
	}
	if live != t.st.Status {
		// The cache lagged: judge against the live column, never the cached one.
		t.st.Status = live
	}
	if why := a.parkedSourceRefusal(live, pipelineHint); why != "" {
		withTargets = false
		return nil, refuse("%s %s (confirmed live)", t.ref, why)
	}
	if live == to {
		return nil, refuse("%s is already in %q (confirmed live)", t.ref, to)
	}

	if err := e.moveItemToStatus(projectID, t.item, field, to, requester); err != nil {
		return nil, fmt.Errorf("moving %s to %s: %w", t.ref, to, err)
	}
	from := t.st.Status
	e.logf(t.st.Number, "overseer", "moved parked %s from %s to %s (requested by %s)\n", t.ref, from, to, requester)

	res := &localapi.ActionResult{
		Action:  localapi.MethodMoveParked,
		Issue:   t.ref,
		Changed: true,
		Summary: fmt.Sprintf("moved %s from parking column %s to %s", t.ref, from, to),
		Writes:  []string{fmt.Sprintf("board status %s -> %s", from, to)},
	}
	return a.finish(res, t, fmt.Sprintf("moved %s from parking column %s to %s (no work started)", t.ref, from, to), requester), nil
}
