package engine

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/warnings"
)

// fetchAppHookConfigFn is the test seam for the App webhook-config read: the
// real call needs an App JWT and the live GitHub API.
var fetchAppHookConfigFn = func(e *Engine) (gh.AppHookConfig, error) {
	keyPEM, err := os.ReadFile(e.cfg.GitHubAppPrivateKeyPath)
	if err != nil {
		return gh.AppHookConfig{}, fmt.Errorf("reading the App private key: %w", err)
	}
	key, err := gh.ParseAppPrivateKey(keyPEM)
	if err != nil {
		return gh.AppHookConfig{}, err
	}
	jwt, err := gh.BuildAppJWT(e.cfg.GitHubAppID, key)
	if err != nil {
		return gh.AppHookConfig{}, err
	}
	return gh.FetchAppHookConfig("", jwt)
}

// panel type of the R7 check. It owns its own type (each Reconcile sweep owns
// one), separate from tailscale_coverage.
const tailscaleWebhookConfigType = "tailscale_webhook_config"

// normalizeWebhookURL makes two spellings of one URL compare equal: the host is
// case-insensitive, a default :443 and a trailing slash are insignificant. A
// value that does not parse is returned trimmed, so it compares as itself.
func normalizeWebhookURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" && p != "443" {
		host += ":" + p
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	out := strings.ToLower(u.Scheme) + "://" + host + path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// checkAppWebhookConfig is the R7 check (#2143): the App's configured webhook
// URL must be this node's Funnel URL and its content type json. A mismatch is a
// warnings-panel entry; an unreadable config is one log line, never fatal and
// never a panel entry. wantURL "" (the node name is not known) skips the check.
func (e *Engine) checkAppWebhookConfig(tag, wantURL string) {
	if wantURL == "" {
		return
	}
	cfg, err := fetchAppHookConfigFn(e)
	if err != nil {
		e.logf(0, tag, "could not read the App's webhook configuration (%v) — skipping the webhook URL check\n", err)
		return
	}
	var conds []warnings.Condition
	if normalizeWebhookURL(cfg.URL) != normalizeWebhookURL(wantURL) {
		conds = append(conds, warnings.Condition{
			Subject: "url",
			Title:   "The GitHub App's webhook URL is not this daemon's Funnel URL",
			Detail: fmt.Sprintf("The App's webhook URL is %q but this daemon receives deliveries at %q, so GitHub is not sending events here.\n\n"+
				"Fix: set the App's webhook URL to %s (App settings → General → Webhook).", cfg.URL, wantURL, wantURL),
			Fingerprint: "tailscale_webhook_config:url",
			LogLine:     fmt.Sprintf("the GitHub App's webhook URL is %q but this daemon's Funnel URL is %q — set the App's webhook URL to the latter", cfg.URL, wantURL),
		})
	}
	if ct := strings.ToLower(strings.TrimSpace(cfg.ContentType)); ct != "json" && ct != "application/json" {
		conds = append(conds, warnings.Condition{
			Subject: "content_type",
			Title:   "The GitHub App's webhook content type is not JSON",
			Detail: fmt.Sprintf("The App's webhook content type is %q; the daemon requires JSON and rejects anything else.\n\n"+
				"Fix: set the App's webhook content type to application/json (App settings → General → Webhook).", cfg.ContentType),
			Fingerprint: "tailscale_webhook_config:content_type",
			LogLine:     fmt.Sprintf("the GitHub App's webhook content type is %q — set it to application/json", cfg.ContentType),
		})
	}
	if err := warnings.Reconcile(e.warnWriter(tag), tailscaleWebhookConfigType, conds); err != nil {
		e.logf(0, tag, "recording %s warning: %v\n", tailscaleWebhookConfigType, err)
	}
}
