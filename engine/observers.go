package engine

import (
	"fmt"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// wakeChFlags is the set of ChangeFlags that imply "this item may need work" and
// should wake the poll loop immediately. Changes that don't include any of these
// flags are suppressed — they represent internal bookkeeping that doesn't affect
// dispatch eligibility (e.g. token usage, invocation outcomes, heartbeats, PID-sets).
//
// WorkerLifecycleChanged (not the broader WorkerChanged) is used here so that
// WorkerHeartbeat and WorkerPIDSet — which fire every 30s for every active worker —
// don't enqueue items into mayNeedWork or trigger wake signals. Only the transitions
// that actually change dispatch eligibility (WorkerEntered, WorkerExited) do so.
const wakeChFlags = itemstate.StatusChanged |
	itemstate.LabelsChanged |
	itemstate.CommentsChanged |
	itemstate.LockChanged |
	itemstate.LinkedPRChanged |
	itemstate.AssigneesChanged |
	itemstate.WorkerLifecycleChanged

// cycleSetFlags is the subset of wakeChFlags used by newMayNeedWorkObserver to
// populate the cycleSet (pre-filter bypass set). WorkerLifecycleChanged is
// intentionally excluded: a WorkerExited from an early-return goroutine (e.g. a
// dep-blocked item) carries no new information and must not bypass the cooldown
// gate for items that did no useful work. The wake channel (newWakeChObserver) still
// uses the full wakeChFlags — non-blocked items are re-evaluated promptly on any
// worker exit. See ADR-039 and §9.9 in docs/state-machine.md.
const cycleSetFlags = wakeChFlags &^ itemstate.WorkerLifecycleChanged

// newWakeChObserver returns an Observer that sends a non-blocking wake signal on
// wakeCh whenever a Change includes any of the wakeChFlags. This replaces the
// unconditional wakeCh send in webhook.go, adding Change-flag-based filtering.
func newWakeChObserver(wakeCh chan struct{}) itemstate.Observer {
	return itemstate.ObserverFunc(func(change itemstate.Change, _ itemstate.Snapshot) {
		if change.Fields&wakeChFlags == 0 {
			return
		}
		select {
		case wakeCh <- struct{}{}:
		default:
		}
	})
}

// newMayNeedWorkObserver returns an Observer that adds the item's issueKey to the
// provided set whenever a Change includes any of the cycleSetFlags. The set is
// protected by mu. This replaces the seenUpdatedAt-based early-exit in
// itemMayNeedWork: items in the set are dispatched in the next poll cycle; items
// absent from the set (and without a bypass label) are skipped.
//
// cycleSetFlags excludes WorkerLifecycleChanged (see its definition) so that
// early-return goroutine exits do not bypass the cooldown gate.
func newMayNeedWorkObserver(mu *sync.Mutex, set *map[string]bool) itemstate.Observer {
	return itemstate.ObserverFunc(func(change itemstate.Change, _ itemstate.Snapshot) {
		if change.Fields&cycleSetFlags == 0 {
			return
		}
		key := fmt.Sprintf("%s#%d", change.Repo, change.Number)
		mu.Lock()
		(*set)[key] = true
		mu.Unlock()
	})
}

// InvocationObserver is registered on engine.store and fires a tui.JobCompletedEvent
// whenever InvocationChanged is observed. It replaces the ad-hoc
// emitStructural(JobCompletedEvent{...}) calls in poll.go, ci.go, merge_gate.go, and
// reviews.go. All three fields (LastInvocationCompleted, LastInvocationBlocked,
// LastTokenUsage) are set atomically by InvocationRecorded, so the observer reads a
// consistent view from the Snapshot.
type InvocationObserver struct {
	Stages []*stages.Stage
	Emit   func(tui.Event)
}

// OnChange implements itemstate.Observer.
func (o *InvocationObserver) OnChange(change itemstate.Change, snap itemstate.Snapshot) {
	if change.Fields&itemstate.InvocationChanged == 0 {
		return
	}
	if o.Emit == nil {
		return
	}
	st := snap.State()
	model := ""
	if s := stages.FindStage(o.Stages, st.Status); s != nil {
		model = s.Model
	}
	o.Emit(tui.JobCompletedEvent{
		IssueNumber:    st.Number,
		Repo:           st.Repo,
		Title:          st.Title,
		StageName:      st.Status,
		StageModel:     model,
		IsComment:      st.LastInvocationIsComment,
		Success:        !st.LastInvocationErrored, // false when the Claude process exited non-zero
		Completed:      st.LastInvocationCompleted,
		BlockedOnInput: st.LastInvocationBlocked,
		Duration:       st.LastInvocationDuration,
		CompletedAt:    time.Now(),
		TurnsUsed:      st.LastTokenUsage.TurnsUsed,
		MaxTurns:       st.LastTokenUsage.MaxTurns,
		CostUSD:        st.LastTokenUsage.CostUSD,
	})
}

// StageChangeObserver is registered on cacheImpl.store and fires a
// tui.StageChangedEvent whenever StatusChanged is observed. This allows the TUI to
// reactively update the displayed stage for an active item without waiting for the
// next poll cycle.
type StageChangeObserver struct {
	Emit func(tui.Event)
}

// OnChange implements itemstate.Observer.
func (o *StageChangeObserver) OnChange(change itemstate.Change, snap itemstate.Snapshot) {
	if change.Fields&itemstate.StatusChanged == 0 {
		return
	}
	if o.Emit == nil {
		return
	}
	st := snap.State()
	o.Emit(tui.StageChangedEvent{
		Repo:     st.Repo,
		Number:   st.Number,
		Title:    st.Title,
		NewStage: st.Status,
	})
}

// PushUnblockObserver fires on two distinct events and removes fabrik:blocked when
// all blockers are resolved:
//
//  1. StateChanged (blocker closes): scans Store.All() for items that carry
//     fabrik:blocked and list the closing issue in their BlockedBy slice, then
//     checks whether every remaining blocker is closed.
//
//  2. BlockedByChanged (dependent's BlockedBy first populated via deep-fetch):
//     inspects only the changed item's own snapshot; if it carries fabrik:blocked
//     and all listed blockers are already closed in the store, removes the label.
//
// This dual-trigger ensures the dependent unblocks within seconds regardless of
// which event arrives first — the blocker's close or the dependent's first
// deep-fetch. Neither StateChanged nor BlockedByChanged is in wakeChFlags or
// cycleSetFlags, so this observer does not trigger a poll wake — label removal
// is a direct side effect only.
type PushUnblockObserver struct {
	Store  *itemstate.Store
	Remove func(owner, repo string, number int)
	// Logf is an optional diagnostic hook. When non-nil, it is called at key
	// branch points so push-unblock decisions are traceable in fabrik.log.
	Logf func(format string, args ...any)
}

func (o *PushUnblockObserver) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// allBlockersClosed checks whether every dep in blockedBy is closed, preferring
// the store's view (fresher than dep.State from the last board fetch).
func (o *PushUnblockObserver) allBlockersClosed(defaultRepo string, blockedBy []gh.Dependency) bool {
	for _, dep := range blockedBy {
		depRepo := dep.Repo
		if depRepo == "" {
			depRepo = defaultRepo
		}
		if depSnap, err := o.Store.Get(depRepo, dep.Number); err == nil {
			if !depSnap.IsClosed() {
				return false
			}
		} else if dep.State != "CLOSED" {
			return false
		}
	}
	return true
}

// OnChange implements itemstate.Observer.
func (o *PushUnblockObserver) OnChange(change itemstate.Change, snap itemstate.Snapshot) {
	if o.Store == nil || o.Remove == nil {
		return
	}

	// Path 1: blocker closes → scan all items for dependents.
	if change.Fields&itemstate.StateChanged != 0 && snap.IsClosed() {
		closedRepo := change.Repo
		closedNum := change.Number

		matched := 0
		for _, x := range o.Store.All() {
			xState := x.State()

			// Only consider items carrying fabrik:blocked.
			hasBlocked := false
			for _, l := range xState.Labels {
				if l == "fabrik:blocked" {
					hasBlocked = true
					break
				}
			}
			if !hasBlocked {
				continue
			}

			// Skip if the closing issue is not in this item's BlockedBy list.
			hasDep := false
			for _, dep := range xState.BlockedBy {
				depRepo := dep.Repo
				if depRepo == "" {
					depRepo = xState.Repo
				}
				if depRepo == closedRepo && dep.Number == closedNum {
					hasDep = true
					break
				}
			}
			if !hasDep {
				continue
			}

			if !o.allBlockersClosed(xState.Repo, xState.BlockedBy) {
				o.logf("dependent %s#%d listed blocker %s#%d but other blockers still open — skip\n",
					xState.Repo, xState.Number, closedRepo, closedNum)
				continue
			}

			xOwner, xRepo := parseOwnerRepo(xState.Repo)
			if xOwner == "" {
				continue
			}
			xNum := xState.Number
			matched++
			o.logf("blocker %s#%d closed → removing fabrik:blocked from dependent %s#%d\n",
				closedRepo, closedNum, xState.Repo, xNum)
			go o.Remove(xOwner, xRepo, xNum)
		}
		// Silent when no dependents match — common case for closes of unrelated issues.
		_ = matched
	}

	// Path 2: dependent's BlockedBy just populated via deep-fetch → check only this item.
	// Uses return (not continue) because there is nothing after this block; the two paths
	// are independent and both idempotent if a single Change somehow carries both flags.
	if change.Fields&itemstate.BlockedByChanged != 0 {
		xState := snap.State()

		// Only act if the item carries fabrik:blocked.
		hasBlocked := false
		for _, l := range xState.Labels {
			if l == "fabrik:blocked" {
				hasBlocked = true
				break
			}
		}
		if !hasBlocked {
			return
		}

		// No-op if BlockedBy is empty after the mutation (e.g., blockers cleared on re-fetch).
		if len(xState.BlockedBy) == 0 {
			return
		}

		if !o.allBlockersClosed(xState.Repo, xState.BlockedBy) {
			return
		}

		xOwner, xRepo := parseOwnerRepo(xState.Repo)
		if xOwner == "" {
			return
		}
		go o.Remove(xOwner, xRepo, xState.Number)
	}
}
