package pruefer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/githubauth"
	ptui "github.com/handarbeit/fabrik/pruefer/tui"
)

// testDerivedRepoSet builds a githubauth.DerivedRepoSet directly from a list
// of "owner/repo" strings, treating every well-formed entry as fully
// granted with no filtering/capping — the "no derivation, just trust the
// list" shape most pre-#1641 test fixtures assumed when they set
// Config.WatchedRepos directly. A malformed entry is silently skipped,
// mirroring internal/githubauth.derivedRepoFromFullName's own handling.
// Tests that need FilteredOut/Truncated/Capped semantics construct a
// githubauth.DerivedRepoSet directly instead of using this helper.
func testDerivedRepoSet(repos ...string) githubauth.DerivedRepoSet {
	var out []githubauth.DerivedRepo
	for _, spec := range repos {
		owner, name, ok := strings.Cut(spec, "/")
		if !ok || owner == "" || name == "" {
			continue
		}
		out = append(out, githubauth.DerivedRepo{Repo: spec, Owner: owner, RepoName: name})
	}
	return githubauth.DerivedRepoSet{Repos: out}
}

// withDerivedRepos is a small test-fixture helper: it seeds d's derived
// state directly from repos (see testDerivedRepoSet) and returns d, so a
// test can build a Daemon{} literal and apply the derived-set migration in
// one expression — e.g. `d := withDerivedRepos(&Daemon{...}, "owner/repo")`.
// See the pruefer test-fixture migration note in ADR-1641.
func withDerivedRepos(d *Daemon, repos ...string) *Daemon {
	d.derived = testDerivedRepoSet(repos...)
	return d
}

// fakeLister extends fakeReviewer with ListOpenPRs and FetchPRDetails,
// keyed by "owner/repo".
type fakeLister struct {
	*fakeReviewer
	mu            sync.Mutex
	prsByRepo     map[string][]gh.PRDetails
	listErrByRepo map[string]error

	// detailsByKey/detailsErrByKey override FetchPRDetails for a specific
	// "owner/repo#N", taking precedence over falling back to prsByRepo.
	detailsByKey    map[string]*gh.PRDetails
	detailsErrByKey map[string]error

	// listOpenPRsCalls counts ListOpenPRs invocations; listOpenPRsBlock,
	// when non-nil, is received from before each call returns — letting a
	// test hold a poll cycle open long enough to observe reconciliation-poll
	// coalescing. listOpenPRsHistory records the "owner/repo" key of every
	// call in order, so a test can assert which specific repos were (or
	// were not) polled in a given cycle — e.g. a reload-removed repo must
	// not appear in the next cycle's history (#1640, AC2).
	listOpenPRsCalls   int
	listOpenPRsBlock   <-chan struct{}
	listOpenPRsHistory []string
}

func newFakeLister() *fakeLister {
	return &fakeLister{
		fakeReviewer:    newFakeReviewer(),
		prsByRepo:       map[string][]gh.PRDetails{},
		listErrByRepo:   map[string]error{},
		detailsByKey:    map[string]*gh.PRDetails{},
		detailsErrByKey: map[string]error{},
	}
}

func (f *fakeLister) ListOpenPRs(owner, repo string) ([]gh.PRDetails, error) {
	key := owner + "/" + repo
	f.mu.Lock()
	f.listOpenPRsCalls++
	f.listOpenPRsHistory = append(f.listOpenPRsHistory, key)
	block := f.listOpenPRsBlock
	err := f.listErrByRepo[key]
	prs := f.prsByRepo[key]
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if err != nil {
		return nil, err
	}
	return prs, nil
}

func (f *fakeLister) listOpenPRsCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listOpenPRsCalls
}

// listOpenPRsHistorySnapshot returns a copy of every "owner/repo" key
// ListOpenPRs has been called with, in order.
func (f *fakeLister) listOpenPRsHistorySnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.listOpenPRsHistory))
	copy(out, f.listOpenPRsHistory)
	return out
}

// FetchPRDetails returns the PR configured via detailsByKey for
// "owner/repo#N" if present, otherwise falls back to searching prsByRepo
// (mirroring a real client, where FetchPRDetails and ListOpenPRs observe
// the same underlying GitHub state) — so tests seeding only prsByRepo don't
// also need to duplicate that data into detailsByKey.
func (f *fakeLister) FetchPRDetails(owner, repo string, prNumber int) (*gh.PRDetails, error) {
	key := fmt.Sprintf("%s/%s#%d", owner, repo, prNumber)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.detailsErrByKey[key]; err != nil {
		return nil, err
	}
	if pr, ok := f.detailsByKey[key]; ok {
		return pr, nil
	}
	for _, pr := range f.prsByRepo[owner+"/"+repo] {
		if pr.Number == prNumber {
			prCopy := pr
			return &prCopy, nil
		}
	}
	return nil, fmt.Errorf("fakeLister: no PR #%d configured for %s/%s", prNumber, owner, repo)
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

	d := withDerivedRepos(&Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: 2},
		BotLogin: "pruefer-bot[bot]",
	}, "owner/repo")

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

	d := withDerivedRepos(&Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"owner/repo"}, MaxDiffBytes: 5, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
	}, "owner/repo")
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

// TestDaemonPoll_SkipsMalformedWatchedRepo covers an empty derived set
// (e.g. a malformed watched_repos entry, which internal/githubauth's own
// owner-parsing already skips and logs before Derive ever runs — see
// distinctOwnersLogging/derivedRepoFromFullName) — poll() must not panic.
func TestDaemonPoll_SkipsMalformedWatchedRepo(t *testing.T) {
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	d := withDerivedRepos(&Daemon{
		Clients:  map[string]GitHubLister{},
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"not-a-valid-repo-spec"}},
		BotLogin: "pruefer-bot[bot]",
	}, "not-a-valid-repo-spec")
	d.poll(context.Background()) // must not panic
}

func TestDaemonPoll_ContinuesAfterOneRepoListFails(t *testing.T) {
	client := newFakeLister()
	client.listErrByRepo["owner/broken"] = fmt.Errorf("500 internal server error")
	client.prsByRepo["owner/good"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1"}}
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)

	d := withDerivedRepos(&Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"owner/broken", "owner/good"}, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
	}, "owner/broken", "owner/good")
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

	d := withDerivedRepos(&Daemon{
		// Clients is keyed by lower-cased owner — see poll()'s and
		// rederiveRepos' doc comments for why (execute.go, the real caller,
		// always lower-cases when populating this map).
		Clients: map[string]GitHubLister{"ownera": clientA, "ownerb": clientB},
		Claude:  claude,
		Clone:   clone,
		Config: Config{
			WatchedRepos:   []string{"ownerA/repoA", "ownerB/repoB"},
			ConcurrencyCap: 3,
		},
		BotLogin: "pruefer-bot[bot]",
	}, "ownerA/repoA", "ownerB/repoB")
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
	d := withDerivedRepos(&Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   claude,
		Clone:    clone,
		Config:   Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
		Emit:     emit,
	}, "owner/repo")
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

// TestDaemonRun_PreAcquiredLockNotReacquiredOrReleased covers the hand-off
// execute.go relies on: it acquires the lock itself (mirroring Execute's
// call to acquireLock before githubauth.Reconcile), hands it to Daemon via
// preAcquiredLock, and confirms Run neither tries to acquire its own lock
// (which would fail — flock is per-open-file-description, so a second
// os.OpenFile+Flock from the same process on the same path blocks/fails
// exactly as it would from a second process) nor releases the caller's
// lock when it returns (releasing it is the caller's responsibility, since
// the caller may need the lock to stay held past Run's own lifetime — e.g.
// Execute's defer runs after Run returns). Without this, a Daemon built the
// way Execute builds it (preAcquiredLock set) could either fail to start
// (if Run ignored the field) or leave the lock unexpectedly released out
// from under a caller still relying on it.
func TestDaemonRun_PreAcquiredLockNotReacquiredOrReleased(t *testing.T) {
	dir := t.TempDir()
	lockFile, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("acquireLock: %v", err)
	}
	defer func() {
		if lockFile != nil {
			releaseLock(lockFile)
		}
	}()

	client := newFakeLister()
	claude := &mockClaudeInvoker{}
	clone, _ := fakeClone(t, nil)
	d := &Daemon{
		Clients:         map[string]GitHubLister{"owner": client},
		Claude:          claude,
		Clone:           clone,
		Config:          Config{PollInterval: time.Hour},
		FabrikDir:       dir,
		preAcquiredLock: lockFile,
	}

	// Cancelled up front rather than after a sleep: Run's own loop calls
	// poll() once synchronously before ever reaching its ctx.Done() select,
	// so if Run wrongly tried to reacquire the lock we already hold, it
	// would fail immediately inside that first call — no warm-up delay is
	// needed to observe that failure, and this avoids depending on an
	// arbitrary sleep duration being "long enough."
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run with a pre-acquired lock returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}

	// The lock must still be held by us — Run must not have released it.
	if !lockHeld(t, d.lockPath()) {
		t.Error("lock was released by Run, but releasing a preAcquiredLock is the caller's responsibility")
	}

	if err := releaseLock(lockFile); err != nil {
		t.Fatalf("releaseLock: %v", err)
	}
	lockFile = nil // released; skip the deferred double-release
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

// releaseCheckCounter starts a test server standing in for GitHub's release
// API, counting hits and always reporting "up to date" (same tag as the
// running version) so no download/exec is ever attempted.
func releaseCheckCounter(t *testing.T) (srv *httptest.Server, hits func() int) {
	t.Helper()
	var mu sync.Mutex
	count := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		fmt.Fprint(w, `{"tag_name": "v0.0.1", "assets": []}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return count
	}
}

// TestDaemonRun_UpgradeCheckFiresAfterPollWhenEnabled verifies that Run
// invokes the upgrade check at the poll boundary when Config.AutoUpgrade is
// set, and that the 30-minute throttle suppresses repeat checks across
// several fast poll cycles within the same Run call.
func TestDaemonRun_UpgradeCheckFiresAfterPollWhenEnabled(t *testing.T) {
	srv, hits := releaseCheckCounter(t)

	origBase := releaseAPIBaseURL
	releaseAPIBaseURL = srv.URL
	t.Cleanup(func() { releaseAPIBaseURL = origBase })
	setVersion(t, "v0.0.1")

	d := &Daemon{
		Config:    Config{PollInterval: 15 * time.Millisecond, AutoUpgrade: true},
		FabrikDir: t.TempDir(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := hits(); got != 1 {
		t.Errorf("release-check hits = %d, want exactly 1 (throttle should suppress repeats across fast poll cycles)", got)
	}
}

// TestDaemonRun_UpgradeCheckSkippedWhenDisabled verifies that Run never
// invokes the upgrade check when Config.AutoUpgrade is false (the default) —
// an operator must opt in.
func TestDaemonRun_UpgradeCheckSkippedWhenDisabled(t *testing.T) {
	srv, hits := releaseCheckCounter(t)

	origBase := releaseAPIBaseURL
	releaseAPIBaseURL = srv.URL
	t.Cleanup(func() { releaseAPIBaseURL = origBase })
	setVersion(t, "v0.0.1")

	d := &Daemon{
		Config:    Config{PollInterval: 15 * time.Millisecond, AutoUpgrade: false},
		FabrikDir: t.TempDir(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := hits(); got != 0 {
		t.Errorf("release-check hits = %d, want 0 (AutoUpgrade is false)", got)
	}
}

// TestDaemonRun_UpgradeCheckSkippedWhenContextAlreadyCancelled guards the
// ctx.Err() == nil guard in Run's loop: neither selfupgrade helper takes a
// context and a successful upgrade ends in syscall.Exec, so once started
// nothing can abort it. This reproduces the shutdown race the guard exists
// for — a cancellation that has already landed by the time poll() returns —
// and asserts the upgrade check is suppressed rather than firing and
// silently discarding the pending shutdown.
func TestDaemonRun_UpgradeCheckSkippedWhenContextAlreadyCancelled(t *testing.T) {
	srv, hits := releaseCheckCounter(t)

	origBase := releaseAPIBaseURL
	releaseAPIBaseURL = srv.URL
	t.Cleanup(func() { releaseAPIBaseURL = origBase })
	setVersion(t, "v0.0.1")

	d := &Daemon{
		Config:    Config{PollInterval: 15 * time.Millisecond, AutoUpgrade: true},
		FabrikDir: t.TempDir(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before Run's first poll() even starts

	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := hits(); got != 0 {
		t.Errorf("release-check hits = %d, want 0 (ctx was already cancelled before the poll boundary)", got)
	}
}
