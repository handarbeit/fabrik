package sim

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/stages"
)

// Lifecycle channel events (#2123, ADR 2123): item-added, column-changed, closed,
// the progress_via_column noise option and restart silence. Like the other
// channel scenarios these are serial (SetChannelTimingForTest) and need
// EnvOptions.BoardCache, without which the engine has no write-through and the
// engine's own board move would only ever be a later, unattributed deep fetch.

// lifecycleStages is the smoke pipeline plus an unmanaged Backlog parking column.
func lifecycleStages() []*stages.Stage {
	return append([]*stages.Stage{{Name: "Backlog", Order: 0, Unmanaged: true}}, smokeStages()...)
}

// parked are the labels that make the engine leave an item alone, so a scenario
// about events is not also a scenario about stage work.
var parked = []string{"fabrik:paused", "fabrik:awaiting-input"}

func fileParked(t *testing.T, env *Env, title, status string, extra ...string) int {
	t.Helper()
	return FileIssue(t, env, title, "body", status, append(append([]string(nil), parked...), extra...)...)
}

func typesOf(sink *simSink, typ channelevents.EventType) []channelevents.Event {
	return sink.ofType(typ, 0)
}

func TestChannelItemAddedForPostStartupArrivalOnly(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: lifecycleStages(), BoardCache: true})
	_, sink := startChannel(t, env)

	// Present at startup: populated by the first polls, never "added".
	fileParked(t, env, "startup managed", "Specify", "fabrik:cruise")
	fileParked(t, env, "startup parked", "Backlog")
	RunPolls(t, env, 3)
	settleWait()
	if got := typesOf(sink, channelevents.ItemAdded); len(got) != 0 {
		t.Fatalf("items present at startup produced item-added: %+v", got)
	}
	if got := typesOf(sink, channelevents.LabelApplied); len(got) != 0 {
		t.Fatalf("startup population produced label events: %+v", got)
	}

	// Arrives after startup already on cruise.
	num := fileParked(t, env, "late arrival", "Specify", "fabrik:cruise")
	RunPolls(t, env, 3)
	settleWait()
	got := sink.ofType(channelevents.ItemAdded, num)
	if len(got) != 1 {
		t.Fatalf("item-added for #%d = %d, want exactly 1 (all events: %+v)", num, len(got), sink.all())
	}
	ev := got[0]
	if !hasLabel(ev.Labels, "fabrik:cruise") || ev.Meta["autonomy"] != "cruise" || ev.Meta["column"] != "Specify" || ev.Meta["title"] != "late arrival" {
		t.Fatalf("arrival event incomplete: labels=%v meta=%v", ev.Labels, ev.Meta)
	}
	if len(typesOf(sink, channelevents.ItemAdded)) != 1 {
		t.Fatalf("an unrelated item-added was published: %+v", typesOf(sink, channelevents.ItemAdded))
	}
	// Later polls never announce it again.
	RunPolls(t, env, 3)
	settleWait()
	if n := len(sink.ofType(channelevents.ItemAdded, num)); n != 1 {
		t.Fatalf("item-added repeated: %d", n)
	}
}

// Non-vacuity: without a board cache the first poll populates the store through
// IssueOpened (Created), so only the population gate keeps the startup items
// silent. Opening the gate first announces them, which is what the first test's
// silence rests on.
func TestChannelItemAddedStartupSilenceIsNotVacuous(t *testing.T) {
	for _, open := range []bool{false, true} {
		env := NewEnv(t, EnvOptions{Stages: lifecycleStages()})
		_, sink := startChannel(t, env)
		if open {
			env.Engine.MarkBoardPopulatedForTest()
		}
		fileParked(t, env, "startup managed", "Specify", "fabrik:cruise")
		RunPolls(t, env, 3)
		settleWait()
		n := len(typesOf(sink, channelevents.ItemAdded))
		if !open && n != 0 {
			t.Fatalf("startup item announced with the gate closed: %+v", sink.all())
		}
		if open && n == 0 {
			t.Fatal("with the gate open the startup item should be announced; the startup-silence rule cannot fail")
		}
	}
}

func TestChannelItemAddedScopes(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: lifecycleStages(), BoardCache: true})
	hub, allSink := startChannel(t, env)
	cruiseSink, yoloSink := &simSink{}, &simSink{}
	if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "cruise", Labels: []string{"fabrik:cruise"}, Events: []channelevents.EventType{channelevents.ItemAdded}}); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "yolo", Labels: []string{"fabrik:yolo"}, Events: []channelevents.EventType{channelevents.ItemAdded}}); err != nil {
		t.Fatal(err)
	}
	hub.Attach("cruise", cruiseSink)
	hub.Attach("yolo", yoloSink)
	fileParked(t, env, "startup", "Specify")
	RunPolls(t, env, 3)
	num := fileParked(t, env, "scoped", "Specify", "fabrik:cruise")
	RunPolls(t, env, 3)
	settleWait()
	if len(cruiseSink.ofType(channelevents.ItemAdded, num)) != 1 {
		t.Fatalf("a subscriber watching fabrik:cruise must learn of the arrival: %+v (S saw %+v)", cruiseSink.all(), allSink.all())
	}
	if len(yoloSink.all()) != 0 {
		t.Fatalf("a yolo watcher received a cruise arrival: %+v", yoloSink.all())
	}
}

func TestChannelColumnChangedHumanMove(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: lifecycleStages(), BoardCache: true})
	_, sink := startChannel(t, env)
	num := fileParked(t, env, "parked then moved", "Backlog")
	RunPolls(t, env, 3)
	settleWait()
	if got := typesOf(sink, channelevents.ColumnChanged); len(got) != 0 {
		t.Fatalf("startup produced column-changed: %+v", got)
	}

	env.Sim.Sim().SeedProjectItem(env.Owner, env.ProjectNum, env.OwnerRepo, num, false, "Specify")
	RunPolls(t, env, 3)
	settleWait()
	got := sink.ofType(channelevents.ColumnChanged, num)
	if len(got) != 1 {
		t.Fatalf("column-changed = %d, want exactly 1 (all: %+v)", len(got), sink.all())
	}
	m := got[0].Meta
	if m["from"] != "Backlog" || m["to"] != "Specify" {
		t.Fatalf("from/to wrong: %v", m)
	}
	if m["actor"] != "human" && m["actor"] != "unknown" {
		t.Fatalf("a hand move's actor = %q, want human or unknown", m["actor"])
	}
	if _, has := m["via"]; has {
		t.Fatalf("hand move carries via: %v", m)
	}
}

func TestChannelColumnChangedEngineAdvanceIsFabrikOnce(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: lifecycleStages(), BoardCache: true})
	t.Cleanup(env.Engine.RegisterObservers()) // the poll admits work reactively (ADR-1592)
	_, sink := startChannel(t, env)
	fileParked(t, env, "startup", "Specify")
	RunPolls(t, env, 3)
	num := FileIssue(t, env, "advances", "body", "Specify", "fabrik:cruise")
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait()
		return len(sink.ofType(channelevents.ColumnChanged, num)) >= 2
	}, 60)

	seen := map[string]int{}
	var first channelevents.Event
	for _, ev := range sink.ofType(channelevents.ColumnChanged, num) {
		seen[ev.Meta["from"]+">"+ev.Meta["to"]]++
		if ev.Meta["from"] == "Specify" && ev.Meta["to"] == "Research" {
			first = ev
		}
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("move %s published %d times, want 1", k, n)
		}
	}
	if first.Type == "" {
		t.Fatalf("no Specify>Research column-changed; got %v", seen)
	}
	if first.Meta["actor"] != "fabrik" {
		t.Fatalf("engine advance actor = %q, want fabrik (%v)", first.Meta["actor"], first.Meta)
	}
}

func TestChannelProgressViaColumnDropsStageLabels(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: lifecycleStages(), BoardCache: true})
	t.Cleanup(env.Engine.RegisterObservers())
	hub, control := startChannel(t, env) // "S": no option, every label
	none := []string{}
	if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "P", ExcludeLabels: &none, ProgressViaColumn: true}); err != nil {
		t.Fatal(err)
	}
	progress := &simSink{}
	hub.Attach("P", progress)
	fileParked(t, env, "startup", "Specify")
	RunPolls(t, env, 3)
	num := FileIssue(t, env, "noisy", "body", "Specify", "fabrik:cruise")
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait()
		return len(progress.ofType(channelevents.ColumnChanged, num)) >= 2
	}, 60)

	isStage := func(l string) bool {
		return channelevents.GlobMatch("stage:*:complete", l) || channelevents.GlobMatch("stage:*:in_progress", l)
	}
	stageOnControl := 0
	for _, typ := range []channelevents.EventType{channelevents.LabelApplied, channelevents.LabelRemoved} {
		for _, ev := range control.ofType(typ, num) {
			if isStage(ev.Label) {
				stageOnControl++
			}
		}
		for _, ev := range progress.ofType(typ, num) {
			if isStage(ev.Label) {
				t.Errorf("progress_via_column delivered a stage label: %s", ev.Label)
			}
		}
	}
	if stageOnControl == 0 {
		t.Fatal("the control subscription saw no stage-label events; the suppression assertion is vacuous")
	}
	if len(progress.ofType(channelevents.LabelApplied, num))+len(progress.ofType(channelevents.LabelRemoved, num)) == 0 {
		t.Fatal("other label events must still be delivered")
	}
	if len(progress.ofType(channelevents.ColumnChanged, num)) < 2 {
		t.Fatal("column-changed must still be delivered")
	}
}

// restartWorld seeds open labelled items plus Done, Backlog and closed ones and
// warms the engine on them, returning the item numbers that are open and parked.
func restartWorld(t *testing.T, env *Env) (open []int, other []int) {
	t.Helper()
	stageLabels := []string{"stage:Specify:complete", "stage:Research:complete", "stage:Plan:complete", "fabrik:cruise", "enhancement"}
	for i := 0; i < 4; i++ {
		open = append(open, fileParked(t, env, "open "+strconv.Itoa(i), "Specify", stageLabels...))
	}
	other = append(other,
		FileIssue(t, env, "done one", "body", "Done", stageLabels...),
		FileIssue(t, env, "backlog one", "body", "Backlog", "bug", "fabrik:yolo"),
	)
	closed := FileIssue(t, env, "closed done", "body", "Done", stageLabels...)
	if err := env.Sim.Sim().CloseIssue(env.Owner, env.Repo, closed); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	other = append(other, closed)
	RunPolls(t, env, 4)
	return open, other
}

func labelEventCount(sink *simSink) int {
	return len(typesOf(sink, channelevents.LabelApplied)) + len(typesOf(sink, channelevents.LabelRemoved))
}

// TestChannelRestartIsSilent is the #2123 restart scenario: a daemon restart (or
// self-upgrade exec) with a subscriber attached across it publishes no label
// events and no re-published state events through the first deep fetch and the
// first reconcile, for open, Done, Backlog and closed items alike — then still
// publishes a real change exactly once. The legacy baseline rule makes the same
// scenario flood, which is what shows the silence is the rule's doing.
func TestChannelRestartIsSilent(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		legacy := legacy
		name := "new-rule"
		if legacy {
			name = "legacy-rule"
		}
		t.Run(name, func(t *testing.T) {
			env := NewEnv(t, EnvOptions{Stages: lifecycleStages(), BoardCache: true})
			open, _ := restartWorld(t, env)

			dir := t.TempDir()
			restore := engine.SetChannelTimingForTest(20*time.Millisecond, time.Hour)
			t.Cleanup(restore)
			r := RestartEnv(t, env)
			r.BoardCache, r.Wake = r.Engine.UseBoardCacheForTest()
			r.Engine.SetLegacyLabelBaselineForTest(legacy)
			hub := r.Engine.StartChannelEventsForTest(dir)
			if hub == nil {
				t.Fatal("hub did not start")
			}
			t.Cleanup(r.Engine.StopChannelEventsForTest)
			none := []string{}
			if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "S", ExcludeLabels: &none}); err != nil {
				t.Fatal(err)
			}
			sink := &simSink{}
			hub.Attach("S", sink)

			// Wave 1: bootstrap from the label-less probe, then the first deep fetches.
			RunPolls(t, r, 4)
			settleWait()
			wave1 := labelEventCount(sink)

			// Wave 2: the first reconcile, which carries every label on every item.
			board, err := r.Sim.FetchProjectBoard(r.Owner, r.Repo, r.ProjectNum, r.cfg.OwnerType)
			if err != nil {
				t.Fatalf("FetchProjectBoard: %v", err)
			}
			r.BoardCache.Reconcile(board)
			RunPolls(t, r, 2)
			settleWait()
			total := labelEventCount(sink)

			if legacy {
				if wave1 == 0 || total <= wave1 {
					t.Fatalf("non-vacuity: the legacy rule must flood in both waves (wave1=%d total=%d)", wave1, total)
				}
				return
			}
			if total != 0 {
				for _, ev := range sink.all() {
					t.Logf("event: %s %s #%d", ev.Type, ev.Label, ev.Issue)
				}
				t.Fatalf("restart published %d label events, want 0", total)
			}
			for _, typ := range []channelevents.EventType{channelevents.Paused, channelevents.AwaitingInput, channelevents.Escalated,
				channelevents.Stalled, channelevents.ItemAdded, channelevents.ColumnChanged, channelevents.Closed, channelevents.Merged} {
				if got := typesOf(sink, typ); len(got) != 0 {
					t.Fatalf("restart re-published %s: %+v", typ, got)
				}
			}

			// A real change after baseline is still exactly one event.
			if err := r.Sim.Sim().AddLabelToIssue(r.Owner, r.Repo, open[0], "operator-label"); err != nil {
				t.Fatal(err)
			}
			RunPolls(t, r, 3)
			settleWait()
			got := sink.ofType(channelevents.LabelApplied, open[0])
			if len(got) != 1 || got[0].Label != "operator-label" {
				t.Fatalf("real label change after baseline: %+v", sink.all())
			}
		})
	}
}

func indexOfEvent(evs []channelevents.Event, pred func(channelevents.Event) bool) int {
	for i, ev := range evs {
		if pred(ev) {
			return i
		}
	}
	return -1
}

func TestChannelManualCloseNamesColumn(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: lifecycleStages(), BoardCache: true})
	_, sink := startChannel(t, env)
	num := fileParked(t, env, "to be closed", "Specify")
	RunPolls(t, env, 3)
	settleWait()
	if got := typesOf(sink, channelevents.Closed); len(got) != 0 {
		t.Fatalf("closed before any close: %+v", got)
	}
	if err := env.Sim.Sim().CloseIssue(env.Owner, env.Repo, num); err != nil {
		t.Fatal(err)
	}
	RunPolls(t, env, 3)
	settleWait()
	got := sink.ofType(channelevents.Closed, num)
	if len(got) != 1 {
		t.Fatalf("closed events = %d, want 1 (all: %+v)", len(got), sink.all())
	}
	m := got[0].Meta
	if m["column"] != "Specify" || m["landing"] != "false" {
		t.Fatalf("manual close meta: %v", m)
	}
	if m["actor"] != "human" && m["actor"] != "unknown" {
		t.Fatalf("manual close actor = %q", m["actor"])
	}
	if got[0].Stage != "Specify" {
		t.Fatalf("stage = %q", got[0].Stage)
	}
}

// TestChannelLandingPublishesMergedBeforeCloseTracking drives one item to Done
// under yolo and checks that `merged` precedes the close and every close-tracking
// event, which say they belong to the landing.
func TestChannelLandingPublishesMergedBeforeCloseTracking(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: lifecycleStages(), BoardCache: true})
	t.Cleanup(env.Engine.RegisterObservers())
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	_, sink := startChannel(t, env)
	fileParked(t, env, "startup", "Specify")
	RunPolls(t, env, 3)
	num := FileIssue(t, env, "lands", "body", "Specify", "fabrik:yolo")
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait()
		return len(sink.ofType(channelevents.Merged, num)) > 0 && len(sink.ofType(channelevents.Closed, num)) > 0
	}, 120)

	all := sink.all()
	merged := indexOfEvent(all, func(ev channelevents.Event) bool { return ev.Type == channelevents.Merged && ev.Issue == num })
	closed := indexOfEvent(all, func(ev channelevents.Event) bool { return ev.Type == channelevents.Closed && ev.Issue == num })
	if merged < 0 || closed < 0 || merged > closed {
		t.Fatalf("merged at %d, closed at %d; want merged first", merged, closed)
	}
	if n := len(sink.ofType(channelevents.Merged, num)); n != 1 {
		t.Fatalf("merged published %d times, want 1", n)
	}
	for i, ev := range all {
		if ev.Issue != num || (ev.Type != channelevents.LabelApplied && ev.Type != channelevents.LabelRemoved) {
			continue
		}
		if closeTrackingLabelForTest(ev.Label) && i < merged {
			t.Errorf("close-tracking label event %s %s precedes merged", ev.Type, ev.Label)
		}
	}
	if c := all[closed]; c.Meta["landing"] != "true" || c.Meta["actor"] != "fabrik" {
		t.Fatalf("a landing close must say so and be attributed to fabrik: %v", c.Meta)
	}
}

func closeTrackingLabelForTest(l string) bool {
	return l == "fabrik:awaiting-close" || l == "fabrik:awaiting-member-close" || strings.HasPrefix(l, "fabrik:credited-pr:")
}
