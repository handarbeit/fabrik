package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// trainChannelEngine is trainTestEngine with the channel hub running and one
// subscriber to the merge-train events.
func trainChannelEngine(t *testing.T) (*Engine, *chanSink, *mockGitHubClient) {
	t.Helper()
	oldD, oldT := channelDebounce, channelTick
	channelDebounce, channelTick = 20*time.Millisecond, time.Hour
	t.Cleanup(func() { channelDebounce, channelTick = oldD, oldT })

	client := &mockGitHubClient{}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.fabrikDir = t.TempDir()
	eng.startChannelEvents()
	t.Cleanup(eng.closeChannelEvents)
	ce := eng.channelEvents()
	if _, err := ce.hub.Subscribe(channelevents.Subscription{Subscriber: "T", Events: []channelevents.EventType{
		channelevents.MergeTrainEjected, channelevents.MergeTrainFailed,
	}}); err != nil {
		t.Fatal(err)
	}
	sink := newChanSink()
	ce.hub.Attach("T", sink)
	return eng, sink, client
}

func seedTrainMember(eng *Engine, n int) gh.ProjectItem {
	item := makeTrainItem(n, "member")
	eng.store.Apply(itemstate.IssueOpened{Item: item})
	return item
}

func TestChannelMergeTrainEjectedCarriesFailingChecks(t *testing.T) {
	eng, sink, _ := trainChannelEngine(t)
	member := seedTrainMember(eng, 1)
	diag := &trainCIDiagnostic{
		FailedChecks:   []gh.CheckRun{{Name: "unit-tests"}, {Name: "lint"}},
		FailedContexts: []string{"required/build"},
		PRNum:          9,
	}
	eng.ejectMember("owner", "repo", member, "trial CI failed on the combined branch\nsecond line", diag, nil, true)

	ev := sink.next(t)
	if ev.Type != channelevents.MergeTrainEjected || ev.Issue != 1 {
		t.Fatalf("%+v", ev)
	}
	requireMeta(t, ev, "stage", "cause", "reason", "failing_checks", "ejections", "stays_queued")
	if ev.Meta["failing_checks"] != "lint,required/build,unit-tests" {
		t.Errorf("failing_checks = %q", ev.Meta["failing_checks"])
	}
	if strings.Contains(ev.Meta["reason"], "second line") {
		t.Errorf("reason must be one line: %q", ev.Meta["reason"])
	}
	if ev.Meta["cause"] != "trial" || ev.Meta["stays_queued"] != "true" || ev.Meta["ejections"] != "1" {
		t.Errorf("meta: %+v", ev.Meta)
	}
	sink.quiet(t, 150*time.Millisecond)
}

func TestChannelMergeTrainEjectedForReviewFindingsAndCapCount(t *testing.T) {
	eng, sink, _ := trainChannelEngine(t)
	member := seedTrainMember(eng, 2)
	eng.ejectMember("owner", "repo", member, "unresolved review finding", nil, nil, false)
	ev := sink.next(t)
	if ev.Meta["cause"] != "review-findings" || ev.Meta["stays_queued"] != "false" {
		t.Fatalf("%+v", ev.Meta)
	}
	eng.ejectMember("owner", "repo", member, "conflict", nil, nil, true)
	eng.ejectMember("owner", "repo", member, "conflict", nil, nil, true)
	sink.next(t)
	third := sink.next(t)
	if third.Meta["ejections"] != "3" || third.Meta["max_ejections"] != "3" {
		t.Fatalf("the cap-reaching ejection should say so: %+v", third.Meta)
	}
}

func TestChannelMergeTrainFailedRunawayGuard(t *testing.T) {
	eng, sink, _ := trainChannelEngine(t)
	member := seedTrainMember(eng, 3)
	eng.fireRunawayGuard(context.Background(), "owner", "repo", "main", []gh.ProjectItem{member}, 20)
	ev := sink.next(t)
	if ev.Type != channelevents.MergeTrainFailed || ev.Meta["cause"] != "runaway-guard" {
		t.Fatalf("%+v", ev)
	}
	requireMeta(t, ev, "stage", "cause", "reason")
	// The same episode is not announced twice.
	eng.fireRunawayGuard(context.Background(), "owner", "repo", "main", []gh.ProjectItem{member}, 20)
	sink.quiet(t, 150*time.Millisecond)
}
