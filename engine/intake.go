package engine

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// delivery is one GitHub webhook event handed to the engine by a transport
// after the transport's own signature check and normalisation (#2351).
type delivery struct {
	// ID is the X-GitHub-Delivery GUID; empty when the transport has none.
	ID        string
	EventType string
	Payload   []byte
	// Transport names the carrier ("webhook", "hookdeck", …), for logs.
	Transport string
}

// Intake bounds. Variables (not constants) so tests can shrink them.
var (
	// intakeMaxPerKey caps one item's pending deliveries; intakeMaxTotal caps all
	// pending deliveries. An overflowing delivery is dropped (and forgotten by the
	// ledger so a redelivery is not suppressed) and the next reconcile heals it —
	// intake never blocks a transport's ack.
	intakeMaxPerKey = 256
	intakeMaxTotal  = 4096
	// intakeMaxBytes caps the payload bytes held by queued and in-flight
	// deliveries. The count caps alone would allow gigabytes if applies stall,
	// because a body may be up to webhookMaxBodyBytes (25 MiB). A delivery that
	// would push the total over the cap is dropped like a depth overflow, except
	// that one is always admitted when nothing else is held, so a single maximum
	// size delivery can never be refused for size alone.
	intakeMaxBytes = 256 << 20
	// intakeConcurrency bounds how many items apply at once. Waiting for a slot
	// preserves each item's order.
	intakeConcurrency = 8
	// intakeCloseGrace is how long Close waits for in-flight applies.
	intakeCloseGrace = 5 * time.Second
)

// intake is the transport-neutral delivery stage (#2351, ADR 2351). Every
// transport hands it deliveries; it
//
//  1. dedupes by delivery ID against the persisted ledger (one check-and-record,
//     so two transports carrying one ID yield one application),
//  2. enqueues onto a per-item serial queue and returns at once, so the
//     transport acknowledges before any of the apply function's network work, and
//  3. runs the apply function from a worker goroutine per active key, in arrival
//     order, with panic recovery.
//
// Ordering is by arrival at the engine; the Store's observed-at rule (ADR 2348)
// orders writes by GitHub time underneath.
type intake struct {
	ledger *deliveryLedger
	apply  func(eventType string, payload []byte)
	logf   func(format string, args ...any)
	now    func() time.Time
	// note is called once per Submit before dedupe, so liveness counters see
	// every arriving delivery, duplicates included.
	note func()

	sem chan struct{}

	mu     sync.Mutex
	queues map[string][]delivery
	total  int
	// bytes is the payload bytes of every delivery counted in total.
	bytes  int
	closed bool
	wg     sync.WaitGroup
	// idle is signalled whenever total drops to zero (drain).
	idle *sync.Cond
}

func newIntake(ledger *deliveryLedger, apply func(eventType string, payload []byte), logf func(format string, args ...any), now func() time.Time) *intake {
	in := &intake{
		ledger: ledger,
		apply:  apply,
		logf:   logf,
		now:    now,
		sem:    make(chan struct{}, intakeConcurrency),
		queues: make(map[string][]delivery),
	}
	in.idle = sync.NewCond(&in.mu)
	return in
}

func (in *intake) log(format string, args ...any) {
	if in.logf != nil {
		in.logf(format, args...)
	}
}

// Submit accepts a delivery. It never blocks on the apply function. It reports
// whether the delivery was queued; false means duplicate, dropped on overflow, or
// intake closed — the transport acknowledges in every case.
func (in *intake) Submit(d delivery) bool {
	if in == nil {
		return false
	}
	if in.note != nil {
		in.note()
	}
	now := in.now()
	in.mu.Lock()
	if in.closed {
		in.mu.Unlock()
		return false
	}
	in.mu.Unlock()
	if !in.ledger.Accept(d.ID, now) {
		in.log("duplicate delivery %s (%s via %s) — already applied\n", d.ID, d.EventType, d.Transport)
		return false
	}
	key := deliveryKey(d.EventType, d.Payload)

	in.mu.Lock()
	if in.closed {
		in.mu.Unlock()
		in.ledger.Forget(d.ID)
		return false
	}
	if in.total >= intakeMaxTotal || len(in.queues[key]) >= intakeMaxPerKey ||
		(in.total > 0 && in.bytes+len(d.Payload) > intakeMaxBytes) {
		in.mu.Unlock()
		in.ledger.Forget(d.ID)
		in.log("intake queue full for %s — dropping %s delivery %s; reconcile will heal it\n", key, d.EventType, d.ID)
		return false
	}
	q, running := in.queues[key]
	in.queues[key] = append(q, d)
	in.total++
	in.bytes += len(d.Payload)
	if !running {
		in.wg.Add(1)
		go in.work(key)
	}
	in.mu.Unlock()
	return true
}

// work drains one key's queue in order, then exits (a goroutine per ACTIVE key).
func (in *intake) work(key string) {
	defer in.wg.Done()
	for {
		in.mu.Lock()
		q := in.queues[key]
		if len(q) == 0 {
			delete(in.queues, key)
			in.mu.Unlock()
			return
		}
		d := q[0]
		// Keep the key present (len>0 slot semantics): shrink, don't delete, so
		// Submit keeps seeing this key as running until the worker exits.
		in.queues[key] = q[1:]
		in.mu.Unlock()

		in.sem <- struct{}{}
		in.run(d)
		<-in.sem

		in.mu.Lock()
		in.total--
		in.bytes -= len(d.Payload)
		if in.total == 0 {
			in.idle.Broadcast()
		}
		in.mu.Unlock()
	}
}

// run applies one delivery. A panic is logged and swallowed: it must not wedge
// the key or kill the daemon (a net/http handler panic used to be recovered by
// the server; a worker goroutine's is not).
func (in *intake) run(d delivery) {
	defer func() {
		if r := recover(); r != nil {
			in.log("panic applying %s delivery %s: %v\n%s", d.EventType, d.ID, r, debug.Stack())
		}
	}()
	if in.apply != nil {
		in.apply(d.EventType, d.Payload)
	}
}

// Drain blocks until every queued delivery has been applied (tests, shutdown).
func (in *intake) Drain() {
	if in == nil {
		return
	}
	in.mu.Lock()
	for in.total > 0 {
		in.idle.Wait()
	}
	in.mu.Unlock()
}

// Close stops accepting, waits up to grace for in-flight applies and flushes
// the ledger. Deliveries still queued after the grace are dropped — the spec
// accepts that loss (reconcile heals it).
func (in *intake) Close(grace time.Duration) {
	if in == nil {
		return
	}
	in.mu.Lock()
	in.closed = true
	in.mu.Unlock()
	done := make(chan struct{})
	go func() {
		in.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		in.log("intake close: gave up waiting for in-flight deliveries after %v\n", grace)
	}
	in.ledger.Close()
}

// Flush persists the ledger without closing (before an exec).
func (in *intake) Flush() {
	if in == nil {
		return
	}
	in.ledger.Flush()
}

// keyPayload is the minimal set of fields deliveryKey reads.
type keyPayload struct {
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue       *struct{ Number int } `json:"issue"`
	PullRequest *struct{ Number int } `json:"pull_request"`
	CheckRun    *keyCheck             `json:"check_run"`
	CheckSuite  *keyCheck             `json:"check_suite"`
	ProjectItem *struct {
		NodeID string `json:"node_id"`
	} `json:"projects_v2_item"`
}

type keyCheck struct {
	HeadSHA      string                 `json:"head_sha"`
	PullRequests []struct{ Number int } `json:"pull_requests"`
}

// deliveryKey derives the per-item serial-queue key. Deliveries that share a key
// are applied one at a time in arrival order; different keys proceed
// independently.
//
//   - issues / issue_comment / pull_request* events: "<repo>#<number>". An issue
//     and a pull request share a number space, so an `issues` and a `pull_request`
//     event for one number share a key.
//   - check_run / check_suite: the linked PR number when present
//     ("<repo>#<n>", the same key as the PR itself), else "<repo>@<head sha>".
//   - projects_v2_item: "item:<node id>".
//   - everything else (installation, project, repository, unparseable): the
//     repository, or "global" — so such events are still ordered relative to
//     themselves.
func deliveryKey(eventType string, payload []byte) string {
	var p keyPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return "global"
	}
	repo := p.Repository.FullName
	num := func(n int) string { return repo + "#" + strconv.Itoa(n) }
	switch {
	case eventType == "projects_v2_item":
		if p.ProjectItem != nil && p.ProjectItem.NodeID != "" {
			return "item:" + p.ProjectItem.NodeID
		}
	case eventType == "check_run" || eventType == "check_suite":
		c := p.CheckRun
		if eventType == "check_suite" {
			c = p.CheckSuite
		}
		if c != nil && repo != "" {
			if len(c.PullRequests) > 0 && c.PullRequests[0].Number > 0 {
				return num(c.PullRequests[0].Number)
			}
			if c.HeadSHA != "" {
				return repo + "@" + c.HeadSHA
			}
		}
	case strings.HasPrefix(eventType, "pull_request"):
		if p.PullRequest != nil && p.PullRequest.Number > 0 && repo != "" {
			return num(p.PullRequest.Number)
		}
	case eventType == "issues" || eventType == "issue_comment":
		if p.Issue != nil && p.Issue.Number > 0 && repo != "" {
			return num(p.Issue.Number)
		}
	}
	if repo != "" {
		return "repo:" + repo
	}
	return "global"
}

// String makes a delivery loggable without its payload.
func (d delivery) String() string {
	return fmt.Sprintf("%s/%s(%s)", d.Transport, d.EventType, d.ID)
}
