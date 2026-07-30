package hookdeck

import (
	"testing"
	"time"
)

func TestNormalizeEvent_PullRequestOpened(t *testing.T) {
	body := []byte(`{
		"action": "opened",
		"number": 42,
		"pull_request": {"number": 42},
		"repository": {"name": "fabrik", "owner": {"login": "handarbeit"}},
		"installation": {"id": 999}
	}`)
	at := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)

	ev, err := normalizeEvent(body, "pull_request", "delivery-1", at)
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if ev.DeliveryID != "delivery-1" {
		t.Errorf("DeliveryID = %q, want 'delivery-1'", ev.DeliveryID)
	}
	if ev.EventType != "pull_request" {
		t.Errorf("EventType = %q, want 'pull_request'", ev.EventType)
	}
	if ev.Action != "opened" {
		t.Errorf("Action = %q, want 'opened'", ev.Action)
	}
	if ev.Owner != "handarbeit" || ev.Repo != "fabrik" {
		t.Errorf("Owner/Repo = %q/%q, want handarbeit/fabrik", ev.Owner, ev.Repo)
	}
	if ev.ResourceID != "42" {
		t.Errorf("ResourceID = %q, want '42'", ev.ResourceID)
	}
	if ev.Installation != 999 {
		t.Errorf("Installation = %d, want 999", ev.Installation)
	}
	if !ev.ReceivedAt.Equal(at) {
		t.Errorf("ReceivedAt = %v, want %v", ev.ReceivedAt, at)
	}
	if len(ev.Payload) == 0 {
		t.Error("expected Payload to carry the raw body")
	}
}

func TestNormalizeEvent_PullRequestSynchronize(t *testing.T) {
	body := []byte(`{"action":"synchronize","pull_request":{"number":7},"repository":{"name":"fabrik","owner":{"login":"handarbeit"}}}`)
	ev, err := normalizeEvent(body, "pull_request", "delivery-2", time.Now())
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if ev.Action != "synchronize" || ev.ResourceID != "7" {
		t.Errorf("got Action=%q ResourceID=%q, want synchronize/7", ev.Action, ev.ResourceID)
	}
}

func TestNormalizeEvent_PullRequestReadyForReview(t *testing.T) {
	body := []byte(`{"action":"ready_for_review","pull_request":{"number":9},"repository":{"name":"fabrik","owner":{"login":"handarbeit"}}}`)
	ev, err := normalizeEvent(body, "pull_request", "delivery-3", time.Now())
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if ev.Action != "ready_for_review" || ev.ResourceID != "9" {
		t.Errorf("got Action=%q ResourceID=%q, want ready_for_review/9", ev.Action, ev.ResourceID)
	}
}

func TestNormalizeEvent_InstallationRepositoriesChange(t *testing.T) {
	// installation_repositories events have no top-level "repository" or
	// "pull_request" field — only installation + action.
	body := []byte(`{"action":"added","installation":{"id":555}}`)
	ev, err := normalizeEvent(body, "installation_repositories", "delivery-4", time.Now())
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if ev.EventType != "installation_repositories" || ev.Action != "added" {
		t.Errorf("got EventType=%q Action=%q, want installation_repositories/added", ev.EventType, ev.Action)
	}
	if ev.Installation != 555 {
		t.Errorf("Installation = %d, want 555", ev.Installation)
	}
	if ev.Owner != "" || ev.Repo != "" || ev.ResourceID != "" {
		t.Errorf("expected empty Owner/Repo/ResourceID for an installation-scoped event, got %q/%q/%q", ev.Owner, ev.Repo, ev.ResourceID)
	}
}

func TestNormalizeEvent_MalformedBody(t *testing.T) {
	_, err := normalizeEvent([]byte(`not json`), "pull_request", "delivery-5", time.Now())
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestNormalizeEvent_UnrecognizedEventType(t *testing.T) {
	// An event type Pruefer doesn't act on (e.g. "star") should still
	// normalize without error — the sink decides what to do with it.
	body := []byte(`{"action":"created"}`)
	ev, err := normalizeEvent(body, "star", "delivery-6", time.Now())
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if ev.EventType != "star" {
		t.Errorf("EventType = %q, want 'star'", ev.EventType)
	}
}
