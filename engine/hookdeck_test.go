package engine

import (
	"errors"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/events"
	"github.com/handarbeit/fabrik/tui"
)

// newTestHookdeckManager builds a hookdeckManager suitable for unit tests:
// no real hookdeck.Source network activity is ever triggered, since these
// tests call the health/drop/coverage methods directly rather than Start.
func newTestHookdeckManager(t *testing.T) (*hookdeckManager, chan tui.Event) {
	t.Helper()
	events := make(chan tui.Event, 100)
	hm := newHookdeckManager(
		func(int, string, string, ...any) {},
		func(e tui.Event) { events <- e },
		nil,
		nil,
		"test-api-key",
		"test-webhook-secret",
	)
	return hm, events
}

func TestHookdeckManager_ImplementsEventIngestionManager(t *testing.T) {
	var _ eventIngestionManager = (*hookdeckManager)(nil)
}

func TestHookdeckManager_IsDisabledAlwaysFalse(t *testing.T) {
	hm, _ := newTestHookdeckManager(t)
	if hm.IsDisabled() {
		t.Error("hookdeckManager.IsDisabled() = true, want always false (no per-repo circuit breaker analogue)")
	}
}

func TestHookdeckManager_EchoMethodsAreNoOps(t *testing.T) {
	hm, _ := newTestHookdeckManager(t)
	// These must not panic and must have no observable effect — there is no
	// pending-echo state on hookdeckManager to assert against; the absence
	// of a field to mutate is itself the point (Decision #3).
	hm.RegisterEcho("issues", "edited", "key")
	hm.RegisterEchoIfSubscribed("projects_v2_item", "edited", "key")
	hm.MatchEcho("issues", "edited", "key")
}

func TestHookdeckManager_HealthTransitions(t *testing.T) {
	hm, eventsCh := newTestHookdeckManager(t)

	if hm.IsHealthyOrStartingUp() {
		t.Fatal("before any health event, expected initial state Unhealthy (not yet StartingUp)")
	}

	hm.handleHealth(events.HealthEvent{State: events.HealthConnected})
	if !hm.IsHealthyOrStartingUp() {
		t.Error("after HealthConnected, IsHealthyOrStartingUp() = false, want true")
	}
	select {
	case ev := <-eventsCh:
		wse, ok := ev.(tui.WebhookStatusEvent)
		if !ok {
			t.Fatalf("emitted event type = %T, want tui.WebhookStatusEvent", ev)
		}
		if wse.State != string(WebhookStreamHealthy) {
			t.Errorf("emitted State = %q, want %q", wse.State, WebhookStreamHealthy)
		}
	default:
		t.Fatal("expected a WebhookStatusEvent to be emitted on health transition")
	}

	hm.handleHealth(events.HealthEvent{State: events.HealthReconnecting})
	if hm.IsHealthyOrStartingUp() {
		t.Error("after HealthReconnecting, IsHealthyOrStartingUp() = true, want false")
	}
}

func TestHookdeckManager_SignatureDriftForcesUnhealthy(t *testing.T) {
	hm, _ := newTestHookdeckManager(t)

	// Healthy connectivity...
	hm.handleHealth(events.HealthEvent{State: events.HealthConnected})
	if !hm.IsHealthyOrStartingUp() {
		t.Fatal("expected healthy after HealthConnected")
	}

	// ...but a sustained signature-drift episode must force Unhealthy
	// regardless of transport connectivity (R4).
	hm.handleSignatureDrift(true)
	if hm.IsHealthyOrStartingUp() {
		t.Error("after signature drift activated, IsHealthyOrStartingUp() = true, want false (forced Unhealthy)")
	}

	// Recovery: drift clears, and the last known connectivity state
	// (Healthy) takes back over.
	hm.handleSignatureDrift(false)
	if !hm.IsHealthyOrStartingUp() {
		t.Error("after signature drift recovered, IsHealthyOrStartingUp() = false, want true (connHealth was Healthy)")
	}
}

func TestHookdeckManager_DropAccounting(t *testing.T) {
	hm, _ := newTestHookdeckManager(t)

	hm.recordDrop(events.DropSignatureInvalid)
	hm.recordDrop(events.DropSignatureInvalid)
	hm.recordDrop(events.DropDedupe)

	counts := hm.DropCounts()
	if counts[events.DropSignatureInvalid] != 2 {
		t.Errorf("DropSignatureInvalid count = %d, want 2", counts[events.DropSignatureInvalid])
	}
	if counts[events.DropDedupe] != 1 {
		t.Errorf("DropDedupe count = %d, want 1", counts[events.DropDedupe])
	}
}

func TestHookdeckManager_HandleFeedsDeltaFnAndCountsEvents(t *testing.T) {
	var gotType string
	var gotPayload []byte
	hm := newHookdeckManager(
		func(int, string, string, ...any) {},
		func(tui.Event) {},
		nil,
		func(eventType string, payload []byte) {
			gotType = eventType
			gotPayload = payload
		},
		"key", "secret",
	)

	hm.Handle(nil, events.GitHubEvent{EventType: "issues", Payload: []byte(`{"a":1}`)})

	if gotType != "issues" {
		t.Errorf("deltaFn eventType = %q, want %q", gotType, "issues")
	}
	if string(gotPayload) != `{"a":1}` {
		t.Errorf("deltaFn payload = %q, want %q", gotPayload, `{"a":1}`)
	}
	hm.mu.Lock()
	count := hm.eventCounts["issues"]
	hm.mu.Unlock()
	if count != 1 {
		t.Errorf("eventCounts[issues] = %d, want 1", count)
	}
}

func TestHookdeckManager_UpdateReposAndManagedRepos(t *testing.T) {
	hm, _ := newTestHookdeckManager(t)
	hm.UpdateRepos(map[string]bool{"b/two": true, "a/one": true})
	got := hm.ManagedRepos()
	want := []string{"a/one", "b/two"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ManagedRepos() = %v, want %v", got, want)
	}
}

func TestCheckHookdeckInstallationCoverage_NoGap(t *testing.T) {
	orig := fetchInstallationRepositoriesFn
	defer func() { fetchInstallationRepositoriesFn = orig }()
	fetchInstallationRepositoriesFn = func(baseURL, token string) ([]string, bool, error) {
		return []string{"a/one", "b/two"}, false, nil
	}

	hm, _ := newTestHookdeckManager(t)
	hm.UpdateRepos(map[string]bool{"a/one": true, "b/two": true})

	e := &Engine{hostClient: gh.NewClient("fake-installation-token")}
	e.checkHookdeckInstallationCoverage(hm)

	hm.mu.Lock()
	note := hm.installationNote
	hm.mu.Unlock()
	if note != "" {
		t.Errorf("installationNote = %q, want empty (full coverage)", note)
	}
}

func TestCheckHookdeckInstallationCoverage_SetsNoteOnGap(t *testing.T) {
	orig := fetchInstallationRepositoriesFn
	defer func() { fetchInstallationRepositoriesFn = orig }()
	fetchInstallationRepositoriesFn = func(baseURL, token string) ([]string, bool, error) {
		return []string{"a/one"}, false, nil // b/two missing from the installation's grant
	}

	hm, _ := newTestHookdeckManager(t)
	hm.UpdateRepos(map[string]bool{"a/one": true, "b/two": true})

	e := &Engine{hostClient: gh.NewClient("fake-installation-token")}
	e.checkHookdeckInstallationCoverage(hm)

	hm.mu.Lock()
	note := hm.installationNote
	hm.mu.Unlock()
	want := "installation missing: 1 repo(s) (b/two)"
	if note != want {
		t.Errorf("installationNote = %q, want %q", note, want)
	}
}

func TestCheckHookdeckInstallationCoverage_NoManagedRepos(t *testing.T) {
	var called bool
	orig := fetchInstallationRepositoriesFn
	defer func() { fetchInstallationRepositoriesFn = orig }()
	fetchInstallationRepositoriesFn = func(baseURL, token string) ([]string, bool, error) {
		called = true
		return nil, false, nil
	}

	hm, _ := newTestHookdeckManager(t)
	e := &Engine{hostClient: gh.NewClient("fake-installation-token")}
	e.checkHookdeckInstallationCoverage(hm)

	if called {
		t.Error("fetchInstallationRepositoriesFn was called with zero managed repos, want skipped")
	}
}

func TestCheckHookdeckInstallationCoverage_FetchErrorLeavesNoteUnchanged(t *testing.T) {
	orig := fetchInstallationRepositoriesFn
	defer func() { fetchInstallationRepositoriesFn = orig }()

	hm, _ := newTestHookdeckManager(t)
	hm.UpdateRepos(map[string]bool{"a/one": true})
	hm.setInstallationCoverageNote([]string{"a/one"}) // pre-existing note

	fetchInstallationRepositoriesFn = func(baseURL, token string) ([]string, bool, error) {
		return nil, false, errors.New("boom")
	}
	e := &Engine{hostClient: gh.NewClient("fake-installation-token")}
	e.checkHookdeckInstallationCoverage(hm)

	hm.mu.Lock()
	note := hm.installationNote
	hm.mu.Unlock()
	want := "installation missing: 1 repo(s) (a/one)"
	if note != want {
		t.Errorf("installationNote = %q after fetch error, want unchanged %q", note, want)
	}
}
