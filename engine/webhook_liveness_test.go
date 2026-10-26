package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/warnings"
)

const livenessHookURL = "https://n.t.ts.net/webhook"

// livenessAPI fake: a mutable delivery log, per-attempt URLs for the detail
// read and recorded redelivery requests.
type fakeLivenessAPI struct {
	mu          sync.Mutex
	log         []gh.AppHookDelivery // newest first
	urls        map[int64]string     // attempt id -> URL; missing = livenessHookURL
	listErr     error
	detailErr   error
	redeliverEr error
	onRedeliver func(id int64)
	redelivered []int64
	detailReads int
	nextID      int64
}

func (f *fakeLivenessAPI) ListAppHookDeliveries(cursor string, perPage int) ([]gh.AppHookDelivery, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	return append([]gh.AppHookDelivery(nil), f.log...), "", nil
}

func (f *fakeLivenessAPI) RedeliverAppHookDelivery(id int64) error {
	f.mu.Lock()
	f.redelivered = append(f.redelivered, id)
	err, fn := f.redeliverEr, f.onRedeliver
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if fn != nil {
		fn(id)
	}
	return nil
}

func (f *fakeLivenessAPI) FetchAppHookDelivery(id int64) (gh.AppHookDeliveryDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detailReads++
	if f.detailErr != nil {
		return gh.AppHookDeliveryDetail{}, f.detailErr
	}
	u, ok := f.urls[id]
	if !ok {
		u = livenessHookURL
	}
	return gh.AppHookDeliveryDetail{URL: u}, nil
}

// add prepends an attempt (newest first).
func (f *fakeLivenessAPI) add(d gh.AppHookDelivery) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append([]gh.AppHookDelivery{d}, f.log...)
}

func (f *fakeLivenessAPI) requests() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.redelivered...)
}

type fakeLivenessLedger struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (l *fakeLivenessLedger) ledgerReceivedAt(id string, now time.Time) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.at[id]
	if !ok || now.Sub(at) > deliveryLedgerWindow {
		return time.Time{}, false
	}
	return at, true
}

type recRouteSink struct {
	mu   sync.Mutex
	last localapi.RouteLiveness
}

func (s *recRouteSink) SetRouteLiveness(rl localapi.RouteLiveness) {
	s.mu.Lock()
	s.last = rl
	s.mu.Unlock()
}

func (s *recRouteSink) get() localapi.RouteLiveness {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

type livenessHarness struct {
	t        *testing.T
	api      *fakeLivenessAPI
	ledger   *fakeLivenessLedger
	sink     *recRouteSink
	r        *routeLiveness
	mu       sync.Mutex
	now      time.Time
	hookURL  string
	hookErr  error
	reported []warnings.Condition
	resolved []string
}

func newLivenessHarness(t *testing.T, warmup time.Duration) *livenessHarness {
	t.Helper()
	h := &livenessHarness{
		t:       t,
		api:     &fakeLivenessAPI{urls: map[int64]string{}},
		ledger:  &fakeLivenessLedger{at: map[string]time.Time{}},
		sink:    &recRouteSink{},
		now:     time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC),
		hookURL: livenessHookURL,
	}
	h.r = newRouteLiveness(routeLivenessConfig{
		Tag:    "tailscale",
		API:    h.api,
		Ledger: h.ledger,
		Sink:   h.sink,
		HookURL: func() (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.hookURL, h.hookErr
		},
		Logf: func(string, ...any) {},
		Now:  h.clock,
		// The fake clock advances by what is slept; nothing really waits.
		Wait: func(ctx context.Context, d time.Duration) bool {
			if ctx.Err() != nil {
				return false
			}
			h.advance(d)
			return true
		},
		Warmup:  warmup,
		Report:  func(c warnings.Condition) { h.mu.Lock(); h.reported = append(h.reported, c); h.mu.Unlock() },
		Resolve: func(k string) { h.mu.Lock(); h.resolved = append(h.resolved, k); h.mu.Unlock() },
	})
	if warmup == 0 {
		// The daemon has been up a while: failures from the last hour count.
		h.r.started = h.now.Add(-time.Hour)
	}
	return h
}

func (h *livenessHarness) clock() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }
func (h *livenessHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func (h *livenessHarness) att(id int64, guid string, code int, status string, ago time.Duration, redelivery bool) gh.AppHookDelivery {
	return gh.AppHookDelivery{ID: id, GUID: guid, StatusCode: code, Status: status, DeliveredAt: h.clock().Add(-ago), Redelivery: redelivery, Event: "issues"}
}

func (h *livenessHarness) liveness() string { return h.sink.get().Liveness }
func (h *livenessHarness) reports() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.reported)
}

// A recent 2xx to the current URL makes the route reachable, with no warning,
// and no redelivery is requested for it.
func TestLiveness_Recent2xxIsReachable(t *testing.T) {
	h := newLivenessHarness(t, 0)
	h.api.add(h.att(1, "g1", 200, "OK", 2*time.Minute, false))
	h.r.evaluate(context.Background(), true)
	if got := h.sink.get(); got.Liveness != localapi.RouteReachable || !got.EvaluatedAt.Valid {
		t.Fatalf("route = %+v", got)
	}
	if len(h.api.requests()) != 0 || h.reports() != 0 {
		t.Fatalf("requests=%v reports=%d", h.api.requests(), h.reports())
	}
}

// A run of failed attempts to the current URL with no later success is
// unreachable with GitHub's status text; the panel entry appears only after the
// verdict has held for the sustained window, and clears on the next success.
func TestLiveness_FailureRunIsUnreachableAndPanelIsSustained(t *testing.T) {
	h := newLivenessHarness(t, 0)
	h.api.add(h.att(2, "g2", 502, "failed to connect to host", 2*time.Minute, false))
	h.api.add(h.att(1, "g1", 502, "failed to connect to host", 4*time.Minute, false))
	// Newest first: put 2 ahead of 1.
	h.api.mu.Lock()
	h.api.log = []gh.AppHookDelivery{h.att(2, "g2", 502, "failed to connect to host", 2*time.Minute, false), h.att(1, "g1", 502, "failed to connect to host", 4*time.Minute, false)}
	h.api.mu.Unlock()
	h.r.evaluate(context.Background(), true)
	got := h.sink.get()
	if got.Liveness != localapi.RouteUnreachable || got.Reason != "failed to connect to host" {
		t.Fatalf("route = %+v", got)
	}
	if h.reports() != 0 {
		t.Fatal("a fresh unreachable verdict must not raise the panel entry yet")
	}
	// Still failing after the sustained window: one entry, naming the reason.
	h.advance(livenessSustained + time.Minute)
	h.api.mu.Lock()
	h.api.log = []gh.AppHookDelivery{h.att(3, "g3", 502, "failed to connect to host", time.Minute, false), h.att(2, "g2", 502, "failed to connect to host", 3*time.Minute, false)}
	h.api.mu.Unlock()
	h.r.evaluate(context.Background(), true)
	if h.reports() != 1 || !strings.Contains(h.reported[0].Detail, "failed to connect to host") || h.reported[0].Type != livenessPanelType {
		t.Fatalf("reports = %+v", h.reported)
	}
	// Recovery: a fresh 2xx is reachable and resolves the entry.
	h.api.add(h.att(4, "g4", 200, "OK", 10*time.Second, false))
	h.r.evaluate(context.Background(), true)
	if h.liveness() != localapi.RouteReachable {
		t.Fatalf("route = %+v", h.sink.get())
	}
	wantKey := warnings.Condition{Type: livenessPanelType, Subject: "tailscale"}.Key()
	if len(h.resolved) != 1 || h.resolved[0] != wantKey {
		t.Fatalf("resolved = %v", h.resolved)
	}
}

// Failed attempts that went to an earlier hook URL do not count against the
// current route; neither does a lone failure.
func TestLiveness_EarlierURLAndLoneFailureDoNotCount(t *testing.T) {
	h := newLivenessHarness(t, 0)
	h.api.urls[1], h.api.urls[2] = "https://old.example/webhook", "https://old.example/webhook"
	h.api.mu.Lock()
	h.api.log = []gh.AppHookDelivery{h.att(2, "g2", 502, "x", time.Minute, false), h.att(1, "g1", 502, "x", 2*time.Minute, false)}
	h.api.mu.Unlock()
	h.r.evaluate(context.Background(), false)
	if h.liveness() != localapi.RouteUnknown {
		t.Fatalf("old-URL failures decided %+v", h.sink.get())
	}
	// A lone failure to the current URL is not a run.
	h.api.urls = map[int64]string{}
	h.api.mu.Lock()
	h.api.log = []gh.AppHookDelivery{h.att(5, "g5", 502, "x", time.Minute, false)}
	h.api.mu.Unlock()
	h.r.evaluate(context.Background(), true)
	if h.liveness() != localapi.RouteUnknown {
		t.Fatalf("a lone failure decided %+v", h.sink.get())
	}
}

// A failure inside the warm-up (a tailscale Funnel still being routed) never
// counts; the same failures after it do.
func TestLiveness_WarmupFailuresDoNotCount(t *testing.T) {
	h := newLivenessHarness(t, 90*time.Second)
	h.api.mu.Lock()
	h.api.log = []gh.AppHookDelivery{h.att(2, "g2", 502, "x", 10*time.Second, false), h.att(1, "g1", 502, "x", 20*time.Second, false)}
	h.api.mu.Unlock()
	h.r.evaluate(context.Background(), false)
	if h.liveness() != localapi.RouteUnknown {
		t.Fatalf("warm-up failures decided %+v", h.sink.get())
	}
	h.advance(3 * time.Minute)
	h.api.mu.Lock()
	h.api.log = []gh.AppHookDelivery{h.att(4, "g4", 502, "x", 10*time.Second, false), h.att(3, "g3", 502, "x", 20*time.Second, false)}
	h.api.mu.Unlock()
	h.r.evaluate(context.Background(), true)
	if h.liveness() != localapi.RouteUnreachable {
		t.Fatalf("post-warm-up failures decided %+v", h.sink.get())
	}
}

// FR-008/SC-005: failing to read the log, the config, an attempt's URL, or to
// request a redelivery never produces unreachable.
func TestLiveness_ReadFailuresAreNeverUnreachable(t *testing.T) {
	forbidden := fmt.Errorf("403: %w", gh.ErrAppUnauthorized)
	for name, setup := range map[string]func(*livenessHarness){
		"list":   func(h *livenessHarness) { h.api.listErr = forbidden },
		"rate":   func(h *livenessHarness) { h.api.listErr = &gh.RateLimitError{} },
		"config": func(h *livenessHarness) { h.hookErr = errors.New("no key") },
		"detail": func(h *livenessHarness) { h.api.detailErr = errors.New("boom") },
		"redeliver": func(h *livenessHarness) {
			h.api.redeliverEr = errors.New("returned 422")
			h.ledger.at["g1"] = h.clock().Add(-time.Hour)
			h.api.mu.Lock()
			h.api.log = []gh.AppHookDelivery{h.att(1, "g1", 200, "OK", time.Hour, false)}
			h.api.mu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newLivenessHarness(t, 0)
			h.api.mu.Lock()
			h.api.log = []gh.AppHookDelivery{h.att(2, "g2", 502, "x", time.Minute, false), h.att(1, "g1", 502, "x", 2*time.Minute, false)}
			h.api.mu.Unlock()
			setup(h)
			h.r.evaluate(context.Background(), true)
			if h.liveness() != localapi.RouteUnknown {
				t.Fatalf("%s: route = %+v", name, h.sink.get())
			}
		})
	}
}

// FR-007: with no delivery the ledger holds (or only one past the age margin),
// nothing is requested and the state stays unknown.
func TestLiveness_NoEligibleDeliveryRequestsNothing(t *testing.T) {
	h := newLivenessHarness(t, 0)
	h.api.add(h.att(1, "g1", 200, "OK", 3*time.Hour, false)) // old success: undecided
	h.r.evaluate(context.Background(), true)                 // ledger empty
	h.ledger.at["g1"] = h.clock().Add(-21 * time.Hour)       // held, but past the 20 h margin
	h.api.add(h.att(2, "g2", 200, "OK", 3*time.Hour, false))
	h.ledger.at["g2"] = h.clock().Add(-21 * time.Hour)
	h.r.evaluate(context.Background(), true)
	if len(h.api.requests()) != 0 || h.liveness() != localapi.RouteUnknown {
		t.Fatalf("requests=%v route=%+v", h.api.requests(), h.sink.get())
	}
	// A ping is never a probe candidate.
	ping := h.att(3, "g3", 200, "OK", 3*time.Hour, false)
	ping.Event = "ping"
	h.api.add(ping)
	h.ledger.at["g3"] = h.clock().Add(-time.Hour)
	h.r.evaluate(context.Background(), true)
	if len(h.api.requests()) != 0 {
		t.Fatalf("a ping was probed: %v", h.api.requests())
	}
}

// The probe's copy arriving at the receiver decides reachable; a real delivery
// arriving is not a probe copy and decides reachable on its own.
func TestLiveness_ProbeArrivalDecidesReachable(t *testing.T) {
	h := newLivenessHarness(t, 0)
	h.api.add(h.att(7, "g-probe", 200, "OK", 3*time.Hour, false))
	h.ledger.at["g-probe"] = h.clock().Add(-3 * time.Hour)
	var probeFlag bool
	h.api.onRedeliver = func(id int64) { probeFlag = h.r.Arrival("g-probe") }
	h.r.evaluate(context.Background(), true)
	if got := h.api.requests(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("requests = %v", got)
	}
	if !probeFlag {
		t.Fatal("the redelivered copy was not recognised as a probe copy")
	}
	if h.liveness() != localapi.RouteReachable {
		t.Fatalf("route = %+v", h.sink.get())
	}

	// A real arrival, no probe outstanding.
	h2 := newLivenessHarness(t, 0)
	if h2.r.Arrival("real") {
		t.Fatal("a real delivery was taken for a probe copy")
	}
	if h2.liveness() != localapi.RouteReachable {
		t.Fatalf("real arrival decided %+v", h2.sink.get())
	}
}

// A receiver that knows its own URL does not count a probe copy as proof while
// the App points somewhere else (a redelivery that went to the old address).
func TestLiveness_ProbeArrivalIgnoredWhenAppPointsElsewhere(t *testing.T) {
	h := newLivenessHarness(t, 0)
	h.r.cfg.OwnURL = func() string { return "https://mine.example/webhook" }
	h.api.add(h.att(7, "g-probe", 200, "OK", 3*time.Hour, false))
	h.ledger.at["g-probe"] = h.clock().Add(-3 * time.Hour)
	h.api.onRedeliver = func(id int64) { h.r.Arrival("g-probe") }
	h.r.evaluate(context.Background(), true)
	if h.liveness() == localapi.RouteReachable {
		t.Fatal("a probe copy proved a route the App no longer points at")
	}
}

// A real delivery proves the route only for the App's current hook URL: where
// the receiver knows its own URL and the App points elsewhere, an arrival (for
// example the replayer's redelivery to the old address) decides nothing.
func TestLiveness_RealArrivalIgnoredWhenAppPointsElsewhere(t *testing.T) {
	h := newLivenessHarness(t, 0)
	h.r.cfg.OwnURL = func() string { return "https://mine.example/webhook" }
	h.r.evaluate(context.Background(), true) // reads the hook URL (livenessHookURL), nothing else
	if h.r.Arrival("real") {
		t.Fatal("a real delivery was taken for a probe copy")
	}
	if h.liveness() == localapi.RouteReachable {
		t.Fatal("an arrival proved a route the App no longer points at")
	}
	// The same arrival at the URL the App is configured with does prove it.
	h.r.cfg.OwnURL = func() string { return livenessHookURL }
	h.r.Arrival("real-2")
	if h.liveness() != localapi.RouteReachable {
		t.Fatalf("route = %+v", h.sink.get())
	}
}

// A probe copy that arrives after the probe's wait ended is still the probe's:
// it is not taken for a real delivery (the transport leaves last_delivery_at
// alone), still proves the route, and is recognised once only and only inside
// the grace.
func TestLiveness_LateProbeCopyIsStillAProbeCopy(t *testing.T) {
	h := newLivenessHarness(t, 0)
	h.api.add(h.att(7, "g-probe", 200, "OK", 3*time.Hour, false))
	h.ledger.at["g-probe"] = h.clock().Add(-3 * time.Hour)
	h.r.evaluate(context.Background(), true) // the wait times out: unknown
	if h.liveness() != localapi.RouteUnknown {
		t.Fatalf("route = %+v", h.sink.get())
	}
	if !h.r.Arrival("g-probe") {
		t.Fatal("a late probe copy was taken for a real delivery")
	}
	if h.liveness() != localapi.RouteReachable {
		t.Fatalf("a late probe copy did not prove the route: %+v", h.sink.get())
	}
	if h.r.Arrival("g-probe") {
		t.Fatal("the late-copy allowance was not consumed")
	}

	// Past the grace the GUID is an ordinary delivery again.
	h2 := newLivenessHarness(t, 0)
	h2.api.add(h2.att(7, "g-probe", 200, "OK", 3*time.Hour, false))
	h2.ledger.at["g-probe"] = h2.clock().Add(-3 * time.Hour)
	h2.r.evaluate(context.Background(), true)
	h2.advance(livenessLateGrace + time.Second)
	if h2.r.Arrival("g-probe") {
		t.Fatal("a copy past the grace was still taken for a probe copy")
	}
}

// The probe's new attempt in the log decides when the copy never arrives.
func TestLiveness_ProbeResolvedFromTheLog(t *testing.T) {
	h := newLivenessHarness(t, 0)
	h.api.add(h.att(7, "g-probe", 200, "OK", 3*time.Hour, false))
	h.ledger.at["g-probe"] = h.clock().Add(-3 * time.Hour)
	h.api.onRedeliver = func(id int64) {
		h.api.add(gh.AppHookDelivery{ID: 8, GUID: "g-probe", StatusCode: 200, Status: "OK", DeliveredAt: h.clock(), Redelivery: true, Event: "issues"})
	}
	h.r.evaluate(context.Background(), true)
	if h.liveness() != localapi.RouteReachable {
		t.Fatalf("route = %+v", h.sink.get())
	}
}

// A probe whose result never shows is unknown, never unreachable.
func TestLiveness_ProbeTimeoutIsUnknown(t *testing.T) {
	h := newLivenessHarness(t, 0)
	h.api.add(h.att(7, "g-probe", 200, "OK", 3*time.Hour, false))
	h.ledger.at["g-probe"] = h.clock().Add(-3 * time.Hour)
	h.r.evaluate(context.Background(), true)
	if len(h.api.requests()) != 1 || h.liveness() != localapi.RouteUnknown {
		t.Fatalf("requests=%v route=%+v", h.api.requests(), h.sink.get())
	}
}

// Two failed probe attempts (the early retry) make a run: unreachable, with
// GitHub's reason, established from probes alone.
func TestLiveness_FailedProbesEstablishUnreachable(t *testing.T) {
	h := newLivenessHarness(t, 0)
	h.api.add(h.att(7, "g-probe", 200, "OK", 3*time.Hour, false))
	h.ledger.at["g-probe"] = h.clock().Add(-3 * time.Hour)
	next := int64(100)
	h.api.onRedeliver = func(int64) {
		next++
		h.api.add(gh.AppHookDelivery{ID: next, GUID: "g-probe", StatusCode: 502, Status: "failed to connect to host", DeliveredAt: h.clock(), Redelivery: true, Event: "issues"})
	}
	h.r.evaluate(context.Background(), true)
	if h.liveness() != localapi.RouteUnknown {
		t.Fatalf("one failed probe decided %+v", h.sink.get())
	}
	h.advance(livenessRetryDelay + time.Second)
	h.r.evaluate(context.Background(), false)
	if got := h.sink.get(); got.Liveness != localapi.RouteUnreachable || got.Reason != "failed to connect to host" {
		t.Fatalf("route = %+v", got)
	}
	if n := len(h.api.requests()); n != 2 {
		t.Fatalf("requests = %d, want the probe and one retry", n)
	}
}

// A changed hook URL drops the old verdict and re-checks at once.
func TestLiveness_HookURLChangeResets(t *testing.T) {
	h := newLivenessHarness(t, 0)
	h.api.add(h.att(1, "g1", 200, "OK", time.Minute, false))
	h.r.evaluate(context.Background(), true)
	if h.liveness() != localapi.RouteReachable {
		t.Fatalf("route = %+v", h.sink.get())
	}
	h.mu.Lock()
	h.hookURL = "https://new.example/webhook"
	h.mu.Unlock()
	h.api.urls[1] = livenessHookURL // the old success went to the old URL
	h.r.evaluate(context.Background(), true)
	if h.liveness() != localapi.RouteUnknown {
		t.Fatalf("a success for the old URL still counts: %+v", h.sink.get())
	}
}

// Reachable evidence that is not refreshed ages out to unknown.
func TestLiveness_ReachableAgesOut(t *testing.T) {
	h := newLivenessHarness(t, 0)
	h.api.add(h.att(1, "g1", 200, "OK", time.Minute, false))
	h.r.evaluate(context.Background(), true)
	h.advance(3 * livenessQuiet)
	h.r.evaluate(context.Background(), false)
	if h.liveness() != localapi.RouteUnknown {
		t.Fatalf("stale reachable stayed %+v", h.sink.get())
	}
}

// SC-007: on a dead endpoint over 24 hours the probe stays inside the hourly
// cap and the per-delivery cap, and never loops on one delivery.
func TestLiveness_RequestVolumeIsBoundedOverADay(t *testing.T) {
	h := newLivenessHarness(t, 0)
	for i := 0; i < 6; i++ {
		g := fmt.Sprintf("g%d", i)
		h.api.add(h.att(int64(i+1), g, 200, "OK", time.Duration(i+3)*time.Hour, false))
		h.ledger.at[g] = h.clock().Add(-time.Duration(i+3) * time.Hour)
	}
	h.r.evaluate(context.Background(), true)
	for i := 0; i < 24*60; i++ {
		h.advance(time.Minute)
		h.r.evaluate(context.Background(), false)
	}
	reqs := h.api.requests()
	if len(reqs) > livenessProbesPerHour*24 {
		t.Fatalf("%d requests in 24 h, cap %d", len(reqs), livenessProbesPerHour*24)
	}
	per := map[int64]int{}
	for _, id := range reqs {
		per[id]++
		if per[id] > livenessMaxProbesPerGUID {
			t.Fatalf("delivery %d probed %d times", id, per[id])
		}
	}
	if len(reqs) == 0 {
		t.Fatal("no probe was ever made")
	}
}

func TestClassifyAttempts_ThrottledAndPingAreIgnored(t *testing.T) {
	now := time.Now()
	thr := now
	attempts := []gh.AppHookDelivery{
		{ID: 4, GUID: "a", StatusCode: 502, Status: "x", DeliveredAt: now.Add(-time.Minute), ThrottledAt: &thr},
		{ID: 3, GUID: "b", StatusCode: 200, Status: "OK", DeliveredAt: now.Add(-time.Minute), Event: "ping"},
		{ID: 2, GUID: "c", StatusCode: 0, Status: "failed to connect to host", DeliveredAt: now.Add(-2 * time.Minute)},
		{ID: 1, GUID: "d", StatusCode: 301, Status: "moved", DeliveredAt: now.Add(-3 * time.Minute)},
	}
	res := classifyAttempts(attempts, now, now.Add(-time.Hour), func(int64) (bool, bool) { return true, true })
	if res.state != localapi.RouteUnreachable || res.reason != "failed to connect to host" {
		t.Fatalf("res = %+v", res)
	}
}

func TestSanitizeStatusText(t *testing.T) {
	if got := sanitizeStatusText("a\n`b`  c", 502); got != "a 'b' c" {
		t.Errorf("got %q", got)
	}
	if got := sanitizeStatusText("", 504); got != "HTTP status 504" {
		t.Errorf("got %q", got)
	}
	if got := sanitizeStatusText(strings.Repeat("x", 500), 0); len(got) > 170 {
		t.Errorf("len %d", len(got))
	}
}

// The probe never applies anything: this file talks to the GitHub API and the
// read-only ledger lookup only (the replay file's twin of FR-008).
func TestWebhookLivenessNeverSubmits(t *testing.T) {
	src, err := os.ReadFile("webhook_liveness.go")
	if err != nil {
		t.Fatal(err)
	}
	var code []string
	for _, line := range strings.Split(string(src), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		code = append(code, line)
	}
	text := strings.Join(code, "\n")
	for _, bad := range []string{".Submit(", "Accept(", "SetSink", "sink(", ".sink", "ledgerSeen", "Seen("} {
		if strings.Contains(text, bad) {
			t.Errorf("webhook_liveness.go must not reference %q", bad)
		}
	}
}
