package pruefer

import (
	"context"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	ptui "github.com/handarbeit/fabrik/pruefer/tui"
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

func (c *concurrencyTrackingInvoker) Review(ctx context.Context, req ReviewRequest) (ReviewResult, error) {
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
	return ReviewResult{Text: "reviewed"}, nil
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
		Clients:  map[string]GitHubLister{"owner": client},
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
		Clients:  map[string]GitHubLister{"owner": client},
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
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	d := &Daemon{
		Clients:  map[string]GitHubLister{},
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
		Clients:  map[string]GitHubLister{"owner": client},
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

// TestDaemonPoll_RoutesEachRepoThroughOwnersToken is the multi-installation
// routing test called for in the issue's Testing section: two owners, two
// distinct fakeLister clients with distinct tokens, watched repos spanning
// both — every repo's diff/submit/clone calls must only ever touch its own
// owner's client/token, never the other's. A misrouted repo would either hit
// the wrong fakeLister (asserted via per-client submitCallCount) or clone
// with the wrong token (asserted via the clone spy).
func TestDaemonPoll_RoutesEachRepoThroughOwnersToken(t *testing.T) {
	clientA := newFakeLister()
	clientA.token = "tok-ownerA"
	clientA.prsByRepo["ownerA/repoA"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "shaA"}}

	clientB := newFakeLister()
	clientB.token = "tok-ownerB"
	clientB.prsByRepo["ownerB/repoB"] = []gh.PRDetails{{Number: 2, Author: "bob", HeadSHA: "shaB"}}

	claude := &mockClaudeInvoker{}

	var cloneMu sync.Mutex
	var cloneTokens []string
	clone := func(ctx context.Context, owner, repo, token string, prNumber int) (string, func(), error) {
		cloneMu.Lock()
		cloneTokens = append(cloneTokens, owner+"/"+repo+"="+token)
		cloneMu.Unlock()
		return t.TempDir(), func() {}, nil
	}

	d := &Daemon{
		// Clients is keyed by lower-cased owner — see distinctOwners' and
		// poll()'s doc comments for why (execute.go, the real caller,
		// always lower-cases when populating this map).
		Clients: map[string]GitHubLister{"ownera": clientA, "ownerb": clientB},
		Claude:  claude,
		Clone:   clone,
		Config: Config{
			WatchedRepos:   []string{"ownerA/repoA", "ownerB/repoB"},
			ConcurrencyCap: 3,
		},
		BotLogin: "pruefer-bot[bot]",
	}
	d.poll(context.Background())

	if clientA.submitCallCount() != 1 {
		t.Errorf("clientA (ownerA) submitCallCount = %d, want 1", clientA.submitCallCount())
	}
	if clientB.submitCallCount() != 1 {
		t.Errorf("clientB (ownerB) submitCallCount = %d, want 1", clientB.submitCallCount())
	}
	for _, call := range clientA.submitCalls {
		if call.owner != "ownerA" {
			t.Errorf("clientA received a submit call for owner %q — should only ever see ownerA", call.owner)
		}
	}
	for _, call := range clientB.submitCalls {
		if call.owner != "ownerB" {
			t.Errorf("clientB received a submit call for owner %q — should only ever see ownerB", call.owner)
		}
	}

	cloneMu.Lock()
	defer cloneMu.Unlock()
	wantTokens := map[string]bool{"ownerA/repoA=tok-ownerA": true, "ownerB/repoB=tok-ownerB": true}
	if len(cloneTokens) != 2 {
		t.Fatalf("clone calls = %v, want exactly 2", cloneTokens)
	}
	for _, ct := range cloneTokens {
		if !wantTokens[ct] {
			t.Errorf("unexpected clone token pairing %q — each repo must clone with its own owner's token", ct)
		}
	}
}

// buildParityFixture returns a fresh Daemon (with the given Emit hook) plus
// its client/claude fakes, seeded with a fixed mix of PRs: one eligible for
// review, one skipped as draft, and one that fails to clone (a genuine
// error). Used by TestDaemonPoll_TUIOffAndOnProduceIdenticalDecisions to
// build two independent runs from an identical starting fixture.
func buildParityFixture(t *testing.T, emit func(ptui.Event)) (*Daemon, *fakeLister, *mockClaudeInvoker) {
	t.Helper()
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = []gh.PRDetails{
		{Number: 1, Author: "alice", HeadSHA: "sha1"},
		{Number: 2, Author: "alice", HeadSHA: "sha2", Draft: true},
	}
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)
	d := &Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
		Emit:     emit,
	}
	return d, client, claude
}

// TestDaemonPoll_TUIOffAndOnProduceIdenticalDecisions asserts the issue's
// core "no behaviour coupling" requirement: running poll() with Daemon.Emit
// nil (TUI-off/headless) versus set to a recorder (TUI-on) must produce
// identical review decisions — same submit count, same claude invocation
// count — for the same inputs. The TUI-on run must also actually observe
// events, proving the wiring is exercised rather than trivially satisfied by
// both runs doing nothing.
func TestDaemonPoll_TUIOffAndOnProduceIdenticalDecisions(t *testing.T) {
	dOff, clientOff, claudeOff := buildParityFixture(t, nil)
	dOff.poll(context.Background())

	var mu sync.Mutex
	var events []ptui.Event
	dOn, clientOn, claudeOn := buildParityFixture(t, func(ev ptui.Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	dOn.poll(context.Background())

	if clientOff.submitCallCount() != 1 {
		t.Fatalf("TUI-off: submitCallCount() = %d, want 1 (only the non-draft PR reviewed)", clientOff.submitCallCount())
	}
	if clientOn.submitCallCount() != clientOff.submitCallCount() {
		t.Errorf("submitCallCount mismatch: TUI-off=%d, TUI-on=%d, want equal", clientOff.submitCallCount(), clientOn.submitCallCount())
	}
	if claudeOn.callCount() != claudeOff.callCount() {
		t.Errorf("claude callCount mismatch: TUI-off=%d, TUI-on=%d, want equal", claudeOff.callCount(), claudeOn.callCount())
	}

	mu.Lock()
	n := len(events)
	mu.Unlock()
	if n == 0 {
		t.Error("TUI-on run emitted no events — Emit wiring is not actually exercised by this test")
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

func TestDistinctOwners(t *testing.T) {
	got := distinctOwners([]string{"a/one", "b/two", "a/three", "malformed", "b/four"})
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("distinctOwners = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("distinctOwners[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDaemonRun_LockPreventsSecondInstance(t *testing.T) {
	dir := t.TempDir()
	client := newFakeLister()
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	d1 := &Daemon{Clients: map[string]GitHubLister{"owner": client}, Claude: claude, Clone: clone, Config: Config{PollInterval: time.Hour}, FabrikDir: dir}
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	errCh := make(chan error, 1)
	go func() { errCh <- d1.Run(ctx1) }()

	// Wait until d1 actually holds the lock, rather than assuming a fixed
	// sleep is long enough — probe the same lock file ourselves.
	waitUntil(t, 2*time.Second, func() bool { return lockHeld(t, d1.lockPath()) })

	d2 := &Daemon{Clients: map[string]GitHubLister{"owner": client}, Claude: claude, Clone: clone, Config: Config{PollInterval: time.Hour}, FabrikDir: dir}
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

// lockHeld reports whether some other process/goroutine currently holds an
// exclusive flock on path, by attempting (and immediately releasing) a
// non-blocking flock of our own.
func lockHeld(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false // lock file/dir may not exist yet
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
