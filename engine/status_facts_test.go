package engine

import (
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// #2121: the retry and gate-hold facts reach the Store whether or not a channel
// hub runs; a daemon with no hub is the default one, so fabrik_status must be
// able to name them there.
func TestStatusFacts_RetryAndGateHoldRecordedWithoutAHub(t *testing.T) {
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	if e.channelEvents() != nil {
		t.Fatal("test premise: no hub should be running")
	}
	bootstrapItem(t, e, 21, nil)
	item := gh.ProjectItem{Number: 21, Repo: "owner/repo", Status: "Implement"}
	due := e.now().Add(time.Minute)

	e.emitRetryScheduled(item, e.cfg.Stages[2], "wall-time-kill", due)
	e.noteGateHold(item, gateFeedback, "1 unprocessed review body", []string{"review-body:5483772616"})

	snap, _ := e.store.Peek("owner/repo", 21)
	st := snap.State()
	if st.PendingRetry == nil || st.PendingRetry.Stage != "Implement" || st.PendingRetry.Cause != "wall-time-kill" || !st.PendingRetry.Due.Equal(due) {
		t.Errorf("PendingRetry = %+v", st.PendingRetry)
	}
	if st.GateHold == nil || st.GateHold.Gate != gateFeedback || len(st.GateHold.IDs) != 1 || st.GateHold.IDs[0] != "review-body:5483772616" {
		t.Errorf("GateHold = %+v", st.GateHold)
	}

	e.noteGateClear(item, gateFeedback)
	snap, _ = e.store.Peek("owner/repo", 21)
	if got := snap.State().GateHold; got != nil {
		t.Errorf("GateHold = %+v after noteGateClear", got)
	}
}

// A hold the gate stopped confirming ended; the next hold starts a new episode
// with its own Since instead of inheriting the old one.
func TestStatusFacts_LapsedGateHoldRestartsItsClock(t *testing.T) {
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	bootstrapItem(t, e, 22, nil)
	item := gh.ProjectItem{Number: 22, Repo: "owner/repo", Status: "Implement"}
	clock := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)
	setNow := func(t time.Time) { clock = t; e.SetClock(stubClock{t: clock}) }
	setNow(clock)

	e.noteGateHold(item, gateFeedback, "r", nil)
	first := clock
	setNow(clock.Add(e.wedgeLapseFor(gateFeedback) / 2)) // within the lapse: same episode
	e.noteGateHold(item, gateFeedback, "r", nil)
	snap, _ := e.store.Peek("owner/repo", 22)
	if h := snap.State().GateHold; !h.Since.Equal(first) || !h.ConfirmedAt.Equal(clock) {
		t.Fatalf("GateHold = %+v, want Since %v and ConfirmedAt %v", h, first, clock)
	}
	setNow(clock.Add(e.wedgeLapseFor(gateFeedback) + time.Minute)) // lapsed
	e.noteGateHold(item, gateFeedback, "r", nil)
	snap, _ = e.store.Peek("owner/repo", 22)
	if h := snap.State().GateHold; !h.Since.Equal(clock) {
		t.Errorf("Since = %v, want the new episode's %v", h.Since, clock)
	}
}

// The daemon clean stop comment carries the stable marker the `daemon-stop`
// cause is read from.
func TestStatusFacts_CleanStopCommentCarriesTheDaemonStopMarker(t *testing.T) {
	if body := cleanStopPauseComment(t); !strings.Contains(body, pauseCauseDaemonStopMarker) {
		t.Errorf("clean-stop comment lacks %q:\n%s", pauseCauseDaemonStopMarker, body)
	}
}

// Only the post-landing merge-tree path knows why it applied fabrik:rebase-needed.
func TestStatusFacts_PostLandingInvalidationRecordsTheRebaseCause(t *testing.T) {
	w := newInvalidateWorld(t)
	conflicting := w.member(2, map[string]string{"shared.txt": sharedLines(map[int]string{2: "member two"})})
	clean := w.member(4, map[string]string{"other.txt": "x\n"})
	bootstrapItem(t, w.eng, 2, []string{"fabrik:rebase-needed"})
	bootstrapItem(t, w.eng, 4, []string{"fabrik:rebase-needed"})
	w.land(1, map[string]string{"shared.txt": sharedLines(map[int]string{2: "landed"})})

	w.scan(conflicting, clean)

	cause := func(n int) string {
		snap, _ := w.eng.store.Peek("owner/repo", n)
		return snap.State().RebaseCause
	}
	if got := cause(2); got != rebaseCauseAfterLanding {
		t.Errorf("RebaseCause(#2) = %q, want %q", got, rebaseCauseAfterLanding)
	}
	if got := cause(4); got != "" {
		t.Errorf("RebaseCause(#4) = %q, a member that was not invalidated has no known cause", got)
	}
}
