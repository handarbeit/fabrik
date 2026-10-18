package engine

import (
	"fmt"
	"strconv"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// The overseer's set_blocked_by action (#2385, ADR 2385): add and remove
// blockedBy edges on one issue, so an overseer can order work through the MCP
// instead of raw GraphQL under a second token. It lives beside
// overseer_actions.go, not in localapi*.go / channel_*.go, for the same reason
// that file does (the read-path static scans).
//
// The writes are the existing AddBlockedByIssue and the new
// RemoveBlockedByIssue; the comment is the existing finish/postAudit path. The
// action starts no work, moves no column and writes no label: in particular it
// never adds or removes fabrik:blocked, which stays with checkDependencies'
// live read of BlockedBy (ADR-1419). An item that loses its last blocker is
// released by the engine's normal path; the only nudge is an already-due
// "dep-blocked" admission cooldown (see nudgeRelease).

// blockerRef names one issue as "owner/repo" plus number.
type blockerRef struct {
	repo   string
	number int
}

func (r blockerRef) String() string { return issueRef(r.repo, r.number) }

// parseBlockerRef parses "owner/repo#N", "#N" or a bare "N". A bare number
// means the TARGET's repo (targetRepo), not the daemon's single repo as it does
// for an issue argument.
func parseBlockerRef(s, targetRepo string) (blockerRef, error) {
	raw := s
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	if i := strings.LastIndex(s, "#"); i > 0 {
		n, err := strconv.Atoi(s[i+1:])
		if err != nil || n <= 0 || !isRealRepoKey(s[:i]) {
			return blockerRef{}, fmt.Errorf("bad blocker %q: want owner/repo#N or N", raw)
		}
		return blockerRef{repo: s[:i], number: n}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return blockerRef{}, fmt.Errorf("bad blocker %q: want owner/repo#N or N", raw)
	}
	return blockerRef{repo: targetRepo, number: n}, nil
}

// depRef normalises a cached edge to a blockerRef: an empty Dependency.Repo
// means the owning item's repo.
func depRef(itemRepo string, d gh.Dependency) blockerRef {
	if d.Repo == "" {
		return blockerRef{repo: itemRepo, number: d.Number}
	}
	return blockerRef{repo: d.Repo, number: d.Number}
}

func sameBlocker(a, b blockerRef) bool {
	return a.number == b.number && strings.EqualFold(a.repo, b.repo)
}

// edgeCached reports whether target's cached edges already hold ref.
func edgeCached(st *itemstate.ItemState, ref blockerRef) bool {
	for _, d := range st.BlockedBy {
		if sameBlocker(depRef(st.Repo, d), ref) {
			return true
		}
	}
	return false
}

// edgeKnown reports whether an item's cached edges can be trusted as the
// complete list: only a deep fetch makes them authoritative. A write-through add
// (this action's, or spawn's) puts one edge into the cache of a never-fetched
// item, which would make a "len(BlockedBy) > 0" test call a partial list
// complete, so a removal of an unseen real edge would be reported as a no-op and
// a cycle through unseen edges as "no cycle". This is deliberately stricter than
// fabrik_status's blockers "unknown" display (localapi.go blockers), which also
// reads a non-empty list as known: here a wrong answer would skip a write or
// miss a cycle, there it only labels a display.
func edgeKnown(st *itemstate.ItemState) bool {
	return !st.LastDeepFetchAt.IsZero()
}

// blockerCycle walks the cached blockedBy edges transitively from start looking
// for target, which would make "target blocked by start" close a cycle. It reads
// the Store through peek only (never Store.Get, whose fallback fetcher makes a
// live read), and its polarity is the opposite of detectCycle's: a node it
// cannot see, or whose edges the cache cannot vouch for, is reported as unknown
// and never counted as "no cycle". A closed node is a leaf: it satisfies a
// dependency, so its own edges cannot hold the target (the gate,
// openDependencies, ignores a closed blocker the same way). startClosed is the
// resolver's knowledge about start itself.
//
// A definite cycle wins over an unknown node found elsewhere. cycle is the path
// start -> ... -> target as "blocked by" hops, rendered for the refusal.
func blockerCycle(peek func(repo string, n int) (itemstate.ItemState, bool), target, start blockerRef, startClosed bool) (cycle []blockerRef, unknown *blockerRef) {
	if sameBlocker(target, start) {
		return []blockerRef{target, start}, nil
	}
	if startClosed {
		return nil, nil
	}
	type node = blockerRef
	parent := map[string]*node{}
	key := func(r node) string { return strings.ToLower(r.repo) + "#" + strconv.Itoa(r.number) }
	parent[key(start)] = nil
	queue := []node{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		st, ok := peek(cur.repo, cur.number)
		if !ok {
			if unknown == nil {
				c := cur
				unknown = &c
			}
			continue
		}
		if st.IsClosed {
			continue
		}
		if !edgeKnown(&st) {
			if unknown == nil {
				c := cur
				unknown = &c
			}
			continue
		}
		for _, d := range st.BlockedBy {
			next := depRef(st.Repo, d)
			if sameBlocker(next, target) {
				// cur is blocked by the target: path target <- start ... <- cur.
				path := []blockerRef{target}
				for p := &cur; p != nil; p = parent[key(*p)] {
					path = append(path, *p)
				}
				// path is target, cur, ..., start; reverse the tail to start..cur.
				out := []blockerRef{path[len(path)-1]}
				for i := len(path) - 2; i >= 1; i-- {
					out = append(out, path[i])
				}
				return append(out, target), nil
			}
			if _, seen := parent[key(next)]; seen {
				continue
			}
			c := cur
			parent[key(next)] = &c
			queue = append(queue, next)
		}
	}
	return nil, unknown
}

// resolvedBlocker is a blocker that is established to be a writable issue.
type resolvedBlocker struct {
	ref    blockerRef
	nodeID string
	closed bool
}

// resolveBlocker establishes that ref is an issue (never a pull request) and
// finds its node ID, from the Store first and otherwise one live REST read
// (FetchProjectItem). It never guesses: any failure returns a reason, so the
// caller refuses before writing.
func (a *overseerActor) resolveBlocker(ref blockerRef) (resolvedBlocker, string) {
	if snap, ok := a.e.store.Peek(ref.repo, ref.number); ok {
		st := snap.State()
		if st.IsPR {
			return resolvedBlocker{}, fmt.Sprintf("%s is a pull request, not an issue; only issues can block", ref)
		}
		if st.ID != "" {
			return resolvedBlocker{ref: blockerRef{repo: st.Repo, number: st.Number}, nodeID: st.ID, closed: st.IsClosed}, ""
		}
	}
	owner, repo := parseOwnerRepo(ref.repo)
	if owner == "" || repo == "" {
		return resolvedBlocker{}, fmt.Sprintf("%s is not a valid owner/repo reference", ref)
	}
	pi, err := a.e.client.FetchProjectItem(owner, repo, ref.number)
	switch {
	case err != nil:
		return resolvedBlocker{}, fmt.Sprintf("%s could not be resolved (%v)", ref, err)
	case pi == nil:
		return resolvedBlocker{}, fmt.Sprintf("%s does not resolve to an issue", ref)
	case pi.IsPR:
		return resolvedBlocker{}, fmt.Sprintf("%s is a pull request, not an issue; only issues can block", ref)
	case pi.ID == "":
		return resolvedBlocker{}, fmt.Sprintf("%s resolved without a node id, so it cannot be linked", ref)
	}
	return resolvedBlocker{ref: ref, nodeID: pi.ID, closed: pi.IsClosed}, ""
}

// edgePlan is one requested edge after the guard phase.
type edgePlan struct {
	op      string // localapi.EdgeAdd / EdgeRemove
	ref     blockerRef
	noop    string // non-empty: nothing to write, and why
	blocker resolvedBlocker
}

// SetBlockedBy adds and removes blockedBy edges on one target issue. Every guard
// is evaluated for every requested edge before any GitHub write, so a refusal
// never leaves a partial change; only a GitHub failure part-way through the
// write loop can, and that is reported exactly.
func (a *overseerActor) SetBlockedBy(p localapi.SetBlockedByParams) (*localapi.ActionResult, error) {
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

	if len(p.Add) == 0 && len(p.Remove) == 0 {
		return nil, refuse("nothing to do: name at least one blocker to add or remove")
	}
	if t.st.ID == "" {
		return nil, refuse("%s has no known node id, so its blockers cannot be changed", t.ref)
	}
	target := blockerRef{repo: t.st.Repo, number: t.st.Number}

	// Parse and de-duplicate both lists; a blocker may not be in both.
	parse := func(list []string, op string) ([]edgePlan, error) {
		var out []edgePlan
		for _, s := range list {
			ref, perr := parseBlockerRef(s, target.repo)
			if perr != nil {
				return nil, refuse("%v", perr)
			}
			dup := false
			for _, q := range out {
				if sameBlocker(q.ref, ref) {
					dup = true
					break
				}
			}
			if !dup {
				out = append(out, edgePlan{op: op, ref: ref})
			}
		}
		return out, nil
	}
	adds, err := parse(p.Add, localapi.EdgeAdd)
	if err != nil {
		return nil, err
	}
	removes, err := parse(p.Remove, localapi.EdgeRemove)
	if err != nil {
		return nil, err
	}
	for _, ad := range adds {
		if sameBlocker(ad.ref, target) {
			return nil, refuse("%s cannot block itself", t.ref)
		}
		for _, rm := range removes {
			if sameBlocker(ad.ref, rm.ref) {
				return nil, refuse("%s is in both add and remove; name each blocker in only one list", ad.ref)
			}
		}
	}

	// Guard phase over the whole request: cached no-ops, then resolution of every
	// edge that needs a write, then the cycle check for every such add.
	plans := append(adds, removes...)
	for i := range plans {
		pl := &plans[i]
		cached := edgeCached(&t.st, pl.ref)
		switch {
		case pl.op == localapi.EdgeAdd && cached:
			pl.noop = "already in the cached edges"
			continue
		case pl.op == localapi.EdgeRemove && !cached:
			if !edgeKnown(&t.st) {
				// An item the daemon never deep-fetched (a parked card) has an empty
				// cache that means "unknown", not "no blockers": calling the removal a
				// no-op would silently leave a real edge in place.
				return nil, refuse("cannot tell whether %s is blocked by %s: the cached blockers of %s are unknown (it has not been fetched yet); retry once the daemon has fetched it", t.ref, pl.ref, t.ref)
			}
			pl.noop = "not in the cached edges"
			continue
		}
		rb, why := a.resolveBlocker(pl.ref)
		if why != "" {
			return nil, refuse("%s", why)
		}
		pl.blocker = rb
		if pl.op != localapi.EdgeAdd {
			continue // removals need no cycle check
		}
		cycle, unknown := blockerCycle(a.peekState, target, rb.ref, rb.closed)
		if cycle != nil {
			hops := make([]string, len(cycle))
			for j, c := range cycle {
				hops[j] = c.String()
			}
			return nil, refuse("adding %s as a blocker of %s would close a cycle (%s)", pl.ref, t.ref, strings.Join(hops, " is blocked by "))
		}
		if unknown != nil {
			return nil, refuse("cannot rule out a cycle from adding %s to %s: the cached blockers of %s are unknown (it is not tracked by this daemon or has not been fetched yet); retry once the daemon has fetched it", pl.ref, t.ref, unknown)
		}
	}

	res := &localapi.ActionResult{Action: localapi.MethodSetBlockedBy, Issue: t.ref}
	var applied, removedOnce []string
	wroteRemove := false
	for i := range plans {
		pl := &plans[i]
		er := localapi.EdgeResult{Blocker: pl.ref.String(), Op: pl.op}
		if pl.noop != "" {
			er.Outcome, er.Reason = localapi.EdgeNoop, pl.noop
			res.Edges = append(res.Edges, er)
			continue
		}
		var werr error
		if pl.op == localapi.EdgeAdd {
			werr = e.client.AddBlockedByIssue(t.st.ID, pl.blocker.nodeID)
		} else {
			werr = e.client.RemoveBlockedByIssue(t.st.ID, pl.blocker.nodeID)
		}
		if werr != nil {
			// Stop at the first failure. The cache holds exactly what was written.
			for j := i; j < len(plans); j++ {
				res.Edges = append(res.Edges, localapi.EdgeResult{Blocker: plans[j].ref.String(), Op: plans[j].op, Outcome: localapi.EdgeNotApplied, Reason: "the call stopped at an earlier failure"})
			}
			return nil, a.partialBlockedByFailure(t, requester, pl, werr, res.Edges, applied, removedOnce)
		}
		state := "OPEN"
		if pl.blocker.closed {
			state = "CLOSED"
		}
		dep := gh.Dependency{Number: pl.blocker.ref.number, Repo: pl.blocker.ref.repo, State: state}
		if pl.op == localapi.EdgeAdd {
			e.store.Apply(itemstate.BlockedByEdgeAdded{Repo: t.st.Repo, Number: t.st.Number, Dep: dep})
			applied = append(applied, pl.ref.String())
			res.Writes = append(res.Writes, "added blocker "+pl.ref.String())
		} else {
			e.store.Apply(itemstate.BlockedByEdgeRemoved{Repo: t.st.Repo, Number: t.st.Number, Dep: dep})
			removedOnce = append(removedOnce, pl.ref.String())
			res.Writes = append(res.Writes, "removed blocker "+pl.ref.String())
			wroteRemove = true
		}
		er.Outcome = localapi.EdgeApplied
		res.Edges = append(res.Edges, er)
	}

	if len(res.Writes) == 0 {
		res.AuditComment = localapi.AuditNone
		res.Summary = fmt.Sprintf("no change to the blockers of %s: every requested edge was already in the requested state", t.ref)
		res.Notes = append(res.Notes, "no-ops are judged against the daemon's cached edges, not a live read")
		return res, nil
	}
	res.Changed = true
	res.Summary = fmt.Sprintf("changed the blockers of %s: %s", t.ref, blockedByWhat(applied, removedOnce))
	res.Notes = append(res.Notes, "this tool never adds or removes fabrik:blocked: the engine's dependency check decides that from a live read on the item's next evaluation, and starts no work itself")
	if wroteRemove && a.nudgeRelease(t) {
		res.Notes = append(res.Notes, "the item carries fabrik:blocked; it is re-checked on the next poll rather than after the dependency recheck interval")
	}
	e.logf(t.st.Number, "overseer", "blockers of %s changed: %s (requested by %s)\n", t.ref, blockedByWhat(applied, removedOnce), requester)
	return a.finish(res, t, fmt.Sprintf("changed the `blockedBy` edges of %s: %s", t.ref, blockedByWhat(applied, removedOnce)), requester), nil
}

// peekState adapts the Store's no-fetch read to blockerCycle.
func (a *overseerActor) peekState(repo string, n int) (itemstate.ItemState, bool) {
	snap, ok := a.e.store.Peek(repo, n)
	if !ok {
		return itemstate.ItemState{}, false
	}
	return snap.State(), true
}

// nudgeRelease records an already-due "dep-blocked" admission cooldown on a
// target that still carries fabrik:blocked, after an edge was removed. Removal
// does not bump the issue's updatedAt (#977) and BlockedByChanged wakes nothing,
// so without it the item would wait out the whole recheck interval before
// checkDependencies looks again. The cooldown is an in-memory admission hint
// only: the engine's live read still alone decides the label (ADR-1419), so a
// wrong nudge costs one extra evaluation. Reports whether it recorded one.
func (a *overseerActor) nudgeRelease(t *overseerTarget) bool {
	snap, ok := a.e.store.Peek(t.st.Repo, t.st.Number)
	if !ok || !hasLabelStr(snap.State().Labels, "fabrik:blocked") {
		return false
	}
	a.e.store.Apply(itemstate.CooldownRecorded{Repo: t.st.Repo, Number: t.st.Number, Reason: "dep-blocked", Until: a.e.now()})
	return true
}

// partialBlockedByFailure reports a GitHub failure part-way through the write
// loop, following SetAutonomy's precedent: when something was already written
// the audit trail records it ("PARTIALLY applied") and the error names what was
// and was not applied; when nothing was, no comment is posted.
func (a *overseerActor) partialBlockedByFailure(t *overseerTarget, requester string, failed *edgePlan, werr error, edges []localapi.EdgeResult, added, removed []string) error {
	doing := "add"
	if failed.op == localapi.EdgeRemove {
		doing = "remove"
	}
	var notApplied []string
	for _, er := range edges {
		if er.Outcome == localapi.EdgeNotApplied {
			notApplied = append(notApplied, er.Op+" "+er.Blocker)
		}
	}
	if len(added)+len(removed) == 0 {
		return fmt.Errorf("changing the blockers of %s failed at %s %s (%v); no writes had landed; no audit comment posted", t.ref, doing, failed.ref, werr)
	}
	applied := blockedByWhat(added, removed)
	note := "audit comment posted"
	what := fmt.Sprintf("PARTIALLY applied blocker changes to %s: %s, then failed at %s `%s`; not applied: %s", t.ref, applied, doing, failed.ref, strings.Join(notApplied, ", "))
	if cerr := a.postAudit(t, localapi.MethodSetBlockedBy, what, requester); cerr != nil {
		note = fmt.Sprintf("the audit comment also failed (%v)", cerr)
	}
	a.e.logf(t.st.Number, "overseer", "blockers of %s partially changed (%s; failed at %s %s: %v; requested by %s)\n", t.ref, applied, doing, failed.ref, werr, requester)
	return fmt.Errorf("changing the blockers of %s failed at %s %s (%v); already applied: %s; not applied: %s; %s", t.ref, doing, failed.ref, werr, applied, strings.Join(notApplied, ", "), note)
}

// blockedByWhat renders the applied edges, always fully qualified owner/repo#N.
func blockedByWhat(added, removed []string) string {
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ", "))
	}
	return strings.Join(parts, "; ")
}
