package engine

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/tui"
)

var waitingNow = time.Date(2026, 10, 13, 14, 0, 0, 0, time.UTC)

// classifyWaiting runs the real classifier over a minimal input.
func classifyWaiting(status string, kind attention.StageKind, mod func(*attention.Input), labels ...string) attention.Result {
	in := attention.Input{
		Now:       waitingNow,
		Cfg:       attention.Config{StallThreshold: 30 * time.Minute, CIWaitTimeout: 30 * time.Minute, CIBackstopTimeout: 4 * time.Hour, ReviewWaitTimeout: time.Hour},
		Status:    status,
		StageKind: kind,
		Labels:    labels,
	}
	if mod != nil {
		mod(&in)
	}
	return attention.Classify(in)
}

// TestWaitingText_R1Examples builds each R1 example from the classifier's own output.
func TestWaitingText_R1Examples(t *testing.T) {
	since := time.Date(2026, 10, 13, 13, 52, 0, 0, time.UTC)
	due := time.Date(2026, 10, 13, 14, 3, 0, 0, time.UTC)
	clock := waitingClock

	cases := []struct {
		name string
		res  attention.Result
		f    waitingFacts
		kind tui.WaitingKind
		want string
	}{
		{
			name: "CI",
			res:  classifyWaiting("Validate", attention.KindManaged, nil, "fabrik:awaiting-ci"),
			f:    waitingFacts{prNumber: 2236, checksDone: 5, checksTotal: 7, since: since},
			kind: tui.WaitingExternal,
			want: "CI on PR #2236 (5/7 checks, since " + clock(since) + ")",
		},
		{
			name: "CI without facts omits them",
			res:  classifyWaiting("Validate", attention.KindManaged, nil, "fabrik:awaiting-ci"),
			kind: tui.WaitingExternal,
			want: "CI",
		},
		{
			name: "review with a requested reviewer",
			res:  classifyWaiting("Implement", attention.KindManaged, nil, "fabrik:awaiting-review"),
			f:    waitingFacts{reviewers: []string{"handarbeit-pruefer"}, since: since},
			kind: tui.WaitingExternal,
			want: "review from handarbeit-pruefer (since " + clock(since) + ")",
		},
		{
			name: "review nobody requested",
			res:  classifyWaiting("Implement", attention.KindManaged, nil, "fabrik:awaiting-review"),
			kind: tui.WaitingExternal,
			want: "review",
		},
		{
			name: "retry time slice",
			res: classifyWaiting("Implement", attention.KindManaged, func(in *attention.Input) {
				in.Retry = &attention.RetryFact{Stage: "Implement", Cause: attention.CauseMaxTurns, Due: due}
			}),
			f:    waitingFacts{retryDue: due},
			kind: tui.WaitingExternal,
			want: "retry at " + clock(due) + " (time slice)",
		},
		{
			name: "rebase",
			res:  classifyWaiting("Validate", attention.KindManaged, nil, "fabrik:rebase-needed"),
			kind: tui.WaitingExternal,
			want: "rebase (fabrik:rebase-needed)",
		},
		{
			name: "merge train",
			res:  classifyWaiting("Queued", attention.KindHolding, nil),
			f:    waitingFacts{trainStage: "Queued", trainPos: 2, trainOf: 3},
			kind: tui.WaitingExternal,
			want: "landing in merge train (Queued, position 2 of 3)",
		},
		{
			name: "merge train without a position",
			res:  classifyWaiting("Queued", attention.KindHolding, nil),
			f:    waitingFacts{trainStage: "Queued"},
			kind: tui.WaitingExternal,
			want: "landing in merge train (Queued)",
		},
		{
			name: "human reply",
			res:  classifyWaiting("Implement", attention.KindManaged, nil, "fabrik:paused", "fabrik:awaiting-input"),
			kind: tui.WaitingHuman,
			want: "human reply (paused)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, ok := waitingKept(c.res)
			if !ok {
				t.Fatalf("not kept: state=%s code=%s", c.res.State, c.res.Code)
			}
			if kind != c.kind {
				t.Errorf("kind = %v, want %v (state=%s code=%s)", kind, c.kind, c.res.State, c.res.Code)
			}
			if got := waitingText(c.res, c.f); got != c.want {
				t.Errorf("text = %q, want %q (code=%s)", got, c.want, c.res.Code)
			}
		})
	}
}

func TestWaitingKept_ExcludesNonWaits(t *testing.T) {
	for _, res := range []attention.Result{
		{State: attention.Working, Code: "working"},
		{State: attention.Stalled, Code: attention.CodeNoProgress},
		{State: attention.Idle, Code: "idle"},
		{State: attention.Waiting, Code: attention.CodeWaitingForDispatch},
		{State: attention.Waiting, Code: attention.CodeQueuedForSlot},
		{State: attention.Waiting, Code: attention.CodeCIRed},
	} {
		if _, ok := waitingKept(res); ok {
			t.Errorf("%s/%s kept, want dropped", res.State, res.Code)
		}
	}
}

func TestWaitingText_FallbackTrimsSummary(t *testing.T) {
	long := "gate holding; " + strings.Repeat("x", 200)
	got := waitingText(attention.Result{State: attention.Waiting, Code: "gate-hold", Summary: long}, waitingFacts{})
	if got != "gate holding" {
		t.Errorf("got %q", got)
	}
	got = waitingText(attention.Result{State: attention.Waiting, Code: "other", Summary: strings.Repeat("y", 200)}, waitingFacts{})
	if r := []rune(got); len(r) != waitingTextMax || r[len(r)-1] != '…' {
		t.Errorf("got %d runes %q", len(r), got)
	}
}

func TestSortWaitingRows(t *testing.T) {
	t0 := waitingNow
	mk := func(n int, kind tui.WaitingKind, rank int, deadline, since time.Time) waitingRow {
		return waitingRow{item: tui.WaitingItem{Number: n, Kind: kind}, rank: rank, deadline: deadline, since: since, ref: issueRef("o/r", n)}
	}
	rows := []waitingRow{
		mk(1, tui.WaitingExternal, -1, time.Time{}, t0.Add(-time.Hour)),
		mk(2, tui.WaitingExternal, -1, t0.Add(time.Hour), time.Time{}),
		mk(3, tui.WaitingHuman, 1, time.Time{}, time.Time{}),
		mk(4, tui.WaitingExternal, -1, t0.Add(time.Minute), time.Time{}),
		mk(5, tui.WaitingHuman, 0, time.Time{}, time.Time{}),
		mk(6, tui.WaitingExternal, -1, time.Time{}, t0.Add(-2*time.Hour)),
		mk(7, tui.WaitingExternal, -1, time.Time{}, time.Time{}),
	}
	sortWaitingRows(rows)
	var got []int
	for _, r := range rows {
		got = append(got, r.item.Number)
	}
	want := []int{5, 3, 4, 2, 6, 1, 7}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// waitingEngine is an engine over a client that panics on any call: sampling must
// answer from the Store alone.
func waitingEngine(t *testing.T) *Engine {
	t.Helper()
	e := apiEngine(t, 0)
	e.store = itemstate.NewStore(&countingFetcher{})
	return e
}

func TestSampleWaiting_AwaitingCIRowNamesThePR(t *testing.T) {
	e := waitingEngine(t)
	e.seedAPIItem(t, 1, "Validate", "fabrik:awaiting-ci", "stage:Validate:complete")
	e.store.Apply(itemstate.PRDetailsUpdated{Repo: "owner/repo", Number: 1, PRNumber: 2236, Title: "pr", State: "open"})
	e.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, LinkedPRNum: 2236, SHA: "abc"})
	e.store.Apply(itemstate.CheckRunCompleted{Repo: "owner/repo", SHA: "abc", Run: gh.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "success"}})
	at := e.now().Add(-5 * time.Minute)
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: 1, Label: "fabrik:awaiting-ci", At: at})

	ev := e.sampleWaiting(e.now())
	if len(ev.Items) != 1 || ev.WaitCount != 1 || ev.HumanCount != 0 {
		t.Fatalf("event = %+v", ev)
	}
	it := ev.Items[0]
	want := "CI on PR #2236 (1/1 checks, since " + waitingClock(at) + ")"
	if it.Kind != tui.WaitingExternal || it.Number != 1 || it.Stage != "Validate" || it.Text != want {
		t.Errorf("item = %+v, want text %q", it, want)
	}
}

func TestSampleWaiting_RestartShapedOmitsSince(t *testing.T) {
	e := waitingEngine(t)
	e.seedAPIItem(t, 1, "Validate", "fabrik:awaiting-ci", "stage:Validate:complete")
	ev := e.sampleWaiting(e.now())
	if len(ev.Items) != 1 {
		t.Fatalf("event = %+v", ev)
	}
	if got := ev.Items[0].Text; got != "CI" {
		t.Errorf("text = %q, want bare CI with no fabricated facts", got)
	}
}

func TestSampleWaiting_HumanRowAndExclusions(t *testing.T) {
	e := waitingEngine(t)
	e.seedAPIItem(t, 1, "Implement", "fabrik:paused", "fabrik:awaiting-input") // human
	e.seedAPIItem(t, 2, "Implement", "fabrik:blocked")                         // own event path
	e.seedAPIItem(t, 3, "Backlog", "fabrik:awaiting-ci")                       // unmanaged column
	e.seedAPIItem(t, 4, "Implement")                                           // idle / nothing
	e.seedAPIItem(t, 5, "Implement")                                           // running worker
	e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 5, StageName: "Implement", StartedAt: e.now()})

	ev := e.sampleWaiting(e.now())
	if len(ev.Items) != 1 || ev.Items[0].Number != 1 || ev.Items[0].Kind != tui.WaitingHuman || ev.HumanCount != 1 || ev.WaitCount != 0 {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Items[0].Text != "human reply (paused)" {
		t.Errorf("text = %q", ev.Items[0].Text)
	}
}

func TestSampleWaiting_CapAndTotals(t *testing.T) {
	e := waitingEngine(t)
	for n := 1; n <= waitingRowsMax+3; n++ {
		e.seedAPIItem(t, n, "Validate", "fabrik:awaiting-ci", "stage:Validate:complete")
	}
	e.seedAPIItem(t, 9, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	ev := e.sampleWaiting(e.now())
	if len(ev.Items) != waitingRowsMax || ev.Omitted != 4 || ev.WaitCount != waitingRowsMax+3 || ev.HumanCount != 1 {
		t.Fatalf("items=%d omitted=%d wait=%d human=%d", len(ev.Items), ev.Omitted, ev.WaitCount, ev.HumanCount)
	}
	if ev.Items[0].Kind != tui.WaitingHuman {
		t.Errorf("human row must sort first, got %+v", ev.Items[0])
	}
}

func TestSampleWaiting_MergeTrainPosition(t *testing.T) {
	e := waitingEngine(t)
	for n := 1; n <= 3; n++ {
		e.seedAPIItem(t, n, "Queued")
		e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: n, FreshState: gh.ProjectItem{
			ID: "I_" + string(rune('A'+n)), Number: n, Repo: "owner/repo", Status: "Queued", Labels: []string{"x"}, LabelsAbsent: false,
		}})
	}
	ev := e.sampleWaiting(e.now())
	if len(ev.Items) != 3 {
		t.Fatalf("event = %+v", ev)
	}
	found := false
	for _, it := range ev.Items {
		if it.Number == 2 {
			found = true
			if !strings.Contains(it.Text, "position 2 of 3") {
				t.Errorf("text = %q", it.Text)
			}
		}
	}
	if !found {
		t.Fatalf("no row for #2: %+v", ev)
	}
}

func TestSampleWaiting_RetryRow(t *testing.T) {
	e := waitingEngine(t)
	e.seedAPIItem(t, 1, "Implement")
	due := e.now().Add(10 * time.Minute)
	e.store.Apply(itemstate.RetryScheduled{Repo: "owner/repo", Number: 1, Retry: itemstate.PendingRetry{Stage: "Implement", Cause: "max-turns", Due: due, At: e.now()}})
	ev := e.sampleWaiting(e.now())
	if len(ev.Items) != 1 || ev.Items[0].Text != "retry at "+waitingClock(due)+" (time slice)" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestWaitingPublisher_EmitsOnChangeOnly(t *testing.T) {
	e := waitingEngine(t)
	ch := make(chan tui.Event, 16)
	e.events = ch
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.runWaitingPublisher(ctx, 5*time.Millisecond)

	next := func() tui.WaitingItemsEvent {
		t.Helper()
		select {
		case raw := <-ch:
			ev, ok := raw.(tui.WaitingItemsEvent)
			if !ok {
				t.Fatalf("unexpected event %T", raw)
			}
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("no WaitingItemsEvent")
		}
		return tui.WaitingItemsEvent{}
	}
	if ev := next(); len(ev.Items) != 0 {
		t.Fatalf("first = %+v, want empty snapshot", ev)
	}
	select {
	case raw := <-ch:
		t.Fatalf("unchanged snapshot emitted %+v", raw)
	case <-time.After(60 * time.Millisecond):
	}
	e.seedAPIItem(t, 1, "Validate", "fabrik:awaiting-ci", "stage:Validate:complete")
	if ev := next(); len(ev.Items) != 1 || ev.Items[0].Number != 1 {
		t.Fatalf("after seed = %+v", ev)
	}
}

// TestWaitingPublisherSourceDoesNotReachGitHub pins #2238's "no new GitHub calls
// from the TUI path": the publisher reads the Store only.
func TestWaitingPublisherSourceDoesNotReachGitHub(t *testing.T) {
	b, err := os.ReadFile("waiting_publish.go")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"e.client", "e.readClient", "FetchLabelAppliedAt", "labelAppliedAt(", "store.Get(", "e.cache()", ".FetchItem(", "FetchProjectItem", "settleDecisionAnchors"}
	for i, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		for _, bad := range forbidden {
			if strings.Contains(line, bad) {
				t.Errorf("waiting_publish.go:%d reaches a GitHub-capable path (%q): %s", i+1, bad, strings.TrimSpace(line))
			}
		}
	}
}

// TestPollNothingToDoWordingMatchesTheHeader pins the log line the header's wait
// count replaces (#2238 R4): the header matches the displayed string, so the
// engine's wording and tui.HeaderNothingToDo must not drift apart.
func TestPollNothingToDoWordingMatchesTheHeader(t *testing.T) {
	b, err := os.ReadFile("poll.go")
	if err != nil {
		t.Fatal(err)
	}
	if tui.HeaderNothingToDo != "[poll] nothing to do" {
		t.Fatalf("HeaderNothingToDo = %q", tui.HeaderNothingToDo)
	}
	if !strings.Contains(string(b), `e.logf(0, "poll", "nothing to do\n")`) {
		t.Error(`poll.go no longer logs [poll] nothing to do; update tui.HeaderNothingToDo and its header test`)
	}
}
