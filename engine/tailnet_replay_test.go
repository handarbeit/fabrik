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
	// The startup grace passes at once; the follow-up delay never fires.
	return startTailscaleEngineReplayWait(t, enabled, api, func(ctx context.Context, d time.Duration) bool {
		if d == tailnetReplayStartGrace {
			return true
		}
		<-ctx.Done()
		return false
	})
}

// startTailscaleEngineReplayWait is startTailscaleEngineReplay with the
// replayer's sleep (start grace, follow-up delay) under the test's control.
func startTailscaleEngineReplayWait(t *testing.T, enabled bool, api hookDeliveryAPI, wait func(context.Context, time.Duration) bool) (*Engine, func()) {
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
	eng.SetReplayWaitForTest(wait)
	if api != nil {
		eng.SetReplayAPIForTest(api)
	}
	orig := fetchAppHookConfigFn
	fetchAppHookConfigFn = func(context.Context, *Engine) (gh.AppHookConfig, error) {
		return gh.AppHookConfig{}, errors.New("no app")
	}
	t.Cleanup(func() { fetchAppHookConfigFn = orig })
	cache, _ := eng.UseBoardCacheForTest()
	cleanup := startIngestion(t, eng, cache)
	return eng, cleanup
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

// The startup pass of a tailscale daemon waits tailnetReplayStartGrace after the
// node and its listener are up (Funnel ingress needs a while to route to a new
// node; a redelivery requested earlier fails), and shutdown cancels the wait.
func TestTailscaleReplay_StartupPassWaitsForTheGrace(t *testing.T) {
	installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	fake := &fakeHookAPI{}
	release := make(chan struct{})
	waits := make(chan time.Duration, 4)
	_, cleanup := startTailscaleEngineReplayWait(t, true, fake, func(ctx context.Context, d time.Duration) bool {
		waits <- d
		select {
		case <-release:
			return true
		case <-ctx.Done():
			return false
		}
	})
	defer cleanup()
	select {
	case d := <-waits:
		if d != tailnetReplayStartGrace || tailnetReplayStartGrace != 90*time.Second {
			t.Fatalf("grace = %s (constant %s), want 90s", d, tailnetReplayStartGrace)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the startup pass did not wait on the grace")
	}
	time.Sleep(50 * time.Millisecond)
	if fake.listCount() != 0 {
		t.Fatalf("a pass ran before the grace elapsed (%d)", fake.listCount())
	}
	close(release)
	replayWaitFor(t, "startup pass after the grace", func() bool { return fake.listCount() >= 1 })
}

func TestTailscaleReplay_ShutdownCancelsTheGrace(t *testing.T) {
	installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	fake := &fakeHookAPI{}
	ctx, cancel := context.WithCancel(context.Background())
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	entered := make(chan struct{})
	e.SetReplayWaitForTest(func(c context.Context, d time.Duration) bool {
		close(entered)
		<-c.Done()
		return false
	})
	r := newDeliveryReplayer(fake, &fakeReplayLedger{seen: map[string]bool{}}, &recordingStatus{}, nil, time.Now)
	r.wait = e.replayWaitOverride
	trackReplayer(t, r)
	t.Cleanup(cancel)
	e.startDeliveryReplayAfter(ctx, r, tailnetReplayStartGrace)
	<-entered
	cancel()
	if !r.waitStopped(replayStopWait) {
		t.Fatal("replayer goroutines did not exit after cancel")
	}
	if fake.listCount() != 0 {
		t.Fatalf("a cancelled grace still ran a pass (%d)", fake.listCount())
	}
}
