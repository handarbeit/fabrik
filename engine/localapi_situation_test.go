package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// situationEngine is a channel engine (hub running, subscriber "T" attached with
// the semantic default, so it takes situation) over a store whose GitHub fallback
// fails the test if it is ever called, and a client that panics on any call: the
// builder must answer from memory alone. ahead ages every seeded item.
func situationEngine(t *testing.T, ahead time.Duration) (*Engine, *chanSink, *adjClock, *channelEvents, *countingFetcher) {
	t.Helper()
	oldD, oldT := channelDebounce, channelTick
	channelDebounce, channelTick = 20*time.Millisecond, time.Hour
	t.Cleanup(func() { channelDebounce, channelTick = oldD, oldT })
	e := apiEngine(t, 0)
	fb := &countingFetcher{}
	e.store = itemstate.NewStore(fb)
	e.SetAnomalyThresholdsForTest(5*time.Minute, 10*time.Minute, 5*time.Minute)
	e, sink := channelEngineOn(t, e, ahead, &channelevents.Subscription{EventsDefault: true})
	return e, sink, e.clock.(*adjClock), e.channelEvents(), fb
}

// seedSettled places issue n at Validate, settled under cruise, with an open PR,
// one approving review and one passing check.
func (e *Engine) seedSettled(t *testing.T, n int) {
	t.Helper()
	e.seedAPIItem(t, n, "Validate", "fabrik:cruise", "stage:Validate:complete")
	e.store.Apply(itemstate.PRDetailsUpdated{Repo: "owner/repo", Number: n, PRNumber: 34, Title: "pr", State: "open"})
	e.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: n, LinkedPRNum: 34, SHA: "abc123"})
	e.store.Apply(itemstate.CheckRunCompleted{Repo: "owner/repo", SHA: "abc123", Run: gh.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "success"}})
	e.store.Apply(itemstate.PRReviewSubmitted{Repo: "owner/repo", Number: n, Review: gh.PRReview{Author: "alice", State: "APPROVED", DatabaseID: 7}})
}

func issuesOf(sec localapi.SituationSection[localapi.SituationEntry]) []string {
	out := []string{}
	for _, en := range sec.Entries {
		out = append(out, en.Issue)
	}
	return out
}

func TestSituation_EachCategoryLandsInItsSectionInRankOrder(t *testing.T) {
	// The clock is 2h ahead, so every item's StatusEnteredAt is 2h old unless it
	// shows recent activity.
	e, _, clk, _, fb := situationEngine(t, 2*time.Hour)
	t0 := clk.Now()
	recent := func(n int) {
		e.store.Apply(itemstate.StageAttempted{Repo: "owner/repo", Number: n, StageName: "Implement", At: t0})
	}

	e.seedAPIItem(t, 1, "Implement", "fabrik:paused", "fabrik:awaiting-input") // needs-human
	e.seedAPIItem(t, 2, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	e.store.Apply(itemstate.EnginePaused{Repo: "owner/repo", Number: 2, StageName: "Implement"}) // escalated
	e.seedSettled(t, 3)                                                                          // awaiting a merge decision
	e.seedAPIItem(t, 4, "Implement", "fabrik:awaiting-ci")                                       // CI gate, by label
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: 4, Label: "fabrik:awaiting-ci", At: t0.Add(-5 * time.Minute)})
	e.seedAPIItem(t, 5, "Implement") // review gate, by a recorded hold
	recent(5)
	e.noteGateHold(gh.ProjectItem{Repo: "owner/repo", Number: 5, Status: "Implement"}, gateReview, "waiting for reviewers to respond", nil)
	e.seedSpecifyComplete(t, 6, 3*24*time.Hour) // pending decision
	e.seedAPIItem(t, 7, "Implement")            // no progress for 2h: stalled
	e.seedAPIItem(t, 8, "Implement")            // working: on no list
	recent(8)
	e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 8, StageName: "Implement", StartedAt: t0})
	// Two minutes on: inside the hold lapse, so the review hold is still live.
	clk.set(t0.Add(2 * time.Minute))
	recent(4)
	recent(6)

	v := e.buildSituation(nil)

	if got := issuesOf(v.HumanInput); strings.Join(got, ",") != "owner/repo#1,owner/repo#2" {
		t.Errorf("human_input = %v (needs-human ranks before escalated)", got)
	}
	if got := issuesOf(v.MergeDecisions); strings.Join(got, ",") != "owner/repo#3" {
		t.Errorf("merge_decisions = %v", got)
	}
	if got := issuesOf(v.Gates); strings.Join(got, ",") != "owner/repo#4,owner/repo#5" {
		t.Errorf("gates = %v (longest-held first: #4 for 7m, #5 for 2m)", got)
	}
	if got := issuesOf(v.Decisions); strings.Join(got, ",") != "owner/repo#6" {
		t.Errorf("decisions = %v", got)
	}
	if got := issuesOf(v.Stalled); strings.Join(got, ",") != "owner/repo#7" {
		t.Errorf("stalled = %v", got)
	}
	if v.NothingNeedsYou {
		t.Error("items need attention, but nothing_needs_you is set")
	}

	// Each item appears once across the item sections (FR-012).
	seen := map[string]string{}
	for name, sec := range map[string]localapi.SituationSection[localapi.SituationEntry]{
		"merge_decisions": v.MergeDecisions, "gates": v.Gates, "human_input": v.HumanInput, "decisions": v.Decisions, "stalled": v.Stalled,
	} {
		for _, en := range sec.Entries {
			if prev, dup := seen[en.Issue]; dup {
				t.Errorf("%s is in both %s and %s", en.Issue, prev, name)
			}
			seen[en.Issue] = name
		}
	}
	if len(seen) != 7 {
		t.Errorf("placed %d items, want 7 (the working one is on no list): %v", len(seen), seen)
	}

	// Section-specific facts.
	m := v.MergeDecisions.Entries[0]
	if m.Merge == nil || !m.Merge.PR.Valid || m.Merge.PR.V != 34 || m.Merge.CI.V != "green" || m.Merge.Review.V != "alice: APPROVED" {
		t.Errorf("merge decision facts = %+v", m.Merge)
	}
	if m.Title == "" || !m.URL.Valid || len(m.Options) == 0 || m.Options[0].Action == "" {
		t.Errorf("an entry carries title, link and options: %+v", m)
	}
	g4, g5 := v.Gates.Entries[0].Gate, v.Gates.Entries[1].Gate
	if g4.Gate != gateCI || g4.HeldSeconds.V != 420 || g5.Gate != gateReview || g5.HeldSeconds.V != 120 || g5.Reason == "" {
		t.Errorf("gates = %+v / %+v", g4, g5)
	}
	if d := v.Decisions.Entries[0]; d.Pending == nil || d.Pending.Kind != "decision" || len(d.Options) == 0 {
		t.Errorf("decision = %+v", d)
	} else if tools := fmt.Sprint(d.Options); !strings.Contains(tools, "fabrik_advance") {
		t.Errorf("a decision carries the tool that acts on it: %s", tools)
	}
	if h := v.HumanInput.Entries[0].Human; h == nil || h.ResumeVia == "" {
		t.Errorf("human input names how it is resumed: %+v", h)
	}

	if n := fb.calls.Load(); n != 0 {
		t.Fatalf("the builder reached the Store's GitHub fallback %d times", n)
	}
}

func TestSituation_BoundedWithExactOmittedCounts(t *testing.T) {
	e, _, _, _, _ := situationEngine(t, 0)
	for n := 1; n <= situationSectionMax+3; n++ {
		e.seedAPIItem(t, n, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	}
	v := e.buildSituation(nil)
	if len(v.HumanInput.Entries) != situationSectionMax || v.HumanInput.Omitted != 3 {
		t.Fatalf("human_input = %d entries, %d omitted; want %d and 3", len(v.HumanInput.Entries), v.HumanInput.Omitted, situationSectionMax)
	}
	if first, last := v.HumanInput.Entries[0].Issue, v.HumanInput.Entries[situationSectionMax-1].Issue; first != "owner/repo#1" || last != fmt.Sprintf("owner/repo#%d", situationSectionMax) {
		t.Errorf("the most urgent are kept, in order: %s .. %s", first, last)
	}
	if v.MergeDecisions.Omitted != 0 || v.Gates.Omitted != 0 {
		t.Errorf("an untruncated section reports no omission: %+v %+v", v.MergeDecisions, v.Gates)
	}
	ev := renderSituation(v, e.now(), channelevents.TriggerRequest)
	if !strings.Contains(ev.Content, "and 3 more not shown") || ev.Meta["omitted"] != "3" || ev.Meta["human_input"] != fmt.Sprint(situationSectionMax+3) {
		t.Errorf("the rendering reports the cut: meta=%v\n%s", ev.Meta, ev.Content)
	}
}

// seedEscalated places issue n paused by the engine (escalated, rank 1); a plain
// paused item is needs-human (rank 0).
func (e *Engine) seedEscalated(t *testing.T, n int) {
	t.Helper()
	e.seedAPIItem(t, n, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	e.store.Apply(itemstate.EnginePaused{Repo: "owner/repo", Number: n, StageName: "Implement"})
}

// The two tests below seed rank and issue-number order in disagreement, so they
// fail if the section sort ignores attention rank (the other tests seed items
// whose number order happens to equal their rank order).
func TestSituation_RankBeatsIssueNumber(t *testing.T) {
	e, _, _, _, _ := situationEngine(t, 0)
	e.seedEscalated(t, 1)
	e.seedAPIItem(t, 2, "Implement", "fabrik:paused", "fabrik:awaiting-input") // needs-human
	v := e.buildSituation(nil)
	if got := issuesOf(v.HumanInput); strings.Join(got, ",") != "owner/repo#2,owner/repo#1" {
		t.Fatalf("human_input = %v; needs-human (#2, rank 0) must come before escalated (#1, rank 1)", got)
	}
	if r0, r1 := v.HumanInput.Entries[0].Rank, v.HumanInput.Entries[1].Rank; r0 >= r1 {
		t.Errorf("ranks = %d, %d; the first entry must be the more urgent", r0, r1)
	}
}

func TestSituation_BoundKeepsTheMostUrgentNotTheLowestNumbers(t *testing.T) {
	e, _, _, _, _ := situationEngine(t, 0)
	// Ten escalated items on the low numbers, three more urgent needs-human items
	// on the high ones: by number the cut would drop the urgent ones.
	for n := 1; n <= situationSectionMax; n++ {
		e.seedEscalated(t, n)
	}
	urgent := []int{situationSectionMax + 1, situationSectionMax + 2, situationSectionMax + 3}
	for _, n := range urgent {
		e.seedAPIItem(t, n, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	}
	v := e.buildSituation(nil)
	if len(v.HumanInput.Entries) != situationSectionMax || v.HumanInput.Omitted != 3 {
		t.Fatalf("human_input = %d entries, %d omitted; want %d and 3", len(v.HumanInput.Entries), v.HumanInput.Omitted, situationSectionMax)
	}
	want := []string{}
	for _, n := range urgent {
		want = append(want, fmt.Sprintf("owner/repo#%d", n))
	}
	for n := 1; n <= situationSectionMax-len(urgent); n++ {
		want = append(want, fmt.Sprintf("owner/repo#%d", n))
	}
	if got := issuesOf(v.HumanInput); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("kept = %v\nwant  %v (all needs-human first, then the lowest-numbered escalated)", got, want)
	}
}

func TestSituation_EmptySystemIsAnExplicitNothingNeedsYou(t *testing.T) {
	e, _, _, _, _ := situationEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement") // fine and recently entered
	v := e.buildSituation(nil)
	if !v.NothingNeedsYou || v.UnclassifiedItems != 0 {
		t.Fatalf("view = %+v", v)
	}
	m := toJSON(t, v)
	for _, k := range []string{"merge_decisions", "gates", "human_input", "decisions", "stalled", "merge_train", "anomalies"} {
		sec, ok := m[k].(map[string]any)
		if !ok {
			t.Fatalf("%s missing from the wire view", k)
		}
		if entries, ok := sec["entries"].([]any); !ok || len(entries) != 0 {
			t.Errorf("%s.entries = %v, want an explicit empty list", k, sec["entries"])
		}
	}
	ev := renderSituation(v, e.now(), channelevents.TriggerRequest)
	if !strings.HasPrefix(ev.Content, "Situation: nothing needs you.") || ev.Meta["nothing_needs_you"] != "true" {
		t.Errorf("rendering = %q %v", ev.Content, ev.Meta)
	}
}

// After a restart the daemon cannot vouch for how long a hold has lasted or what
// labels an item carries: those are unknown, never "now" or a healthy default.
func TestSituation_UnknownAfterRestart(t *testing.T) {
	e, _, _, _, _ := situationEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement", "fabrik:awaiting-ci") // no label time on record
	e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{ID: "I_X", Number: 2, Repo: "owner/repo", Status: "Implement", LabelsAbsent: true}})
	v := e.buildSituation(nil)
	if len(v.Gates.Entries) != 1 {
		t.Fatalf("gates = %+v", v.Gates)
	}
	g := v.Gates.Entries[0].Gate
	if g.HeldSince.Valid || g.HeldSeconds.Valid {
		t.Errorf("a hold of unknown start must be unknown: %+v", g)
	}
	if b := toJSON(t, v); b["gates"].(map[string]any)["entries"].([]any)[0].(map[string]any)["gate"].(map[string]any)["held_since"] != localapi.UnknownText {
		t.Errorf("held_since must marshal as %q", localapi.UnknownText)
	}
	if v.UnclassifiedItems != 1 || v.NothingNeedsYou {
		t.Errorf("an item with unread labels is reported, and 'nothing needs you' is not claimed: %+v", v)
	}
}

func TestSituation_AnomaliesPollRetryAndDivergence(t *testing.T) {
	e, sink, clk, ce, _ := situationEngine(t, 0)
	e.cfg.PollSeconds = 30
	e.seedAPIItem(t, 1, "Implement")
	t0 := clk.Now()
	e.health.beginPoll(t0, pollVerdict{}) // a poll that never finishes
	e.emitRetryScheduled(anomalyItem(1), implementStage(), "wall-time-kill", t0.Add(time.Minute))
	release := e.HoldGeneralWorkerSlotsForTest()
	defer release()
	// The engine writes a label, then a reconcile that predates it reverts it, all
	// inside the divergence window (5 min in this fixture).
	clk.set(t0.Add(7 * time.Minute))
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "model:opus"})
	clk.set(t0.Add(8 * time.Minute))
	e.store.Apply(itemstate.ShallowBoardItemUpdated{Repo: "owner/repo", Number: 1, Item: gh.ProjectItem{Number: 1, Repo: "owner/repo", Title: "item 1", URL: "https://github.com/owner/repo/issues/1", Status: "Implement"}})
	checkAndDrain(ce, sink)

	v := e.buildSituation(nil)
	kinds := map[string]localapi.SituationAnomaly{}
	for _, a := range v.Anomalies.Entries {
		kinds[a.Kind+"/"+a.Scope] = a
	}
	if a, ok := kinds["pipeline-stalled/poll"]; !ok || a.Subject != "poll" {
		t.Errorf("an overdue poll is a pipeline-stalled anomaly: %+v", v.Anomalies.Entries)
	}
	if a, ok := kinds["retry-starved/item"]; !ok || a.Subject != "owner/repo#1" || a.Title == "" || !a.URL.Valid || !a.Since.Valid {
		t.Errorf("retry-starved anomaly = %+v (all: %+v)", a, v.Anomalies.Entries)
	}
	if a, ok := kinds["cache-divergence/item"]; !ok || a.Subject != "owner/repo#1" || !strings.Contains(a.Summary, "label:model:opus") {
		t.Errorf("a recent cache divergence is active: %+v", v.Anomalies.Entries)
	}
	if v.NothingNeedsYou {
		t.Error("anomalies are active")
	}
	if !v.Health.Poll.Overdue.Valid || !v.Health.Poll.Overdue.V {
		t.Errorf("health poll = %+v", v.Health.Poll)
	}
	if v.Health.Workers.MaxConcurrent.V != 3 {
		t.Errorf("health workers = %+v", v.Health.Workers)
	}
}

func TestSituation_ItemWedgedIsAnAnomalyNotAGate(t *testing.T) {
	e, sink, clk, ce := wedgeEngine(t)
	holdFor(t, e, clk, ce, sink, wedgedItem(), 14, time.Minute) // announced as item-wedged
	v := e.buildSituation(nil)
	var found bool
	for _, a := range v.Anomalies.Entries {
		found = found || (a.Kind == localapi.AnomalyItemWedged && a.Subject == "owner/repo#1")
	}
	if !found {
		t.Fatalf("an announced item-wedged hold is an anomaly: %+v", v.Anomalies.Entries)
	}
	for _, en := range v.Gates.Entries {
		if en.Issue == "owner/repo#1" {
			t.Errorf("the wedged item is also listed under gates: %+v", en)
		}
	}
}

func TestSituation_ScopeNarrowsItemsNotAccountSections(t *testing.T) {
	e, _, _, _, _ := situationEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{ID: "I_O", Number: 9, Repo: "other/repo", Title: "o", Status: "Implement", Labels: []string{"fabrik:paused", "fabrik:awaiting-input"}}})
	scope := func(ev channelevents.Event) bool { return ev.Repo == "other/repo" }
	v := e.buildSituation(scope)
	if got := issuesOf(v.HumanInput); strings.Join(got, ",") != "other/repo#9" {
		t.Fatalf("scoped human_input = %v", got)
	}
	if v.Health.Workers.MaxConcurrent.V != 3 {
		t.Errorf("account health is never narrowed: %+v", v.Health)
	}
}

func TestSituationTool_ZeroGitHubCallsAndPushNotes(t *testing.T) {
	e, _, _, _, fb := situationEngine(t, 0)
	e.seedSettled(t, 3)
	b := e.LocalAPIBackend().(localAPIBackend)

	res, err := b.Situation(localapi.SituationParams{})
	if err != nil {
		t.Fatal(err)
	}
	if res.AsOf.IsZero() || len(res.View.MergeDecisions.Entries) != 1 || res.Pushed || res.PushNote != "" {
		t.Fatalf("result = %+v", res)
	}
	for _, tc := range []struct {
		name string
		p    localapi.SituationParams
		want string
	}{
		{"no subscriber", localapi.SituationParams{Push: true}, "no subscriber name"},
		{"unknown subscriber", localapi.SituationParams{Push: true, Subscriber: "ghost"}, "has no subscription that takes situation"},
	} {
		r, err := b.Situation(tc.p)
		if err != nil || r.Pushed || !strings.Contains(r.PushNote, tc.want) {
			t.Errorf("%s: pushed=%v note=%q err=%v", tc.name, r.Pushed, r.PushNote, err)
		}
	}
	if n := fb.calls.Load(); n != 0 {
		t.Fatalf("the tool reached the Store's GitHub fallback %d times", n)
	}
}

// The new builder files are read-path source: they must sit inside the globs of
// the static GitHub-free scans, so the scans cannot be escaped by naming (R3).
func TestSituationSourcesAreInsideTheGitHubFreeScans(t *testing.T) {
	for _, tc := range []struct{ file, glob string }{
		{"localapi_situation.go", "localapi*.go"},
		{"channel_situation.go", "channel_*.go"},
	} {
		m, err := filepath.Glob(tc.glob)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, f := range m {
			found = found || f == tc.file
		}
		if !found {
			t.Errorf("%s is not matched by the %s scan glob", tc.file, tc.glob)
		}
		if _, err := os.Stat(tc.file); err != nil {
			t.Error(err)
		}
	}
}
