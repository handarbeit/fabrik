package engine

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	"github.com/handarbeit/fabrik/warnings"
)

// applyDelivery is the intake's apply function: the board cache's delta, then
// layer1, the Layer 1 status refresh (a live GitHub round trip for an issues or
// issue_comment delivery). Once the intake is closing, layer1 is skipped (#2484,
// ADR 2484): the delta is already in the Store and the channel consumer's final
// flush announces it, while the refresh is a freshness check the next process
// repeats at its first listing. Running it made each delivery cost a network
// round trip, serially per item, inside a grace that is only seconds long, so a
// delivery queued behind a slow one was dropped unapplied by the SIGHUP exec.
// The sim's intake seam calls this same method, so production and the sim cannot
// differ in what a closing intake does.
func (e *Engine) applyDelivery(cache *boardcache.CacheImpl, eventType string, payload []byte, receivedAt time.Time, layer1 func()) {
	cache.ApplyDeltaAt(eventType, payload, receivedAt)
	if e.intake.Closing() && !e.chanTest.drainSlackOff.Load() {
		return
	}
	layer1()
}

// setUpIngestion starts every configured event-ingestion transport behind one
// transport-neutral intake (#2351, ADR 2351) and returns the cleanup the caller
// defers. The transports are independent: each configured one is started on its
// own and a failure of one leaves the others running.
//
// It builds, once, the three engine-level pieces every transport shares:
//   - the delivery ledger + intake (dedupe, per-item order, ack before apply),
//   - the echo registry, wired into the board cache so echo matching works for
//     every transport, and
//   - the reconnect callback that requests a catch-up reconcile.
//
// With no transport configured (or none started) the engine is poll-only: no
// intake, no echo registry, and the cleanup is a no-op.
func (e *Engine) setUpIngestion(ctx context.Context, cacheImpl *boardcache.CacheImpl) func() {
	wantWebhooks := e.cfg.Webhooks
	wantHookdeck := e.cfg.EventSource == EventSourceHookdeck
	wantDirect := e.cfg.EventSource == EventSourceDirect
	wantTailscale := e.cfg.EventSource == EventSourceTailscale
	if !wantWebhooks && !wantHookdeck && !wantDirect && !wantTailscale && len(e.extraTransports) == 0 {
		return func() {}
	}

	set := &transportSet{}
	e.transports = set
	e.echo = newEchoRegistry()
	e.echo.setSubscribed(set.SubscribesTo)

	var applyAt func(eventType string, payload []byte, receivedAt time.Time)
	if cacheImpl != nil {
		applyAt = func(eventType string, payload []byte, receivedAt time.Time) {
			e.applyDelivery(cacheImpl, eventType, payload, receivedAt, func() {
				e.applyLayer1StatusRefresh(eventType, payload, cacheImpl)
			})
		}
		// Inject MatchEcho so ApplyDelta can clear pending echo entries on incoming
		// webhooks — for every transport, once.
		cacheImpl.SetMatchEchoFn(e.echo.MatchEcho)
		cacheImpl.SetMatchEchoReportedFn(e.echo.MatchEchoReported)
		cacheImpl.SetMatchEchoViaFn(e.echo.MatchEchoVia)
	}
	ledger := newDeliveryLedger(deliveryLedgerPath(e.fabrikDir), e.now(), func(format string, args ...any) {
		e.logf(0, "webhook", format, args...)
	})
	in := newIntake(ledger, nil, func(format string, args ...any) {
		e.logf(0, "webhook", format, args...)
	}, e.now)
	in.applyAt = applyAt
	// Liveness counters see every arriving delivery, duplicates included.
	in.note = func() { e.health.noteWebhookEvent(e.now()) }
	e.intake = in
	e.echo.onMatch = in.stats.noteEchoMatched
	in.faults = e.faults
	e.echo.disabled = e.faults.active(FaultNoEcho)
	sink := func(d delivery) { in.Submit(d) }

	// Bootstrap the cache before accepting events so no delta is dropped into an
	// empty cache during the startup window. Probe-based bootstrap costs ~250
	// GraphQL nodes (vs ~2350 for a full shallow fetch). Terminal seeding for
	// closed cleanup-stage items happens via seedTerminalFromProbeItems after
	// bootstrap, preventing the first poll from re-deep-fetching every closed
	// Done item. Done once, before the first transport starts.
	bootstrapped := false
	bootstrap := func() {
		if bootstrapped || cacheImpl == nil {
			return
		}
		bootstrapped = true
		probeItems, projectID, probeErr := e.client.ProbeProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType)
		if probeErr != nil {
			e.logf(0, "cache", "startup probe failed — cache will be populated on first poll: %v\n", probeErr)
		} else if projectID != "" && len(probeItems) > 0 {
			cacheImpl.BootstrapFromProbe(probeItems, projectID)
			e.seedTerminalFromProbeItems(probeItems)
		} else if projectID != "" {
			e.logf(0, "cache", "startup probe returned 0 items — deferring to first poll\n")
		}
	}

	var initialRepos map[string]bool
	if e.cfg.Owner != "" && e.cfg.Repo != "" {
		initialRepos = map[string]bool{e.cfg.Owner + "/" + e.cfg.Repo: true}
	}

	if wantWebhooks {
		e.startWebhookTransport(ctx, set, sink, initialRepos, bootstrap)
	}
	if wantHookdeck {
		e.startHookdeckTransport(ctx, set, sink, initialRepos, bootstrap)
	}
	if wantDirect {
		e.startDirectTransport(ctx, set, sink, initialRepos, bootstrap)
	}
	if wantTailscale {
		e.startTailscaleTransport(ctx, set, sink, initialRepos, bootstrap)
	}
	// Transports the engine has no built-in knowledge of (a test fake today) attach through the very same wiring.
	for _, t := range e.extraTransports {
		e.attachTransport(ctx, set, t, sink, bootstrap)
	}

	if set.Empty() {
		// Nothing started: poll-only. Drop the intake and echo registry so no
		// engine write reserves an echo that nothing can ever match.
		e.echo, e.intake = nil, nil
		ledger.Close()
		return func() {}
	}
	go e.echo.run(ctx)
	return func() {
		set.StopAll()
		in.Close(intakeCloseGrace)
	}
}

// attachTransport is the one place a transport is connected to the engine: its
// deliveries go to the intake sink, its reconnects request a catch-up reconcile,
// the cache is bootstrapped, and — only once it has started — it joins the set
// that health, coverage and repo-set updates fan out to. A transport that fails
// to start is not attached and the others keep running.
func (e *Engine) attachTransport(ctx context.Context, set *transportSet, t ingestTransport, sink func(delivery), bootstrap func()) bool {
	return e.attachTransportErr(ctx, set, t, sink, bootstrap) == nil
}

// attachTransportErr is attachTransport returning the start error, for a
// transport whose failure is reported to the operator (the direct listener).
func (e *Engine) attachTransportErr(ctx context.Context, set *transportSet, t ingestTransport, sink func(delivery), bootstrap func()) error {
	t.SetSink(sink)
	// A reconnect requests a catch-up reconcile and, when delivery replay is
	// enabled (#2144), a replay pass. Trigger is nil-safe and coalescing.
	t.SetReconnectHandler(func() {
		e.requestReconcile()
		e.replayer.Trigger()
		e.liveness.Trigger()
	})
	bootstrap()
	if err := t.Start(ctx); err != nil {
		e.logf(0, t.Name(), "could not start: %v\n", err)
		return err
	}
	set.Add(t)
	return nil
}

// startWebhookTransport starts the gh webhook forward transport (PAT mode).
func (e *Engine) startWebhookTransport(ctx context.Context, set *transportSet, sink func(delivery), initialRepos map[string]bool, bootstrap func()) {
	cleanupFn := func(repos []string) error {
		var firstErr error
		for _, r := range repos {
			owner, repo := parseOwnerRepo(r)
			if owner == "" {
				e.logf(0, "webhook", "skipping malformed repo in cleanup: %q\n", r)
				continue
			}
			if err := e.client.DeleteForwardingHooks(owner, repo); err != nil {
				e.logf(0, "webhook", "orphan hook cleanup failed for %s/%s: %v\n", owner, repo, err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		return firstErr
	}
	// Seed the known-repo set so the first subprocess invocation has at least
	// one --repo arg. Multi-repo boards discover additional repos via UpdateRepos
	// after the first poll, at which point the subprocess restarts with the full set.
	wm := newWebhookManager(e.logf, e.wakeCh, e.emit, initialRepos, e.cfg.WebhookEvents, nil, cleanupFn)
	wm.SetPort(e.cfg.WebhookPort)
	wm.SetCoverageCheck(func() { e.checkWebhookHookCoverage(wm) })
	if !e.attachTransport(ctx, set, wm, sink, bootstrap) {
		return
	}
	// Echo misses drive only this transport's health (ADR 042): the "subprocess
	// looks alive but isn't delivering" failure mode is specific to gh forward.
	e.echo.setMissFn(wm.noteEchoMisses)
	// R5 startup assertion (#1142): verify a forwarding hook actually exists for
	// each managed repo, warning (never failing startup) on any gap. Best-effort
	// here — on a fresh multi-repo board, wm's repo set isn't populated until the
	// first UpdateRepos call during poll, so this startup pass only reliably
	// covers single-repo boards; the periodic reconcileLoop check re-runs it once
	// the full managed set is known and on every tick thereafter.
	e.checkWebhookHookCoverage(wm)
}

// startHookdeckTransport starts the Hookdeck transport (event_source: hookdeck,
// App auth). A missing credential falls back to polling for this transport.
func (e *Engine) startHookdeckTransport(ctx context.Context, set *transportSet, sink func(delivery), initialRepos map[string]bool, bootstrap func()) {
	// apiKeyEnvName/secretEnvName hold the *names* of the environment
	// variables, never their values: the secrets themselves (apiKey,
	// webhookSecret below) go only to newHookdeckManager and are never
	// logged. The explicit "...Name" suffix keeps that distinction plain
	// to readers, and to CodeQL, whose clear-text-logging heuristic read
	// the earlier apiKeyEnv/secretEnv spellings as sensitive values
	// flowing into the log call below - a false positive, since what is
	// logged is the variable name (e.g. "$HOOKDECK_API_KEY").
	apiKeyEnvName := e.cfg.HookdeckAPIKeyEnv
	if apiKeyEnvName == "" {
		apiKeyEnvName = DefaultHookdeckAPIKeyEnv
	}
	secretEnvName := e.cfg.HookdeckWebhookSecretEnv
	if secretEnvName == "" {
		secretEnvName = DefaultHookdeckWebhookSecretEnv
	}
	apiKey := os.Getenv(apiKeyEnvName)
	webhookSecret := os.Getenv(secretEnvName)
	if apiKey == "" || webhookSecret == "" {
		var missing []string
		if apiKey == "" {
			missing = append(missing, apiKeyEnvName)
		}
		if webhookSecret == "" {
			missing = append(missing, secretEnvName)
		}
		e.logf(0, "hookdeck", "event_source: hookdeck requires %s to be set — falling back to polling only\n",
			strings.Join(missing, " and "))
		return
	}
	hm := newHookdeckManager(e.logf, e.emit, initialRepos, nil, apiKey, webhookSecret)
	hm.SetCoverageCheck(func() { e.checkHookdeckInstallationCoverage(hm) })
	if !e.attachTransport(ctx, set, hm, sink, bootstrap) {
		return
	}
	e.logf(0, "hookdeck", "event_source: hookdeck — API key from $%s, webhook secret from $%s\n", apiKeyEnvName, secretEnvName)
	// R5 startup assertion (#1142): verify the App installation's granted-repo set
	// covers every managed repo, warning (never failing startup) on any gap — the
	// App-mode analogue of checkWebhookHookCoverage.
	e.checkHookdeckInstallationCoverage(hm)
}

// startDirectTransport starts the direct App webhook transport (event_source:
// direct, App auth; #2142). Configuration mistakes were already refused in New();
// a listen failure here (port in use, bad address) leaves the daemon poll-only
// and is reported on the warnings panel, resolved by a later successful start.
func (e *Engine) startDirectTransport(ctx context.Context, set *transportSet, sink func(delivery), initialRepos map[string]bool, bootstrap func()) {
	// secretEnvName is the variable *name*; the secret value goes only to the
	// transport and is never logged.
	secretEnvName := appWebhookSecretEnvName(e.cfg)
	listenAddr := e.cfg.DirectListen
	tr := newAppWebhookTransport("direct", appWebhookPath(e.cfg),
		func() (net.Listener, error) { return net.Listen("tcp", listenAddr) },
		os.Getenv(secretEnvName), e.logf, e.emit)
	tr.UpdateRepos(initialRepos)
	tr.SetCoverageCheck(func() {
		e.checkAppInstallationCoverage(tr, appCoverageSpec{Tag: "direct", PanelType: "direct_coverage", Ref: "#2142"})
	})
	driftKey := e.wireAppWebhookWarnings(tr, "direct", "direct App webhook", secretEnvName)

	e.replayer = e.newDeliveryReplayer(tr, "direct")
	e.liveness = e.newRouteLiveness(tr, "direct", nil, 0)

	listenKey := warnings.Condition{Type: "direct_listen", Subject: listenAddr}.Key()
	if err := e.attachTransportErr(ctx, set, tr, sink, bootstrap); err != nil {
		// Not attached (poll-only), but still visible in fabrik_health as a
		// "not listening" row, not only as the panel entry.
		set.AddFailed(tr)
		if rerr := warnings.Report(e.warnWriter("direct"), warnings.Condition{
			Type:    "direct_listen",
			Subject: listenAddr,
			Title:   fmt.Sprintf("Direct App webhook listener could not start on %s", listenAddr),
			Detail: fmt.Sprintf("event_source: direct could not listen on %s: %v\n\nThe daemon is running poll-only. "+
				"Fix: free the port or correct direct_listen (--direct-listen / FABRIK_DIRECT_LISTEN), then restart.", listenAddr, err),
			Fingerprint: "direct_listen:" + listenAddr,
			LogLine:     fmt.Sprintf("direct App webhook listener could not start on %s: %v — running poll-only", listenAddr, err),
		}); rerr != nil {
			e.logf(0, "direct", "recording listen warning: %v\n", rerr)
		}
		return
	}
	if err := warnings.Resolve(listenKey); err != nil {
		e.logf(0, "direct", "clearing listen warning: %v\n", err)
	}
	// A fresh process starts with no signature-failure streak, but the panel
	// entry is persisted: clear one left by a previous run (the in-memory
	// tracker only resolves it on an active→inactive transition).
	if err := warnings.Resolve(driftKey); err != nil {
		e.logf(0, "direct", "clearing stale signature-drift warning: %v\n", err)
	}
	e.logf(0, "direct", "event_source: direct — listening on %s%s (plain HTTP; TLS is the fronting proxy's job), secret from $%s\n",
		tr.Addr(), appWebhookPath(e.cfg), secretEnvName)
	// Startup assertion: the installation must grant every managed repo.
	e.checkAppInstallationCoverage(tr, appCoverageSpec{Tag: "direct", PanelType: "direct_coverage", Ref: "#2142"})
	// Replay failed deliveries from while the daemon was not receiving (#2144).
	// Background, after the listener is up: it never delays startup.
	e.startDeliveryReplay(ctx, e.replayer)
	// Webhook route liveness from GitHub's delivery log (#2452).
	stopLiveness := e.startRouteLiveness(ctx, e.liveness)
	tr.SetStopHook(stopLiveness)
}

// wireAppWebhookWarnings installs the signature-drift handler the two App
// webhook event sources (direct, tailscale) share and returns the panel key of
// its entry. tag is the log tag and the panel-type prefix
// (<tag>_signature_drift); desc names the receiver in the operator text.
func (e *Engine) wireAppWebhookWarnings(tr *appWebhookTransport, tag, desc, secretEnvName string) string {
	typ := tag + "_signature_drift"
	driftKey := warnings.Condition{Type: typ, Subject: tr.Name()}.Key()
	tr.SetDriftHandler(func(active bool) {
		if !active {
			if err := warnings.Resolve(driftKey); err != nil {
				e.logf(0, tag, "clearing signature-drift warning: %v\n", err)
			}
			return
		}
		if err := warnings.Report(e.warnWriter(tag), warnings.Condition{
			Type:    typ,
			Subject: tr.Name(),
			Title:   "App webhook signatures are failing verification",
			Detail: "Every recent delivery to the " + desc + " listener failed signature verification, so no events are being applied (the daemon is effectively poll-only).\n\n" +
				"Fix: make the App's webhook secret equal the value of $" + secretEnvName + ", and set the App's webhook content type to application/json.",
			Fingerprint: typ,
			LogLine:     desc + ": signature verification is failing — check that the App's webhook secret equals $" + secretEnvName,
		}); err != nil {
			e.logf(0, tag, "recording signature-drift warning: %v\n", err)
		}
	})
	return driftKey
}
