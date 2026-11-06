package engine

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// Test doubles the engine's wiring tests share with internal/appwebhook's own
// tests (the originals are package-private there).

const livenessHookURL = "https://n.t.ts.net/webhook"

type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.WriteString(strings.TrimSpace(fmt.Sprintf(format, args...)) + "\n")
}
func (l *syncLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// directHarness posts signed requests to a started transport.
type directHarness struct {
	tr   *appWebhookTransport
	url  string
	logs *syncLog
}

func (h *directHarness) post(t *testing.T, body []byte, sig, event, id string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.url, bytes.NewReader(body))
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

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

// LivenessAPI fake: a mutable delivery log, per-attempt URLs for the detail
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
	listCalls   int
	nextID      int64
}

func (f *fakeLivenessAPI) ListAppHookDeliveries(cursor string, perPage int) ([]gh.AppHookDelivery, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
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

func (l *fakeReplayLedger) Seen(id string) bool { return l.seen[id] }
func (l *fakeReplayLedger) Newest() time.Time   { return l.newest }
