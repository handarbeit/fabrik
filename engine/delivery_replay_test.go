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
)

// fakeHookAPI is a scripted deliveries log: pages[i] is returned for cursor
// "c<i>" ("" = page 0), newest first.
type fakeHookAPI struct {
	mu          sync.Mutex
	pages       [][]gh.AppHookDelivery
	listErr     error
	redeliverFn func(id int64) error
	listCalls   int
	redelivered []int64
}

func (f *fakeHookAPI) ListAppHookDeliveries(cursor string, perPage int) ([]gh.AppHookDelivery, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	idx := 0
	if cursor != "" {
		fmt.Sscanf(cursor, "c%d", &idx)
	}
	if idx >= len(f.pages) {
		return nil, "", nil
	}
	next := ""
	if idx+1 < len(f.pages) {
		next = fmt.Sprintf("c%d", idx+1)
	}
	return f.pages[idx], next, nil
}

func (f *fakeHookAPI) RedeliverAppHookDelivery(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redelivered = append(f.redelivered, id)
	if f.redeliverFn != nil {
		return f.redeliverFn(id)
	}
	return nil
}

type fakeReplayLedger struct {
	seen   map[string]bool
	newest time.Time
}

func (l *fakeReplayLedger) ledgerSeen(id string) bool { return l.seen[id] }
func (l *fakeReplayLedger) ledgerNewest() time.Time   { return l.newest }

type recordingStatus struct {
	mu   sync.Mutex
	last localapi.ReplayHealth
	n    int
}

func (s *recordingStatus) SetReplayStatus(h localapi.ReplayHealth) {
	s.mu.Lock()
	s.last = h
	s.n++
	s.mu.Unlock()
}

var replayTestNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func dlv(id int64, guid string, code int, ago time.Duration) gh.AppHookDelivery {
	return gh.AppHookDelivery{ID: id, GUID: guid, StatusCode: code, DeliveredAt: replayTestNow.Add(-ago), Event: "issues"}
}

type replayHarness struct {
	r      *deliveryReplayer
	api    *fakeHookAPI
	ledger *fakeReplayLedger
	status *recordingStatus
	logs   []string
}

func newReplayHarness(t *testing.T, pages ...[]gh.AppHookDelivery) *replayHarness {
	t.Helper()
	h := &replayHarness{
		api:    &fakeHookAPI{pages: pages},
		ledger: &fakeReplayLedger{seen: map[string]bool{}},
		status: &recordingStatus{},
	}
	h.r = newDeliveryReplayer(h.api, h.ledger, h.status, func(f string, a ...any) {
		h.logs = append(h.logs, fmt.Sprintf(f, a...))
	}, func() time.Time { return replayTestNow })
	oldPace := replayPace
	replayPace = 0
	t.Cleanup(func() { replayPace = oldPace })
	return h
}

func (h *replayHarness) logText() string { return strings.Join(h.logs, "") }

func TestReplayPass_RequestsExactlyFailedUnseen(t *testing.T) {
	h := newReplayHarness(t, []gh.AppHookDelivery{
		dlv(10, "failed-502", 502, time.Minute),
		dlv(9, "ok", 200, 2*time.Minute),
		dlv(8, "failed-seen", 500, 3*time.Minute),
		dlv(7, "no-response", 0, 4*time.Minute),
		dlv(6, "failed-then-ok", 500, 5*time.Minute),
		dlv(5, "redirect", 302, 6*time.Minute),
		{ID: 4, GUID: "ping-guid", StatusCode: 500, DeliveredAt: replayTestNow.Add(-7 * time.Minute), Event: "ping"},
		dlv(3, "too-old", 500, 3*time.Hour),
	})
	// A redelivery of failed-then-ok succeeded: it appears newer than the original.
	h.api.pages[0] = append([]gh.AppHookDelivery{dlv(11, "failed-then-ok", 200, 30*time.Second)}, h.api.pages[0]...)
	h.ledger.seen["failed-seen"] = true

	h.r.pass(context.Background())

	want := []int64{10, 7, 5}
	if fmt.Sprint(h.api.redelivered) != fmt.Sprint(want) {
		t.Fatalf("redelivered = %v, want %v", h.api.redelivered, want)
	}
	s := h.status.last
	if s.FoundFailed.V != 3 || s.Requested.V != 3 || s.Capped.V != 0 || s.Failed.V != 0 || s.Outcome != "ok" || !s.LastPassAt.Valid {
		t.Errorf("summary = %+v", s)
	}

	// A second pass must not request the same GUIDs again.
	h.api.redelivered = nil
	h.r.pass(context.Background())
	if len(h.api.redelivered) != 0 {
		t.Errorf("second pass re-requested %v", h.api.redelivered)
	}
}

func TestReplayPass_LookbackBound(t *testing.T) {
	pages := [][]gh.AppHookDelivery{{
		dlv(4, "recent", 500, 30*time.Minute),
		dlv(3, "ninety-min", 500, 90*time.Minute),
		dlv(2, "twenty-hours", 500, 20*time.Hour),
		dlv(1, "thirty-hours", 500, 30*time.Hour),
	}}
	// Empty ledger: 1h lookback.
	h := newReplayHarness(t, pages...)
	h.r.pass(context.Background())
	if fmt.Sprint(h.api.redelivered) != "[4]" {
		t.Errorf("empty ledger: redelivered %v, want [4]", h.api.redelivered)
	}
	// Newest ledger entry 3h ago: bound = 3h5m back.
	h = newReplayHarness(t, pages...)
	h.ledger.newest = replayTestNow.Add(-3 * time.Hour)
	h.r.pass(context.Background())
	if fmt.Sprint(h.api.redelivered) != "[4 3]" {
		t.Errorf("3h ledger: redelivered %v, want [4 3]", h.api.redelivered)
	}
	// Newest ledger entry long ago: clamped to the 24h ledger window.
	h = newReplayHarness(t, pages...)
	h.ledger.newest = replayTestNow.Add(-100 * time.Hour)
	h.r.pass(context.Background())
	if fmt.Sprint(h.api.redelivered) != "[4 3 2]" {
		t.Errorf("old ledger: redelivered %v, want [4 3 2]", h.api.redelivered)
	}
}

func TestReplayPass_PageCap(t *testing.T) {
	old := replayMaxPages
	replayMaxPages = 2
	t.Cleanup(func() { replayMaxPages = old })
	var pages [][]gh.AppHookDelivery
	for p := 0; p < 5; p++ {
		pages = append(pages, []gh.AppHookDelivery{dlv(int64(100-p), fmt.Sprintf("g%d", p), 500, time.Duration(p+1)*time.Minute)})
	}
	h := newReplayHarness(t, pages...)
	h.r.pass(context.Background())
	if h.api.listCalls != 2 {
		t.Errorf("list calls = %d, want 2 (page cap)", h.api.listCalls)
	}
	if len(h.api.redelivered) != 2 {
		t.Errorf("redelivered %v", h.api.redelivered)
	}
	if !strings.Contains(h.logText(), "page cap") || h.status.last.Outcome != "capped" {
		t.Errorf("cap not reported: outcome=%q logs=%q", h.status.last.Outcome, h.logText())
	}
}

func TestReplayPass_RequestCap(t *testing.T) {
	old := replayMaxRedeliveries
	replayMaxRedeliveries = 3
	t.Cleanup(func() { replayMaxRedeliveries = old })
	var page []gh.AppHookDelivery
	for i := 0; i < 8; i++ {
		page = append(page, dlv(int64(50-i), fmt.Sprintf("g%d", i), 500, time.Duration(i+1)*time.Minute))
	}
	h := newReplayHarness(t, page)
	h.r.pass(context.Background())
	if len(h.api.redelivered) != 3 {
		t.Fatalf("redelivered %d, want 3", len(h.api.redelivered))
	}
	s := h.status.last
	if s.FoundFailed.V != 8 || s.Requested.V != 3 || s.Capped.V != 5 || s.Outcome != "capped" {
		t.Errorf("summary = %+v", s)
	}
	if !strings.Contains(h.logText(), "3-request cap") {
		t.Errorf("cap not logged: %q", h.logText())
	}
}

func TestReplayPass_DegradesOnListingError(t *testing.T) {
	for name, err := range map[string]error{
		"403":       fmt.Errorf("GitHub App API returned 403: x: %w", gh.ErrAppUnauthorized),
		"404":       fmt.Errorf("returned 404: %w", gh.ErrNotFound),
		"ratelimit": &gh.RateLimitError{StatusCode: 429},
		"other":     errors.New("boom"),
	} {
		h := newReplayHarness(t, nil)
		h.api.listErr = err
		h.r.pass(context.Background())
		if len(h.api.redelivered) != 0 || h.api.listCalls != 1 {
			t.Errorf("%s: redelivered=%v listCalls=%d", name, h.api.redelivered, h.api.listCalls)
		}
		degraded := 0
		for _, l := range h.logs {
			if strings.Contains(l, "reconcile only") {
				degraded++
			}
		}
		if degraded != 1 {
			t.Errorf("%s: want exactly one degrade line, got %d in %q", name, degraded, h.logText())
		}
		if !strings.HasPrefix(h.status.last.Outcome, "degraded") {
			t.Errorf("%s: outcome = %q", name, h.status.last.Outcome)
		}
	}
}

func TestReplayPass_RedeliveryErrors(t *testing.T) {
	page := func() []gh.AppHookDelivery {
		var p []gh.AppHookDelivery
		for i := 0; i < 6; i++ {
			p = append(p, dlv(int64(60-i), fmt.Sprintf("g%d", i), 500, time.Duration(i+1)*time.Minute))
		}
		return p
	}

	// 422 (spam guard) ends the pass at once; the unrequested rest counts as capped.
	h := newReplayHarness(t, page())
	h.api.redeliverFn = func(id int64) error {
		if id == 59 {
			return errors.New("redelivering: GitHub App API returned 422: spammed")
		}
		return nil
	}
	h.r.pass(context.Background())
	if len(h.api.redelivered) != 2 {
		t.Errorf("422: redelivered %v", h.api.redelivered)
	}
	if s := h.status.last; s.Requested.V != 1 || s.Failed.V != 1 || s.Capped.V != 4 || !strings.HasPrefix(s.Outcome, "degraded") {
		t.Errorf("422 summary = %+v", s)
	}
	// The failed GUID is not remembered as requested: a later pass may retry it.
	h.api.redeliverFn = nil
	h.api.redelivered = nil
	h.r.pass(context.Background())
	if len(h.api.redelivered) != 5 || h.api.redelivered[0] != 59 {
		t.Errorf("retry pass redelivered %v", h.api.redelivered)
	}

	// An isolated per-delivery error is counted and the pass continues.
	h = newReplayHarness(t, page())
	h.api.redeliverFn = func(id int64) error {
		if id == 58 {
			return errors.New("transient")
		}
		return nil
	}
	h.r.pass(context.Background())
	if len(h.api.redelivered) != 6 || h.status.last.Failed.V != 1 || h.status.last.Requested.V != 5 {
		t.Errorf("isolated error: redelivered=%v summary=%+v", h.api.redelivered, h.status.last)
	}

	// Three in a row abort.
	h = newReplayHarness(t, page())
	h.api.redeliverFn = func(int64) error { return errors.New("transient") }
	h.r.pass(context.Background())
	if len(h.api.redelivered) != 3 || h.status.last.Failed.V != 3 || !strings.HasPrefix(h.status.last.Outcome, "degraded") {
		t.Errorf("abort: redelivered=%v summary=%+v", h.api.redelivered, h.status.last)
	}

	// 403 on redelivery ends the pass.
	h = newReplayHarness(t, page())
	h.api.redeliverFn = func(int64) error { return fmt.Errorf("x: %w", gh.ErrAppUnauthorized) }
	h.r.pass(context.Background())
	if len(h.api.redelivered) != 1 {
		t.Errorf("403 on redelivery: %v", h.api.redelivered)
	}
}

func TestReplayer_TriggerCoalescesAndStopsOnCancel(t *testing.T) {
	h := newReplayHarness(t, []gh.AppHookDelivery{dlv(1, "g", 500, time.Minute)})
	for i := 0; i < 5; i++ {
		h.r.Trigger()
	}
	if len(h.r.trigger) != 1 {
		t.Fatalf("trigger queue = %d, want 1", len(h.r.trigger))
	}
	var nilR *deliveryReplayer
	nilR.Trigger() // must not panic

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.r.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for {
		h.api.mu.Lock()
		n := len(h.api.redelivered)
		h.api.mu.Unlock()
		if n == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("pass did not run")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit on cancel")
	}
}

func TestReplayer_StatusMarkedEnabledBeforeFirstPass(t *testing.T) {
	h := newReplayHarness(t)
	if h.status.n != 1 || h.status.last.LastPassAt.Valid {
		t.Errorf("enabled marker not set: n=%d last=%+v", h.status.n, h.status.last)
	}
}

// FR-008: replay talks to the GitHub API and the read-only ledger lookups only.
func TestDeliveryReplayNeverSubmits(t *testing.T) {
	src, err := os.ReadFile("delivery_replay.go")
	if err != nil {
		t.Fatal(err)
	}
	// Strip comments so the doc text that names the forbidden calls is exempt.
	var code []string
	for _, line := range strings.Split(string(src), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		code = append(code, line)
	}
	text := strings.Join(code, "\n")
	for _, bad := range []string{".Submit(", "Accept(", "SetSink", "sink(", ".sink"} {
		if strings.Contains(text, bad) {
			t.Errorf("delivery_replay.go must not reference %q", bad)
		}
	}
}
