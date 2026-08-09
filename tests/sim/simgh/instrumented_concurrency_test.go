package simgh

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// R6/AC8: the mutation log and the fault table are written from every engine
// worker goroutine on every call, and the log in particular is the most likely
// place in this package to introduce its first data race.
//
// Operation counts stay modest, following concurrency_test.go's convention:
// the model's cost is dominated by git worktree churn, and a bigger fan-out
// buys no additional confidence about the two mutexes actually under test.

// TestLogAndFaultsAreConcurrencySafe hammers both instruments from several
// goroutines at once, mixing reads, mutations, faulted calls and log queries.
// Run under -race, a missing lock surfaces here.
func TestLogAndFaultsAreConcurrencySafe(t *testing.T) {
	const (
		workers        = 8
		callsPerWorker = 12
	)

	in, _ := newInstrumented(t)
	// Every worker gets its own issue, so the model-level contention is real
	// but the assertions below stay per-issue and exact.
	for w := 0; w < workers; w++ {
		in.Sim().SeedIssue("acme/widgets", IssueSeed{
			Number: 100 + w,
			Title:  fmt.Sprintf("worker %d", w),
			Status: "Implement",
		})
	}
	if err := in.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// A fault narrowed to one worker's issue: the fault table is consulted on
	// every call from every goroutine, and its per-method call counter is
	// shared, so this exercises the narrowing path under contention too.
	in.Faults().FailWhen("AddLabelToIssue",
		func(a Args) bool { return a.Number == 100 },
		Always, ErrRateLimit())

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			issue := 100 + w
			for i := 0; i < callsPerWorker; i++ {
				label := fmt.Sprintf("worker-%d-call-%d", w, i)
				// Mutation (faulted for worker 0), read, and a concurrent
				// query of the log itself — the query path takes the same
				// mutex the writers do.
				_ = in.AddLabelToIssue("acme", "widgets", issue, label)
				_, _ = in.FetchLabels("acme", "widgets", issue)
				_ = in.Log().Len()
				_ = in.Faults().CallCount("AddLabelToIssue")
			}
		}(w)
	}
	wg.Wait()

	entries := in.Log().Entries()
	wantTotal := workers * callsPerWorker * 2
	if len(entries) != wantTotal {
		t.Fatalf("log has %d entries, want %d — a concurrent append was lost or duplicated", len(entries), wantTotal)
	}

	// No entry may be left pending: every reserved slot must have been
	// completed by the goroutine that took it.
	for _, e := range entries {
		if e.Pending {
			t.Fatalf("entry %d left pending after all workers finished: %s", e.Seq, e)
		}
	}

	// Sequence numbers must be a dense permutation of 0..n-1 — no gaps, no
	// duplicates. That is the property a racy append would break.
	seen := make([]bool, len(entries))
	for i, e := range entries {
		if e.Seq != i {
			t.Fatalf("entry at index %d carries Seq %d; the log is not densely numbered", i, e.Seq)
		}
		if seen[e.Seq] {
			t.Fatalf("sequence %d appears twice", e.Seq)
		}
		seen[e.Seq] = true
	}

	// Every call appears exactly once, attributed to the right issue.
	for w := 0; w < workers; w++ {
		issue := 100 + w
		byIssue := in.Log().ByIssue(issue)
		if len(byIssue) != callsPerWorker*2 {
			t.Errorf("issue %d has %d entries, want %d", issue, len(byIssue), callsPerWorker*2)
		}
		for i := 0; i < callsPerWorker; i++ {
			label := fmt.Sprintf("worker-%d-call-%d", w, i)
			matches := in.Log().Find(LabelAdded(issue, label))
			if len(matches) != 1 {
				t.Errorf("label %s appears in %d entries, want 1", label, len(matches))
			}
		}
	}

	// The narrowed fault fired for exactly one worker's calls and no others.
	injected := in.Log().Find(WasInjected())
	if len(injected) != callsPerWorker {
		t.Errorf("%d injected failures, want %d (worker 0's mutations only)", len(injected), callsPerWorker)
	}
	for _, e := range injected {
		if e.Args.Number != 100 {
			t.Errorf("the narrowed fault fired on issue %d", e.Args.Number)
		}
	}
	if got := in.Faults().CallCount("AddLabelToIssue"); got != workers*callsPerWorker {
		t.Errorf("CallCount = %d, want %d", got, workers*callsPerWorker)
	}
}

// A single goroutine's calls must be in exact order in the log, whatever else
// is happening concurrently. That is the ordering guarantee every real
// assertion relies on — ADR-060's "awaiting-done first" is one worker's
// sequential code — and the two-phase append is what provides it.
func TestOrderingWithinAGoroutineIsExactUnderContention(t *testing.T) {
	const (
		noisemakers = 6
		steps       = 10
	)

	in, _ := newInstrumented(t)
	in.Sim().SeedIssue("acme/widgets", IssueSeed{Number: 200, Title: "ordered", Status: "Implement"})
	for w := 0; w < noisemakers; w++ {
		in.Sim().SeedIssue("acme/widgets", IssueSeed{Number: 300 + w, Title: "noise", Status: "Implement"})
	}
	if err := in.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	stop := make(chan struct{})
	var noise sync.WaitGroup
	for w := 0; w < noisemakers; w++ {
		noise.Add(1)
		go func(w int) {
			defer noise.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = in.AddLabelToIssue("acme", "widgets", 300+w, fmt.Sprintf("n-%d-%d", w, i))
			}
		}(w)
	}

	// The observed goroutine: a strict sequence the log must reproduce.
	for i := 0; i < steps; i++ {
		if err := in.AddLabelToIssue("acme", "widgets", 200, fmt.Sprintf("step-%d", i)); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	close(stop)
	noise.Wait()

	var lastSeq = -1
	for i := 0; i < steps; i++ {
		seq, err := in.Log().IndexOf(LabelAdded(200, fmt.Sprintf("step-%d", i)))
		if err != nil {
			t.Fatalf("step %d missing from the log: %v", i, err)
		}
		if seq <= lastSeq {
			t.Fatalf("step %d logged at sequence %d, not after step %d's %d — ordering within one goroutine is not exact",
				i, seq, i-1, lastSeq)
		}
		lastSeq = seq
	}
}

// Advancing the clock while workers are in flight must not race.
//
// This is how a harness driving Engine.Run() necessarily uses the clock: there
// is no instant at which nothing is running, so "advance, then observe" is
// always concurrent with somebody's read. The instrumentation layer widened the
// exposure sharply — Now is read on every intercepted call, not only on the
// model reads that stamp a timestamp — and a schedule drain reads it too, so an
// unguarded clock would race from an arbitrary worker, far from the Advance
// that caused it. Run under -race, which is what makes this test mean anything.
func TestAdvancingTheClockUnderTrafficIsRaceFree(t *testing.T) {
	const workers = 6

	in, clk := newInstrumented(t)
	// Check runs are keyed by SHA string alone, and a drain is repo-wide, so a
	// literal key is enough here — this test is about the clock, not about git.
	const sha = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	// A scheduled step so the drain path — which reads the clock while holding
	// mu — is exercised alongside the log's own read of it. Its instant sits
	// inside the advance loop below, so it lands while the workers are running
	// rather than after they stop.
	in.Sim().SeedCheckRunsAfter("acme/widgets", sha, time.Minute,
		gh.CheckRun{ID: 700, Name: "build", Status: "completed", Conclusion: "failure"})
	if err := in.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// The advances must genuinely overlap the reads. Without a barrier the main
	// goroutine finishes its whole loop before the workers are scheduled, and
	// nothing ever accesses the clock concurrently — the test then passes with
	// an unguarded clock roughly half the time, which is worse than not having
	// it. Every worker signals once it is in its loop, and the advances are
	// spread with Gosched so they interleave rather than burst.
	stop := make(chan struct{})
	var running, wg sync.WaitGroup
	running.Add(workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				if i == 0 {
					running.Done()
				}
				select {
				case <-stop:
					return
				default:
				}
				_, _ = in.FetchCheckRuns("acme", "widgets", sha)
				_ = in.AddLabelToIssue("acme", "widgets", 7, fmt.Sprintf("t-%d-%d", w, i))
			}
		}(w)
	}
	running.Wait()

	for i := 0; i < 200; i++ {
		clk.Advance(time.Second)
		runtime.Gosched()
	}
	close(stop)
	wg.Wait()

	// And the step still landed exactly once, rather than being applied
	// repeatedly by the racing readers.
	runs, err := in.FetchCheckRuns("acme", "widgets", sha)
	if err != nil {
		t.Fatalf("FetchCheckRuns: %v", err)
	}
	if len(runs) != 1 || checkVerdict(runs[0]) != "failure" {
		t.Errorf("after the advance, check runs = %+v, want the single scheduled failure", runs)
	}
}

// Snapshot's own instruments must survive being taken while the log is being
// written — Entries() copies under the lock, so a snapshot concurrent with
// traffic is safe even though the two halves may disagree about a call that
// landed between them (documented in snapshot.go).
func TestSnapshotIsSafeWhileTheLogIsBeingWritten(t *testing.T) {
	in, _ := newInstrumented(t)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			_, _ = in.FetchLabels("acme", "widgets", 7)
		}
	}()

	staging := t.TempDir()
	for i := 0; i < 3; i++ {
		if _, err := in.Snapshot(staging); err != nil {
			t.Errorf("Snapshot during concurrent traffic: %v", err)
			break
		}
	}
	wg.Wait()
}
