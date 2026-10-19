package engine

import (
	"context"
	"os"
	"strings"

	"github.com/handarbeit/fabrik/boardcache"
)

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
	if !wantWebhooks && !wantHookdeck {
		return func() {}
	}

	set := &transportSet{}
	e.transports = set
	e.echo = newEchoRegistry()
	e.echo.setSubscribed(set.SubscribesTo)

	var apply func(eventType string, payload []byte)
	if cacheImpl != nil {
		apply = func(eventType string, payload []byte) {
			cacheImpl.ApplyDelta(eventType, payload)
			e.applyLayer1StatusRefresh(eventType, payload, cacheImpl)
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
	in := newIntake(ledger, apply, func(format string, args ...any) {
		e.logf(0, "webhook", format, args...)
	}, e.now)
	// Liveness counters see every arriving delivery, duplicates included.
	in.note = func() { e.health.noteWebhookEvent(e.now()) }
	e.intake = in
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
	wm.SetSink(sink)
	wm.SetReconnectHandler(e.requestReconcile)
	wm.SetCoverageCheck(func() { e.checkWebhookHookCoverage(wm) })
	bootstrap()
	if err := wm.Start(ctx); err != nil {
		return
	}
	set.Add(wm)
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
	hm.SetSink(sink)
	hm.SetReconnectHandler(e.requestReconcile)
	hm.SetCoverageCheck(func() { e.checkHookdeckInstallationCoverage(hm) })
	bootstrap()
	if err := hm.Start(ctx); err != nil {
		e.logf(0, "hookdeck", "could not start: %v\n", err)
		return
	}
	set.Add(hm)
	e.logf(0, "hookdeck", "event_source: hookdeck — API key from $%s, webhook secret from $%s\n", apiKeyEnvName, secretEnvName)
	// R5 startup assertion (#1142): verify the App installation's granted-repo set
	// covers every managed repo, warning (never failing startup) on any gap — the
	// App-mode analogue of checkWebhookHookCoverage.
	e.checkHookdeckInstallationCoverage(hm)
}
