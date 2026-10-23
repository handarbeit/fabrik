package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// startTailscaleEngineReplay is startTailscaleEngine with the delivery replay
// switch (#2144) and an injected delivery-log API.
func startTailscaleEngineReplay(t *testing.T, enabled bool, api hookDeliveryAPI) (*Engine, func()) {
	t.Helper()
	isolateWarnings(t)
	oldPace := replayPace
	replayPace = 0
	t.Cleanup(func() { replayPace = oldPace })
	t.Setenv(DefaultAppWebhookSecretEnv, directTestSecret)
	t.Setenv(DefaultTailscaleAuthKeyEnv, tailscaleTestKey)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.fabrikDir = t.TempDir()
	eng.cfg.EventSource = EventSourceTailscale
	eng.cfg.TailscaleHostname = "fabrik-models"
	eng.cfg.AppWebhookPath = directTestPath
	eng.cfg.ReplayFailedDeliveries = enabled
	if api != nil {
		eng.SetReplayAPIForTest(api)
	}
	orig := fetchAppHookConfigFn
	fetchAppHookConfigFn = func(*Engine) (gh.AppHookConfig, error) { return gh.AppHookConfig{}, errors.New("no app") }
	t.Cleanup(func() { fetchAppHookConfigFn = orig })
	cache, _ := eng.UseBoardCacheForTest()
	return eng, eng.setUpIngestion(context.Background(), cache)
}

// #2144 for the tailscale transport: with replay enabled, a pass runs at start
// (after the node and Funnel listener are up) and again on a tailnet recovery,
// never on the drop itself or on the first connect's baseline, and the
// transport's health row reports the last pass.
func TestTailscaleReplay_RunsAtStartAndOnRecovery(t *testing.T) {
	nodes := installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	fake := &fakeHookAPI{}
	eng, cleanup := startTailscaleEngineReplay(t, true, fake)
	defer cleanup()
	tr := tailscaleFromEngine(t, eng)
	n := (*nodes)[0]

	if eng.replayer == nil {
		t.Fatal("no replayer for a tailscale transport with replay enabled")
	}
	replayWaitFor(t, "startup pass", func() bool { return fake.listCount() >= 1 })
	replayWaitFor(t, "startup pass summary", func() bool {
		r := tr.HealthRow().Replay
		return r != nil && r.LastPassAt.Valid
	})
	if r := tr.HealthRow().Replay; r.Outcome != "ok" {
		t.Errorf("health replay = %+v", r)
	}
	if got := fake.listCount(); got != 1 {
		t.Fatalf("passes after start = %d, want exactly the startup pass", got)
	}

	// A drop alone triggers nothing; the recovery does.
	n.emit(tailnetState{Connected: false, BackendState: "Stopped", Funnel: tailnetFunnelUnknown})
	if got := fake.listCount(); got != 1 {
		t.Fatalf("a tailnet drop ran a replay pass (%d)", got)
	}
	n.emit(tailnetState{Connected: true, BackendState: "Running", Funnel: tailnetFunnelOK})
	replayWaitFor(t, "recovery pass", func() bool { return fake.listCount() >= 2 })
}

// A Funnel that comes back on a connected node is a recovery too (R6).
func TestTailscaleReplay_FunnelRecoveryTriggersAPass(t *testing.T) {
	nodes := installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	fake := &fakeHookAPI{}
	eng, cleanup := startTailscaleEngineReplay(t, true, fake)
	defer cleanup()
	_ = tailscaleFromEngine(t, eng)
	n := (*nodes)[0]
	replayWaitFor(t, "startup pass", func() bool { return fake.listCount() >= 1 })

	n.emit(tailnetState{Connected: true, BackendState: "Running", Funnel: tailnetFunnelUnavailable})
	n.emit(tailnetState{Connected: true, BackendState: "Running", Funnel: tailnetFunnelOK})
	replayWaitFor(t, "funnel recovery pass", func() bool { return fake.listCount() >= 2 })
}

// With replay off the deliveries API is never called — at start or on a
// recovery — and the health row carries no replay object.
func TestTailscaleReplay_DisabledMakesNoAPICalls(t *testing.T) {
	nodes := installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	fake := &fakeHookAPI{}
	eng, cleanup := startTailscaleEngineReplay(t, false, fake)
	defer cleanup()
	tr := tailscaleFromEngine(t, eng)
	n := (*nodes)[0]
	if eng.replayer != nil {
		t.Fatal("replayer must not exist when replay is disabled")
	}
	n.emit(tailnetState{Connected: false, BackendState: "Stopped"})
	n.emit(tailnetState{Connected: true, BackendState: "Running", Funnel: tailnetFunnelOK})
	time.Sleep(50 * time.Millisecond)
	if fake.listCount() != 0 || len(fake.redeliveredIDs()) != 0 {
		t.Fatalf("API called with replay disabled: list=%d redeliver=%v", fake.listCount(), fake.redeliveredIDs())
	}
	if tr.HealthRow().Replay != nil {
		t.Fatal("health row must carry no replay object when disabled")
	}
}

// A node that fails to start leaves the daemon poll-only: no replay pass runs.
func TestTailscaleReplay_FailedStartRunsNoPass(t *testing.T) {
	installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet {
		f := newFakeTailnet(cfg, "n.t.ts.net")
		f.upErr = errors.New("control plane unreachable")
		return f
	})
	fake := &fakeHookAPI{}
	_, cleanup := startTailscaleEngineReplay(t, true, fake)
	defer cleanup()
	time.Sleep(50 * time.Millisecond)
	if fake.listCount() != 0 {
		t.Fatalf("a replay pass ran although the node never started (%d)", fake.listCount())
	}
}
