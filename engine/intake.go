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
	// ReceivedAt is when the intake received the delivery, stamped by Submit
	// before the ledger check. The apply function orders a delivery that carries
	// no GitHub time by this, not by apply time: a delivery waits behind its
	// item's queue and an apply slot, and its apply makes network calls, so apply
	// time can lag receipt by seconds and would otherwise look newer than reads
	// and engine writes made while it waited (#2351, amending #2348).
	ReceivedAt time.Time
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
	// intakeQuiesceGrace is how long a self-upgrade exec waits for queued and
	// in-flight deliveries before it flushes the ledger and execs anyway.
	intakeQuiesceGrace = 10 * time.Second
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
	// applyAt, when set, is used instead of apply and also receives the
	// delivery's receipt time.
	applyAt func(eventType string, payload []byte, receivedAt time.Time)
	logf    func(format string, args ...any)
	now     func() time.Time
	// receivedNow is the clock that stamps delivery.ReceivedAt. It is the WALL
	// clock (nil = time.Now), not `now`: the Store compares receipt time with the
	// times of reads and engine writes, which are wall-clock stamps, while `now`
	// is the engine clock the sim bed replaces with a fake one.
	receivedNow func() time.Time
	// note is called once per Submit before dedupe, so liveness counters see
	// every arriving delivery, duplicates included.
	note func()

	sem chan struct{}

	mu     sync.Mutex
	queues map[string][]delivery
	// applying maps a sequence number to the ID of each delivery whose apply has
	// started and not finished (they are counted in total but no longer queued).
	applying map[uint64]string
	applySeq uint64
	total    int
	// bytes is the payload bytes of every delivery counted in total.
	bytes  int
	closed bool
	// quiesced refuses new work like closed, but reversibly (Resume): it is the
	// state of a process about to exec, which must survive a failed exec.
	quiesced bool
	wg       sync.WaitGroup
	// idle is signalled whenever total drops to zero (drain).
	idle *sync.Cond
}

func newIntake(ledger *deliveryLedger, apply func(eventType string, payload []byte), logf func(format string, args ...any), now func() time.Time) *intake {
	in := &intake{
		ledger:   ledger,
		apply:    apply,
		logf:     logf,
		now:      now,
		sem:      make(chan struct{}, intakeConcurrency),
		queues:   make(map[string][]delivery),
		applying: make(map[uint64]string),
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
	if in.receivedNow != nil {
		d.ReceivedAt = in.receivedNow()
	} else {
		d.ReceivedAt = time.Now()
	}
	in.mu.Lock()
	if in.closed || in.quiesced {
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
	if in.closed || in.quiesced {
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

// popFront removes the first delivery of q. It zeroes the vacated slot: the
// reslice alone leaves the consumed delivery — and its payload, up to
// webhookMaxBodyBytes — referenced by the backing array until an append
// reallocates or the key drains, which would hold memory the byte cap already
// considers released.
func popFront(q []delivery) (delivery, []delivery) {
	d := q[0]
	q[0] = delivery{}
	return d, q[1:]
}

// work drains one key's queue in order, then exits (a goroutine per ACTIVE key).
// It takes an apply slot BEFORE popping: a delivery waiting for a slot stays in
// its queue, where dropQueued (Close) and forgetPending (Quiesce) can see it. A
// popped delivery is therefore always one whose apply is starting, never one
// parked on the semaphore.
func (in *intake) work(key string) {
	defer in.wg.Done()
	for {
		in.sem <- struct{}{}
		in.mu.Lock()
		q := in.queues[key]
		if len(q) == 0 {
			delete(in.queues, key)
			in.mu.Unlock()
			<-in.sem
			return
		}
		// Keep the key present (len>0 slot semantics): shrink, don't delete, so
		// Submit keeps seeing this key as running until the worker exits.
		var d delivery
		d, in.queues[key] = popFront(q)
		in.applySeq++
		seq := in.applySeq
		in.applying[seq] = d.ID
		in.mu.Unlock()

		in.run(d)
		<-in.sem

		in.mu.Lock()
		delete(in.applying, seq)
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
	switch {
	case in.applyAt != nil:
		in.applyAt(d.EventType, d.Payload, d.ReceivedAt)
	case in.apply != nil:
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

// Close stops accepting, waits up to grace for queued and in-flight applies and
// flushes the ledger. If the grace expires, deliveries still QUEUED are dropped —
// their IDs are forgotten by the ledger so a redelivery is accepted, and
// reconcile heals them (the spec accepts that loss) — and the workers exit after
// the apply each is currently running, so nothing is applied against torn-down
// state after Close returns. A delivery already applying is left to finish, but
// its ID is forgotten too: the shutdown or exec that follows may cut it off.
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
		dropped := in.dropQueued()
		in.log("intake close: gave up waiting for in-flight deliveries after %v — dropped %d queued delivery(ies); reconcile will heal them\n", grace, dropped)
	}
	in.ledger.Close()
}

// Quiesce refuses new deliveries (they are acknowledged by their transport and
// healed by the next reconcile, and never recorded in the ledger) and waits until
// nothing is queued or in flight, or until grace expires. It reports whether the
// intake drained. It is the step before an exec: a delivery the ledger already
// records as seen but that is still queued or applying would otherwise be lost to
// the exec while its redelivery stays suppressed. The wait is bounded and nothing
// is torn down, so a failed exec leaves a process that Resume returns to service.
// If the grace expires with work still pending, the IDs of those deliveries are
// forgotten in the ledger (the deliveries keep running) so the flush that follows
// does not persist as seen a delivery the new process never applied.
func (in *intake) Quiesce(grace time.Duration) bool {
	if in == nil {
		return true
	}
	in.mu.Lock()
	in.quiesced = true
	expired := false
	timer := time.AfterFunc(grace, func() {
		in.mu.Lock()
		expired = true
		in.idle.Broadcast()
		in.mu.Unlock()
	})
	for in.total > 0 && !expired {
		in.idle.Wait()
	}
	pending := in.total
	if pending > 0 {
		// Whatever is still queued or applying may be lost to the exec, and the
		// caller flushes the ledger next: forget those IDs so the flush does not
		// persist deliveries the new process never applied (their redelivery is
		// then accepted, as after a Close drop). The deliveries themselves keep
		// running, so a failed exec that Resumes loses nothing.
		in.forgetPendingLocked()
	}
	in.mu.Unlock()
	timer.Stop()
	if pending > 0 {
		in.log("intake quiesce: %d delivery(ies) still queued or applying after %v — proceeding; their IDs are forgotten and reconcile will heal them\n", pending, grace)
		return false
	}
	return true
}

// Resume accepts deliveries again after a Quiesce whose exec did not happen.
func (in *intake) Resume() {
	if in == nil {
		return
	}
	in.mu.Lock()
	in.quiesced = false
	in.mu.Unlock()
}

// forgetPendingLocked forgets in the ledger the ID of every queued and applying
// delivery. in.mu must be held.
func (in *intake) forgetPendingLocked() {
	for _, q := range in.queues {
		for _, d := range q {
			in.ledger.Forget(d.ID)
		}
	}
	for _, id := range in.applying {
		in.ledger.Forget(id)
	}
}

// dropQueued discards every delivery that has not started applying, forgetting
// its ID, and reports how many were dropped. It also forgets the ID of every
// delivery still applying (it is left to finish, but the process is shutting down
// or about to exec, so the apply may be cut off and the ledger flush that follows
// must not record it as seen). A key keeps its (empty) slot so its worker exits
// after the apply it is running.
func (in *intake) dropQueued() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	dropped := 0
	for key, q := range in.queues {
		for _, d := range q {
			in.ledger.Forget(d.ID)
			in.total--
			in.bytes -= len(d.Payload)
			dropped++
		}
		in.queues[key] = nil
	}
	for _, id := range in.applying {
		in.ledger.Forget(id)
	}
	if in.total == 0 {
		in.idle.Broadcast()
	}
	return dropped
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
