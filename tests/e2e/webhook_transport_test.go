//go:build e2e

package e2e

import (
	"time"

	"testing"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// tailscaleHookWarning is the fingerprint of the warnings-panel entry the daemon
// raises when the App's webhook URL is not its own Funnel URL (#2143, R7).
const tailscaleHookWarning = "tailscale_webhook_config:url"

// brokenHookURL can never reach the bed: .invalid is reserved and never resolves.
const brokenHookURL = "https://fabrik-e2e-broken-route.invalid/webhook"

// transportHealthy is the definition of "healthy" this test holds the daemon to
// (#2143, #2425): the transport is listening, the Funnel name resolves publicly and a
// delivery has been received.
func transportHealthy(tr *localapi.TransportHealth) bool {
	return tr != nil && tr.State == "listening" &&
		tr.Funnel.Valid && tr.Funnel.V == "ok" &&
		tr.LastDeliveryAt.Valid
}

// falseHealthy reports a health reading that claims the webhook feed is healthy
// while the transport's own row says it is not.
func falseHealthy(h *localapi.HealthResult) bool {
	if h.Webhook.Mode != "healthy" || len(h.Webhook.Transports) == 0 {
		return false
	}
	tr := h.Webhook.Transports[0]
	return tr.State != "listening" || (tr.Funnel.Valid && tr.Funnel.V != "ok")
}

// TestWebhookTransportHealthAndBrokenRoute (FR-010): fabrik_health reports the
// tailscale transport healthy only once the Funnel name resolves publicly and a
// delivery has been received. With the App's hook URL deliberately pointed elsewhere
// (restored in cleanup, with a durable marker so a killed run is repaired by the next
// webhook test) the daemon shows the tailscale_webhook_config warning, receives no
// further delivery, and never reports a false healthy; after the restore the warning
// clears and deliveries resume. The warning has no local API surface, so that half
// reads the bed's warnings panel file (a documented exception, ADR 2427-a).
func TestWebhookTransportHealthAndBrokenRoute(t *testing.T) {
	env := LoadEnv(t)
	webhookLegSetup(t, env)
	repo := env.RepoAlpha
	issue, _ := fileInertItem(t, env, repo, "e2e webhook transport health",
		"e2e scenario for #2427 FR-010: transport health with a healthy and a broken route. Paused so no stage runs.")
	probe := func(label string) {
		t.Helper()
		ensureLabel(t, env, repo, label)
		AddLabel(t, env, repo, issue, label)
	}

	// Baseline: healthy only with the Funnel name resolving and a delivery received.
	probe("e2e-webhook-health-1")
	var tr *localapi.TransportHealth
	if !awaitCond(12*time.Minute, func() bool {
		row, err := healthTransport(env)
		tr = row
		return err == nil && transportHealthy(row)
	}) {
		webhookEffectMissing(t, env, "the tailscale transport did not report healthy (listening, Funnel ok, a delivery received) within 12m on a leg whose delivery preflight passed: %+v", tr)
	}
	if h, err := bedHealth(env); err == nil && falseHealthy(h) {
		t.Errorf("fabrik_health reports webhook mode healthy while the transport row is not: %+v", h.Webhook)
	}
	if bedWarning(env, tailscaleHookWarning) {
		t.Fatalf("the %s warning is present before the route was broken", tailscaleHookWarning)
	}
	lastDelivery := tr.LastDeliveryAt.V

	// Break the route; the warning appears on the next reconcile tick and no delivery arrives.
	original := breakHookURL(t, env, brokenHookURL)
	t.Logf("App webhook URL pointed at %s (was %s)", brokenHookURL, original)
	if !awaitCond(10*time.Minute, func() bool { return bedWarning(env, tailscaleHookWarning) }) {
		t.Fatalf("with the App's webhook URL pointed elsewhere the daemon never raised the %s warning within 10m", tailscaleHookWarning)
	}
	probe("e2e-webhook-health-2")
	time.Sleep(60 * time.Second)
	h, err := bedHealth(env)
	if err != nil {
		t.Fatalf("reading fabrik_health with the route broken: %v", err)
	}
	if falseHealthy(h) {
		t.Errorf("fabrik_health reports a false healthy with the route broken: %+v", h.Webhook)
	}
	if len(h.Webhook.Transports) > 0 {
		row := h.Webhook.Transports[0]
		if row.LastDeliveryAt.Valid && row.LastDeliveryAt.V.After(lastDelivery) {
			t.Errorf("a delivery arrived (%v, after %v) while the App's webhook URL pointed elsewhere", row.LastDeliveryAt.V, lastDelivery)
		}
	}

	// Restore: the warning clears and deliveries resume.
	if err := restoreHookURLFromMarker(env); err != nil {
		t.Fatalf("restoring the App's webhook URL: %v", err)
	}
	if !awaitCond(10*time.Minute, func() bool { return !bedWarning(env, tailscaleHookWarning) }) {
		t.Errorf("the %s warning did not clear within 10m of restoring the App's webhook URL", tailscaleHookWarning)
	}
	probe("e2e-webhook-health-3")
	if !awaitCond(5*time.Minute, func() bool {
		row, err := healthTransport(env)
		return err == nil && row.LastDeliveryAt.Valid && row.LastDeliveryAt.V.After(lastDelivery)
	}) {
		webhookEffectMissing(t, env, "no delivery arrived within 5m of restoring the App's webhook URL")
	}
}
