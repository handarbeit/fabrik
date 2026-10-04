package engine

import (
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
)

const apiRepo = "owner/repo"

// seedBaseline adds an item and lets the deriver record it as its baseline: the
// first evaluation of an item never emits, so a test of a *transition* must let
// the item settle first.
func seedBaseline(t *testing.T, e *Engine, n int, status string, labels ...string) {
	t.Helper()
	e.seedAPIItem(t, n, status, labels...)
	time.Sleep(120 * time.Millisecond)
}

func lbl(e *Engine, n int, labels ...string) {
	for _, l := range labels {
		e.store.Apply(itemstate.LocalLabelAdded{Repo: apiRepo, Number: n, Label: l})
	}
}

// onlyTypes subscribes to just the named event types (label events excluded
// unless named) so assertions are not drowned by label-applied noise.
func onlyTypes(ts ...channelevents.EventType) *channelevents.Subscription {
	return &channelevents.Subscription{Events: ts}
}

func requireMeta(t *testing.T, ev channelevents.Event, keys ...string) {
	t.Helper()
	for _, k := range append([]string{"repo", "issue", "event"}, keys...) {
		if ev.Meta[k] == "" {
			t.Errorf("%s: meta %q missing: %+v", ev.Type, k, ev.Meta)
		}
	}
	if ev.Meta["event"] != string(ev.Type) {
		t.Errorf("meta.event = %q, want %q", ev.Meta["event"], ev.Type)
	}
	if ev.Content == "" {
		t.Errorf("%s: empty content", ev.Type)
	}
}

// ---- pause family (mirrors the attention classifier) ----

func TestChannelPausedEventOnce(t *testing.T) {
	e, sink := channelEngine(t, 0, onlyTypes(channelevents.Paused, channelevents.AwaitingInput, channelevents.Escalated))
	seedBaseline(t, e, 1, "Implement")
	lbl(e, 1, "fabrik:paused")
	ev := sink.next(t)
	if ev.Type != channelevents.Paused || ev.Issue != 1 {
		t.Fatalf("unexpected: %+v", ev)
	}
	requireMeta(t, ev, "stage", "code", "reason")
	// An unrelated change while still paused is not a new transition.
	lbl(e, 1, "model:opus")
	sink.quiet(t, 250*time.Millisecond)
}

func TestChannelAwaitingInputEvent(t *testing.T) {
	e, sink := channelEngine(t, 0, onlyTypes(channelevents.Paused, channelevents.AwaitingInput, channelevents.Escalated))
	seedBaseline(t, e, 1, "Implement")
	lbl(e, 1, "fabrik:paused", "fabrik:awaiting-input")
	ev := sink.next(t)
	if ev.Type != channelevents.AwaitingInput {
		t.Fatalf("want awaiting-input, got %+v", ev)
	}
	requireMeta(t, ev, "code")
	sink.quiet(t, 250*time.Millisecond)
}

func TestChannelEscalatedEvent(t *testing.T) {
	e, sink := channelEngine(t, 0, onlyTypes(channelevents.Paused, channelevents.AwaitingInput, channelevents.Escalated))
	seedBaseline(t, e, 1, "Implement")
	// A settle scan exhausting its retries: fabrik:paused plus PausedByEngine.
	e.store.Apply(itemstate.EnginePaused{Repo: apiRepo, Number: 1, StageName: "Implement"})
	lbl(e, 1, "fabrik:paused")
	ev := sink.next(t)
	if ev.Type != channelevents.Escalated {
		t.Fatalf("want escalated, got %+v", ev)
	}
	requireMeta(t, ev, "code", "reason")
	sink.quiet(t, 250*time.Millisecond)
}

func TestChannelAwaitingInputStale(t *testing.T) {
	e, sink := channelEngine(t, 0, onlyTypes(channelevents.AwaitingInputStale))
	for n := 1; n <= 3; n++ {
		seedBaseline(t, e, n, "Implement")
		lbl(e, n, "fabrik:paused", "fabrik:awaiting-input")
	}
	time.Sleep(120 * time.Millisecond)

	// #1: a later run finished without asking for input -> stale.
	e.store.Apply(itemstate.InvocationRecorded{Repo: apiRepo, Number: 1, Completed: true, Duration: time.Second})
	ev := sink.next(t)
	if ev.Type != channelevents.AwaitingInputStale || ev.Issue != 1 {
		t.Fatalf("unexpected: %+v", ev)
	}
	requireMeta(t, ev, "stage")
	// A second run in the same episode does not repeat it.
	e.store.Apply(itemstate.InvocationRecorded{Repo: apiRepo, Number: 1, Completed: true, Duration: 2 * time.Second})
	// #2: the run asked for input -> not stale.
	e.store.Apply(itemstate.InvocationRecorded{Repo: apiRepo, Number: 2, Blocked: true})
	// #3: the engine paused it itself -> not stale.
	e.store.Apply(itemstate.EnginePaused{Repo: apiRepo, Number: 3, StageName: "Implement"})
	e.store.Apply(itemstate.InvocationRecorded{Repo: apiRepo, Number: 3, Completed: true})
	sink.quiet(t, 300*time.Millisecond)
}

// ---- cycle-limit-near ----

func TestChannelCycleLimitNearOncePerApproachAndResetsOnDecrement(t *testing.T) {
	e, sink := channelEngine(t, 0, onlyTypes(channelevents.CycleLimitNear))
	seedBaseline(t, e, 1, "Implement")
	inc := func() {
		e.store.Apply(itemstate.ReviewCycleIncremented{Repo: apiRepo, Number: 1, StageName: "Implement"})
	}
	dec := func() {
		e.store.Apply(itemstate.ReviewCycleDecremented{Repo: apiRepo, Number: 1, StageName: "Implement"})
	}
	step := func(f func()) { f(); time.Sleep(80 * time.Millisecond) }

	for i := 0; i < 3; i++ { // 3 of 5: not near yet
		step(inc)
	}
	sink.quiet(t, 100*time.Millisecond)
	step(inc) // 4 of 5: one short of the limit
	ev := sink.next(t)
	if ev.Type != channelevents.CycleLimitNear || ev.Meta["counter"] != "review-cycles" || ev.Meta["count"] != "4" || ev.Meta["limit"] != "5" {
		t.Fatalf("unexpected: %+v", ev)
	}
	requireMeta(t, ev, "stage")
	step(inc) // 5 of 5: at the limit, no further "near"
	sink.quiet(t, 150*time.Millisecond)
	step(dec) // no-op refund: back to 4 -> still not below limit-1
	step(dec) // 3
	sink.quiet(t, 100*time.Millisecond)
	step(inc) // 4 again: a fresh approach fires again
	if again := sink.next(t); again.Type != channelevents.CycleLimitNear {
		t.Fatalf("expected a second approach to fire, got %+v", again)
	}
}

func TestChannelCycleLimitNearUnknownLimitNeverFires(t *testing.T) {
	e, sink := channelEngine(t, 0, onlyTypes(channelevents.CycleLimitNear))
	e.cfg.MaxReviewCycles = 0 // zero config is "unknown", never "limit 0"
	seedBaseline(t, e, 1, "Implement")
	for i := 0; i < 3; i++ {
		e.store.Apply(itemstate.ReviewCycleIncremented{Repo: apiRepo, Number: 1, StageName: "Implement"})
	}
	sink.quiet(t, 250*time.Millisecond)
}

// ---- label-derived: landing-verification-failed, blocker-cleared, merged ----

func TestChannelLandingVerificationFailedBlockerClearedAndMerged(t *testing.T) {
	e, sink := channelEngine(t, 0, onlyTypes(channelevents.LandingVerificationFailed, channelevents.BlockerCleared, channelevents.Merged))
	seedBaseline(t, e, 1, "Validate")

	lbl(e, 1, "fabrik:landing-verification-failed")
	ev := sink.next(t)
	if ev.Type != channelevents.LandingVerificationFailed {
		t.Fatalf("%+v", ev)
	}
	requireMeta(t, ev, "stage")

	lbl(e, 1, "fabrik:blocked")
	sink.quiet(t, 100*time.Millisecond)
	e.store.Apply(itemstate.LocalLabelRemoved{Repo: apiRepo, Number: 1, Label: "fabrik:blocked"})
	if ev := sink.next(t); ev.Type != channelevents.BlockerCleared {
		t.Fatalf("%+v", ev)
	}

	// Merge-train landing: the credited PR label comes first, then the
	// verification marker; pr names the credited integration PR.
	lbl(e, 1, "fabrik:credited-pr:77", "fabrik:awaiting-landing-verification")
	m := sink.next(t)
	if m.Type != channelevents.Merged || m.PR != 77 || m.Meta["pr"] != "77" {
		t.Fatalf("merged: %+v", m)
	}
	sink.quiet(t, 150*time.Millisecond)
}

// ---- hooks ----

func hookItem(e *Engine, n int) gh.ProjectItem {
	return gh.ProjectItem{Repo: apiRepo, Number: n}
}

func TestChannelCITimeoutHook(t *testing.T) {
	e, sink := channelEngine(t, 0, onlyTypes(channelevents.CITimeout))
	seedBaseline(t, e, 1, "Validate")
	e.store.Apply(itemstate.PRDetailsUpdated{Repo: apiRepo, Number: 1, PRNumber: 34, State: "open"})
	e.emitCITimeout(hookItem(e, 1), &stages.Stage{Name: "Validate"})
	ev := sink.next(t)
	if ev.Type != channelevents.CITimeout || ev.PR != 34 || ev.Stage != "Validate" {
		t.Fatalf("%+v", ev)
	}
	requireMeta(t, ev, "stage", "pr", "timeout", "ci")
}

func TestChannelReviewTimeoutHookNotesEarlyReviews(t *testing.T) {
	e, sink := channelEngine(t, 0, onlyTypes(channelevents.ReviewTimeout))
	seedBaseline(t, e, 1, "Validate")
	e.store.Apply(itemstate.PRDetailsUpdated{Repo: apiRepo, Number: 1, PRNumber: 34, State: "open"})
	waitStart := time.Now()
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: apiRepo, Number: 1, Label: "fabrik:awaiting-review", At: waitStart})
	e.store.Apply(itemstate.PRReviewSubmitted{Repo: apiRepo, Number: 1, Review: gh.PRReview{Author: "early-bird", State: "COMMENTED", DatabaseID: 1, SubmittedAt: waitStart.Add(-time.Hour)}})
	e.store.Apply(itemstate.PRReviewSubmitted{Repo: apiRepo, Number: 1, Review: gh.PRReview{Author: "latecomer", State: "COMMENTED", DatabaseID: 2, SubmittedAt: waitStart.Add(time.Minute)}})
	e.emitReviewTimeout(hookItem(e, 1), &stages.Stage{Name: "Validate"})
	ev := sink.next(t)
	if ev.Type != channelevents.ReviewTimeout || ev.Meta["reviews_before_wait"] != "early-bird" {
		t.Fatalf("%+v", ev)
	}
	requireMeta(t, ev, "stage", "pr", "timeout", "pending_reviewers", "reviews_before_wait")
}

func TestChannelReviewTimeoutUnknownWaitStartAfterRestart(t *testing.T) {
	e, sink := channelEngine(t, 0, onlyTypes(channelevents.ReviewTimeout))
	seedBaseline(t, e, 1, "Validate")
	e.emitReviewTimeout(hookItem(e, 1), &stages.Stage{Name: "Validate"})
	if ev := sink.next(t); ev.Meta["reviews_before_wait"] != "unknown" {
		t.Fatalf("an unrecorded wait start must read unknown, got %+v", ev.Meta)
	}
}

func TestChannelChildrenSpawnedHook(t *testing.T) {
	e, sink := channelEngine(t, 0, onlyTypes(channelevents.ChildrenSpawned))
	seedBaseline(t, e, 1, "Implement")
	e.emitChildrenSpawned(hookItem(e, 1), []string{"owner/repo#101", "owner/repo#102"})
	ev := sink.next(t)
	if ev.Type != channelevents.ChildrenSpawned || ev.Meta["count"] != "2" || !strings.Contains(ev.Meta["children"], "owner/repo#102") {
		t.Fatalf("%+v", ev)
	}
	requireMeta(t, ev, "stage", "children", "count")
}

// ---- stalled: agrees with the read API's attention view ----

func attentionStateOf(t *testing.T, e *Engine, n int) string {
	t.Helper()
	res, err := localAPIBackend{e: e}.Status(statusParams(n))
	if err != nil {
		t.Fatal(err)
	}
	return res.Attention.State
}

func TestChannelStalledAgreesWithReadAPI(t *testing.T) {
	e, sink := channelEngineTick(t, 0, onlyTypes(channelevents.Stalled), 40*time.Millisecond)
	seedBaseline(t, e, 1, "Implement")
	// Two hours pass with nothing observable changing.
	e.clock.(*adjClock).set(time.Now().Add(2 * time.Hour))

	if got := attentionStateOf(t, e, 1); got != "stalled" {
		t.Fatalf("read API says %q, want stalled", got)
	}
	ev := sink.next(t)
	if ev.Type != channelevents.Stalled || ev.Issue != 1 {
		t.Fatalf("%+v", ev)
	}
	requireMeta(t, ev, "stage", "code", "reason")
	// Edge-triggered: later ticks must not repeat it.
	sink.quiet(t, 300*time.Millisecond)
}

func TestChannelHealthyLongRunningWorkerIsNotStalled(t *testing.T) {
	e, sink := channelEngineTick(t, 40*time.Minute, onlyTypes(channelevents.Stalled), 40*time.Millisecond)
	e.stageByName("Implement").MaxWallTime = 2 * time.Hour // budget well beyond the 40m of silence
	seedBaseline(t, e, 1, "Implement")
	e.store.Apply(itemstate.WorkerEntered{Repo: apiRepo, Number: 1, StageName: "Implement", StartedAt: time.Now()})
	time.Sleep(150 * time.Millisecond)

	if got := attentionStateOf(t, e, 1); got != "working" {
		t.Fatalf("read API says %q, want working", got)
	}
	sink.quiet(t, 400*time.Millisecond)
}

func TestChannelExpiredCooldownAloneIsNotAStall(t *testing.T) {
	// Recently active (clock not ahead) with only an expired cooldown on record.
	e, sink := channelEngineTick(t, 0, onlyTypes(channelevents.Stalled), 40*time.Millisecond)
	seedBaseline(t, e, 1, "Implement")
	e.store.Apply(itemstate.CooldownRecorded{Repo: apiRepo, Number: 1, Reason: "retry", Until: time.Now().Add(-time.Hour)})
	time.Sleep(150 * time.Millisecond)

	if got := attentionStateOf(t, e, 1); got == "stalled" {
		t.Fatalf("read API classifies an expired cooldown as a stall: %q", got)
	}
	sink.quiet(t, 400*time.Millisecond)
}

// ---- claude-limit (account-wide) ----

func TestChannelClaudeLimitSuspendedAndLifted(t *testing.T) {
	e, sink := channelEngineTick(t, 0, onlyTypes(channelevents.ClaudeLimitSuspended, channelevents.ClaudeLimitLifted), time.Hour)
	e.activateClaudeSuspension(0, nil, e.now())
	ev := sink.next(t)
	if ev.Type != channelevents.ClaudeLimitSuspended || ev.Meta["until"] == "" || ev.Issue != 0 {
		t.Fatalf("%+v", ev)
	}
	// Extending the same suspension is not a new transition.
	e.activateClaudeSuspension(0, nil, e.now().Add(time.Minute))
	sink.quiet(t, 200*time.Millisecond)

	e.clearClaudeSuspension("test")
	if lifted := sink.next(t); lifted.Type != channelevents.ClaudeLimitLifted {
		t.Fatalf("%+v", lifted)
	}
	sink.quiet(t, 150*time.Millisecond)
}

func TestChannelClaudeLimitLazyLiftByDeadline(t *testing.T) {
	e, sink := channelEngineTick(t, 0, onlyTypes(channelevents.ClaudeLimitSuspended, channelevents.ClaudeLimitLifted), 40*time.Millisecond)
	e.claudeSuspendMu.Lock()
	e.claudeSuspendedUntil = e.now().Add(time.Hour)
	e.claudeSuspendMu.Unlock()
	e.channelNudge()
	if ev := sink.next(t); ev.Type != channelevents.ClaudeLimitSuspended {
		t.Fatalf("%+v", ev)
	}
	// The deadline passes with no clear call: the ticker notices.
	e.clock.(*adjClock).set(time.Now().Add(2 * time.Hour))
	if ev := sink.next(t); ev.Type != channelevents.ClaudeLimitLifted {
		t.Fatalf("lazy lift not reported: %+v", ev)
	}
}

// Account-wide events reach a subscriber whose scope is one issue.
func TestChannelAccountWideEventIgnoresIssueScope(t *testing.T) {
	sub := &channelevents.Subscription{Issues: []channelevents.IssueRef{{Repo: apiRepo, Number: 99}}}
	e, sink := channelEngine(t, 0, sub)
	e.activateClaudeSuspension(0, nil, e.now())
	if ev := sink.next(t); ev.Type != channelevents.ClaudeLimitSuspended {
		t.Fatalf("%+v", ev)
	}
}

func statusParams(n int) localapi.StatusParams {
	return localapi.StatusParams{Issue: issueRef(apiRepo, n)}
}
