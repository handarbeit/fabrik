package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func payloadFor(repo string, issue int) []byte {
	return []byte(fmt.Sprintf(`{"repository":{"full_name":%q},"issue":{"number":%d}}`, repo, issue))
}

// recorder collects applied deliveries in order.
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) apply(eventType string, payload []byte) {
	r.mu.Lock()
	r.seen = append(r.seen, eventType+":"+string(payload))
	r.mu.Unlock()
}
func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func newTestIntake(apply func(string, []byte)) *intake {
	return newIntake(newDeliveryLedger("", time.Now(), nil), apply, nil, time.Now)
}

func TestDeliveryKey(t *testing.T) {
	cases := []struct {
		name, event, payload, want string
	}{
		{"issues", "issues", `{"repository":{"full_name":"o/r"},"issue":{"number":4}}`, "o/r#4"},
		{"issue_comment on an issue", "issue_comment", `{"repository":{"full_name":"o/r"},"issue":{"number":4}}`, "o/r#4"},
		{"issue_comment on a PR shares the PR number", "issue_comment", `{"repository":{"full_name":"o/r"},"issue":{"number":9,"pull_request":{}}}`, "o/r#9"},
		{"pull_request", "pull_request", `{"repository":{"full_name":"o/r"},"pull_request":{"number":9}}`, "o/r#9"},
		{"pull_request_review", "pull_request_review", `{"repository":{"full_name":"o/r"},"pull_request":{"number":9}}`, "o/r#9"},
		{"pull_request_review_comment", "pull_request_review_comment", `{"repository":{"full_name":"o/r"},"pull_request":{"number":9}}`, "o/r#9"},
		{"check_run with a linked PR serialises with the PR", "check_run", `{"repository":{"full_name":"o/r"},"check_run":{"head_sha":"abc","pull_requests":[{"number":9}]}}`, "o/r#9"},
		{"check_suite with a linked PR", "check_suite", `{"repository":{"full_name":"o/r"},"check_suite":{"head_sha":"abc","pull_requests":[{"number":9}]}}`, "o/r#9"},
		{"check_run without a PR keys on the head sha", "check_run", `{"repository":{"full_name":"o/r"},"check_run":{"head_sha":"abc","pull_requests":[]}}`, "o/r@abc"},
		{"projects_v2_item keys on the item node", "projects_v2_item", `{"repository":{"full_name":"o/r"},"projects_v2_item":{"node_id":"PVTI_1"}}`, "item:PVTI_1"},
		{"projects_v2_item with no node id falls back to the repo", "projects_v2_item", `{"repository":{"full_name":"o/r"},"projects_v2_item":{}}`, "repo:o/r"},
		{"installation event falls back to the repo", "installation", `{"repository":{"full_name":"o/r"}}`, "repo:o/r"},
		{"no repository falls back to global", "installation", `{"action":"created"}`, "global"},
		{"unparseable payload is global", "issues", `not json`, "global"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deliveryKey(tc.event, []byte(tc.payload)); got != tc.want {
				t.Errorf("deliveryKey(%s) = %q, want %q", tc.event, got, tc.want)
			}
		})
	}
	// An issues event and a pull_request event for one number share a key.
	if deliveryKey("issues", payloadFor("o/r", 9)) != deliveryKey("pull_request", []byte(`{"repository":{"full_name":"o/r"},"pull_request":{"number":9}}`)) {
		t.Error("an issue and a PR with the same number must share a queue key")
	}
}

// A duplicate delivery applies once — same transport, and across transports.
func TestIntake_DuplicateAppliesOnce(t *testing.T) {
	rec := &recorder{}
	in := newTestIntake(rec.apply)
	p := payloadFor("o/r", 1)
	if !in.Submit(delivery{ID: "X", EventType: "issues", Payload: p, Transport: "webhook"}) {
		t.Fatal("first delivery rejected")
	}
	if in.Submit(delivery{ID: "X", EventType: "issues", Payload: p, Transport: "webhook"}) {
		t.Fatal("same-transport redelivery was queued")
	}
	if in.Submit(delivery{ID: "X", EventType: "issues", Payload: p, Transport: "hookdeck"}) {
		t.Fatal("cross-transport duplicate was queued")
	}
	in.Drain()
	if got := len(rec.list()); got != 1 {
		t.Fatalf("applied %d times, want 1", got)
	}
}

// A delivery applied before a restart is not applied again after it.
func TestIntake_DuplicateAppliesOnceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deliveries.json")
	rec := &recorder{}
	p := payloadFor("o/r", 1)

	first := newIntake(newDeliveryLedger(path, time.Now(), nil), rec.apply, nil, time.Now)
	first.Submit(delivery{ID: "X", EventType: "issues", Payload: p})
	first.Drain()
	first.Close(time.Second) // shutdown flushes the ledger

	// The engine restarts: a fresh intake reloads the persisted record.
	second := newIntake(newDeliveryLedger(path, time.Now(), nil), rec.apply, nil, time.Now)
	if second.Submit(delivery{ID: "X", EventType: "issues", Payload: p, Transport: "hookdeck"}) {
		t.Fatal("a delivery applied before the restart was queued again after it")
	}
	second.Submit(delivery{ID: "Y", EventType: "issues", Payload: p})
	second.Drain()
	if got := len(rec.list()); got != 2 {
		t.Fatalf("applied %d times across the restart, want 2 (X once, Y once)", got)
	}
}

// A delivery with no ID cannot be deduped, so it is applied every time.
func TestIntake_EmptyIDIsAlwaysApplied(t *testing.T) {
	rec := &recorder{}
	in := newTestIntake(rec.apply)
	for i := 0; i < 3; i++ {
		in.Submit(delivery{EventType: "issues", Payload: payloadFor("o/r", 1)})
	}
	in.Drain()
	if got := len(rec.list()); got != 3 {
		t.Fatalf("applied %d, want 3 (never dropped)", got)
	}
}

// The acknowledgement does not depend on the apply: Submit returns while the
// apply is still blocked (SC-002).
func TestIntake_SubmitDoesNotWaitForApply(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	in := newTestIntake(func(string, []byte) {
		started <- struct{}{}
		<-release
	})
	done := make(chan struct{})
	go func() { in.Submit(delivery{ID: "a", EventType: "issues", Payload: payloadFor("o/r", 1)}); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked on the apply function")
	}
	<-started
	close(release)
	in.Drain()
}

// Two deliveries for one item apply in arrival order, even when the first is the
// slow one (SC-003).
func TestIntake_SameItemAppliesInArrivalOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	in := newTestIntake(func(_ string, payload []byte) {
		if strings.Contains(string(payload), `"action":"first"`) {
			time.Sleep(60 * time.Millisecond)
		}
		mu.Lock()
		order = append(order, string(payload))
		mu.Unlock()
	})
	mk := func(action string) []byte {
		return []byte(fmt.Sprintf(`{"action":%q,"repository":{"full_name":"o/r"},"issue":{"number":1}}`, action))
	}
	in.Submit(delivery{ID: "1", EventType: "issues", Payload: mk("first")})
	in.Submit(delivery{ID: "2", EventType: "issues", Payload: mk("second")})
	in.Submit(delivery{ID: "3", EventType: "issues", Payload: mk("third")})
	in.Drain()
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || !strings.Contains(order[0], "first") || !strings.Contains(order[1], "second") || !strings.Contains(order[2], "third") {
		t.Fatalf("apply order = %v, want first, second, third", order)
	}
}

// A slow delivery for item A does not delay item B (US2 scenario 3).
func TestIntake_DifferentItemsDoNotBlockEachOther(t *testing.T) {
	releaseA := make(chan struct{})
	appliedB := make(chan struct{})
	in := newTestIntake(func(_ string, payload []byte) {
		switch {
		case strings.Contains(string(payload), `"number":1`):
			<-releaseA
		case strings.Contains(string(payload), `"number":2`):
			close(appliedB)
		}
	})
	t.Cleanup(func() { close(releaseA); in.Drain() })
	in.Submit(delivery{ID: "a", EventType: "issues", Payload: payloadFor("o/r", 1)})
	in.Submit(delivery{ID: "b", EventType: "issues", Payload: payloadFor("o/r", 2)})
	select {
	case <-appliedB:
	case <-time.After(2 * time.Second):
		t.Fatal("item B was delayed behind item A's slow apply")
	}
}

// A panicking apply must not wedge the key or kill the daemon (FR-006).
func TestIntake_PanicDoesNotWedgeTheKey(t *testing.T) {
	var calls atomic.Int32
	rec := &recorder{}
	in := newIntake(newDeliveryLedger("", time.Now(), nil), func(et string, p []byte) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		rec.apply(et, p)
	}, nil, time.Now)
	in.Submit(delivery{ID: "a", EventType: "issues", Payload: payloadFor("o/r", 1)})
	in.Submit(delivery{ID: "b", EventType: "issues", Payload: payloadFor("o/r", 1)})
	in.Drain()
	if got := len(rec.list()); got != 1 {
		t.Fatalf("delivery after a panicking one: applied %d, want 1", got)
	}
	// And the key accepts new work afterwards.
	in.Submit(delivery{ID: "c", EventType: "issues", Payload: payloadFor("o/r", 1)})
	in.Drain()
	if got := len(rec.list()); got != 2 {
		t.Fatalf("applied %d after recovery, want 2", got)
	}
}

// An overflowing delivery is dropped and forgotten, so a redelivery of it is not
// suppressed; other items are unaffected.
func TestIntake_OverflowDropsForgetsAndRecovers(t *testing.T) {
	oldPer, oldTotal := intakeMaxPerKey, intakeMaxTotal
	intakeMaxPerKey, intakeMaxTotal = 2, 100
	t.Cleanup(func() { intakeMaxPerKey, intakeMaxTotal = oldPer, oldTotal })

	release := make(chan struct{})
	rec := &recorder{}
	in := newTestIntake(func(et string, p []byte) { <-release; rec.apply(et, p) })

	p := payloadFor("o/r", 1)
	var queued, dropped int
	for i := 0; i < 6; i++ {
		if in.Submit(delivery{ID: fmt.Sprintf("d%d", i), EventType: "issues", Payload: p}) {
			queued++
		} else {
			dropped++
		}
	}
	if dropped == 0 {
		t.Fatal("a deep queue never dropped anything")
	}
	if !in.Submit(delivery{ID: "other", EventType: "issues", Payload: payloadFor("o/r", 2)}) {
		t.Fatal("an unrelated item was affected by another item's overflow")
	}
	close(release)
	in.Drain()

	// The dropped deliveries were forgotten: their redelivery is accepted.
	if !in.Submit(delivery{ID: "d5", EventType: "issues", Payload: p}) {
		t.Fatal("a dropped delivery stayed in the ledger and its redelivery was suppressed")
	}
	in.Drain()
	if got := len(rec.list()); got != queued+1+1 {
		t.Fatalf("applied %d, want %d queued + other + redelivered", got, queued+2)
	}
}

func TestIntake_NoteSeesEveryArrivalIncludingDuplicates(t *testing.T) {
	in := newTestIntake(func(string, []byte) {})
	var n atomic.Int32
	in.note = func() { n.Add(1) }
	p := payloadFor("o/r", 1)
	in.Submit(delivery{ID: "a", EventType: "issues", Payload: p})
	in.Submit(delivery{ID: "a", EventType: "issues", Payload: p})
	in.Drain()
	if n.Load() != 2 {
		t.Fatalf("liveness note ran %d times, want 2 (duplicates still prove the stream is alive)", n.Load())
	}
}

func TestIntake_CloseStopsAcceptingAndFlushes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	rec := &recorder{}
	in := newIntake(newDeliveryLedger(path, time.Now(), nil), rec.apply, nil, time.Now)
	in.Submit(delivery{ID: "a", EventType: "issues", Payload: payloadFor("o/r", 1)})
	in.Close(2 * time.Second)
	if in.Submit(delivery{ID: "b", EventType: "issues", Payload: payloadFor("o/r", 1)}) {
		t.Fatal("a closed intake accepted a delivery")
	}
	reloaded := newDeliveryLedger(path, time.Now(), nil)
	if reloaded.Accept("a", time.Now()) {
		t.Fatal("Close did not persist the accepted delivery")
	}
}

// Close is bounded: it does not wait forever on a stuck apply.
func TestIntake_CloseIsBounded(t *testing.T) {
	stuck := make(chan struct{})
	in := newTestIntake(func(string, []byte) { <-stuck })
	t.Cleanup(func() { close(stuck) })
	in.Submit(delivery{ID: "a", EventType: "issues", Payload: payloadFor("o/r", 1)})
	done := make(chan struct{})
	go func() { in.Close(100 * time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return within its grace period")
	}
}

func TestIntake_NilIsInert(t *testing.T) {
	var in *intake
	if in.Submit(delivery{ID: "a"}) {
		t.Fatal("nil intake accepted a delivery")
	}
	in.Drain()
	in.Close(time.Second)
	in.Flush()
}

// The byte cap bounds memory held by queued payloads, not just their count: a
// delivery that would push the held bytes over the cap is dropped and forgotten
// (reconcile heals it), a lone delivery is always admitted, and the bytes are
// released once applied.
func TestIntake_ByteCapDropsForgetsAndReleases(t *testing.T) {
	oldBytes := intakeMaxBytes
	intakeMaxBytes = 1000
	t.Cleanup(func() { intakeMaxBytes = oldBytes })

	release := make(chan struct{})
	rec := &recorder{}
	in := newTestIntake(func(et string, p []byte) { <-release; rec.apply(et, p) })

	pad := func(repo string, n int) []byte {
		return append(payloadFor(repo, n), make([]byte, 600)...) // ~600+ bytes each
	}
	// A lone delivery above the cap is still admitted (nothing else is held).
	big := append(payloadFor("o/r", 1), make([]byte, 5000)...)
	if !in.Submit(delivery{ID: "big", EventType: "issues", Payload: big}) {
		t.Fatal("a lone oversized-for-the-cap delivery must still be admitted")
	}
	// While it is held, anything further would exceed the cap.
	if in.Submit(delivery{ID: "next", EventType: "issues", Payload: pad("o/r", 2)}) {
		t.Fatal("a delivery over the byte cap was queued")
	}
	close(release)
	in.Drain()

	in.mu.Lock()
	held := in.bytes
	in.mu.Unlock()
	if held != 0 {
		t.Fatalf("%d payload bytes still counted after every delivery was applied", held)
	}
	// The dropped delivery was forgotten, so its redelivery is accepted.
	if !in.Submit(delivery{ID: "next", EventType: "issues", Payload: pad("o/r", 2)}) {
		t.Fatal("a byte-cap drop stayed in the ledger and its redelivery was suppressed")
	}
	in.Drain()
}

// popFront must not leave the consumed delivery's payload referenced by the
// queue's backing array.
func TestPopFront_ClearsTheVacatedSlot(t *testing.T) {
	q := []delivery{
		{ID: "a", Payload: make([]byte, 1024)},
		{ID: "b", Payload: []byte("b")},
	}
	d, rest := popFront(q)
	if d.ID != "a" || len(d.Payload) != 1024 {
		t.Fatalf("popped %+v, want the first delivery intact", d)
	}
	if len(rest) != 1 || rest[0].ID != "b" {
		t.Fatalf("rest = %+v, want only b", rest)
	}
	if q[0].ID != "" || q[0].Payload != nil {
		t.Fatalf("vacated slot still holds %+v: the payload stays reachable through the backing array", q[0])
	}
}
