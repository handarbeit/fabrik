package github

import (
	"errors"
	"fmt"
)

// webhookForwarderURL is the config.url that gh webhook forward registers at GitHub.
const webhookForwarderURL = "https://webhook-forwarder.github.com/hook"

type repoHook struct {
	ID     int `json:"id"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

// DeleteForwardingHooks deletes repo hooks matching webhookForwarderURL; 404
// on DELETE is success. It exists solely to tidy up the per-repo hook `gh
// webhook forward` leaves behind, so its only call site
// (engine/poll.go's cleanupFn, gated on e.cfg.Webhooks, independent of auth
// mode) only ever runs when --webhooks is on.
//
// As of #1752, engine.RefuseWebhooksWithGitHubApp refuses --webhooks
// together with GitHub App auth at startup (resolveGitHubAppAuth), because
// gh webhook forward itself is feature-gated to user tokens and cannot work
// under an installation token — see that function's doc comment. That
// refusal means this call site is structurally unreachable under App auth:
// the engine can never start in a --webhooks + App-auth configuration, so
// this function only ever runs against a PAT-authenticated client in
// practice. It remains correct and reachable under PAT mode, which is the
// only mode it now runs in.
func (c *Client) DeleteForwardingHooks(owner, repo string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/hooks?per_page=100", c.baseURL, owner, repo)
	var hooks []repoHook
	if err := c.restGetJSON(url, &hooks); err != nil {
		return fmt.Errorf("listing hooks for %s/%s: %w", owner, repo, err)
	}

	for _, h := range hooks {
		if h.Config.URL != webhookForwarderURL {
			continue
		}
		delURL := fmt.Sprintf("%s/repos/%s/%s/hooks/%d", c.baseURL, owner, repo, h.ID)
		if err := c.restDelete(delURL); err != nil && !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("deleting forwarding hook %d for %s/%s: %w", h.ID, owner, repo, err)
		}
	}
	return nil
}

// HasForwardingHook reports whether owner/repo currently has a `gh webhook
// forward` hook registered (config.url == webhookForwarderURL). Used by the
// engine's R5 startup/periodic coverage assertion (#1142) to detect the
// singular-`--repo`-flag failure mode: a managed repo silently receiving no
// webhooks while the stream is otherwise reported healthy. A 404 listing
// hooks (e.g. insufficient permission) is treated as "no hook found" rather
// than an error, mirroring DeleteForwardingHooks's own 404-is-success
// posture — this check is advisory, never fatal. Paginated via the same
// paginateREST helper FetchComments/etc. use (PR review finding: an
// unpaginated single-page listing could silently miss a forwarding hook
// past the first 100 on a repo with unusually many hooks registered).
func (c *Client) HasForwardingHook(owner, repo string) (bool, error) {
	hooks, err := paginateREST[repoHook](c, fmt.Sprintf("hooks for %s/%s", owner, repo), func(page int) string {
		return fmt.Sprintf("%s/repos/%s/%s/hooks?per_page=%d&page=%d", c.baseURL, owner, repo, restPageSize, page)
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("listing hooks for %s/%s: %w", owner, repo, err)
	}
	for _, h := range hooks {
		if h.Config.URL == webhookForwarderURL {
			return true, nil
		}
	}
	return false, nil
}
