package gate

import (
	"fmt"
	"github.com/handarbeit/fabrik/tests/e2e/bedevents"
	"os"
	"path/filepath"
	"strings"
)

// CheckBedWebhookProvisioning is the static, no-spend half of the webhook bed
// checks (#2408, ADR-2408), run beside bed-topology: when the run selects the
// tailscale event source and plans an app leg, every bed must have what the
// embedded Funnel receiver needs, so a mis-provisioned bed fails here with a
// named message instead of from the engine's startup refusal (or, worse, from a
// delivery preflight an hour into the run). Per bed:
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
// no-op unless E2E_EVENT_SOURCE=tailscale and an app leg is planned.
func (g *Gate) CheckBedWebhookProvisioning(modes []string) error {
	if g.Cfg.EventSource != bedevents.Tailscale || !authModesInclude(modes, "app") {
		return nil
	}
	var problems []string
	hosts := map[string]*Gate{}
	for _, b := range g.beds() {
		dir := b.Cfg.TestBed
		envFile := filepath.Join(dir, ".env")
		get := func(k string) string { return strings.TrimSpace(envFileLastValue(envFile, k)) }
		host := get("FABRIK_TAILSCALE_HOSTNAME")
		if host == "" {
			problems = append(problems, fmt.Sprintf("%s: FABRIK_TAILSCALE_HOSTNAME is not set in %s — each bed needs its own tailnet node name", b.bedLabel(), envFile))
		} else if o, dup := hosts[strings.ToLower(host)]; dup {
			problems = append(problems, fmt.Sprintf("%s and %s use the same tailnet hostname %q — each bed needs its own node (and its own App webhook URL)", o.bedLabel(), b.bedLabel(), host))
		} else {
			hosts[strings.ToLower(host)] = b
		}
		secretVar := orDefault(get("FABRIK_APP_WEBHOOK_SECRET_ENV"), "FABRIK_GITHUB_WEBHOOK_SECRET")
		if get(secretVar) == "" {
			problems = append(problems, fmt.Sprintf("%s: the webhook secret variable %s is not set in %s", b.bedLabel(), secretVar, envFile))
		}
		keyVar := orDefault(get("FABRIK_TAILSCALE_AUTH_KEY_ENV"), "FABRIK_TS_AUTHKEY")
		if get(keyVar) == "" && !tailscaleStateExists(dir, get("FABRIK_TAILSCALE_STATE_DIR")) {
			problems = append(problems, fmt.Sprintf("%s: no tailnet auth key (%s in %s) and no stored tailscale login under the bed — the node cannot join the tailnet", b.bedLabel(), keyVar, envFile))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("\n############################################################\n")
	sb.WriteString("## PRECONDITION FAILED: E2E_EVENT_SOURCE=tailscale but a bed is not provisioned for webhook delivery (#2408)\n")
	sb.WriteString("##\n")
	for _, p := range problems {
		fmt.Fprintf(&sb, "##   %s\n", p)
	}
	sb.WriteString("##\n")
	sb.WriteString("## See tests/e2e/README.md (\"Webhook delivery\") for the per-bed setup.\n")
	sb.WriteString("############################################################")
	return &ExitError{Code: ExitPreconditionFailed, Msg: sb.String()}
}

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
