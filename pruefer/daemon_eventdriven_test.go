package pruefer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/pruefer/events"
)

// fakeEventSource is a test events.EventSource. When runFn is nil, Run
// blocks until ctx is cancelled and returns nil — mirroring a well-behaved
// real source per the events.EventSource contract. Setting runFn lets
// tests simulate a misbehaving source that returns early.
type fakeEventSource struct {
	mu       sync.Mutex
	runCalls int
	runFn    func(ctx context.Context, sink events.EventSink) error
}

func (f *fakeEventSource) Run(ctx context.Context, sink events.EventSink) error {
	f.mu.Lock()
	f.runCalls++
	fn := f.runFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, sink)
	}
	<-ctx.Done()
	return nil
}

func (f *fakeEventSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runCalls
}

// newEventDrivenTestDaemon builds a Daemon wired for runEventDriven tests:
// one watched repo with one eligible PR (so a poll's effect is observable
// via client.submitCallCount()), a blocking-by-default fakeEventSource, and
// a short concurrency cap.
func newEventDrivenTestDaemon(source *fakeEventSource) (*Daemon, *fakeLister) {
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1"}}
	claude := &mockClaudeInvoker{}
	clone := func(ctx context.Context, owner, repo, token string, prNumber int) (string, func(), error) {
		return "/tmp", func() {}, nil
	}
	d := &Daemon{
		Clients:     map[string]GitHubLister{"owner": client},
		Claude:      claude,
		Clone:       clone,
		Config:      Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: 3},
		BotLogin:    "pruefer-bot[bot]",
		EventSource: source,
	}
	return d, client
}

func runDaemonInBackground(t *testing.T, d *Daemon, ctx context.Context) (done chan error) {
	t.Helper()
	done = make(chan error, 1)
	go func() { done <- d.runEventDriven(ctx) }()
	return done
}

func TestDaemonRunEventDriven_StartupReconciliation(t *testing.T) {
	source := &fakeEventSource{}
	d, client := newEventDrivenTestDaemon(source)
	d.Config.ReconciliationStartup = true
	d.Config.ReconciliationFallbackInterval = time.Hour // must not fire during this test

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemonInBackground(t, d, ctx)

	waitUntil(t, 2*time.Second, func() bool { return client.submitCallCount() == 1 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runEventDriven did not return after ctx cancellation")
	}
}

func TestDaemonRunEventDriven_NoStartupReconciliationWhenDisabled(t *testing.T) {
	source := &fakeEventSource{}
	d, client := newEventDrivenTestDaemon(source)
	d.Config.ReconciliationStartup = false
	d.Config.ReconciliationFallbackInterval = time.Hour // must not fire during this test

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDaemonInBackground(t, d, ctx)

	time.Sleep(100 * time.Millisecond)
	if got := client.submitCallCount(); got != 0 {
		t.Errorf("submitCallCount() = %d, want 0 (startup reconciliation disabled, fallback interval far in the future)", got)
	}
}

func TestDaemonRunEventDriven_FallbackIntervalTicks(t *testing.T) {
	source := &fakeEventSource{}
	d, client := newEventDrivenTestDaemon(source)
	d.Config.ReconciliationStartup = false
	d.Config.ReconciliationFallbackInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDaemonInBackground(t, d, ctx)

	waitUntil(t, 2*time.Second, func() bool { return client.submitCallCount() >= 1 })
}

func TestDaemonRunEventDriven_ReconnectTriggersReconciliation(t *testing.T) {
	source := &fakeEventSource{}
	d, client := newEventDrivenTestDaemon(source)
	d.Config.ReconciliationStartup = false
	d.Config.ReconciliationFallbackInterval = time.Hour // must not fire during this test

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDaemonInBackground(t, d, ctx)

	time.Sleep(50 * time.Millisecond) // let runEventDriven start up first
	if got := client.submitCallCount(); got != 0 {
		t.Fatalf("submitCallCount() = %d before any health event, want 0", got)
	}

	handler := d.HealthHandler(ctx)
	handler(events.HealthEvent{State: events.HealthConnected}) // first connect: no-op
	time.Sleep(50 * time.Millisecond)
	if got := client.submitCallCount(); got != 0 {
		t.Errorf("submitCallCount() = %d after the FIRST connect, want 0 (covered by startup reconciliation, not a reconnect)", got)
	}

	handler(events.HealthEvent{State: events.HealthReconnecting})
	handler(events.HealthEvent{State: events.HealthConnected}) // reconnect: must trigger a poll
	waitUntil(t, 2*time.Second, func() bool { return client.submitCallCount() == 1 })
}

func TestDaemonRunEventDriven_FlappingReconnect_CoalescesReconciliationPolls(t *testing.T) {
	source := &fakeEventSource{}
	d, client := newEventDrivenTestDaemon(source)
	d.Config.ReconciliationStartup = false
	d.Config.ReconciliationFallbackInterval = time.Hour // must not fire during this test
	block := make(chan struct{})
	client.listOpenPRsBlock = block

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDaemonInBackground(t, d, ctx)

	time.Sleep(50 * time.Millisecond) // let runEventDriven start up first

	handler := d.HealthHandler(ctx)
	handler(events.HealthEvent{State: events.HealthConnected}) // first connect: no-op
	time.Sleep(20 * time.Millisecond)

	// Simulate a flapping connection: several genuine connect/drop cycles in
	// quick succession, each achieving HealthConnected before failing again —
	// exactly the scenario HealthHandler's coalescing (via
	// triggerReconciliationPoll) guards against.
	for i := 0; i < 5; i++ {
		handler(events.HealthEvent{State: events.HealthReconnecting})
		handler(events.HealthEvent{State: events.HealthConnected})
	}

	waitUntil(t, 2*time.Second, func() bool { return client.listOpenPRsCallCount() >= 1 })
	// Give any wrongly-uncoalesced extra poll cycles a chance to start their
	// own ListOpenPRs call before asserting the negative.
	time.Sleep(50 * time.Millisecond)
	if got := client.listOpenPRsCallCount(); got != 1 {
		t.Errorf("listOpenPRsCallCount() = %d, want 1 (a flapping-reconnect burst must coalesce into a single in-flight poll)", got)
	}

	close(block)
	waitUntil(t, 2*time.Second, func() bool { return client.submitCallCount() == 1 })
}

func TestDaemonRunEventDriven_SourceFailsImmediately_PollFallbackStillWorks(t *testing.T) {
	source := &fakeEventSource{
		runFn: func(ctx context.Context, sink events.EventSink) error {
			return errors.New("boom: source died on startup")
		},
	}
	d, client := newEventDrivenTestDaemon(source)
	d.Config.ReconciliationStartup = false
	d.Config.ReconciliationFallbackInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemonInBackground(t, d, ctx) // must not panic

	waitUntil(t, 2*time.Second, func() bool { return client.submitCallCount() >= 1 })
	if got := source.callCount(); got != 1 {
		t.Errorf("source.callCount() = %d, want 1 (runEventDriven must not busy-loop restarting a dead source)", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runEventDriven did not return after ctx cancellation")
	}
}

func TestDaemonRun_DispatchesToEventDrivenWhenEventSourceSet(t *testing.T) {
	source := &fakeEventSource{}
	d, client := newEventDrivenTestDaemon(source)
	d.FabrikDir = t.TempDir()
	d.Config.ReconciliationStartup = true
	d.Config.ReconciliationFallbackInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	waitUntil(t, 2*time.Second, func() bool { return client.submitCallCount() == 1 })
	waitUntil(t, 2*time.Second, func() bool { return source.callCount() == 1 })

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run() = %v, want nil after ctx cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}
