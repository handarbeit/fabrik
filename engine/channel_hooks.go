package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Transition hooks for the channel-event deriver (#1968). Each is called from
// an engine transition point *after* the engine's own writes, is a void call
// that cannot change a return value or a decision (R10), reads item state
// through Store.Peek only (R9), and is a no-op when no hub is running. A panic
// is recovered and logged.

// hookEvent runs fn under recover when a hub is running and hands every event
// it returns to the deriver's queue.
func (e *Engine) hookEvent(item gh.ProjectItem, what string, fn func(st *itemstate.ItemState) []channelevents.Event) {
	ce := e.channelEvents()
	if ce == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			e.logf(item.Number, "channel", "%s hook panic recovered: %v\n", what, r)
		}
	}()
	snap, ok := e.store.Peek(itemOwnerRepoString(item, e.defaultRepo()), item.Number)
	if !ok {
		return
	}
	st := snap.State()
	ce.enqueue(fn(&st)...)
}

// emitCITimeout reports a fresh CI-gate timeout pause (not a reapplied one: the
// episode already announced itself).
func (e *Engine) emitCITimeout(item gh.ProjectItem, stage *stages.Stage) {
	e.hookEvent(item, "ci-timeout", func(st *itemstate.ItemState) []channelevents.Event {
		ev := e.baseEvent(st, channelevents.CITimeout)
		if stage != nil {
			ev.Stage = stage.Name
		}
		wait := e.ciWaitTimeout()
		ev.Meta["timeout"] = wait.String()
		ev.Meta["ci"] = cachedCIVerdict(st.LinkedPR)
		ev.Content = fmt.Sprintf("%s: the CI gate timed out after %s waiting for checks on PR #%d; paused for a human",
			issueRef(st.Repo, st.Number), wait, ev.PR)
		return []channelevents.Event{ev}
	})
}

// emitReviewTimeout reports a review-gate timeout pause, noting reviews
// submitted before the gate started waiting. The wait start is the engine's own
// record-on-write label timestamp, empty after a restart: then the note says
// "unknown" rather than guessing.
func (e *Engine) emitReviewTimeout(item gh.ProjectItem, stage *stages.Stage) {
	e.hookEvent(item, "review-timeout", func(st *itemstate.ItemState) []channelevents.Event {
		ev := e.baseEvent(st, channelevents.ReviewTimeout)
		if stage != nil {
			ev.Stage = stage.Name
		}
		ev.Meta["timeout"] = e.cfg.ReviewWaitTimeout.String()
		var pending []string
		var reviews []gh.PRReview
		if lpr := st.LinkedPR; lpr != nil {
			for _, rr := range lpr.ReviewRequests {
				if rr.Login != "" {
					pending = append(pending, rr.Login)
				}
			}
			reviews = lpr.Reviews
		}
		sort.Strings(pending)
		if len(pending) == 0 {
			ev.Meta["pending_reviewers"] = "none"
		} else {
			ev.Meta["pending_reviewers"] = strings.Join(pending, ",")
		}
		before := "unknown"
		if started := st.LabelAppliedAt["fabrik:awaiting-review"]; !started.IsZero() {
			var who []string
			for _, r := range reviews {
				if r.State == "DISMISSED" || r.State == "PENDING" || r.SubmittedAt.IsZero() {
					continue
				}
				if r.SubmittedAt.Before(started) {
					who = append(who, r.Author)
				}
			}
			sort.Strings(who)
			before = "none"
			if len(who) > 0 {
				before = strings.Join(who, ",")
			}
		}
		ev.Meta["reviews_before_wait"] = before
		ev.Content = fmt.Sprintf("%s: the review gate timed out waiting for reviewers on PR #%d (reviews submitted before the wait began: %s); paused for a human",
			issueRef(st.Repo, st.Number), ev.PR, before)
		return []channelevents.Event{ev}
	})
}

// emitChildrenSpawned reports that the item's scope grew: spawnChildren created
// and wired every child, then marked the parent. spawned lists "owner/repo#N".
func (e *Engine) emitChildrenSpawned(item gh.ProjectItem, spawned []string) {
	e.hookEvent(item, "children-spawned", func(st *itemstate.ItemState) []channelevents.Event {
		ev := e.baseEvent(st, channelevents.ChildrenSpawned)
		ev.Meta["children"] = strings.Join(spawned, ",")
		ev.Meta["count"] = strconv.Itoa(len(spawned))
		ev.Content = fmt.Sprintf("%s spawned %d child issue(s) (%s); it is blocked until they close",
			issueRef(st.Repo, st.Number), len(spawned), strings.Join(spawned, ", "))
		return []channelevents.Event{ev}
	})
}

// labelDerivedEvents are the events whose transition is exactly one label
// change: a landing-verification failure, a cleared blocker and a merge. The
// label delta is the store's own edge, so each fires once per change.
func (e *Engine) labelDerivedEvents(st *itemstate.ItemState, d itemstate.LabelDelta) []channelevents.Event {
	ref := issueRef(st.Repo, st.Number)
	switch {
	case d.Added && d.Label == "fabrik:landing-verification-failed":
		ev := e.baseEvent(st, channelevents.LandingVerificationFailed)
		ev.Content = fmt.Sprintf("%s: landing verification failed — the credited PR did not merge; the issue was reopened and moved back to Validate", ref)
		return []channelevents.Event{ev}
	case !d.Added && d.Label == "fabrik:blocked":
		ev := e.baseEvent(st, channelevents.BlockerCleared)
		ev.Content = fmt.Sprintf("%s: its blockers are resolved; work resumes", ref)
		return []channelevents.Event{ev}
	case d.Added && d.Label == "fabrik:awaiting-landing-verification":
		// Applied immediately after a Done transition attributable to a merge, by
		// every landing path. A merge-train member's own PR is closed, not merged;
		// the credited integration PR is named by fabrik:credited-pr:<N>.
		pr := 0
		for _, l := range st.Labels {
			if strings.HasPrefix(l, "fabrik:credited-pr:") {
				if n, err := strconv.Atoi(strings.TrimPrefix(l, "fabrik:credited-pr:")); err == nil {
					pr = n
				}
			}
		}
		ev := e.baseEvent(st, channelevents.Merged)
		if pr != 0 {
			ev.PR = pr
		}
		ev.DedupKey = fmt.Sprintf("merged:%s:%d", ref, ev.PR)
		ev.Content = fmt.Sprintf("%s landed via PR #%d", ref, ev.PR)
		return []channelevents.Event{ev}
	}
	return nil
}

// trainEvent builds a merge-train event for one member. reason is flattened to
// one short line; failing check names come from the diagnostic the engine
// already holds (never a GitHub read).
func (e *Engine) emitTrainEvent(owner, repo string, issue int, typ channelevents.EventType, cause, reason string, diag *trainCIDiagnostic, extra map[string]string) {
	e.hookEvent(gh.ProjectItem{Repo: owner + "/" + repo, Number: issue}, string(typ), func(st *itemstate.ItemState) []channelevents.Event {
		ev := e.baseEvent(st, typ)
		ev.Meta["cause"] = cause
		line := strings.TrimSpace(strings.SplitN(reason, "\n", 2)[0])
		if len(line) > 240 {
			line = line[:240] + "…"
		}
		if line != "" {
			ev.Meta["reason"] = line
		}
		if names := diagFailingChecks(diag); len(names) > 0 {
			ev.Meta["failing_checks"] = strings.Join(names, ",")
		}
		for k, v := range extra {
			ev.Meta[k] = v
		}
		verb := "was ejected from the merge train"
		if typ == channelevents.MergeTrainFailed {
			verb = "failed in the merge train"
		}
		ev.Content = fmt.Sprintf("%s %s (%s)", issueRef(st.Repo, st.Number), verb, cause)
		if line != "" {
			ev.Content += ": " + line
		}
		if f := ev.Meta["failing_checks"]; f != "" {
			ev.Content += " — failing: " + f
		}
		return []channelevents.Event{ev}
	})
}

// diagFailingChecks lists the failing check names a trial diagnostic carries.
func diagFailingChecks(diag *trainCIDiagnostic) []string {
	if diag == nil {
		return nil
	}
	var names []string
	for _, c := range diag.FailedChecks {
		if c.Name != "" {
			names = append(names, c.Name)
		}
	}
	names = append(names, diag.FailedContexts...)
	sort.Strings(names)
	return names
}
