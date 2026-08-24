package pruefer

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// TestDaemon_ApplyReload_WatchedReposAddedPolledNextCycle covers AC1: a
// repo added to watched_repos is polled on the very next poll cycle after
// ApplyReload, with no restart — asserted against a test daemon (not
// manual observation, per the AC's own wording).
func TestDaemon_ApplyReload_WatchedReposAddedPolledNextCycle(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["owner/repo1"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1"}}
	client.prsByRepo["owner/repo2"] = []gh.PRDetails{{Number: 2, Author: "alice", HeadSHA: "sha2"}}

	d := &Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   &mockClaudeInvoker{},
		Clone:    mustFakeClone(t),
		Config:   Config{WatchedRepos: []string{"owner/repo1"}, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
	}

	d.poll(context.Background())
	if hist := client.listOpenPRsHistorySnapshot(); len(hist) != 1 || hist[0] != "owner/repo1" {
		t.Fatalf("first cycle history = %v, want [owner/repo1] only", hist)
	}

	merged, _ := applyConfigReload(d.config(), Config{WatchedRepos: []string{"owner/repo1", "owner/repo2"}, ConcurrencyCap: 3})
	d.ApplyReload(merged, nil, nil) // same owner already has a client — nothing new to add

	d.poll(context.Background())
	hist := client.listOpenPRsHistorySnapshot()
	if len(hist) != 3 {
		t.Fatalf("history after reload = %v, want 3 entries (repo1, then repo1+repo2)", hist)
	}
	if hist[1] != "owner/repo1" || hist[2] != "owner/repo2" {
		t.Errorf("second cycle polled %v, want [owner/repo1 owner/repo2]", hist[1:])
	}
}

// TestDaemon_ApplyReload_RemovedRepoStopsPollingButInFlightReviewCompletes
// covers AC2: a repo removed from watched_repos stops being polled on the
// next cycle, and a review already dispatched against it while the reload
// lands completes rather than being cancelled (R3).
func TestDaemon_ApplyReload_RemovedRepoStopsPollingButInFlightReviewCompletes(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["owner/repoA"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1"}}
	client.prsByRepo["owner/repoB"] = []gh.PRDetails{{Number: 2, Author: "alice", HeadSHA: "sha2"}}

	claude := newConcurrencyTrackingInvoker()

	d := &Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   claude,
		Clone:    mustFakeClone(t),
		Config:   Config{WatchedRepos: []string{"owner/repoA", "owner/repoB"}, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
	}

	done := make(chan struct{})
	go func() {
		d.poll(context.Background())
		close(done)
	}()

	// Wait until both reviews (repoA's and repoB's) have actually
	// dispatched and are blocked on claude.release, so the reload below
	// genuinely lands while repoA's review is in flight.
	waitUntil(t, 2*time.Second, func() bool { return claude.currentCount() == 2 })

	merged, _ := applyConfigReload(d.config(), Config{WatchedRepos: []string{"owner/repoB"}, ConcurrencyCap: 3})
	d.ApplyReload(merged, nil, nil)

	close(claude.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not complete within 5s after releasing all invocations")
	}

	if got := client.submitCallCount(); got != 2 {
		t.Errorf("submitCallCount = %d, want 2 — repoA's review (removed mid-flight) must still complete, not be cancelled", got)
	}

	// The next cycle no longer polls repoA at all.
	d.poll(context.Background())
	hist := client.listOpenPRsHistorySnapshot()
	for _, h := range hist[2:] { // first two entries are the initial cycle's repoA/repoB
		if h == "owner/repoA" {
			t.Errorf("owner/repoA was polled again after being removed from watched_repos: history = %v", hist)
		}
	}
}

// TestDaemon_Semaphore_ConcurrencyCapLiveResize confirms concurrency_cap
// grows/shrinks the semaphore returned by semaphore() following an
// ApplyReload, without needing a restart.
func TestDaemon_Semaphore_ConcurrencyCapLiveResize(t *testing.T) {
	d := &Daemon{Config: Config{ConcurrencyCap: 2}}

	if cap(d.semaphore()) != 2 {
		t.Fatalf("initial semaphore cap = %d, want 2", cap(d.semaphore()))
	}

	d.ApplyReload(Config{ConcurrencyCap: 5}, nil, nil)
	if got := cap(d.semaphore()); got != 5 {
		t.Errorf("semaphore cap after growing ConcurrencyCap = %d, want 5", got)
	}

	d.ApplyReload(Config{ConcurrencyCap: 1}, nil, nil)
	if got := cap(d.semaphore()); got != 1 {
		t.Errorf("semaphore cap after shrinking ConcurrencyCap = %d, want 1", got)
	}
}

// TestDaemon_Semaphore_ResizeDoesNotDisturbInFlightHolder is the mechanism
// this issue's ADR documents: a holder that acquired a slot on the
// pre-resize channel keeps releasing into it, undisturbed by a concurrent
// resize — never blocking, never panicking on a send/receive against a
// channel nobody else references anymore.
func TestDaemon_Semaphore_ResizeDoesNotDisturbInFlightHolder(t *testing.T) {
	d := &Daemon{Config: Config{ConcurrencyCap: 1}}

	sem := d.semaphore()
	sem <- struct{}{} // acquire the only slot, as reviewOne would

	d.ApplyReload(Config{ConcurrencyCap: 3}, nil, nil) // resize while a holder is out

	release := make(chan struct{})
	go func() {
		<-sem // release into the *original* channel, exactly as reviewOne's deferred func() { <-sem }() does
		close(release)
	}()

	select {
	case <-release:
	case <-time.After(2 * time.Second):
		t.Fatal("release into the pre-resize semaphore channel did not complete — resize disturbed an in-flight holder")
	}
}

// TestDaemon_ConcurrentReloadDuringActivePoll exercises AC7 directly: a
// reload (ApplyReload) landing concurrently with an active poll cycle, run
// under -race. This is the surface #1640 introduces: Config and Clients
// were read-only-after-construction before this issue, from many
// goroutines with no synchronization; this test fails under `go test -race`
// against the pre-#1640 code (Config/Clients as plain fields with no cfgMu)
// specifically because both a reader (poll(), via d.config()/d.client())
// and a writer (ApplyReload) touch the same fields concurrently here — see
// #1640/AC7's own "must fail under -race against the pre-fix code" bar.
func TestDaemon_ConcurrentReloadDuringActivePoll(t *testing.T) {
	client := newFakeLister()
	for i := 1; i <= 20; i++ {
		client.prsByRepo["owner/repo"] = append(client.prsByRepo["owner/repo"], gh.PRDetails{
			Number: i, Author: "alice", HeadSHA: fmt.Sprintf("sha%d", i),
		})
	}

	d := &Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   &mockClaudeInvoker{},
		Clone:    mustFakeClone(t),
		Config:   Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: 4},
		BotLogin: "pruefer-bot[bot]",
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// One goroutine hammering poll() cycles...
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				d.poll(context.Background())
			}
		}
	}()

	// ...while another concurrently applies reloads that touch Config,
	// Clients, and the semaphore all at once.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			newCap := 2 + (i % 5)
			d.ApplyReload(Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: newCap}, map[string]GitHubLister{
				"owner": client, // re-affirm the same client, exercising the Clients write path too
			}, nil)
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// mustFakeClone is fakeClone with err always nil, for tests that don't need
// the returned call counter.
func mustFakeClone(t *testing.T) CloneFunc {
	t.Helper()
	fn, _ := fakeClone(t, nil)
	return fn
}

// --- handleReload integration tests ---

// setupReloadDaemon writes the given initial YAML, bootstraps auth and a
// Daemon against it via the real LoadConfig/BootstrapMulti/NewDaemon
// production path (not a hand-built Config literal — that would carry none
// of LoadConfig's resolved defaults, making a later reload's diff against
// it spuriously noisy), and returns the pieces handleReload needs. args is
// the flag slice a later call to handleReload should reuse, mirroring
// Execute()'s own single `args` capture.
func setupReloadDaemon(t *testing.T, dir, keyPath string, installations []gh.AppInstallation) (args []string, authSet *AuthSet, daemon *Daemon, srv *httptest.Server, fake *fakeAppServer, closeLog func() error) {
	t.Helper()
	srv, fake = newFakeAppServer("pruefer-bot", installations, func() time.Time { return time.Now().Add(time.Hour) })

	configPath := filepath.Join(dir, "config.yaml")
	args = []string{"-config", configPath, "-github-app-id", "42", "-github-app-private-key-path", keyPath}

	cfg, err := LoadConfig(args)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	authSet, err = BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}
	clients := map[string]GitHubLister{}
	for owner, c := range authSet.Clients {
		clients[owner] = c
	}
	daemon, closeLog = NewDaemon(cfg, clients, &RealClaudeInvoker{}, nil, authSet.BotLogin)
	return args, authSet, daemon, srv, fake, closeLog
}

// captureLogf swaps in a capturing Logf for the duration of the test and
// returns a snapshot function.
func captureLogf(t *testing.T) func() []string {
	resetLogf(t)
	var mu sync.Mutex
	var lines []string
	Logf = func(prNumber int, tag, format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(lines))
		copy(out, lines)
		return out
	}
}

// TestHandleReload_MalformedConfigLeavesRunningConfigUntouched covers AC3:
// a malformed config file on reload leaves the previously running config in
// effect and logs the parse error; no field is partially applied.
func TestHandleReload_MalformedConfigLeavesRunningConfigUntouched(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\nmodel: sonnet\n"), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	args, authSet, daemon, srv, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{{ID: 111, Account: "handarbeit"}})
	defer srv.Close()
	defer closeLog()
	snapshot := captureLogf(t)

	before := daemon.config()

	// Corrupt the file mid-write, as an operator saving a half-finished
	// edit would.
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos: [unterminated"), 0600); err != nil {
		t.Fatalf("writing malformed config: %v", err)
	}

	handleReload(context.Background(), args, authSet, daemon)

	if got := daemon.config(); !reflect.DeepEqual(got, before) {
		t.Errorf("daemon.config() = %+v, want the original config untouched by the failed reload: %+v", got, before)
	}

	found := false
	for _, l := range snapshot() {
		if strings.Contains(l, "config reload failed") {
			found = true
		}
	}
	if !found {
		t.Errorf("logLines = %v, want a line reporting the reload failure", snapshot())
	}
}

// TestHandleReload_NewOwnerMintsInstallationTokenAndPolledNextCycle covers
// AC5's success path: adding a repo under a previously-unwatched owner
// mints that owner's installation token as part of the reload, and the new
// owner's repo is servable (has a client) immediately after.
func TestHandleReload_NewOwnerMintsInstallationTokenAndPolledNextCycle(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n"), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	args, authSet, daemon, srv, fake, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "newowner"},
	})
	defer srv.Close()
	defer closeLog()
	captureLogf(t)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n  - newowner/repo\n"), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handleReload(ctx, args, authSet, daemon)

	if fake.mintCountFor(222) != 1 {
		t.Errorf("mintCountFor(222 = newowner) = %d, want 1 — reload must mint a token for the newly-watched owner", fake.mintCountFor(222))
	}
	if _, ok := daemon.client("newowner"); !ok {
		t.Error("expected daemon to have a servable client for newowner after reload")
	}
	got := daemon.config()
	if len(got.WatchedRepos) != 2 {
		t.Errorf("daemon.config().WatchedRepos = %v, want 2 entries", got.WatchedRepos)
	}
}

// TestHandleReload_NewOwnerNoInstallation_FailsWholeReloadAllOrNothing
// covers AC5's failure path and R5's all-or-nothing guarantee across a
// multi-owner batch: when one of two newly-watched owners has no App
// installation, neither new owner is registered and the running config is
// untouched — not even the owner that *would* have succeeded. (A token can
// still have been minted-then-abandoned for the earlier, successful owner
// before the batch as a whole failed — an accepted, documented trade-off,
// see adrs/1640-pruefer-config-reload.md — so this test asserts the
// commit-time invariant that actually matters: nothing observable through
// daemon/authSet reflects the failed owner's partial progress.)
func TestHandleReload_NewOwnerNoInstallation_FailsWholeReloadAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n"), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	// "goodowner" has an installation; "badowner" does not.
	args, authSet, daemon, srv, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "goodowner"},
	})
	defer srv.Close()
	defer closeLog()
	captureLogf(t)

	before := daemon.config()
	beforeInstallationCount := authSet.InstallationCount()

	newYAML := "watched_repos:\n  - handarbeit/fabrik\n  - goodowner/repo\n  - badowner/repo\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(newYAML), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}

	handleReload(context.Background(), args, authSet, daemon)

	if got := daemon.config(); !reflect.DeepEqual(got, before) {
		t.Errorf("daemon.config() changed despite a failed reload: got %+v, want unchanged %+v", got, before)
	}
	if _, ok := daemon.client("goodowner"); ok {
		t.Error("goodowner must not be registered — the whole reload failed because badowner had no installation (R5 all-or-nothing)")
	}
	if _, ok := authSet.Clients["goodowner"]; ok {
		t.Error("authSet.Clients must not contain goodowner — CommitOwner must never run for a batch that ultimately fails")
	}
	if authSet.InstallationCount() != beforeInstallationCount {
		t.Errorf("InstallationCount() = %d, want unchanged %d — a minted-but-uncommitted Auth must never be appended to auths", authSet.InstallationCount(), beforeInstallationCount)
	}
}

// TestHandleReload_LogsDiffSummary covers R6/AC6: the reload summary names
// every added/removed repo and every changed field.
func TestHandleReload_LogsDiffSummary(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\nmodel: sonnet\n"), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	args, authSet, daemon, srv, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{{ID: 111, Account: "handarbeit"}})
	defer srv.Close()
	defer closeLog()
	snapshot := captureLogf(t)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos: []\nmodel: opus\n"), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}
	handleReload(context.Background(), args, authSet, daemon)

	joined := strings.Join(snapshot(), "\n")
	if !strings.Contains(joined, "handarbeit/fabrik") {
		t.Errorf("log output missing removed repo name: %s", joined)
	}
	if !strings.Contains(joined, "Model") || !strings.Contains(joined, "sonnet") || !strings.Contains(joined, "opus") {
		t.Errorf("log output missing Model change (sonnet -> opus): %s", joined)
	}
}

// TestHandleReload_RemovedOwnerPrunesClientAndAuth is the handleReload-level
// proof for the #1640 review finding: dropping an owner's last watched repo
// via reload must leave that owner unresolvable through the daemon (not
// just absent from watched_repos) and must drop its Auth from authSet —
// otherwise its refresh goroutine keeps minting tokens for an owner Pruefer
// no longer watches, indefinitely.
func TestHandleReload_RemovedOwnerPrunesClientAndAuth(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n  - verveguy/otherrepo\n"), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	args, authSet, daemon, srv, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "verveguy"},
	})
	defer srv.Close()
	defer closeLog()
	captureLogf(t)

	if _, ok := daemon.client("verveguy"); !ok {
		t.Fatal("expected verveguy to be servable before the reload")
	}
	beforeInstallationCount := authSet.InstallationCount()

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n"), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}
	handleReload(context.Background(), args, authSet, daemon)

	if _, ok := daemon.client("verveguy"); ok {
		t.Error("expected verveguy to no longer be servable through the daemon after its last watched repo was removed")
	}
	if _, ok := authSet.Clients["verveguy"]; ok {
		t.Error("expected authSet.Clients to no longer contain verveguy after the reload")
	}
	if got := authSet.InstallationCount(); got != beforeInstallationCount-1 {
		t.Errorf("InstallationCount() = %d, want %d (verveguy's dedicated installation dropped)", got, beforeInstallationCount-1)
	}
	if _, ok := daemon.client("handarbeit"); !ok {
		t.Error("expected handarbeit (still watched) to remain servable")
	}
}
