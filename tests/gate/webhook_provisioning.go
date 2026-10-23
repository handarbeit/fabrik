package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CheckBedWebhookProvisioning is the static, no-spend half of the webhook bed
// checks (#2408, ADR-2408), run beside bed-topology: when the plan has a webhook
// cell (#2426: an app leg is planned and E2E_EVENTS is not "poll"), at least one bed
// must have what the embedded Funnel receiver needs, so a mis-provisioned run fails
// here with a named message instead of from the engine's startup refusal (or, worse,
// from a delivery preflight an hour into the run). A bed that lacks something is
// not webhook-capable: webhook cells are never assigned to it, and a run no bed can
// serve is refused. Per bed:
//
//   - FABRIK_TAILSCALE_HOSTNAME in the bed's own .env (never shared: the launch
//     strips it from the operator's shell), distinct across beds;
//   - the webhook secret variable (FABRIK_GITHUB_WEBHOOK_SECRET, or the one
//     FABRIK_APP_WEBHOOK_SECRET_ENV names) set in the .env;
//   - a tailnet auth key variable (FABRIK_TS_AUTHKEY, or the one
//     FABRIK_TAILSCALE_AUTH_KEY_ENV names) set, OR a tailscale state file left by
//     an earlier login (the key is required only until the node has joined).
//
// Only the PRESENCE of a value is checked; no value is read into a message. A
// no-op unless a webhook cell is planned (an app leg and E2E_EVENTS not "poll").
func (g *Gate) CheckBedWebhookProvisioning(modes []string) error {
	if !g.webhookCellsPlanned(modes) {
		return nil
	}
	if g.Cfg.EventSourceAliased {
		g.outln("notice: E2E_EVENT_SOURCE=tailscale is deprecated — it is treated as E2E_EVENTS=webhook (a partial run: the poll cells are not planned). The events axis is planned per cell (ADR-2426).")
	}
	var problems []string
	hosts := map[string]*Gate{}
	capable, dup := 0, false
	for _, b := range g.beds() {
		bp := b.bedWebhookProblems()
		if len(bp) == 0 {
			capable++
		}
		problems = append(problems, bp...)
		host := strings.TrimSpace(envFileLastValue(filepath.Join(b.Cfg.TestBed, ".env"), "FABRIK_TAILSCALE_HOSTNAME"))
		if host == "" {
			continue
		}
		if o, seen := hosts[strings.ToLower(host)]; seen {
			problems = append(problems, fmt.Sprintf("%s and %s use the same tailnet hostname %q — each bed needs its own node (and its own App webhook URL)", o.bedLabel(), b.bedLabel(), host))
			dup = true // a shared node name is a misconfiguration, not a capability gap
		} else {
			hosts[strings.ToLower(host)] = b
		}
	}
	// A webhook cell runs only on a webhook-capable bed (#2426, FR-004). Beds that
	// are not capable are reported but do not stop a run another bed can serve;
	// with no capable bed the webhook cell is refused up front, never run poll-only.
	if capable > 0 && !dup {
		for _, p := range problems {
			g.outf("notice: not a webhook-capable bed, webhook cells will not be assigned to it: %s\n", p)
		}
		return nil
	}
	if len(problems) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("\n############################################################\n")
	sb.WriteString("## PRECONDITION FAILED: the plan has a webhook cell but no bed is provisioned for webhook delivery (#2408, #2426)\n")
	sb.WriteString("##\n")
	for _, p := range problems {
		fmt.Fprintf(&sb, "##   %s\n", p)
	}
	sb.WriteString("##\n")
	sb.WriteString("## See tests/e2e/README.md (\"Webhook delivery\") for the per-bed setup, or run E2E_EVENTS=poll for a partial, poll-only run.\n")
	sb.WriteString("############################################################")
	return &ExitError{Code: ExitPreconditionFailed, Msg: sb.String()}
}

// webhookCellsPlanned reports whether the plan for these auth modes includes a
// webhook cell: App auth is planned and E2E_EVENTS does not restrict to poll.
func (g *Gate) webhookCellsPlanned(modes []string) bool {
	return g.Cfg.EventsFilter != EventsPoll && authModesInclude(modes, baselineAuth)
}

// bedWebhookProblems lists what this bed lacks to receive webhooks; none means
// the bed is webhook-capable (#2426, FR-004). Presence checks only.
func (g *Gate) bedWebhookProblems() []string {
	var problems []string
	dir := g.Cfg.TestBed
	envFile := filepath.Join(dir, ".env")
	get := func(k string) string { return strings.TrimSpace(envFileLastValue(envFile, k)) }
	if get("FABRIK_TAILSCALE_HOSTNAME") == "" {
		problems = append(problems, fmt.Sprintf("%s: FABRIK_TAILSCALE_HOSTNAME is not set in %s — each bed needs its own tailnet node name", g.bedLabel(), envFile))
	}
	secretVar := orDefault(get("FABRIK_APP_WEBHOOK_SECRET_ENV"), "FABRIK_GITHUB_WEBHOOK_SECRET")
	if get(secretVar) == "" {
		problems = append(problems, fmt.Sprintf("%s: the webhook secret variable %s is not set in %s", g.bedLabel(), secretVar, envFile))
	}
	keyVar := orDefault(get("FABRIK_TAILSCALE_AUTH_KEY_ENV"), "FABRIK_TS_AUTHKEY")
	if get(keyVar) == "" && !tailscaleStateExists(dir, get("FABRIK_TAILSCALE_STATE_DIR")) {
		problems = append(problems, fmt.Sprintf("%s: no tailnet auth key (%s in %s) and no stored tailscale login under the bed — the node cannot join the tailnet", g.bedLabel(), keyVar, envFile))
	}
	return problems
}

// bedWebhookCapable reports whether this bed can run a webhook cell.
func (g *Gate) bedWebhookCapable() bool { return len(g.bedWebhookProblems()) == 0 }

// tailscaleStateExists reports whether a tailscale state file exists under the
// bed: the configured state dir (relative paths are the bed's), else the default
// .fabrik/state/tailscale. Presence only — the engine decides whether the stored
// profile is actually logged in.
func tailscaleStateExists(bedDir, configured string) bool {
	dir := configured
	if dir == "" {
		dir = filepath.Join(".fabrik", "state", "tailscale")
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(bedDir, dir)
	}
	_, err := os.Stat(filepath.Join(dir, "tailscaled.state"))
	return err == nil
}
