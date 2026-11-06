package appwebhook

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/warnings"
)

// HookChecker is the R7 webhook-config check (#2143): the App's configured
// webhook URL must be this node's Funnel URL and its content type json. Its zero
// value is ready to use; it carries only the log throttle, so one checker per
// consumer keeps a persistent failure from repeating its line.
type HookChecker struct {
	logState hookCheckLogState
}

// HookCheckInput is one check: the consumer supplies the config read, the panel
// and the log function, so the package reads no consumer configuration.
type HookCheckInput struct {
	// Tag is the log tag ("tailscale").
	Tag string
	// WantURL is the URL this node serves; "" (name not known) skips the check.
	WantURL string
	// Fetch reads GET /app/hook/config (App JWT; the secret is never decoded).
	Fetch func(ctx context.Context) (gh.AppHookConfig, error)
	Panel Panel
	Logf  LogFunc
}

// WebhookConfigType is the panel type of the R7 check. It owns its own type (each Reconcile sweep owns
// one), separate from tailscale_coverage.
const WebhookConfigType = "tailscale_webhook_config"

// NormalizeWebhookURL makes two spellings of one URL compare equal: the host is
// case-insensitive, a default :443 and a trailing slash are insignificant. A
// value that does not parse is returned trimmed, so it compares as itself.
func NormalizeWebhookURL(raw string) string {
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

// hookCheckLogState throttles the "could not read" line of the R7 check, which
// runs at startup and on every reconcile tick: a persistent failure (a rotated
// key path, a JWT or clock problem, a standing 401/403) must not repeat the line
// for the life of the process. It logs the first failure of a streak and again
// whenever the error text changes; a successful read ends the streak.
type hookCheckLogState struct {
	mu      sync.Mutex
	lastErr string
}

// failed reports whether this failure should be logged.
func (s *hookCheckLogState) failed(err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg := err.Error(); msg != s.lastErr {
		s.lastErr = msg
		return true
	}
	return false
}

// succeeded ends a failure streak, so the next failure is logged again.
func (s *hookCheckLogState) succeeded() {
	s.mu.Lock()
	s.lastErr = ""
	s.mu.Unlock()
}

// Check runs the check. A mismatch is a warnings-panel entry; an unreadable
// config is one log line per failure streak (hookCheckLogState), never fatal and
// never a panel entry.
func (c *HookChecker) Check(in HookCheckInput) {
	if in.WantURL == "" {
		return
	}
	wantURL := in.WantURL
	cfg, err := in.Fetch(context.Background())
	if err != nil {
		if c.logState.failed(err) {
			in.Logf(0, in.Tag, "could not read the App's webhook configuration (%v) — skipping the webhook URL check (repeats of the same error are not logged)\n", err)
		}
		return
	}
	c.logState.succeeded()
	var conds []warnings.Condition
	if NormalizeWebhookURL(cfg.URL) != NormalizeWebhookURL(wantURL) {
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
	in.Panel.Reconcile(WebhookConfigType, conds)
}
