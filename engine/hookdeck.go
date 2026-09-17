package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/events"
	"github.com/handarbeit/fabrik/internal/events/hookdeck"
	"github.com/handarbeit/fabrik/tui"
)

// hookdeckManager is the App-auth-only Hookdeck ingestion transport (#1142),
// implementing eventIngestionManager alongside webhookManager (PAT mode) —
// see engine/ingestion.go. It wraps a *hookdeck.Source (internal/events/
// hookdeck, extracted from Pruefer's already-tested implementation, R1) and
// feeds normalized events into the exact same deltaFn closure poll.go
// already builds for the PAT-mode transport (Handle implements
// events.EventSink directly on hm — no separate sink type is needed, since
// boardcache/delta.go's seven typed handlers are the unchanged consumer for
// both transports, R2).
//
// Unlike webhookManager, hookdeckManager owns no per-repo subscription
// state: a GitHub App's webhook is one URL/secret covering every repo the
// installation is granted (#1722), so there is nothing here analogous to
// gh webhook forward's --repo flags, UpdateRepos-triggered subprocess
// restarts, or the 422 circuit-breaker (IsDisabled is always false).
// RegisterEcho/RegisterEchoIfSubscribed/MatchEcho are no-ops: the
// echo-check mechanism exists to infer "the subprocess looks alive but
// isn't delivering," a failure mode Hookdeck doesn't have since OnHealth
// reports genuine WebSocket connectivity directly (Decision #3,
// adrs/1142-hookdeck-ingestion-for-app-auth.md).
type hookdeckManager struct {
	mu sync.Mutex

	logFn   func(issueNumber int, tag, format string, args ...any)
	emitFn  func(tui.Event)
	deltaFn func(eventType string, payload []byte) // nil when board cache disabled

	source *hookdeck.Source
	cancel context.CancelFunc

	stopOnce sync.Once

	// health (protected by mu) — connHealth reflects Source's own transport
	// connectivity (Config.OnHealth); sigDriftActive reflects a sustained
	// signature-verification failure streak (Config.OnSignatureDrift, R4);
	// state is the combined, externally-reported value (sigDriftActive
	// forces Unhealthy regardless of connHealth — see recomputeHealthState).
	state          WebhookHealthState
	connHealth     WebhookHealthState
	sigDriftActive bool

	// per-event-type received counts (protected by mu), mirroring
	// webhookManager.eventCounts for TUI/log parity.
	eventCounts map[string]int

	// dropCounts accumulates R4's drop accounting (ADR-1563) by reason.
	// Only the six transport-side DropReason values are ever seen here —
	// the four review-domain reasons (DropUnwatchedOwner and friends) are
	// raised by Daemon.ReviewFromEvent, code that never moves with the
	// extraction (Decision #4).
	dropCounts map[events.DropReason]int

	// managedRepos is the current board's managed-repo set, snapshotted by
	// UpdateRepos — used only by the R5 installation-coverage check
	// (checkHookdeckInstallationCoverage), never to drive a subscription
	// mutation the way webhookManager.repos does.
	managedRepos []string

	// installationNote is the R5 App-mode coverage note (#1142) — set by
	// checkHookdeckInstallationCoverage, surfaced via the same
	// tui.WebhookStatusEvent.CoverageNote field webhookManager uses.
	installationNote string
}

// newHookdeckManager constructs a hookdeckManager and its underlying
// hookdeck.Source, wiring OnHealth/OnDrop/OnSignatureDrift to hm's own
// accounting methods — the same shape as Pruefer's execute.go wiring
// (hookdeck.SetLogf + hookdeck.NewSource(hookdeck.Config{...})), adapted to
// the engine's own logFn/emitFn injection convention instead of Pruefer's
// package-level logf/d.emit.
func newHookdeckManager(
	logFn func(issueNumber int, tag, format string, args ...any),
	emitFn func(tui.Event),
	initialRepos map[string]bool,
	deltaFn func(eventType string, payload []byte),
	apiKey, webhookSecret string,
) *hookdeckManager {
	hm := &hookdeckManager{
		logFn:        logFn,
		emitFn:       emitFn,
		deltaFn:      deltaFn,
		state:        WebhookStreamUnhealthy, // becomes StartingUp when Start launches the source
		eventCounts:  make(map[string]int),
		dropCounts:   make(map[events.DropReason]int),
		managedRepos: sortedRepoList(initialRepos),
	}
	// hookdeck.SetLogf is process-global, not per-Source (see
	// internal/events/hookdeck/log.go) — fine for a single fabrik process,
	// exactly Pruefer's own situation today.
	hookdeck.SetLogf(func(format string, args ...any) {
		logFn(0, "hookdeck", format, args...)
	})
	hm.source = hookdeck.NewSource(hookdeck.Config{
		APIKey:           apiKey,
		WebhookSecret:    webhookSecret,
		OnHealth:         hm.handleHealth,
		OnDrop:           hm.recordDrop,
		OnSignatureDrift: hm.handleSignatureDrift,
	})
	return hm
}

// sortedRepoList returns a sorted slice of repos' keys ("owner/repo").
func sortedRepoList(repos map[string]bool) []string {
	out := make([]string, 0, len(repos))
	for r := range repos {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Start launches the underlying hookdeck.Source on its own goroutine,
// mirroring webhookManager.supervise's fire-and-forget lifecycle shape:
// Source.Run retries every transient transport failure internally and only
// returns on ctx cancellation (or a genuinely fatal error, logged here).
func (hm *hookdeckManager) Start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	hm.cancel = cancel
	hm.transitionHealthState(WebhookStreamStartingUp, "")
	go func() {
		if err := hm.source.Run(runCtx, hm); err != nil {
			hm.logFn(0, "hookdeck", "event source exited: %v\n", err)
		}
	}()
}

// Stop cancels the source's context, causing its Run goroutine to return.
func (hm *hookdeckManager) Stop() {
	hm.stopOnce.Do(func() {
		if hm.cancel != nil {
			hm.cancel()
		}
	})
}

// IsHealthyOrStartingUp implements eventIngestionManager.
func (hm *hookdeckManager) IsHealthyOrStartingUp() bool {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	return hm.state == WebhookStreamStartingUp || hm.state == WebhookStreamHealthy
}

// IsDisabled implements eventIngestionManager. Always false: there is no
// per-repo circuit-breaker analogue for an App-level webhook (see the type
// doc comment above).
func (hm *hookdeckManager) IsDisabled() bool {
	return false
}

// UpdateRepos implements eventIngestionManager. Unlike webhookManager, this
// never triggers a subscription restart — it only records the current
// managed-repo set for the R5 installation-coverage check to compare
// against (checkHookdeckInstallationCoverage).
func (hm *hookdeckManager) UpdateRepos(repos map[string]bool) {
	list := sortedRepoList(repos)
	hm.mu.Lock()
	hm.managedRepos = list
	hm.mu.Unlock()
}

// ManagedRepos returns a sorted snapshot of the current managed-repo set,
// for checkHookdeckInstallationCoverage.
func (hm *hookdeckManager) ManagedRepos() []string {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	out := make([]string, len(hm.managedRepos))
	copy(out, hm.managedRepos)
	return out
}

// RegisterEcho, RegisterEchoIfSubscribed, and MatchEcho implement
// eventIngestionManager as no-ops — see the type doc comment above.
func (hm *hookdeckManager) RegisterEcho(eventType, action, key string)             {}
func (hm *hookdeckManager) RegisterEchoIfSubscribed(eventType, action, key string) {}
func (hm *hookdeckManager) MatchEcho(eventType, action, key string)                {}

// Handle implements events.EventSink, feeding normalized events into the
// exact same deltaFn closure the PAT-mode transport uses (poll.go).
func (hm *hookdeckManager) Handle(ctx context.Context, ev events.GitHubEvent) {
	hm.mu.Lock()
	hm.eventCounts[ev.EventType]++
	hm.mu.Unlock()
	hm.emitCurrentState()
	if hm.deltaFn != nil {
		hm.deltaFn(ev.EventType, ev.Payload)
	}
}

// handleHealth adapts hookdeck.Config.OnHealth to hm's combined health
// state (see recomputeHealthState).
func (hm *hookdeckManager) handleHealth(ev events.HealthEvent) {
	newConnHealth := WebhookStreamUnhealthy
	if ev.State == events.HealthConnected {
		newConnHealth = WebhookStreamHealthy
	}
	hm.mu.Lock()
	hm.connHealth = newConnHealth
	hm.mu.Unlock()
	hm.recomputeHealthState()
}

// handleSignatureDrift adapts hookdeck.Config.OnSignatureDrift (R4,
// ADR-1563) to hm's combined health state: a sustained signature-failure
// streak forces Unhealthy regardless of transport connectivity, since it
// signals a misconfigured webhook secret or a wire-format change — not a
// transient blip.
func (hm *hookdeckManager) handleSignatureDrift(active bool) {
	hm.mu.Lock()
	hm.sigDriftActive = active
	hm.mu.Unlock()
	if active {
		hm.logFn(0, "hookdeck", "signature verification drift: %d consecutive failures with no interleaved success — "+
			"possible misconfigured webhook secret or a Hookdeck wire-format change\n", hookdeck.SignatureDriftThreshold)
	} else {
		hm.logFn(0, "hookdeck", "signature verification recovered after a drift episode\n")
	}
	hm.recomputeHealthState()
}

// recomputeHealthState combines connHealth and sigDriftActive into the
// single externally-reported state and applies it via transitionHealthState.
func (hm *hookdeckManager) recomputeHealthState() {
	hm.mu.Lock()
	newState := hm.connHealth
	if newState == "" {
		newState = WebhookStreamStartingUp
	}
	reason := ""
	if hm.sigDriftActive {
		newState = WebhookStreamUnhealthy
		reason = "signature verification drift"
	}
	hm.mu.Unlock()
	hm.transitionHealthState(newState, reason)
}

// transitionHealthState updates hm.state to newState if it differs, logs
// the transition, and re-emits the current state — mirroring
// webhookManager.transitionHealthState exactly (called via reconcileLoop's
// transitionMgrHealthState type-switch, engine/reconcile.go).
func (hm *hookdeckManager) transitionHealthState(newState WebhookHealthState, reason string) {
	hm.mu.Lock()
	prev := hm.state
	if prev == newState {
		hm.mu.Unlock()
		return
	}
	hm.state = newState
	hm.mu.Unlock()
	if reason != "" {
		hm.logFn(0, "hookdeck", "health state: %s → %s (%s)\n", prev, newState, reason)
	} else {
		hm.logFn(0, "hookdeck", "health state: %s → %s\n", prev, newState)
	}
	hm.emitCurrentState()
}

// reconcileHint applies a reconcileLoop-derived health suggestion (cache
// drift found/absent) without letting it mask an active signature-drift
// episode (#1142 PR review finding): reconcileLoop's own "no drift" signal
// is a proxy for transport health, computed independently of hm's own
// connHealth/sigDriftActive state, so a direct transitionHealthState(Healthy)
// call here would silently clear the Unhealthy state
// handleSignatureDrift(true) set, masking exactly the misconfigured-secret
// condition R4 exists to escalate. A "drift found" hint is always safe to
// apply directly — it can only escalate toward Unhealthy, never mask an
// existing Unhealthy reason.
func (hm *hookdeckManager) reconcileHint(healthy bool, reason string) {
	if healthy {
		hm.recomputeHealthState()
		return
	}
	hm.transitionHealthState(WebhookStreamUnhealthy, reason)
}

// recordDrop implements hookdeck.Config.OnDrop (R4, ADR-1563): accumulates
// a cumulative per-reason count and logs it, mirroring Pruefer's
// Daemon.recordDrop but without a TUI DropEvent channel of its own — the
// engine's tui package has no such event type, so this is log-only for now.
func (hm *hookdeckManager) recordDrop(reason events.DropReason) {
	hm.mu.Lock()
	hm.dropCounts[reason]++
	total := hm.dropCounts[reason]
	hm.mu.Unlock()
	hm.logFn(0, "hookdeck", "dropped delivery: %s (cumulative: %d)\n", reason, total)
}

// DropCounts returns a locked snapshot of every recorded drop reason's
// cumulative count. Exported for tests.
func (hm *hookdeckManager) DropCounts() map[events.DropReason]int {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	out := make(map[events.DropReason]int, len(hm.dropCounts))
	for k, v := range hm.dropCounts {
		out[k] = v
	}
	return out
}

// setInstallationCoverageNote records the result of the R5
// installation-repo-coverage assertion (#1142) and re-emits the current
// state so the TUI/health surface picks up the change immediately —
// mirrors webhookManager.setHookCoverageNote exactly.
func (hm *hookdeckManager) setInstallationCoverageNote(missing []string) {
	var note string
	if len(missing) > 0 {
		note = fmt.Sprintf("installation missing: %d repo(s) (%s)", len(missing), strings.Join(missing, ", "))
	}
	hm.mu.Lock()
	changed := hm.installationNote != note
	hm.installationNote = note
	hm.mu.Unlock()
	if changed {
		hm.emitCurrentState()
	}
}

// emitCurrentState mirrors webhookManager.emitCurrentState exactly, reusing
// the same tui.WebhookStatusEvent shape (State/EventCounts/CoverageNote) so
// the TUI footer indicator works unmodified regardless of which transport
// is active.
func (hm *hookdeckManager) emitCurrentState() {
	if hm.emitFn == nil {
		return
	}
	hm.mu.Lock()
	state := string(hm.state)
	counts := make(map[string]int, len(hm.eventCounts))
	for k, v := range hm.eventCounts {
		counts[k] = v
	}
	note := hm.installationNote
	hm.mu.Unlock()
	hm.emitFn(tui.WebhookStatusEvent{
		State:        state,
		EventCounts:  counts,
		CoverageNote: &note,
	})
}

// fetchInstallationRepositoriesFn overrides gh.FetchInstallationRepositories
// in tests, mirroring webhookManager's killFn/startSubprocessFn override
// convention — the real function issues a live HTTP call keyed on a base
// URL (always "" in production; GHES is refused for App auth entirely) and
// an installation token, neither of which a test can point at an httptest
// server without this seam.
var fetchInstallationRepositoriesFn = gh.FetchInstallationRepositories

// checkHookdeckInstallationCoverage runs the R5 startup/periodic App-mode
// coverage assertion (#1142): compares hm's currently managed repos against
// the GitHub App installation's actual granted-repo set (fetched live via
// github.FetchInstallationRepositories, using the engine's own
// installation token — mirroring claudeGHTokenOverrideFn's "read the
// engine's own minted client's token live" pattern from engine/claude.go)
// and records any gap on hm for health/TUI surfacing. Always non-fatal,
// mirroring checkWebhookHookCoverage's shape exactly. e.hostClient is
// always the App-auth-backed *gh.Client here — RefuseHookdeckWithoutGitHubApp
// guarantees event_source: hookdeck never runs without App auth configured.
func (e *Engine) checkHookdeckInstallationCoverage(hm *hookdeckManager) {
	repos := hm.ManagedRepos()
	if len(repos) == 0 {
		hm.setInstallationCoverageNote(nil)
		return
	}
	if e.hostClient == nil {
		return
	}
	granted, _, err := fetchInstallationRepositoriesFn("", e.hostClient.Token())
	if err != nil {
		e.logf(0, "hookdeck", "WARNING: installation-repo-coverage check failed: %v — leaving prior coverage note in place\n", err)
		return
	}
	grantedSet := make(map[string]bool, len(granted))
	for _, r := range granted {
		grantedSet[r] = true
	}
	var missing []string
	for _, r := range repos {
		if !grantedSet[r] {
			missing = append(missing, r)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		e.logf(0, "hookdeck", "WARNING: GitHub App installation does not cover %d of %d managed repo(s): %s — "+
			"these repos are receiving no webhooks (poll-only); grant the installation access to them (App settings "+
			"→ Install App → Configure); see #1142\n", len(missing), len(repos), strings.Join(missing, ", "))
	}
	hm.setInstallationCoverageNote(missing)
}

var _ eventIngestionManager = (*hookdeckManager)(nil)
