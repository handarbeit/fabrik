package engine

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// pauseEventForItem classifies the item exactly as the deriver does and builds
// its pause-family event.
func pauseEventForItem(t *testing.T, e *Engine, n int, key string) map[string]string {
	t.Helper()
	snap, ok := e.store.Peek("owner/repo", n)
	if !ok {
		t.Fatalf("item %d not in the store", n)
	}
	st := snap.State()
	now := e.now()
	res := attention.Classify(e.attentionInput(&st, now, time.Hour, time.Time{}, e.peekBlockerLookup()))
	return e.pauseEventFor(&st, key, res, now).Meta
}

// R-cause: a derived push carries the same cause fabrik_status reports for the
// state, and omits it when the engine does not know one (#2121).
func TestPauseEventCarriesStatusCauseOnlyWhenKnown(t *testing.T) {
	e := workerEngine(t)
	e.seedAPIItem(t, 70, "Implement", "fabrik:paused", "stage:Implement:in_progress")
	if m := pauseEventForItem(t, e, 70, "paused"); m["cause"] != "" {
		t.Fatalf("a pause with no evidence carries cause %q, want none", m["cause"])
	}
	e.store.Apply(itemstate.IssueCommentCreated{Repo: "owner/repo", Number: 70, Comment: gh.Comment{
		DatabaseID: 1, Body: "🏭 **Fabrik — paused by a daemon clean stop**\n\n" + pauseCauseDaemonStopMarker,
	}})
	m := pauseEventForItem(t, e, 70, "paused")
	if m["cause"] != attention.CauseDaemonStop {
		t.Errorf("push cause = %q, want %q (same as status %q)", m["cause"], attention.CauseDaemonStop, status(t, e, "70").Attention.Cause)
	}
}

// item-wedged names the hold in the status vocabulary.
func TestItemWedgedEventUsesTheGateHoldCause(t *testing.T) {
	e, sink, clk, ce := wedgeEngine(t)
	evs := holdFor(t, e, clk, ce, sink, wedgedItem(), 14, time.Minute)
	w := ofType(evs, "item-wedged")
	if len(w) != 1 || w[0].Meta["cause"] != "gate-hold:feedback" {
		t.Fatalf("item-wedged = %+v, want cause gate-hold:feedback", w)
	}
}
