package github

import (
	"errors"
	"fmt"
)

// webhookForwarderURL is the config.url value that gh webhook forward registers
// at GitHub. Only webhooks with this URL are treated as orphaned forwarding hooks
// eligible for cleanup.
const webhookForwarderURL = "https://webhook-forwarder.github.com/hook"

type repoHook struct {
	ID     int `json:"id"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

// DeleteForwardingHooks lists all webhooks for owner/repo and deletes any created
// by gh webhook forward (identified by config.url matching webhookForwarderURL).
// It is idempotent: if no matching hooks exist, it is a no-op. 404 on DELETE is
// treated as success (hook already gone). Non-forwarding hooks are left untouched.
func (c *Client) DeleteForwardingHooks(owner, repo string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/hooks", c.baseURL, owner, repo)
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
