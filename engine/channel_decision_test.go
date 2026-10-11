package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// decisionChannel starts a channel engine with a debounce long enough for a test
// to record a label time right after seeding an item.
func decisionChannel(t *testing.T) (*Engine, *chanSink) {
	t.Helper()
	oldD, oldT := channelDebounce, channelTick
	channelDebounce, channelTick = 120*time.Millisecond, time.Hour
	t.Cleanup(func() { channelDebounce, channelTick = oldD, oldT })
	return channelEngineOn(t, apiEngine(t, 0), 0, nil)
}

func (e *Engine) seedSpecifyComplete(t *testing.T, n int, age time.Duration) {
	t.Helper()
	e.seedAPIItem(t, n, "Specify", "stage:Specify:complete")
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: n, Label: "stage:Specify:complete", At: e.now().Add(-age)})
}

func TestDecisionPending_AnnouncedOncePerEpisodeWithOptions(t *testing.T) {
	e, sink := decisionChannel(t)
	e.seedSpecifyComplete(t, 1, 68*24*time.Hour)

	ev := sink.nextOfType(t, channelevents.DecisionPending)
	if ev.Issue != 1 || ev.Meta["column"] != "Specify" || ev.Meta["next_stage"] != "Implement" ||
		ev.Meta["decision"] != "advance" || ev.Meta["since_basis"] != attention.SinceGitHub {
		t.Fatalf("event = %+v", ev)
	}
	var opts []attention.Option
	if err := json.Unmarshal([]byte(ev.Meta["options"]), &opts); err != nil || len(opts) != 3 || opts[0].Tool != "fabrik_advance" {
		t.Fatalf("options meta = %q (%v)", ev.Meta["options"], err)
	}
	want := e.now().Add(-68 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if ev.Meta["since"] != want {
		t.Errorf("since = %s, want the GitHub time %s", ev.Meta["since"], want)
	}

	// Not re-published on later evaluations: poke the item several times.
	for i := 0; i < 3; i++ {
		e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "model:opus"})
		e.store.Apply(itemstate.LocalLabelRemoved{Repo: "owner/repo", Number: 1, Label: "model:opus"})
		time.Sleep(200 * time.Millisecond)
	}
	for _, g := range sink.snapshotOfType(channelevents.DecisionPending) {
		if g.DedupKey != ev.DedupKey {
			t.Errorf("a second episode was announced: %+v", g)
		}
	}
	if n := len(sink.snapshotOfType(channelevents.DecisionPending)); n != 1 {
		t.Fatalf("decision-pending published %d times, want exactly 1", n)
	}
}

func (s *chanSink) snapshotOfType(typ channelevents.EventType) []channelevents.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []channelevents.Event
	for _, g := range s.got {
		if g.Type == typ {
			out = append(out, g)
		}
	}
	return out
}

func TestDecisionPending_NeutralisedByTheSeam(t *testing.T) {
	restore := SetDecisionPendingDisabledForTest()
	defer restore()
	e, sink := decisionChannel(t)
	e.seedSpecifyComplete(t, 1, time.Hour)
	time.Sleep(400 * time.Millisecond)
	if n := len(sink.snapshotOfType(channelevents.DecisionPending)); n != 0 {
		t.Fatalf("seam on: %d events", n)
	}
}

func TestDecisionPending_AutonomyEndsTheEpisodeSilently(t *testing.T) {
	e, sink := decisionChannel(t)
	e.seedSpecifyComplete(t, 1, time.Hour)
	sink.nextOfType(t, channelevents.DecisionPending)

	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:cruise"})
	time.Sleep(400 * time.Millisecond)
	if n := len(sink.snapshotOfType(channelevents.SettlementWithdrawn)); n != 0 {
		t.Fatalf("a human setting cruise is not a withdrawal (%d events)", n)
	}
	ce := e.channelEvents()
	started, closed := dpKeys("owner/repo#1")
	if ce.hub.Counter(started) != 1 || ce.hub.Counter(closed) != 1 {
		t.Errorf("episode counters = %d/%d, want 1/1 (closed silently)", ce.hub.Counter(started), ce.hub.Counter(closed))
	}
}

func TestDecisionPending_WithdrawnWhenTheEngineTakesItBack(t *testing.T) {
	e, sink := decisionChannel(t)
	e.seedSpecifyComplete(t, 1, time.Hour)
	first := sink.nextOfType(t, channelevents.DecisionPending)

	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:rebase-needed"})
	w := sink.nextOfType(t, channelevents.SettlementWithdrawn)
	if w.Meta["withdrew"] != "decision-pending" || w.Meta["reason"] != "rebase-needed" {
		t.Fatalf("withdrawal = %+v", w)
	}
	// Exactly one, and a fresh decision follows once it resettles (R-unsettle-3).
	e.store.Apply(itemstate.LocalLabelRemoved{Repo: "owner/repo", Number: 1, Label: "fabrik:rebase-needed"})
	second := sink.nextOfType(t, channelevents.DecisionPending)
	if second.DedupKey == first.DedupKey {
		t.Fatalf("the resettled item reused the first episode's key %s", second.DedupKey)
	}
	if n := len(sink.snapshotOfType(channelevents.SettlementWithdrawn)); n != 1 {
		t.Fatalf("%d withdrawals, want exactly 1", n)
	}
}

func TestDecisionPending_WithdrawalNeutralised(t *testing.T) {
	restore := SetSettlementWithdrawalDisabledForTest()
	defer restore()
	e, sink := decisionChannel(t)
	e.seedSpecifyComplete(t, 1, time.Hour)
	sink.nextOfType(t, channelevents.DecisionPending)
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:rebase-needed"})
	time.Sleep(400 * time.Millisecond)
	if n := len(sink.snapshotOfType(channelevents.SettlementWithdrawn)); n != 0 {
		t.Fatalf("seam on: %d withdrawals", n)
	}
}

func TestDecisionPending_UnknownTimeWaitsForTheRecorder(t *testing.T) {
	restore := SetDecisionAnchorGraceForTest(time.Hour)
	defer restore()
	e, sink := decisionChannel(t)
	e.seedAPIItem(t, 1, "Specify", "stage:Specify:complete") // no GitHub time recorded
	time.Sleep(400 * time.Millisecond)
	if n := len(sink.snapshotOfType(channelevents.DecisionPending)); n != 0 {
		t.Fatalf("announced with only a lower-bound age before the grace elapsed")
	}
	// The recorder lands the time: the next evaluation announces it, exactly.
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: 1, Label: "stage:Specify:complete", At: e.now().Add(-48 * time.Hour)})
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "model:opus"}) // wake the deriver
	ev := sink.nextOfType(t, channelevents.DecisionPending)
	if ev.Meta["since_basis"] != attention.SinceGitHub {
		t.Fatalf("basis = %s", ev.Meta["since_basis"])
	}
}

func TestDecisionPending_UnknownTimeIsAnnouncedAsALowerBoundAfterTheGrace(t *testing.T) {
	restore := SetDecisionAnchorGraceForTest(0)
	defer restore()
	e, sink := decisionChannel(t)
	e.seedAPIItem(t, 1, "Specify", "stage:Specify:complete")
	ev := sink.nextOfType(t, channelevents.DecisionPending)
	if ev.Meta["since_basis"] != attention.SinceFirstSeen || !strings.Contains(ev.Content, "at least since") {
		t.Fatalf("event = %+v", ev)
	}
}

func TestDecisionPending_ParkedBlockerEvent(t *testing.T) {
	e, sink := decisionChannel(t)
	e.seedAPIItem(t, 2, "Backlog")
	e.seedAPIItem(t, 1, "Specify", "fabrik:blocked")
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: 1, Label: "fabrik:blocked", At: e.now().Add(-24 * time.Hour)})
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: gh.ProjectItem{
		ID: "I_B", Number: 1, Repo: "owner/repo", Status: "Specify", Labels: []string{"fabrik:blocked"},
		BlockedBy: []gh.Dependency{{Number: 2, State: "OPEN"}},
	}})
	ev := sink.nextOfType(t, channelevents.DecisionPending)
	if ev.Meta["decision"] != "unblock" || ev.Meta["blockers"] != "owner/repo#2" {
		t.Fatalf("event = %+v", ev)
	}
	if !strings.Contains(ev.Meta["options"], "fabrik_promote") {
		t.Errorf("options = %s", ev.Meta["options"])
	}
}

func TestDecisionPending_CatchUpIncludesSuspendedItems(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedSpecifyComplete(t, 1, 24*time.Hour)
	e.seedAPIItem(t, 2, "Specify", "stage:Specify:complete", "fabrik:cruise")
	evs := e.catchUpEvents()
	var got []channelevents.Event
	for _, ev := range evs {
		if ev.Type == channelevents.DecisionPending {
			got = append(got, ev)
		}
	}
	if len(got) != 1 || got[0].Issue != 1 || got[0].Meta["catch_up"] != "true" || got[0].DedupKey != "" {
		t.Fatalf("catch-up = %+v", got)
	}
}

func TestPauseEventCarriesOptionsAndEngineResumeMethod(t *testing.T) {
	e, sink := decisionChannel(t)
	e.seedAPIItem(t, 1, "Specify")
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: gh.ProjectItem{
		ID: "I_B", Number: 1, Repo: "owner/repo", Status: "Specify",
		Comments: []gh.Comment{{DatabaseID: 5, Body: cleanStopPauseComment(t)}},
	}})
	time.Sleep(300 * time.Millisecond) // let the first sight record its baseline
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:paused"})
	ev := sink.nextOfType(t, channelevents.Paused)
	if ev.Meta["resume"] != attention.ResumeViaLabel || !strings.Contains(ev.Meta["options"], "remove the fabrik:paused label") {
		t.Fatalf("clean-stop pause event: %+v", ev.Meta)
	}
	if strings.Contains(ev.Content, "human comment") {
		t.Errorf("content contradicts the engine's comment: %s", ev.Content)
	}
}

func TestValidateSettledUnderCruiseCarriesOptions(t *testing.T) {
	e := apiEngine(t, 0)
	seedValidate(t, e, 1, "stage:Validate:complete", "fabrik:cruise")
	snap, _ := e.store.Peek("owner/repo", 1)
	st := snap.State()
	ev := e.validateSettledEvent(snap, &st, nil)
	var opts []attention.Option
	if err := json.Unmarshal([]byte(ev.Meta["options"]), &opts); err != nil || len(opts) != 2 ||
		opts[0].Action != "merge PR #34" || opts[0].Tool != "" || opts[1].Tool != "fabrik_set_autonomy yolo" {
		t.Fatalf("options = %q (%v)", ev.Meta["options"], err)
	}
	// Control: yolo acts by itself, so there is no human decision to describe.
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:yolo"})
	e.store.Apply(itemstate.LocalLabelRemoved{Repo: "owner/repo", Number: 1, Label: "fabrik:cruise"})
	snap, _ = e.store.Peek("owner/repo", 1)
	st = snap.State()
	if ev := e.validateSettledEvent(snap, &st, nil); ev.Meta["options"] != "" {
		t.Errorf("yolo settle carries options %q", ev.Meta["options"])
	}
}

func settledAndWithdrawn() *channelevents.Subscription {
	return &channelevents.Subscription{Events: []channelevents.EventType{channelevents.ValidateSettled, channelevents.SettlementWithdrawn}}
}

func TestValidateSettledWithdrawnThenSettlesAgain(t *testing.T) {
	e, sink := channelEngine(t, 0, settledAndWithdrawn())
	seedValidate(t, e, 1, "fabrik:cruise", "stage:Validate:complete")
	hook(e, 1)
	first := sink.nextOfType(t, channelevents.ValidateSettled)

	// Main moved: the PR needs a rebase. Nothing announced a withdrawal before.
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:rebase-needed"})
	w := sink.nextOfType(t, channelevents.SettlementWithdrawn)
	if w.Meta["withdrew"] != "validate-settled" || w.Meta["reason"] != "rebase-needed" {
		t.Fatalf("withdrawal = %+v", w.Meta)
	}
	sink.quiet(t, 300*time.Millisecond) // exactly one

	// The rebase lands and the item resettles: a fresh validate-settled.
	e.store.Apply(itemstate.LocalLabelRemoved{Repo: "owner/repo", Number: 1, Label: "fabrik:rebase-needed"})
	hook(e, 1)
	second := sink.nextOfType(t, channelevents.ValidateSettled)
	if second.DedupKey == first.DedupKey {
		t.Fatalf("resettle reused episode key %s", second.DedupKey)
	}
}

func TestValidateSettledWithdrawalReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(e *Engine)
		reason string
	}{
		{"worker dispatched", func(e *Engine) {
			e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 1, StageName: "Validate", StartedAt: time.Now()})
		}, "worker-dispatched"},
		{"head moved", func(e *Engine) {
			e.store.Apply(itemstate.ValidateCompletedAtSHA{Repo: "owner/repo", Number: 1, SHA: "abc123"})
			e.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, LinkedPRNum: 34, SHA: "def456"})
		}, "ci-on-new-head"},
		{"new feedback", func(e *Engine) {
			e.store.Apply(itemstate.IssueCommentCreated{Repo: "owner/repo", Number: 1, Comment: gh.Comment{DatabaseID: 77, Author: "alice", Body: "please change this", CreatedAt: time.Now()}})
		}, "new-feedback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, sink := channelEngine(t, 0, settledAndWithdrawn())
			seedValidate(t, e, 1, "fabrik:cruise", "stage:Validate:complete")
			hook(e, 1)
			sink.nextOfType(t, channelevents.ValidateSettled)
			tc.mutate(e)
			w := sink.nextOfType(t, channelevents.SettlementWithdrawn)
			if w.Meta["reason"] != tc.reason {
				t.Fatalf("reason = %q, want %q", w.Meta["reason"], tc.reason)
			}
		})
	}
}

func TestValidateSettledMovingOnIsNotAWithdrawal(t *testing.T) {
	e, sink := channelEngine(t, 0, settledAndWithdrawn())
	seedValidate(t, e, 1, "fabrik:cruise", "stage:Validate:complete")
	hook(e, 1)
	sink.nextOfType(t, channelevents.ValidateSettled)
	// A human merges: the item moves to Done and the PR is merged.
	e.store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 1, NewStatus: "Done"})
	sink.quiet(t, 400*time.Millisecond)
}

func TestValidateSettledWithdrawalNeutralised(t *testing.T) {
	restore := SetSettlementWithdrawalDisabledForTest()
	defer restore()
	e, sink := channelEngine(t, 0, settledAndWithdrawn())
	seedValidate(t, e, 1, "fabrik:cruise", "stage:Validate:complete")
	hook(e, 1)
	sink.nextOfType(t, channelevents.ValidateSettled)
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:rebase-needed"})
	sink.quiet(t, 400*time.Millisecond)
}

// A column change that the debounce coalesces (advanced, then complete again
// while still suspended) is a new decision: the old episode ends silently and
// the new one is announced with its own column.
func TestDecisionPending_StaleEpisodeIsReplacedWhenTheColumnChanges(t *testing.T) {
	e, sink := decisionChannel(t)
	e.seedSpecifyComplete(t, 1, time.Hour)
	first := sink.nextOfType(t, channelevents.DecisionPending)

	e.store.Apply(itemstate.LocalLabelRemoved{Repo: "owner/repo", Number: 1, Label: "stage:Specify:complete"})
	e.store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 1, NewStatus: "Implement"})
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "stage:Implement:complete"})
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: 1, Label: "stage:Implement:complete", At: e.now().Add(-time.Minute)})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(sink.snapshotOfType(channelevents.DecisionPending)) < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	got := sink.snapshotOfType(channelevents.DecisionPending)
	if len(got) != 2 {
		t.Fatalf("decision-pending published %d times, want 2 (one per decision)", len(got))
	}
	if got[1].Meta["column"] != "Implement" || got[1].DedupKey == first.DedupKey {
		t.Errorf("second event = %+v, want a new episode in Implement", got[1])
	}
	if n := len(sink.snapshotOfType(channelevents.SettlementWithdrawn)); n != 0 {
		t.Errorf("a replaced episode is not a withdrawal (%d events)", n)
	}
}

// After a restart a parked-blocker item is un-suspended until its first deep
// fetch brings BlockedBy. The persisted episode must stay open through that
// gap: not closed, and not re-announced once the fetch lands.
func TestDecisionPending_PersistedEpisodeSurvivesAnUnhydratedItem(t *testing.T) {
	e, sink := decisionChannel(t)
	ce := e.channelEvents()
	started, closed := dpKeys("owner/repo#1")
	ce.hub.SetCounter(started, 1)

	e.seedAPIItem(t, 2, "Backlog")
	e.seedAPIItem(t, 1, "Specify", "fabrik:blocked")
	time.Sleep(400 * time.Millisecond)
	if c := ce.hub.Counter(closed); c != 0 {
		t.Fatalf("the persisted episode was closed on missing data (closed=%d)", c)
	}

	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: 1, Label: "fabrik:blocked", At: e.now().Add(-24 * time.Hour)})
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: gh.ProjectItem{
		ID: "I_B", Number: 1, Repo: "owner/repo", Status: "Specify", Labels: []string{"fabrik:blocked"},
		BlockedBy: []gh.Dependency{{Number: 2, State: "OPEN"}},
	}})
	time.Sleep(400 * time.Millisecond)
	if n := len(sink.snapshotOfType(channelevents.DecisionPending)); n != 0 {
		t.Errorf("an already-announced episode was announced again (%d events)", n)
	}
	if ce.hub.Counter(started) != 1 || ce.hub.Counter(closed) != 0 {
		t.Errorf("counters = %d/%d, want 1/0", ce.hub.Counter(started), ce.hub.Counter(closed))
	}
}
