package pruefer

import (
	"context"
	"errors"
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
	for _, action := range []string{"opened", "synchronize", "ready_for_review"} {
		t.Run(action, func(t *testing.T) {
			client := newFakeLister()
			client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1"}}
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
	client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1"}}
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
	client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1"}}
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
	client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1"}}
	d := newTestDaemonForEvents(client)
	sink := &daemonEventSink{daemon: d}

	sink.Handle(context.Background(), events.GitHubEvent{EventType: "installation_repositories", Action: "added"})

	// Handle dispatches the reconciliation poll in its own goroutine (like
	// the per-PR path), so the submission is not necessarily observable the
	// instant Handle returns — poll for it.
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
	client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1"}}
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
		{Number: 1, Author: "alice", HeadSHA: "sha1"},
		{Number: 2, Author: "alice", HeadSHA: "sha2"},
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
