package hookdeck

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/handarbeit/fabrik/pruefer/events"
)

// minPayload is the minimal set of fields read from a raw GitHub webhook
// body — enough to route the event (owner/repo/PR number/installation),
// never enough to reconstruct authoritative PR state. Mirrors
// engine/webhook.go's minWebhookPayload and boardcache/delta.go's
// typed-payload pattern, reimplemented here rather than imported (ADR-1113:
// no shared Go imports with engine).
type minPayload struct {
	Action     string `json:"action"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	PullRequest *struct {
		Number int `json:"number"`
	} `json:"pull_request"`
	Installation *struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// normalizeEvent builds an events.GitHubEvent from a raw webhook body and
// its GitHub-supplied metadata (X-GitHub-Event/X-GitHub-Delivery header
// values). Owner/Repo/ResourceID are left empty for event types that carry
// no repository or pull_request field (e.g. an installation/repo-selection
// change) — callers key off EventType for those, not Owner/Repo.
func normalizeEvent(body []byte, eventType, deliveryID string, receivedAt time.Time) (events.GitHubEvent, error) {
	var p minPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return events.GitHubEvent{}, fmt.Errorf("parsing webhook payload: %w", err)
	}

	ev := events.GitHubEvent{
		DeliveryID: deliveryID,
		EventType:  eventType,
		Owner:      p.Repository.Owner.Login,
		Repo:       p.Repository.Name,
		Action:     p.Action,
		ReceivedAt: receivedAt,
		Payload:    json.RawMessage(body),
	}
	if p.PullRequest != nil {
		ev.ResourceID = strconv.Itoa(p.PullRequest.Number)
	}
	if p.Installation != nil {
		ev.Installation = p.Installation.ID
	}
	return ev, nil
}
