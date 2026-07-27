package pruefer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// fakeLister extends fakeReviewer with ListOpenPRs, keyed by "owner/repo".
type fakeLister struct {
	*fakeReviewer
	mu            sync.Mutex
	prsByRepo     map[string][]gh.PRDetails
	listErrByRepo map[string]error
}

func newFakeLister() *fakeLister {
	return &fakeLister{
		fakeReviewer:  newFakeReviewer(),
		prsByRepo:     map[string][]gh.PRDetails{},
		listErrByRepo: map[string]error{},
	}
}

func (f *fakeLister) ListOpenPRs(owner, repo string) ([]gh.PRDetails, error) {
	key := owner + "/" + repo
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.listErrByRepo[key]; err != nil {
		return nil, err
	}
	return f.prsByRepo[key], nil
}

// concurrencyTrackingInvoker blocks each Review call on a shared gate until
// released, tracking the maximum number of concurrent in-flight calls.
type concurrencyTrackingInvoker struct {
	mu      sync.Mutex
	current int
	maxSeen int
	release chan struct{}
}

func newConcurrencyTrackingInvoker() *concurrencyTrackingInvoker {
	return &concurrencyTrackingInvoker{release: make(chan struct{})}
}

func (c *concurrencyTrackingInvoker) Review(ctx context.Context, req ReviewRequest) (string, error) {
	c.mu.Lock()
	c.current++
	if c.current > c.maxSeen {
		c.maxSeen = c.current
	}
	c.mu.Unlock()

	<-c.release

	c.mu.Lock()
	c.current--
	c.mu.Unlock()
	return "reviewed", nil
}

func (c *concurrencyTrackingInvoker) currentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *concurrencyTrackingInvoker) maxSeenCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxSeen
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestDaemonPoll_EnforcesConcurrencyCap(t *testing.T) {
	client := newFakeLister()
	var prs []gh.PRDetails
	for i := 1; i <= 6; i++ {
		prs = append(prs, gh.PRDetails{Number: i, Author: "alice", HeadSHA: fmt.Sprintf("sha%d", i)})
	}
	client.prsByRepo["owner/repo"] = prs

	claude := newConcurrencyTrackingInvoker()
	clone, _ := fakeClone(t, nil)

	d := &Daemon{
		Client:   client,
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: 2},
		BotLogin: "pruefer-bot[bot]",
	}

	done := make(chan struct{})
	go func() {
		d.poll(context.Background())
		close(done)
	}()

	// Wait until the cap is actually saturated before asserting on it —
	// otherwise we might sample before all cap-many goroutines have started.
	waitUntil(t, 2*time.Second, func() bool { return claude.currentCount() == 2 })
	if got := claude.currentCount(); got > 2 {
		t.Errorf("concurrent claude invocations = %d, want <= 2 (ConcurrencyCap)", got)
	}
	// Give any over-eager dispatch a moment to (wrongly) exceed the cap.
	time.Sleep(50 * time.Millisecond)
	if got := claude.currentCount(); got > 2 {
		t.Errorf("concurrent claude invocations = %d, want <= 2 (ConcurrencyCap)", got)
	}

	close(claude.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not complete within 5s after releasing all invocations")
	}

	if claude.maxSeenCount() > 2 {
		t.Errorf("max concurrent claude invocations observed = %d, want <= 2", claude.maxSeenCount())
	}
	if client.submitCallCount() != 6 {
		t.Errorf("expected all 6 eligible PRs to be reviewed, got %d submissions", client.submitCallCount())
	}
}

func TestDaemonPoll_DiffSizeGuardAppliesAcrossRepos(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1"}}
	client.diff = "this diff is way over the tiny cap configured below"
	claude := &mockClaudeInvoker{}
	clone, cloneCalls := fakeClone(t, nil)

	d := &Daemon{
		Client:   client,
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"owner/repo"}, MaxDiffBytes: 5, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
	}
	d.poll(context.Background())

	if cloneCalls.Load() != 0 {
		t.Error("expected the oversized-diff PR to skip before cloning")
	}
	if claude.callCount() != 0 {
		t.Error("expected the oversized-diff PR to skip before invoking claude")
	}
	if client.submitCallCount() != 0 {
		t.Error("expected no review submitted for the oversized-diff PR")
	}
}

func TestDaemonPoll_SkipsMalformedWatchedRepo(t *testing.T) {
	client := newFakeLister()
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	d := &Daemon{
		Client:   client,
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"not-a-valid-repo-spec"}},
		BotLogin: "pruefer-bot[bot]",
	}
	d.poll(context.Background()) // must not panic
}

func TestDaemonPoll_ContinuesAfterOneRepoListFails(t *testing.T) {
	client := newFakeLister()
	client.listErrByRepo["owner/broken"] = fmt.Errorf("500 internal server error")
	client.prsByRepo["owner/good"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1"}}
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	d := &Daemon{
		Client:   client,
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"owner/broken", "owner/good"}, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
	}
	d.poll(context.Background())

	if client.submitCallCount() != 1 {
		t.Errorf("expected the good repo's PR to still be reviewed despite the broken repo failing, got %d submissions", client.submitCallCount())
	}
}

func TestSplitOwnerRepo(t *testing.T) {
	cases := []struct {
		spec      string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		{"handarbeit/fabrik", "handarbeit", "fabrik", true},
		{"handarbeit", "", "", false},
		{"handarbeit/fabrik/extra", "", "", false},
		{"/fabrik", "", "", false},
		{"handarbeit/", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		owner, repo, ok := splitOwnerRepo(tc.spec)
		if ok != tc.wantOK || owner != tc.wantOwner || repo != tc.wantRepo {
			t.Errorf("splitOwnerRepo(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.spec, owner, repo, ok, tc.wantOwner, tc.wantRepo, tc.wantOK)
		}
	}
}

func TestDaemonRun_LockPreventsSecondInstance(t *testing.T) {
	dir := t.TempDir()
	client := newFakeLister()
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	d1 := &Daemon{Client: client, Claude: claude, Clone: clone, Config: Config{PollInterval: time.Hour}, FabrikDir: dir}
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	errCh := make(chan error, 1)
	go func() { errCh <- d1.Run(ctx1) }()

	// Give d1 a moment to acquire the lock.
	time.Sleep(100 * time.Millisecond)

	d2 := &Daemon{Client: client, Claude: claude, Clone: clone, Config: Config{PollInterval: time.Hour}, FabrikDir: dir}
	if err := d2.Run(context.Background()); err == nil {
		t.Fatal("expected the second Daemon.Run to fail while the first holds the lock")
	}

	cancel1()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("first Daemon.Run did not exit after context cancellation")
	}
}
