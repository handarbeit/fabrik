package pruefer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/appwebhook"
	"github.com/handarbeit/fabrik/internal/events"
	"github.com/handarbeit/fabrik/internal/githubauth"
	ptui "github.com/handarbeit/fabrik/pruefer/tui"
)

// sinkHarness is a real Daemon with a recording fakeLister, behind a real
// appSink and the real daemonEventSink.
type sinkHarness struct {
	client *fakeLister
	d      *Daemon
	sink   *appSink
	dedupe *events.Dedupe

	mu    sync.Mutex
	drops map[events.DropReason]int
}

func newSinkHarness(t *testing.T, installations ...int64) *sinkHarness {
	t.Helper()
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"}}
	d := newTestDaemonForEvents(client)
	for _, id := range installations {
		d.derived.Installations = append(d.derived.Installations, githubauth.DerivedInstallation{InstallationID: id, Account: "owner"})
	}
	h := &sinkHarness{client: client, d: d, dedupe: events.NewDedupe(0), drops: map[events.DropReason]int{}}
	d.Emit = func(ev ptui.Event) {
		if de, ok := ev.(ptui.DropEvent); ok {
			h.mu.Lock()
			h.drops[events.DropReason(de.Reason)] = de.Total
			h.mu.Unlock()
		}
	}
	h.sink = newAppSink(context.Background(), d, &daemonEventSink{daemon: d}, h.dedupe)
	return h
}

func (h *sinkHarness) dropCount(r events.DropReason) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.drops[r]
}

func prDelivery(id, action string, installation int64) appwebhook.Delivery {
	inst := ""
	if installation != 0 {
		inst = fmt.Sprintf(`,"installation":{"id":%d}`, installation)
	}
	return appwebhook.Delivery{
		ID: id, EventType: "pull_request", Transport: "direct",
		Payload: []byte(fmt.Sprintf(`{"action":%q,"pull_request":{"number":1},"repository":{"name":"repo","owner":{"login":"owner"}}%s}`, action, inst)),
	}
}

func (h *sinkHarness) expectNoReview(t *testing.T) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	if got := h.client.submitCallCount(); got != 0 {
		t.Errorf("submitCallCount() = %d, want 0", got)
	}
}

func TestAppSink_OwnInstallationDispatchesOneReview(t *testing.T) {
	h := newSinkHarness(t, 111)
	h.sink.Deliver(prDelivery("d1", "opened", 111))
	waitUntil(t, 2*time.Second, func() bool { return h.client.submitCallCount() == 1 })
}

func TestAppSink_DuplicateDeliveryIDDispatchesOnce(t *testing.T) {
	h := newSinkHarness(t, 111)
	h.sink.Deliver(prDelivery("dup", "opened", 111))
	waitUntil(t, 2*time.Second, func() bool { return h.client.submitCallCount() == 1 })
	h.sink.Deliver(prDelivery("dup", "opened", 111))
	time.Sleep(100 * time.Millisecond)
	if got := h.client.submitCallCount(); got != 1 {
		t.Errorf("submitCallCount() = %d, want 1", got)
	}
	if h.dropCount(events.DropDedupe) != 1 {
		t.Errorf("dedupe drops = %d, want 1", h.dropCount(events.DropDedupe))
	}
}

func TestAppSink_ForeignInstallationIgnored(t *testing.T) {
	h := newSinkHarness(t, 111)
	h.sink.Deliver(prDelivery("foreign", "opened", 222))
	h.expectNoReview(t)
	if h.dropCount(events.DropForeignInstallation) != 1 {
		t.Errorf("foreign drops = %d, want 1", h.dropCount(events.DropForeignInstallation))
	}
	// A foreign delivery is not recorded as seen: the ledger the replayer reads
	// stays free of it, and the same GUID is judged on its merits next time.
	if h.dedupe.Seen("foreign") {
		t.Error("a foreign delivery was recorded in the dedupe")
	}
}

func TestAppSink_ForeignInstallationLogsExactlyOneLine(t *testing.T) {
	h := newSinkHarness(t, 111)
	var mu sync.Mutex
	var lines []string
	prev := Logf
	Logf = func(_ int, _ string, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { Logf = prev })

	h.sink.Deliver(prDelivery("foreign-log", "opened", 222))
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "foreign-log") {
			n++
			if !strings.Contains(l, "222") {
				t.Errorf("line does not name the foreign installation: %q", l)
			}
		}
	}
	if n != 1 {
		t.Errorf("lines naming the delivery = %d, want exactly 1 (%q)", n, lines)
	}
}

func TestAppSink_DeliveryWithoutInstallationIDIgnoredOnceOwnershipKnown(t *testing.T) {
	h := newSinkHarness(t, 111)
	h.sink.Deliver(prDelivery("noinst", "opened", 0))
	h.expectNoReview(t)
	if h.dropCount(events.DropForeignInstallation) != 1 {
		t.Errorf("foreign drops = %d, want 1", h.dropCount(events.DropForeignInstallation))
	}
}

func TestAppSink_MissingInstallationIDLogLineDoesNotBlameAForeignApp(t *testing.T) {
	h := newSinkHarness(t, 111)
	var mu sync.Mutex
	var lines []string
	prev := Logf
	Logf = func(_ int, _ string, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { Logf = prev })

	h.sink.Deliver(prDelivery("noinst-log", "opened", 0))
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "noinst-log") {
			n++
			if !strings.Contains(l, "no installation.id") || strings.Contains(l, "another GitHub App") {
				t.Errorf("line should say the delivery carries no installation.id, not blame a foreign App: %q", l)
			}
		}
	}
	if n != 1 {
		t.Errorf("lines naming the delivery = %d, want exactly 1 (%q)", n, lines)
	}
}

func TestAppSink_OwnershipUnknownPassesEverything(t *testing.T) {
	h := newSinkHarness(t) // no installations derived yet
	h.sink.Deliver(prDelivery("early", "opened", 999))
	waitUntil(t, 2*time.Second, func() bool { return h.client.submitCallCount() == 1 })
}

func TestAppSink_PinnedInstallationIDIsOwn(t *testing.T) {
	h := newSinkHarness(t)
	h.d.Config.AppInstallationID = 333
	h.sink.Deliver(prDelivery("pinned-foreign", "opened", 444))
	h.expectNoReview(t)
	h.sink.Deliver(prDelivery("pinned-own", "opened", 333))
	waitUntil(t, 2*time.Second, func() bool { return h.client.submitCallCount() == 1 })
}

func TestAppSink_InstallationCreatedBypassesFilterOthersDoNot(t *testing.T) {
	h := newSinkHarness(t, 111)
	created := appwebhook.Delivery{ID: "inst-new", EventType: "installation", Transport: "direct",
		Payload: []byte(`{"action":"created","installation":{"id":555}}`)}
	h.sink.Deliver(created)
	if h.dropCount(events.DropForeignInstallation) != 0 {
		t.Error("installation created for a new ID must pass the filter")
	}
	if !h.dedupe.Seen("inst-new") {
		t.Error("the passed delivery should have been recorded")
	}

	// installation_repositories for an unknown installation stays filtered, as
	// does any other action on the installation event type.
	for i, d := range []appwebhook.Delivery{
		{ID: "ir", EventType: "installation_repositories", Transport: "direct", Payload: []byte(`{"action":"added","installation":{"id":555}}`)},
		{ID: "idel", EventType: "installation", Transport: "direct", Payload: []byte(`{"action":"deleted","installation":{"id":555}}`)},
	} {
		h.sink.Deliver(d)
		if got := h.dropCount(events.DropForeignInstallation); got != i+1 {
			t.Errorf("%s: foreign drops = %d, want %d", d.ID, got, i+1)
		}
	}
}

func TestAppSink_OwnInstallationLifecycleEventStillRederives(t *testing.T) {
	h := newSinkHarness(t, 111)
	h.sink.Deliver(appwebhook.Delivery{ID: "ir-own", EventType: "installation_repositories", Transport: "direct",
		Payload: []byte(`{"action":"added","installation":{"id":111}}`)})
	waitUntil(t, 2*time.Second, func() bool { return h.client.submitCallCount() == 1 })
}

func TestAppSink_PingAndUnknownEventTypesAreNoOps(t *testing.T) {
	h := newSinkHarness(t, 111)
	h.sink.Deliver(appwebhook.Delivery{ID: "star", EventType: "star", Transport: "direct",
		Payload: []byte(`{"action":"created","installation":{"id":111}}`)})
	h.expectNoReview(t)
}

func TestAppSink_NonObjectPayloadIsMalformed(t *testing.T) {
	h := newSinkHarness(t, 111)
	h.sink.Deliver(appwebhook.Delivery{ID: "arr", EventType: "pull_request", Transport: "direct", Payload: []byte(`[1,2]`)})
	if h.dropCount(events.DropMalformedPayload) != 1 {
		t.Errorf("malformed drops = %d, want 1", h.dropCount(events.DropMalformedPayload))
	}
}
