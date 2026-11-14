package engine

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Unit tests of the class-aware cooldown scheduling (#2532, ADR 2532).

func scheduleEngine(t *testing.T) (*Engine, time.Time, *fakeTransport) {
	t.Helper()
	eng, start := cooldownAdmissionEngine(t, &mockGitHubClient{})
	ft := newFakeTransport("fake")
	eng.transports = &transportSet{}
	eng.transports.Add(ft)
	return eng, start, ft
}

func openAt(eng *Engine, n int, status string) {
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Repo: cooldownTestRepo, Number: n, Title: "x", Status: status}})
}

func TestReliableFor(t *testing.T) {
	var nilSet *transportSet
	if nilSet.ReliableFor(cooldownTestRepo, "") {
		t.Error("a nil set (poll-only) is never reliable")
	}
	eng, _, ft := scheduleEngine(t)
	if !eng.transports.ReliableFor(cooldownTestRepo, "issue_comment") {
		t.Error("a healthy covering transport is reliable")
	}
	ft.mu.Lock()
	ft.healthy = false
	ft.mu.Unlock()
	if eng.transports.ReliableFor(cooldownTestRepo, "") {
		t.Error("an unhealthy transport is not reliable")
	}
	ft.mu.Lock()
	ft.healthy = true
	ft.repos = map[string]bool{"other/repo": true}
	ft.mu.Unlock()
	if eng.transports.ReliableFor(cooldownTestRepo, "") {
		t.Error("a transport that does not cover the repo is not reliable for it")
	}
	ft.mu.Lock()
	ft.repos = nil
	ft.sub = func(ev string) bool { return ev != "issue_dependencies" }
	ft.mu.Unlock()
	if eng.transports.ReliableFor(cooldownTestRepo, "issue_dependencies") {
		t.Error("an unsubscribed event is not reliable")
	}
	if !eng.transports.ReliableFor(cooldownTestRepo, "issue_comment") {
		t.Error("a subscribed event stays reliable")
	}
}

func TestWebhookManagerHealthyForIsStrict(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	wm.mu.Lock()
	wm.state = WebhookStreamStartingUp
	wm.events = []string{"issue_comment"}
	wm.mu.Unlock()
	if !wm.IsHealthyOrStartingUp() {
		t.Fatal("precondition: starting up counts as healthy for the idle cap")
	}
	if wm.HealthyFor("myorg/myrepo", "") {
		t.Error("starting up is not strictly healthy")
	}
	wm.mu.Lock()
	wm.state = WebhookStreamHealthy
	wm.mu.Unlock()
	if !wm.HealthyFor("myorg/myrepo", "issue_comment") {
		t.Error("healthy, managed repo, subscribed event must be reliable")
	}
	if wm.HealthyFor("myorg/other", "") {
		t.Error("an unmanaged repo is not covered")
	}
	if wm.HealthyFor("myorg/myrepo", "issue_dependencies") {
		t.Error("an unsubscribed event is not covered")
	}
	wm.mu.Lock()
	wm.unsubscribableRepos = map[string]bool{"myorg/myrepo": true}
	wm.mu.Unlock()
	if wm.HealthyFor("myorg/myrepo", "") {
		t.Error("a quarantined repo is not covered")
	}
}

func TestPeriodicReEvalInterval(t *testing.T) {
	eng, _, ft := scheduleEngine(t) // poll 60 s: githubRecheckInterval = 10 min
	if got := eng.periodicReEvalInterval(cooldownTestRepo); got != autoSafetyNetInterval {
		t.Errorf("reliable, auto: %v, want %v", got, autoSafetyNetInterval)
	}
	eng.cfg.PeriodicReEvalInterval = 45 * time.Minute
	if got := eng.periodicReEvalInterval(cooldownTestRepo); got != 45*time.Minute {
		t.Errorf("reliable, explicit: %v, want 45m", got)
	}
	ft.mu.Lock()
	ft.healthy = false
	ft.mu.Unlock()
	if got := eng.periodicReEvalInterval(cooldownTestRepo); got != eng.githubRecheckInterval() {
		t.Errorf("unhealthy: %v, want githubRecheckInterval %v", got, eng.githubRecheckInterval())
	}
	eng.cfg.PeriodicReEvalInterval = 2 * time.Minute
	if got := eng.periodicReEvalInterval(cooldownTestRepo); got != 2*time.Minute {
		t.Errorf("unhealthy, explicit below recheck: %v, want 2m", got)
	}
	eng.cfg.PeriodicReEvalInterval = time.Second
	if got := eng.periodicReEvalInterval(cooldownTestRepo); got != minSafetyNetInterval {
		t.Errorf("explicit below the floor: %v, want %v", got, minSafetyNetInterval)
	}
	// poll-only: no transport at all.
	eng.cfg.PeriodicReEvalInterval = 0
	eng.transports = nil
	if got := eng.periodicReEvalInterval(cooldownTestRepo); got != eng.githubRecheckInterval() {
		t.Errorf("poll-only: %v, want githubRecheckInterval", got)
	}
}

func TestPeriodicReEvalInterval_Neutralised(t *testing.T) {
	eng, _, _ := scheduleEngine(t)
	eng.SetEventReplaceableStretchDisabledForTest(true)
	if got := eng.periodicReEvalInterval(cooldownTestRepo); got != eng.githubRecheckInterval() {
		t.Errorf("stretch neutralised: %v, want today's interval", got)
	}
	eng.SetEventReplaceableStretchDisabledForTest(false)
	eng.SetSafetyNetDisabledForTest(true)
	if got := eng.periodicReEvalInterval(cooldownTestRepo); got != neutralisedSafetyNetInterval {
		t.Errorf("safety net neutralised: %v", got)
	}
}

// Event-replaceable reasons are not counted while reliable; safety-net, timer-class
// and unhealthy cases keep the ADR 2528 cap.
func TestEarliestLongCooldown_ClassAware(t *testing.T) {
	eng, start, ft := scheduleEngine(t)
	openAt(eng, 1, "Research")
	rec := func(reason string, d time.Duration) {
		eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 1, Reason: reason, Until: start.Add(d)})
	}
	rec(reasonFeedbackPending, 20*time.Second)
	rec(reasonReviewBlocked, 30*time.Second)
	if got := eng.earliestLongCooldown(start); !got.IsZero() {
		t.Fatalf("reliable ingestion must not count event-replaceable reasons, got %v", got)
	}
	rec(reasonPeriodicReEval, 40*time.Second)
	if got := eng.earliestLongCooldown(start); !got.Equal(start.Add(40 * time.Second)) {
		t.Fatalf("the safety net still counts, got %v", got)
	}
	rec(reasonSpawnResume, 35*time.Second)
	if got := eng.earliestLongCooldown(start); !got.Equal(start.Add(35 * time.Second)) {
		t.Fatalf("a timer-class cooldown still counts, got %v", got)
	}
	ft.mu.Lock()
	ft.healthy = false
	ft.mu.Unlock()
	if got := eng.earliestLongCooldown(start); !got.Equal(start.Add(20 * time.Second)) {
		t.Fatalf("unhealthy ingestion keeps today's cap, got %v", got)
	}
	ft.mu.Lock()
	ft.healthy = true
	ft.mu.Unlock()
	eng.SetEventReplaceableStretchDisabledForTest(true)
	if got := eng.earliestLongCooldown(start); !got.Equal(start.Add(20 * time.Second)) {
		t.Fatalf("neutralised stretch keeps today's cap, got %v", got)
	}
}

// dep-blocked is stretched only when every blocker is cached and covered, because
// DependentRefreshObserver is cache-only.
func TestEarliestLongCooldown_DepBlockedNeedsCachedBlockers(t *testing.T) {
	eng, start, _ := scheduleEngine(t)
	openAt(eng, 1, "Research")
	eng.store.Apply(itemstate.BlockedByEdgeAdded{Repo: cooldownTestRepo, Number: 1, Dep: gh.Dependency{Number: 9, State: "OPEN"}})
	eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 1, Reason: reasonDepBlocked, Until: start.Add(30 * time.Second)})
	if got := eng.earliestLongCooldown(start); !got.Equal(start.Add(30 * time.Second)) {
		t.Fatalf("an uncached blocker must keep the cap, got %v", got)
	}
	openAt(eng, 9, "Research")
	if got := eng.earliestLongCooldown(start); !got.IsZero() {
		t.Fatalf("a cached, covered blocker lets the re-check be stretched, got %v", got)
	}
}

func TestEarliestLongCooldown_ExcludesShortAndExempt(t *testing.T) {
	eng, start, _ := scheduleEngine(t)
	openAt(eng, 1, "Research")
	for _, r := range []string{reasonRetryBackoff, reasonFeedbackDispatchGap, archiveEligibleAtCooldownReason} {
		eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 1, Reason: r, Until: start.Add(10 * time.Second)})
	}
	if got := eng.earliestLongCooldown(start); !got.IsZero() {
		t.Fatalf("short and exempt reasons are not long cooldowns, got %v", got)
	}
	if got := eng.earliestShortCooldown(start); !got.Equal(start.Add(10 * time.Second)) {
		t.Fatalf("feedback-dispatcher-gap is now a short cooldown, got %v", got)
	}
}

func TestTimerRegistry(t *testing.T) {
	eng, start, _ := scheduleEngine(t) // poll 60 s: floor 30 s
	openAt(eng, 1, "Research")
	eng.noteTimerDeadline(cooldownTestRepo, 1, timerReviewWait, 5*time.Minute)
	if got := eng.earliestTimerDeadline(start); !got.Equal(start.Add(5 * time.Minute)) {
		t.Fatalf("deadline = %v", got)
	}
	// A re-note replaces the entry; a due of "now" is floored, so a gate that
	// still says not-yet cannot hot-loop the poll.
	eng.noteTimerDeadline(cooldownTestRepo, 1, timerReviewWait, 0)
	if got := eng.earliestTimerDeadline(start); !got.Equal(start.Add(eng.mergeGateRecheckInterval())) {
		t.Fatalf("floored deadline = %v, want now+%v", got, eng.mergeGateRecheckInterval())
	}
	if keys := eng.consumeDueTimers(start); len(keys) != 0 {
		t.Fatalf("nothing is due yet, got %v", keys)
	}
	keys := eng.consumeDueTimers(start.Add(time.Minute))
	if len(keys) != 1 || keys[0] != cooldownTestRepo+"#1" {
		t.Fatalf("consumed = %v", keys)
	}
	if keys := eng.consumeDueTimers(start.Add(time.Hour)); len(keys) != 0 {
		t.Fatalf("a consumed entry must not fire again, got %v", keys)
	}
	if got := eng.earliestTimerDeadline(start); !got.IsZero() {
		t.Fatalf("registry should be empty, got %v", got)
	}
}

func TestTimerRegistry_NeutralisedAndPruned(t *testing.T) {
	eng, start, _ := scheduleEngine(t)
	openAt(eng, 1, "Research")
	eng.SetTimerWakeDisabledForTest(true)
	eng.noteTimerDeadline(cooldownTestRepo, 1, timerCIBackstop, time.Minute)
	if got := eng.earliestTimerDeadline(start); !got.IsZero() {
		t.Fatalf("neutralised registry records nothing, got %v", got)
	}
	eng.SetTimerWakeDisabledForTest(false)
	eng.noteTimerDeadline(cooldownTestRepo, 1, timerCIBackstop, time.Minute)
	eng.store.Apply(itemstate.LocalIssueClosed{Repo: cooldownTestRepo, Number: 1})
	if got := eng.earliestTimerDeadline(start); !got.IsZero() {
		t.Fatalf("a closed item's deadline is pruned, got %v", got)
	}
}

// FR-007: reliable → unreliable re-clamps the repo's periodic-re-eval entries to
// today's interval and wakes the poll; a flap is one wake.
func TestHealthWatchTick_RevertsStretchedEntries(t *testing.T) {
	eng, start, ft := scheduleEngine(t)
	wake := make(chan struct{}, 1)
	eng.SetWakeCh(wake)
	openAt(eng, 1, "Research")
	eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 1, Reason: reasonPeriodicReEval, Until: start.Add(autoSafetyNetInterval)})

	prev := eng.HealthWatchTickForTest(nil)
	if !prev[cooldownTestRepo] {
		t.Fatal("repo should be reliable")
	}
	select {
	case <-wake:
		t.Fatal("no wake while reliability is unchanged")
	default:
	}
	ft.mu.Lock()
	ft.healthy = false
	ft.mu.Unlock()
	next := eng.HealthWatchTickForTest(prev)
	if next[cooldownTestRepo] {
		t.Fatal("repo should no longer be reliable")
	}
	select {
	case <-wake:
	default:
		t.Fatal("losing reliability must wake the poll")
	}
	snap, _ := eng.store.Peek(cooldownTestRepo, 1)
	if got := snap.CooldownAt(reasonPeriodicReEval); !got.Equal(start.Add(eng.githubRecheckInterval())) {
		t.Fatalf("periodic-re-eval = %v, want start+%v", got, eng.githubRecheckInterval())
	}
	// Flapping while still unreliable wakes nothing further.
	_ = eng.HealthWatchTickForTest(next)
	select {
	case <-wake:
		t.Fatal("no second wake without a new transition")
	default:
	}
}
