package pruefer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/pruefer/events"
)

var errFetchPRDetailsBoom = errors.New("boom: fetching PR details failed")

func newTestDaemonForEvents(client *fakeLister) *Daemon {
	claude := &mockClaudeInvoker{}
	clone := func(ctx context.Context, owner, repo, token string, prNumber int) (string, func(), error) {
		return "/tmp", func() {}, nil
	}
	return &Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
	}
}

func TestDaemonEventSink_PullRequestTriggerActions_DispatchReview(t *testing.T) {
	for _, action := range []string{"opened", "reopened", "synchronize", "ready_for_review"} {
		t.Run(action, func(t *testing.T) {
			client := newFakeLister()
			client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"}}
			d := newTestDaemonForEvents(client)
			sink := &daemonEventSink{daemon: d}

			sink.Handle(context.Background(), events.GitHubEvent{
				EventType: "pull_request", Action: action, Owner: "owner", Repo: "repo", ResourceID: "1",
			})

			waitUntil(t, 2*time.Second, func() bool { return client.submitCallCount() == 1 })
		})
	}
}

func TestDaemonEventSink_PullRequestOtherAction_NoOp(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"}}
	d := newTestDaemonForEvents(client)
	sink := &daemonEventSink{daemon: d}

	sink.Handle(context.Background(), events.GitHubEvent{
		EventType: "pull_request", Action: "closed", Owner: "owner", Repo: "repo", ResourceID: "1",
	})

	time.Sleep(100 * time.Millisecond)
	if got := client.submitCallCount(); got != 0 {
		t.Errorf("submitCallCount() = %d, want 0 (action %q should not trigger a review)", got, "closed")
	}
}

func TestDaemonEventSink_UnrecognizedEventType_NoOp(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"}}
	d := newTestDaemonForEvents(client)
	sink := &daemonEventSink{daemon: d}

	sink.Handle(context.Background(), events.GitHubEvent{EventType: "star", Action: "created"})

	time.Sleep(100 * time.Millisecond)
	if got := client.submitCallCount(); got != 0 {
		t.Errorf("submitCallCount() = %d, want 0 (unrecognized event type must be a no-op)", got)
	}
}

func TestDaemonEventSink_InstallationEvent_TriggersReconciliationPoll(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"}}
	d := newTestDaemonForEvents(client)
	sink := &daemonEventSink{daemon: d}

	sink.Handle(context.Background(), events.GitHubEvent{EventType: "installation_repositories", Action: "added"})

	// Handle dispatches the reconciliation poll in its own goroutine (like
	// the per-PR path), so the submission is not necessarily observable the
	// instant Handle returns — poll for it.
	waitUntil(t, 2*time.Second, func() bool { return client.submitCallCount() == 1 })
}

func TestDaemonEventSink_InstallationEventBurst_CoalescesReconciliationPolls(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"}}
	block := make(chan struct{})
	client.listOpenPRsBlock = block
	d := newTestDaemonForEvents(client)
	sink := &daemonEventSink{daemon: d}

	// Fire a burst of installation events, as a real Hookdeck delivery burst
	// (e.g. an app install across many repos) would — each a distinct
	// delivery, so dedupe wouldn't collapse them at the transport layer.
	// triggerReconciliationPoll's CompareAndSwap runs synchronously within
	// Handle, so by the time the first call returns, the daemon is already
	// marked in-flight and every subsequent call in this loop is coalesced.
	for i := 0; i < 5; i++ {
		sink.Handle(context.Background(), events.GitHubEvent{EventType: "installation_repositories", Action: "added"})
	}

	waitUntil(t, 2*time.Second, func() bool { return client.listOpenPRsCallCount() >= 1 })
	// Give any wrongly-uncoalesced extra poll cycles a chance to start their
	// own ListOpenPRs call before asserting the negative.
	time.Sleep(50 * time.Millisecond)
	if got := client.listOpenPRsCallCount(); got != 1 {
		t.Errorf("listOpenPRsCallCount() = %d, want 1 (a burst of installation events must coalesce into a single in-flight poll)", got)
	}

	close(block)
	waitUntil(t, 2*time.Second, func() bool { return client.submitCallCount() == 1 })
}

func TestDaemonEventSink_UnknownOwner_DropsWithoutPanic(t *testing.T) {
	client := newFakeLister()
	d := newTestDaemonForEvents(client)
	sink := &daemonEventSink{daemon: d}

	sink.Handle(context.Background(), events.GitHubEvent{
		EventType: "pull_request", Action: "opened", Owner: "unknown-owner", Repo: "repo", ResourceID: "1",
	})
	// Must not panic; nothing to assert beyond that.
}

// TestDaemonEventSink_UnwatchedRepo_DropsWithoutReview guards against
// reviewing repos an owner's installation token can reach but that were
// never configured in watched_repos — a real scenario since the Hookdeck
// adapter's session covers every connection visible to its API key (see
// hookdeck/client.go's createSession), not just watched repos.
func TestDaemonEventSink_UnwatchedRepo_DropsWithoutReview(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["owner/other-repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"}}
	d := newTestDaemonForEvents(client) // WatchedRepos: []string{"owner/repo"} only
	sink := &daemonEventSink{daemon: d}

	sink.Handle(context.Background(), events.GitHubEvent{
		EventType: "pull_request", Action: "opened", Owner: "owner", Repo: "other-repo", ResourceID: "1",
	})

	time.Sleep(100 * time.Millisecond)
	if got := client.submitCallCount(); got != 0 {
		t.Errorf("submitCallCount() = %d, want 0 (an event for a repo outside watched_repos must be dropped)", got)
	}
}

// TestDaemonEventSink_CaseMismatchedOwnerInClientsMap_DispatchesReview is the
// regression test for a review finding: ReviewFromEvent looked up
// d.Clients[owner] using the webhook payload's raw-case owner, but d.Clients
// is keyed by strings.ToLower(owner) everywhere it's built or read elsewhere
// (poll(), rate-limit reporting). GitHub delivers each account's
// canonical-case login in webhook payloads, which need not match the casing
// an operator wrote in watched_repos/config — a mismatch silently dropped
// every real-time event for that owner as "no client for owner."
func TestDaemonEventSink_CaseMismatchedOwnerInClientsMap_DispatchesReview(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["MyOrg/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"}}
	claude := &mockClaudeInvoker{}
	clone := func(ctx context.Context, owner, repo, token string, prNumber int) (string, func(), error) {
		return "/tmp", func() {}, nil
	}
	d := &Daemon{
		// Keyed lower-case, matching how Reconciler/execute.go actually
		// build this map — the event below uses GitHub's own canonical
		// casing for the same account, which differs.
		Clients:  map[string]GitHubLister{"myorg": client},
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"MyOrg/repo"}, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
	}
	sink := &daemonEventSink{daemon: d}

	sink.Handle(context.Background(), events.GitHubEvent{
		EventType: "pull_request", Action: "opened", Owner: "MyOrg", Repo: "repo", ResourceID: "1",
	})

	waitUntil(t, 2*time.Second, func() bool { return client.submitCallCount() == 1 })
}

// TestDaemonEventSink_CaseMismatchedWatchedRepoSpec_DispatchesReview is the
// regression test for isWatchedRepo's sibling case-sensitivity bug: it
// compared "owner/repo" against each watched_repos spec with exact-case
// equality, unlike every other owner comparison in this file. An operator's
// literal watched_repos casing can differ from the case GitHub delivers in
// webhook payloads, which previously made isWatchedRepo report a
// legitimately watched repo as unwatched.
func TestDaemonEventSink_CaseMismatchedWatchedRepoSpec_DispatchesReview(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["myorg/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"}}
	claude := &mockClaudeInvoker{}
	clone := func(ctx context.Context, owner, repo, token string, prNumber int) (string, func(), error) {
		return "/tmp", func() {}, nil
	}
	d := &Daemon{
		Clients: map[string]GitHubLister{"myorg": client},
		Claude:  claude,
		Clone:   clone,
		// The operator's literal config casing differs from what the event
		// below delivers, isolating isWatchedRepo's own comparison (the
		// Clients-map lookup above already matches, via strings.ToLower).
		Config:   Config{WatchedRepos: []string{"MyOrg/Repo"}, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
	}
	sink := &daemonEventSink{daemon: d}

	sink.Handle(context.Background(), events.GitHubEvent{
		EventType: "pull_request", Action: "opened", Owner: "myorg", Repo: "repo", ResourceID: "1",
	})

	waitUntil(t, 2*time.Second, func() bool { return client.submitCallCount() == 1 })
}

func TestDaemonEventSink_FetchPRDetailsError_DropsWithoutPanic(t *testing.T) {
	client := newFakeLister()
	client.detailsErrByKey["owner/repo#1"] = errFetchPRDetailsBoom
	d := newTestDaemonForEvents(client)
	sink := &daemonEventSink{daemon: d}

	sink.Handle(context.Background(), events.GitHubEvent{
		EventType: "pull_request", Action: "opened", Owner: "owner", Repo: "repo", ResourceID: "1",
	})

	time.Sleep(100 * time.Millisecond)
	if got := client.submitCallCount(); got != 0 {
		t.Errorf("submitCallCount() = %d, want 0 (a PR-details fetch error must drop the event, not panic)", got)
	}
}

func TestDaemonEventSink_UnparseableResourceID_DropsWithoutPanic(t *testing.T) {
	client := newFakeLister()
	d := newTestDaemonForEvents(client)
	sink := &daemonEventSink{daemon: d}

	sink.Handle(context.Background(), events.GitHubEvent{
		EventType: "pull_request", Action: "opened", Owner: "owner", Repo: "repo", ResourceID: "not-a-number",
	})
	// Must not panic; nothing to assert beyond that.
}

// TestDaemonEventSink_DuplicateDeliverySameSHA_OnlyOneSubmit is the issue's
// core "at-least-once delivery is safe" assertion at the sink layer: even
// if an un-deduped duplicate reaches Handle twice for the same PR at the
// same head SHA, ReviewPR's own SHA-idempotency (alreadyReviewedAtHead)
// must prevent a second SubmitPRReview call.
func TestDaemonEventSink_DuplicateDeliverySameSHA_OnlyOneSubmit(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"}}
	d := newTestDaemonForEvents(client)
	sink := &daemonEventSink{daemon: d}

	ev := events.GitHubEvent{EventType: "pull_request", Action: "opened", Owner: "owner", Repo: "repo", ResourceID: "1"}
	sink.Handle(context.Background(), ev)
	waitUntil(t, 2*time.Second, func() bool { return client.submitCallCount() == 1 })

	// Simulate GitHub now reflecting the just-submitted review at this SHA
	// — this is what makes a second delivery of the same event safe in
	// production (GitHub-derived state, not payload trust).
	client.mu.Lock()
	client.reviews = []gh.PRReview{{Author: d.BotLogin, CommitID: "sha1", State: "COMMENTED"}}
	client.mu.Unlock()

	sink.Handle(context.Background(), ev) // duplicate delivery
	time.Sleep(150 * time.Millisecond)    // let a wrongly-dispatched second review land, if any
	if got := client.submitCallCount(); got != 1 {
		t.Errorf("submitCallCount() = %d, want 1 (duplicate delivery at the same SHA must not double-review)", got)
	}
}

// TestDaemonEventSink_Handle_ReturnsPromptlyUnderSemaphoreSaturation guards
// against a regression where Handle blocked synchronously acquiring
// reviewOne's semaphore slot. Handle is called inline from hookdeck.Source's
// WebSocket read loop (see source.go's handleFrame/ack): blocking there would
// delay acking the current webhook and stall reading every subsequent one,
// exactly what the issue's "ack the webhook promptly ... never run a review
// synchronously in the webhook receiver" requirement forbids.
func TestDaemonEventSink_Handle_ReturnsPromptlyUnderSemaphoreSaturation(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = []gh.PRDetails{
		{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"},
		{Number: 2, Author: "alice", HeadSHA: "sha2", State: "open"},
	}

	release := make(chan struct{})
	started := make(chan struct{}, 2)
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		started <- struct{}{}
		<-release
		return ReviewResult{Text: "mock review"}, nil
	}}
	clone := func(ctx context.Context, owner, repo, token string, prNumber int) (string, func(), error) {
		return "/tmp", func() {}, nil
	}
	d := &Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: 1},
		BotLogin: "pruefer-bot[bot]",
	}
	sink := &daemonEventSink{daemon: d}

	// Saturate the daemon's single concurrency slot with PR #1's review.
	sink.Handle(context.Background(), events.GitHubEvent{
		EventType: "pull_request", Action: "opened", Owner: "owner", Repo: "repo", ResourceID: "1",
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("PR #1's review never started")
	}
	defer close(release)

	// PR #2's event arrives while the only concurrency slot is held: Handle
	// must return immediately rather than blocking on reviewOne's semaphore
	// acquire.
	done := make(chan struct{})
	start := time.Now()
	go func() {
		sink.Handle(context.Background(), events.GitHubEvent{
			EventType: "pull_request", Action: "opened", Owner: "owner", Repo: "repo", ResourceID: "2",
		})
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Errorf("Handle took %s to return while the semaphore was saturated; want near-instant", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Handle blocked on the saturated semaphore instead of returning promptly")
	}
}

// TestDaemonEventSink_DuplicateInFlightEvent_DroppedWithoutConsumingSemaphore
// guards against a regression where a burst of duplicate/rapid webhook
// deliveries for one PR (e.g. several quick synchronize pushes before the
// first review finishes) could each spawn a goroutine that acquires a
// semaphore slot and then blocks waiting for the same PR's stripe lock —
// exhausting the concurrency budget on redundant waits instead of dropping
// immediately. With a 2-slot budget: PR #1's review holds one slot; three
// duplicate PR #1 events must each drop without consuming the second slot,
// which must remain free for PR #2's unrelated review to start promptly.
func TestDaemonEventSink_DuplicateInFlightEvent_DroppedWithoutConsumingSemaphore(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = []gh.PRDetails{
		{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"},
		{Number: 2, Author: "bob", HeadSHA: "sha2", State: "open"},
	}

	release := make(chan struct{})
	started := make(chan int, 4)
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		started <- req.PRNumber
		<-release
		return ReviewResult{Text: "mock review"}, nil
	}}
	clone := func(ctx context.Context, owner, repo, token string, prNumber int) (string, func(), error) {
		return "/tmp", func() {}, nil
	}
	d := &Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: 2},
		BotLogin: "pruefer-bot[bot]",
	}
	sink := &daemonEventSink{daemon: d}

	// Start PR #1's review, occupying one of two slots.
	sink.Handle(context.Background(), events.GitHubEvent{
		EventType: "pull_request", Action: "opened", Owner: "owner", Repo: "repo", ResourceID: "1",
	})
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("PR #1's review never started")
	}

	// Fire several duplicate events for the SAME in-flight PR — each must
	// drop immediately (non-blocking prLock claim fails) rather than
	// spawning a goroutine that holds the second slot waiting on that lock.
	for i := 0; i < 3; i++ {
		sink.Handle(context.Background(), events.GitHubEvent{
			EventType: "pull_request", Action: "synchronize", Owner: "owner", Repo: "repo", ResourceID: "1",
		})
	}

	// PR #2 is unrelated: its event must still claim the second slot and
	// start promptly. If the duplicates above had consumed it instead, this
	// would time out.
	sink.Handle(context.Background(), events.GitHubEvent{
		EventType: "pull_request", Action: "opened", Owner: "owner", Repo: "repo", ResourceID: "2",
	})
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("PR #2's review did not start promptly — duplicate PR #1 events may have consumed the free semaphore slot")
	}

	close(release)
	waitUntil(t, 10*time.Second, func() bool { return client.submitCallCount() == 2 })
}

// TestDaemonEventSink_ClosedOrMergedPR_DropsWithoutReview guards against a
// webhook arriving for a PR that's since closed or merged (e.g. synchronize
// immediately followed by merge, or a stale/delayed delivery). Unlike
// poll(), which only ever sees PRs ListOpenPRs already filtered to open,
// ReviewFromEvent fetches by number regardless of current state — and
// Eligible/ReviewPR do not check PR state themselves — so without an
// explicit guard this would proceed to clone and submit a formal review
// against a closed/merged PR.
func TestDaemonEventSink_ClosedOrMergedPR_DropsWithoutReview(t *testing.T) {
	for _, tc := range []struct {
		name string
		pr   gh.PRDetails
	}{
		{name: "closed", pr: gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1", State: "closed"}},
		{name: "merged", pr: gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1", State: "closed", Merged: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeLister()
			client.detailsByKey["owner/repo#1"] = &tc.pr
			d := newTestDaemonForEvents(client)
			sink := &daemonEventSink{daemon: d}

			sink.Handle(context.Background(), events.GitHubEvent{
				EventType: "pull_request", Action: "synchronize", Owner: "owner", Repo: "repo", ResourceID: "1",
			})

			time.Sleep(100 * time.Millisecond)
			if got := client.submitCallCount(); got != 0 {
				t.Errorf("submitCallCount() = %d, want 0 (a %s PR must be dropped, not reviewed)", got, tc.name)
			}
		})
	}
}

// TestDaemonEventSink_ReviewFromEvent_AcquiresSemaphoreBeforePRLock guards
// against a lock-ordering inversion vs. the poll path: reviewOne/runReview
// acquire the semaphore first, then block on the PR's stripe lock inside
// the spawned goroutine. If ReviewFromEvent acquired those two resources in
// the opposite order (PR lock, then semaphore), it would be a classic AB-BA
// deadlock risk — under semaphore saturation, an event goroutine holding a
// PR's lock while blocked on the semaphore can race a poll goroutine that
// wins a freed slot and then blocks on that same PR's lock, permanently
// consuming the slot; if this recurs across enough distinct PRs to fill
// every slot, the whole daemon wedges with no timeout or ctx-based escape.
//
// This test holds PR #1's stripe lock directly (simulating some other
// in-flight review) and saturates the daemon's single concurrency slot
// with a bystander review of an unrelated PR. If ReviewFromEvent checked
// the PR lock before the semaphore, its (non-blocking) lock attempt would
// fail immediately and it would return without ever touching the — fully
// saturated — semaphore, indistinguishable from this test's perspective.
// Correct (semaphore-first) ordering instead leaves it blocked queuing for
// the semaphore until the bystander's slot is freed.
func TestDaemonEventSink_ReviewFromEvent_AcquiresSemaphoreBeforePRLock(t *testing.T) {
	bystanderPR := gh.PRDetails{Number: 99, Author: "bystander", HeadSHA: "shaY", State: "open"}
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = []gh.PRDetails{bystanderPR}

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		started <- struct{}{}
		<-release
		return ReviewResult{Text: "mock review"}, nil
	}}
	clone := func(ctx context.Context, owner, repo, token string, prNumber int) (string, func(), error) {
		return "/tmp", func() {}, nil
	}
	d := &Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: 1},
		BotLogin: "pruefer-bot[bot]",
	}

	// Saturate the daemon's single concurrency slot with a bystander review
	// of an unrelated PR (#99), the same dispatch path poll() itself uses.
	var wg sync.WaitGroup
	d.reviewOne(context.Background(), &wg, client, "owner", "repo", bystanderPR)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("bystander review never started")
	}

	// Simulate another in-flight review of PR #1 by holding its gate
	// directly — the same state runReview would have already established
	// for a real in-flight review.
	gate, releaseGate := d.acquirePRGate("owner", "repo", 1)
	gate.mu.Lock()
	defer releaseGate()
	defer gate.mu.Unlock()

	done := make(chan struct{})
	go func() {
		d.ReviewFromEvent(context.Background(), "owner", "repo", 1)
		close(done)
	}()

	// With the only slot saturated, ReviewFromEvent must still be blocked
	// queuing for the semaphore, not already returned via an immediate
	// PR-lock failure.
	select {
	case <-done:
		t.Fatal("ReviewFromEvent returned before the semaphore was ever freed — it must be checking the PR lock before the semaphore (the deadlock-prone ordering)")
	case <-time.After(200 * time.Millisecond):
	}

	// Free the bystander's slot: ReviewFromEvent should now acquire it,
	// find PR #1's lock held, drop, and return promptly.
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReviewFromEvent did not return after the semaphore slot was freed")
	}
	waitUntil(t, 2*time.Second, func() bool { return client.submitCallCount() == 1 }) // only the bystander submitted
}

// TestDaemonEventSink_RecordsDropReasons covers the four drop reasons
// ReviewFromEvent originates (see events.DropReason and ADR-1563), each
// asserted at the same code paths TestDaemonEventSink_UnknownOwner_
// DropsWithoutPanic/TestDaemonEventSink_UnwatchedRepo_DropsWithoutReview/
// TestDaemonEventSink_ClosedOrMergedPR_DropsWithoutReview already exercise
// for their "no review happened" assertion — this test adds the "and it
// was counted, under the right reason" half.
func TestDaemonEventSink_RecordsDropReasons(t *testing.T) {
	t.Run("unwatched owner", func(t *testing.T) {
		client := newFakeLister()
		d := newTestDaemonForEvents(client)
		sink := &daemonEventSink{daemon: d}

		sink.Handle(context.Background(), events.GitHubEvent{
			EventType: "pull_request", Action: "opened", Owner: "unknown-owner", Repo: "repo", ResourceID: "1",
		})

		waitUntil(t, 2*time.Second, func() bool { return d.DropCounts()[events.DropUnwatchedOwner] == 1 })
	})

	t.Run("unwatched repo", func(t *testing.T) {
		client := newFakeLister()
		client.prsByRepo["owner/other-repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"}}
		d := newTestDaemonForEvents(client) // WatchedRepos: []string{"owner/repo"} only
		sink := &daemonEventSink{daemon: d}

		sink.Handle(context.Background(), events.GitHubEvent{
			EventType: "pull_request", Action: "opened", Owner: "owner", Repo: "other-repo", ResourceID: "1",
		})

		waitUntil(t, 2*time.Second, func() bool { return d.DropCounts()[events.DropUnwatchedRepo] == 1 })
	})

	t.Run("PR no longer open", func(t *testing.T) {
		client := newFakeLister()
		client.detailsByKey["owner/repo#1"] = &gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1", State: "closed"}
		d := newTestDaemonForEvents(client)
		sink := &daemonEventSink{daemon: d}

		sink.Handle(context.Background(), events.GitHubEvent{
			EventType: "pull_request", Action: "synchronize", Owner: "owner", Repo: "repo", ResourceID: "1",
		})

		time.Sleep(100 * time.Millisecond) // ReviewFromEvent runs in its own goroutine
		if got := d.DropCounts()[events.DropPRNotOpen]; got != 1 {
			t.Errorf("DropCounts()[DropPRNotOpen] = %d, want 1", got)
		}
	})

	t.Run("review already in flight", func(t *testing.T) {
		client := newFakeLister()
		client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"}}
		d := newTestDaemonForEvents(client)

		gate, releaseGate := d.acquirePRGate("owner", "repo", 1)
		gate.mu.Lock()
		defer releaseGate()
		defer gate.mu.Unlock()

		d.ReviewFromEvent(context.Background(), "owner", "repo", 1)

		if got := d.DropCounts()[events.DropReviewInFlight]; got != 1 {
			t.Errorf("DropCounts()[DropReviewInFlight] = %d, want 1", got)
		}
	})
}
