package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/events"
)

// fakeTransport is a transport the engine has never heard of: it implements only
// ingestTransport. Every test using it proves the engine's health, coverage and
// reconnect handling needs no code change for a new transport (#2351 SC-005).
type fakeTransport struct {
	name string

	mu          sync.Mutex
	healthy     bool
	disabled    bool
	started     bool
	stopped     bool
	hints       []bool
	hintReasons []string
	coverage    int
	repos       map[string]bool
	sub         func(string) bool
	sink        func(delivery)
	reconnect   func()
}

func newFakeTransport(name string) *fakeTransport {
	return &fakeTransport{name: name, healthy: true}
}

func (f *fakeTransport) Name() string { return f.name }
func (f *fakeTransport) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return nil
}
func (f *fakeTransport) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
}
func (f *fakeTransport) IsHealthyOrStartingUp() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthy
}
func (f *fakeTransport) IsDisabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disabled
}
func (f *fakeTransport) UpdateRepos(repos map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = repos
}
func (f *fakeTransport) TransitionHealth(healthy bool, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hints = append(f.hints, healthy)
	f.hintReasons = append(f.hintReasons, reason)
	f.healthy = healthy
}
func (f *fakeTransport) CheckCoverage() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.coverage++
}
func (f *fakeTransport) SubscribesTo(ev string) bool {
	if f.sub == nil {
		return true
	}
	return f.sub(ev)
}
func (f *fakeTransport) SetSink(s func(delivery)) { f.sink = s }
func (f *fakeTransport) SetReconnectHandler(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconnect = fn
}

var _ ingestTransport = (*fakeTransport)(nil)

func TestTransportSet_NilAndEmptyAreInert(t *testing.T) {
	var nilSet *transportSet
	nilSet.UpdateRepos(map[string]bool{"a/b": true})
	nilSet.TransitionHealth(true, "")
	nilSet.CheckCoverage()
	nilSet.StopAll()
	if !nilSet.Empty() || nilSet.Healthy() || nilSet.Disabled() || nilSet.SubscribesTo("issues") {
		t.Fatal("a nil transportSet must report no transport, not healthy, not disabled")
	}
	if got := nilSet.Mode(); got != "off" {
		t.Fatalf("Mode() = %q, want off", got)
	}
	if got := (&transportSet{}).Mode(); got != "off" {
		t.Fatalf("empty Mode() = %q, want off", got)
	}
}

func TestTransportSet_FakeThirdTransportGetsHealthAndCoverage(t *testing.T) {
	set := &transportSet{}
	set.Add(newFakeTransport("a"))
	third := newFakeTransport("funnel")
	set.Add(third)

	set.TransitionHealth(false, "3 item(s) drifted")
	set.TransitionHealth(true, "drift reconciled")
	set.CheckCoverage()
	set.UpdateRepos(map[string]bool{"o/r": true})

	third.mu.Lock()
	defer third.mu.Unlock()
	if len(third.hints) != 2 || third.hints[0] || !third.hints[1] {
		t.Errorf("third transport hints = %v, want [false true]", third.hints)
	}
	if third.hintReasons[0] != "3 item(s) drifted" {
		t.Errorf("hint reason = %q", third.hintReasons[0])
	}
	if third.coverage != 1 {
		t.Errorf("coverage checks = %d, want 1", third.coverage)
	}
	if !third.repos["o/r"] {
		t.Error("UpdateRepos did not reach the third transport")
	}
}

func TestTransportSet_AggregatesAreAnyHealthyAndAllDisabled(t *testing.T) {
	a, b := newFakeTransport("a"), newFakeTransport("b")
	set := &transportSet{}
	set.Add(a)
	set.Add(b)

	if set.Mode() != "healthy" || !set.Healthy() {
		t.Fatalf("both healthy: mode=%q", set.Mode())
	}
	a.healthy = false
	if set.Mode() != "healthy" {
		t.Fatalf("one healthy transport keeps the set healthy: mode=%q", set.Mode())
	}
	b.healthy = false
	if set.Mode() != "unhealthy" {
		t.Fatalf("none healthy: mode=%q, want unhealthy", set.Mode())
	}
	a.disabled = true
	if set.Disabled() || set.Mode() != "unhealthy" {
		t.Fatalf("one disabled transport must not disable the set: mode=%q", set.Mode())
	}
	b.disabled = true
	if !set.Disabled() || set.Mode() != "disabled" {
		t.Fatalf("all disabled: mode=%q, want disabled", set.Mode())
	}
}

func TestTransportSet_SingleTransportMatchesItsOwnValues(t *testing.T) {
	// FR-014: with one transport every aggregate equals that transport's own
	// value, so single-transport behaviour is unchanged.
	hm, _ := newTestHookdeckManager(t)
	set := &transportSet{}
	set.Add(hm)
	if set.Healthy() != hm.IsHealthyOrStartingUp() || set.Disabled() != hm.IsDisabled() {
		t.Fatal("single-transport set diverges from the transport")
	}
	hm.handleHealth(events.HealthEvent{State: events.HealthConnected})
	if !set.Healthy() || set.Mode() != "healthy" {
		t.Fatalf("connected hookdeck: mode=%q", set.Mode())
	}
}

func TestTransportSet_SubscribesToIsAnyTransport(t *testing.T) {
	a := newFakeTransport("a")
	a.sub = func(string) bool { return false }
	b := newFakeTransport("b")
	b.sub = func(ev string) bool { return ev == "projects_v2_item" }
	set := &transportSet{}
	set.Add(a)
	set.Add(b)
	if !set.SubscribesTo("projects_v2_item") || set.SubscribesTo("issues") {
		t.Fatal("SubscribesTo must be true exactly when some transport receives the event")
	}
}

// A fake third transport rides reconcileTick with no engine change: it gets the
// coverage re-check and the drift-free health hint.
func TestReconcileTick_DrivesAnyTransport(t *testing.T) {
	t1 := time.Now().Truncate(time.Second)
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items:     []gh.ProjectItem{{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Plan", UpdatedAt: t1}},
	})
	eng.readClient = cache
	client.fetchProjectBoardFn = func(_, _ string, _ int, _ string) (*gh.ProjectBoard, error) {
		return &gh.ProjectBoard{
			ProjectID: "PVT_1",
			Items:     []gh.ProjectItem{{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Plan", UpdatedAt: t1, MilestoneKnown: true}},
		}, nil
	}

	third := newFakeTransport("funnel")
	third.healthy = false
	set := &transportSet{}
	set.Add(third)
	eng.reconcileTick(cache, set)

	third.mu.Lock()
	defer third.mu.Unlock()
	if third.coverage != 1 {
		t.Errorf("coverage re-checks = %d, want 1", third.coverage)
	}
	if len(third.hints) != 1 || !third.hints[0] {
		t.Errorf("health hints = %v, want [true] after a drift-free tick", third.hints)
	}
}

// Two transports run side by side through the ingestion setup: neither blocks the
// other and a failed start of one leaves the other attached.
func TestSetUpIngestion_NoTransportConfiguredIsPollOnly(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	cleanup := eng.setUpIngestion(context.Background(), nil)
	defer cleanup()
	if eng.transports != nil && !eng.transports.Empty() {
		t.Fatal("no transport configured: the engine must stay poll-only")
	}
	if eng.echo != nil || eng.intake != nil {
		t.Fatal("poll-only engine must have no echo registry and no intake")
	}
}

func TestSetUpIngestion_HookdeckWithoutCredentialsIsPollOnly(t *testing.T) {
	t.Setenv(DefaultHookdeckAPIKeyEnv, "")
	t.Setenv(DefaultHookdeckWebhookSecretEnv, "")
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.EventSource = EventSourceHookdeck
	cleanup := eng.setUpIngestion(context.Background(), nil)
	defer cleanup()
	if !eng.transports.Empty() {
		t.Fatal("a transport that could not start must not be attached")
	}
	if eng.echo != nil || eng.intake != nil {
		t.Fatal("with no transport attached the echo registry and intake must be dropped")
	}
}
