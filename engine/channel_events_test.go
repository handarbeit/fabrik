package engine

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

type chanSink struct {
	mu  sync.Mutex
	got []channelevents.Event
	ch  chan channelevents.Event
}

func newChanSink() *chanSink { return &chanSink{ch: make(chan channelevents.Event, 256)} }

func (s *chanSink) Deliver(ev channelevents.Event) error {
	s.mu.Lock()
	s.got = append(s.got, ev)
	s.mu.Unlock()
	s.ch <- ev
	return nil
}
func (s *chanSink) Superseded() {}

// next waits for one delivered event.
func (s *chanSink) next(t *testing.T) channelevents.Event {
	t.Helper()
	select {
	case ev := <-s.ch:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a channel event")
		return channelevents.Event{}
	}
}

// nextOfType skips events of other types (state-derived events may interleave).
func (s *chanSink) nextOfType(t *testing.T, typ channelevents.EventType) channelevents.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-s.ch:
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", typ)
			return channelevents.Event{}
		}
	}
}

// quiet asserts no further event arrives for a short window.
func (s *chanSink) quiet(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case ev := <-s.ch:
		t.Fatalf("unexpected event: %+v", ev)
	case <-time.After(window):
	}
}

// channelEngine builds an engine with the channel hub running, short debounce
// and tick, and one subscriber "T" attached with the supplied subscription
// (everything, opted into all labels, when sub is nil).
func channelEngine(t *testing.T, ahead time.Duration, sub *channelevents.Subscription) (*Engine, *chanSink) {
	t.Helper()
	oldD, oldT := channelDebounce, channelTick
	channelDebounce, channelTick = 20*time.Millisecond, time.Hour
	t.Cleanup(func() { channelDebounce, channelTick = oldD, oldT })

	e := apiEngine(t, ahead)
	e.fabrikDir = t.TempDir()
	e.startChannelEvents()
	t.Cleanup(e.closeChannelEvents)
	ce := e.channelEvents()
	if ce == nil {
		t.Fatal("channel events did not start")
	}
	none := []string{}
	s := channelevents.Subscription{Subscriber: "T", ExcludeLabels: &none}
	if sub != nil {
		s = *sub
		s.Subscriber = "T"
	}
	if _, err := ce.hub.Subscribe(s); err != nil {
		t.Fatal(err)
	}
	sink := newChanSink()
	ce.hub.Attach("T", sink)
	return e, sink
}

func TestChannelLabelEngineWriteThenEchoIsOneEventFromFabrik(t *testing.T) {
	e, sink := channelEngine(t, 0, nil)
	e.seedAPIItem(t, 1, "Implement")

	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:paused"})
	// The webhook echo of the engine's own write is a store no-op.
	e.store.Apply(itemstate.IssueLabeled{Repo: "owner/repo", Number: 1, Label: "fabrik:paused", Sender: "fabrik-app[bot]", EchoOfEngine: true})

	ev := sink.nextOfType(t, channelevents.LabelApplied)
	if ev.Label != "fabrik:paused" || ev.Meta["actor"] != "fabrik" || ev.Stage != "Implement" || ev.Issue != 1 {
		t.Fatalf("unexpected event: %+v", ev)
	}
	for _, k := range []string{"repo", "issue", "event", "stage", "label", "actor"} {
		if ev.Meta[k] == "" {
			t.Errorf("meta %q missing: %+v", k, ev.Meta)
		}
	}
	// Exactly one label-applied: drain anything else and make sure it is not another.
	deadline := time.After(300 * time.Millisecond)
loop:
	for {
		select {
		case more := <-sink.ch:
			if more.Type == channelevents.LabelApplied {
				t.Fatalf("second label-applied event: %+v", more)
			}
		case <-deadline:
			break loop
		}
	}
}

func TestChannelLabelActorAttribution(t *testing.T) {
	e, sink := channelEngine(t, 0, nil)
	e.seedAPIItem(t, 1, "Implement")

	e.store.Apply(itemstate.IssueLabeled{Repo: "owner/repo", Number: 1, Label: "human-label", Sender: "alice"})
	if ev := sink.nextOfType(t, channelevents.LabelApplied); ev.Meta["actor"] != "human" {
		t.Fatalf("human label: %+v", ev)
	}
	e.store.Apply(itemstate.IssueLabeled{Repo: "owner/repo", Number: 1, Label: "bot-label", Sender: "dependabot[bot]"})
	if ev := sink.nextOfType(t, channelevents.LabelApplied); ev.Meta["actor"] != "bot" {
		t.Fatalf("bot label: %+v", ev)
	}
	e.store.Apply(itemstate.IssueUnlabeled{Repo: "owner/repo", Number: 1, Label: "human-label", Sender: "alice"})
	if ev := sink.nextOfType(t, channelevents.LabelRemoved); ev.Meta["actor"] != "human" || ev.Label != "human-label" {
		t.Fatalf("removal: %+v", ev)
	}
	// A reconcile that first observes a label has no sender: unknown.
	e.store.Apply(itemstate.ShallowBoardItemUpdated{Repo: "owner/repo", Number: 1, Item: gh.ProjectItem{
		ID: "I_B", Number: 1, Repo: "owner/repo", Status: "Implement", Labels: []string{"bot-label", "seen-by-poll"},
	}})
	if ev := sink.nextOfType(t, channelevents.LabelApplied); ev.Label != "seen-by-poll" || ev.Meta["actor"] != "unknown" {
		t.Fatalf("reconcile label: %+v", ev)
	}
}

func TestChannelDefaultFilterSuppressesLockChurn(t *testing.T) {
	// A default subscription (no exclude list) must not see locked/in_progress.
	sub := channelevents.Subscription{Events: []channelevents.EventType{channelevents.LabelApplied}}
	e, sink := channelEngine(t, 0, &sub)
	e.seedAPIItem(t, 1, "Implement")

	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:locked:bot"})
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "stage:Implement:in_progress"})
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:blocked"})
	ev := sink.next(t)
	if ev.Label != "fabrik:blocked" {
		t.Fatalf("default filter let %q through first", ev.Label)
	}
	sink.quiet(t, 200*time.Millisecond)
}

func TestChannelNoEventsForInitialPopulation(t *testing.T) {
	e, sink := channelEngine(t, 0, nil)
	e.store.Apply(itemstate.BoardReconciled{Items: []gh.ProjectItem{
		{ID: "I_B", Number: 1, Repo: "owner/repo", Status: "Implement", Labels: []string{"a", "fabrik:paused"}},
	}})
	e.store.Reset([]gh.ProjectItem{
		{ID: "I_C", Number: 2, Repo: "owner/repo", Status: "Implement", Labels: []string{"x", "fabrik:paused"}},
	})
	sink.quiet(t, 300*time.Millisecond)
}

func TestChannelEmitEventNilSafeWithoutHub(t *testing.T) {
	e := apiEngine(t, 0)
	e.emitEvent(channelevents.Event{Type: channelevents.Merged}) // must not panic
	e.channelNudge()
}

// R9 (zero GitHub cost) and R10 (observation only): the channel-event sources
// must not reach a GitHub-capable path or a decision-making gate.
func TestChannelEventsSourceDoesNotReachGitHub(t *testing.T) {
	files, err := filepath.Glob("channel_*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no channel source files found: %v", err)
	}
	more, _ := filepath.Glob("../internal/channelevents/*.go")
	files = append(files, more...)
	forbidden := []string{
		"e.client", "e.readClient", "FetchLabelAppliedAt", "labelAppliedAt(", "store.Get(", "e.cache()",
		".FetchItem(", "FetchProjectItem", "feedbackGateBlocks", "unprocessedFeedback(", "net/http",
		"checkReviewGate(", "attemptMergeOnValidate(", "FetchItemDetails", "FetchPRReviews", "FetchCheckRuns",
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, bad := range forbidden {
				if strings.Contains(line, bad) {
					t.Errorf("%s:%d reaches a GitHub-capable or decision path (%q): %s", f, i+1, bad, strings.TrimSpace(line))
				}
			}
		}
	}
}
