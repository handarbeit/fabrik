package engine

import (
	"fmt"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// wakeChFlags is the set of ChangeFlags that imply "this item may need work" and
// should wake the poll loop immediately. Changes that don't include any of these
// flags are suppressed — they represent internal bookkeeping that doesn't affect
// dispatch eligibility (e.g. token usage, invocation outcomes, stale heartbeats).
const wakeChFlags = itemstate.StatusChanged |
	itemstate.LabelsChanged |
	itemstate.CommentsChanged |
	itemstate.LockChanged |
	itemstate.LinkedPRChanged

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
// provided set whenever a Change includes any of the wakeChFlags. The set is
// protected by mu. This replaces the seenUpdatedAt-based early-exit in
// itemMayNeedWork: items in the set are dispatched in the next poll cycle; items
// absent from the set (and without a bypass label) are skipped.
func newMayNeedWorkObserver(mu *sync.Mutex, set *map[string]bool) itemstate.Observer {
	return itemstate.ObserverFunc(func(change itemstate.Change, _ itemstate.Snapshot) {
		if change.Fields&wakeChFlags == 0 {
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
		Success:        true, // InvocationRecorded is only applied after Claude returns
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
