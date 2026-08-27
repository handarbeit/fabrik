package pruefer

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/githubauth"
)

// writeTestPrivateKey generates a small (test-only) RSA key, writes it as a
// PEM file under dir, and returns the file path.
func writeTestPrivateKey(t *testing.T, dir string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generating test RSA key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	path := filepath.Join(dir, "app-private-key.pem")
	if err := os.WriteFile(path, pemBytes, 0600); err != nil {
		t.Fatalf("writing private key file: %v", err)
	}
	return path
}

// historyLister wraps *fakeLister to additionally record the order and
// identity of every ListOpenPRs call, keyed "owner/repo" — a thin local
// helper rather than a change to the shared fakeLister, since no other
// test needs per-repo call ordering.
type historyLister struct {
	*fakeLister
	mu      sync.Mutex
	history []string
}

func newHistoryLister() *historyLister {
	return &historyLister{fakeLister: newFakeLister()}
}

func (h *historyLister) ListOpenPRs(owner, repo string) ([]gh.PRDetails, error) {
	h.mu.Lock()
	h.history = append(h.history, owner+"/"+repo)
	h.mu.Unlock()
	return h.fakeLister.ListOpenPRs(owner, repo)
}

func (h *historyLister) historySnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.history))
	copy(out, h.history)
	return out
}

// TestDaemon_ApplyReload_WatchedReposAddedPolledNextCycle covers ADR-1640
// AC1: a repo added to watched_repos is polled on the very next poll cycle
// after ApplyReload, with no restart — asserted against a test daemon (not
// manual observation, per the AC's own wording).
func TestDaemon_ApplyReload_WatchedReposAddedPolledNextCycle(t *testing.T) {
	client := newHistoryLister()
	client.prsByRepo["owner/repo1"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1"}}
	client.prsByRepo["owner/repo2"] = []gh.PRDetails{{Number: 2, Author: "alice", HeadSHA: "sha2"}}

	d := withDerivedRepos(&Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   &mockClaudeInvoker{},
		Clone:    mustFakeClone(t),
		Config:   Config{WatchedRepos: []string{"owner/repo1"}, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
	}, "owner/repo1")

	d.poll(context.Background())
	if hist := client.historySnapshot(); len(hist) != 1 || hist[0] != "owner/repo1" {
		t.Fatalf("first cycle history = %v, want [owner/repo1] only", hist)
	}

	merged, _ := applyConfigReload(d.config(), Config{WatchedRepos: []string{"owner/repo1", "owner/repo2"}, ConcurrencyCap: 3})
	d.ApplyReload(merged, nil, nil) // same owner already has a client — nothing new to add
	// Simulate the effect of the reload-triggered re-derivation
	// (daemon.triggerRederivation) that a real watched_repos change would
	// kick off asynchronously — this test only exercises poll()'s own
	// consumption of the derived set, not the async re-derivation path
	// itself (see TestHandleReload_* in this file for that).
	d.ApplyDerivedRepos(testDerivedRepoSet("owner/repo1", "owner/repo2"), d.Clients)

	d.poll(context.Background())
	hist := client.historySnapshot()
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
	client := newHistoryLister()
	client.prsByRepo["owner/repoA"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1"}}
	client.prsByRepo["owner/repoB"] = []gh.PRDetails{{Number: 2, Author: "alice", HeadSHA: "sha2"}}

	claude := newConcurrencyTrackingInvoker()

	d := withDerivedRepos(&Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   claude,
		Clone:    mustFakeClone(t),
		Config:   Config{WatchedRepos: []string{"owner/repoA", "owner/repoB"}, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
	}, "owner/repoA", "owner/repoB")

	done := make(chan struct{})
	go func() {
		d.poll(context.Background())
		close(done)
	}()

	waitUntil(t, 2*time.Second, func() bool { return claude.currentCount() == 2 })

	merged, _ := applyConfigReload(d.config(), Config{WatchedRepos: []string{"owner/repoB"}, ConcurrencyCap: 3})
	d.ApplyReload(merged, nil, nil)
	// See the sibling test above for why ApplyDerivedRepos is called
	// explicitly here too.
	d.ApplyDerivedRepos(testDerivedRepoSet("owner/repoB"), d.Clients)

	close(claude.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not complete within 5s after releasing all invocations")
	}

	if got := client.submitCallCount(); got != 2 {
		t.Errorf("submitCallCount = %d, want 2 — repoA's review (removed mid-flight) must still complete, not be cancelled", got)
	}

	d.poll(context.Background())
	hist := client.historySnapshot()
	for _, h := range hist[2:] {
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
// ADR-1640 documents: a holder that acquired a slot on the pre-resize
// channel keeps releasing into it, undisturbed by a concurrent resize —
// never blocking, never panicking on a send/receive against a channel
// nobody else references anymore.
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
// under -race.
func TestDaemon_ConcurrentReloadDuringActivePoll(t *testing.T) {
	client := newFakeLister()
	for i := 1; i <= 20; i++ {
		client.prsByRepo["owner/repo"] = append(client.prsByRepo["owner/repo"], gh.PRDetails{
			Number: i, Author: "alice", HeadSHA: fmt.Sprintf("sha%d", i),
		})
	}

	d := withDerivedRepos(&Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   &mockClaudeInvoker{},
		Clone:    mustFakeClone(t),
		Config:   Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: 4},
		BotLogin: "pruefer-bot[bot]",
	}, "owner/repo")

	var wg sync.WaitGroup
	stop := make(chan struct{})

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

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			newCap := 2 + (i % 5)
			d.ApplyReload(Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: newCap}, map[string]GitHubLister{
				"owner": client,
			}, nil)
			// Also exercise ApplyDerivedRepos' wholesale rebuild concurrently
			// with an active poll cycle — a new write pattern distinct from
			// ApplyReload's incremental merge (see this method's own risk
			// note in the Plan), so it needs its own race coverage.
			d.ApplyDerivedRepos(testDerivedRepoSet("owner/repo"), map[string]GitHubLister{"owner": client})
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

// TestDaemon_OwnerReviewsInFlight covers the prefix-matching primitive
// drainThenStopAuth polls: present only while a prGate for one of owner's
// repos is held, case-insensitively, and never confused by an unrelated
// owner whose name happens to share a prefix (e.g. "acme" vs "acme2").
func TestDaemon_OwnerReviewsInFlight(t *testing.T) {
	d := &Daemon{}

	if d.ownerReviewsInFlight("acme") {
		t.Fatal("expected no reviews in flight for acme before any gate is acquired")
	}

	_, release := d.acquirePRGate("ACME", "widgets", 7)
	if !d.ownerReviewsInFlight("acme") {
		t.Error("expected ownerReviewsInFlight(\"acme\") to be true while a gate for ACME/widgets#7 is held — matching must be case-insensitive")
	}
	if d.ownerReviewsInFlight("acme2") {
		t.Error("expected ownerReviewsInFlight(\"acme2\") to be false — \"acme\" must not prefix-match \"acme2\"'s repos")
	}

	release()
	if d.ownerReviewsInFlight("acme") {
		t.Error("expected no reviews in flight for acme once the gate was released")
	}
}

// --- handleReload integration tests (a minimal fake GitHub App server, not
// internal/githubauth's own unexported test helpers, since this test lives
// in a different package). ---

// fakeReloadAppServer is a minimal httptest server covering the GitHub App
// endpoints Reconcile/Derive need: app identity (/app), installation
// discovery (/app/installations), token minting
// (/app/installations/{id}/access_tokens), and installation repo
// enumeration (/installation/repositories) — since #1641, Derive calls the
// latter unconditionally for every installation. Unlike internal/githubauth's
// own fakeAppServer, this never needs to serve the manifest-exchange
// endpoint — every test here starts from an already-bootstrapped App (an
// explicit AppID + on-disk PEM), never the first-run flow.
type fakeReloadAppServer struct {
	mu               sync.Mutex
	mintCalls        map[int64]int
	installTokenToID map[string]int64
	// selectedRepos maps an installation ID to the repos it grants access
	// to — populated by tests that need the derived (reviewed) set to
	// actually contain repos, not just resolve a client per owner.
	selectedRepos map[int64][]string
}

func newFakeReloadAppServer(t *testing.T, slug string, installations []gh.AppInstallation) (*httptest.Server, *fakeReloadAppServer) {
	t.Helper()
	fake := &fakeReloadAppServer{mintCalls: map[int64]int{}, installTokenToID: map[string]int64{}, selectedRepos: map[int64][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id": 42, "slug": %q}`, slug)
	})
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") != "" && r.URL.Query().Get("page") != "1" {
			w.Write([]byte("[]"))
			return
		}
		w.Write([]byte("["))
		for i, inst := range installations {
			if i > 0 {
				w.Write([]byte(","))
			}
			selection := inst.RepositorySelection
			if selection == "" {
				selection = "all"
			}
			fmt.Fprintf(w, `{"id": %d, "account": {"login": %q}, "repository_selection": %q}`, inst.ID, inst.Account, selection)
		}
		w.Write([]byte("]"))
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		trimmed := strings.TrimPrefix(r.URL.Path, "/app/installations/")
		trimmed = strings.TrimSuffix(trimmed, "/access_tokens")
		var instID int64
		fmt.Sscanf(trimmed, "%d", &instID)
		fake.mu.Lock()
		fake.mintCalls[instID]++
		token := fmt.Sprintf("tok-%d-%d", instID, fake.mintCalls[instID])
		fake.installTokenToID[token] = instID
		fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token": %q, "expires_at": %q}`, token, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		fake.mu.Lock()
		instID, ok := fake.installTokenToID[auth]
		repos := fake.selectedRepos[instID]
		fake.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"repositories": [`))
		for i, r := range repos {
			if i > 0 {
				w.Write([]byte(","))
			}
			fmt.Fprintf(w, `{"full_name": %q}`, r)
		}
		w.Write([]byte("]}"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, fake
}

func (f *fakeReloadAppServer) mintCountFor(installationID int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mintCalls[installationID]
}

// setupReloadDaemon writes the given initial YAML, reconciles auth and
// builds a Daemon against it via the real LoadConfig/githubauth.Reconcile/
// NewDaemon production path (including the initial derivation, wired via
// Daemon.Reconciler/ApplyDerivedRepos exactly as Execute does), and returns
// the pieces handleReload needs. args is the flag slice a later call to
// handleReload should reuse, mirroring Execute()'s own single `args` capture.
func setupReloadDaemon(t *testing.T, dir, keyPath string, installations []gh.AppInstallation) (args []string, reconciler *githubauth.Reconciler, daemon *Daemon, srv *httptest.Server, fake *fakeReloadAppServer, closeLog func() error) {
	t.Helper()
	srv, fake = newFakeReloadAppServer(t, "pruefer-bot", installations)

	configPath := filepath.Join(dir, "config.yaml")
	args = []string{"-config", configPath, "-github-app-id", "42", "-github-app-private-key-path", keyPath}

	cfg, err := LoadConfig(args)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	reconciler, err = githubauth.Reconcile(context.Background(), githubauth.Options{
		AppID:             cfg.AppID,
		AppInstallationID: cfg.AppInstallationID,
		AppPrivateKeyPath: cfg.AppPrivateKeyPath,
		AppStatePath:      filepath.Join(dir, "app-state.json"),
		WatchedRepos:      cfg.WatchedRepos,
		MaxDerivedRepos:   cfg.MaxDerivedRepos,
		BaseURL:           srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	cfg.AppStatePath = filepath.Join(dir, "app-state.json")
	derived := reconciler.LastDerived()
	clients := map[string]GitHubLister{}
	for _, inst := range derived.Installations {
		c, err := reconciler.ClientForRepo(context.Background(), inst.Account, "")
		if err != nil {
			continue
		}
		clients[strings.ToLower(inst.Account)] = c
	}
	daemon, closeLog = NewDaemon(cfg, clients, &RealClaudeInvoker{}, nil, reconciler.BotLogin())
	daemon.Reconciler = reconciler
	daemon.ApplyDerivedRepos(derived, clients)
	return args, reconciler, daemon, srv, fake, closeLog
}

// waitForRederivation polls until cond is true or timeout elapses, for tests
// that trigger an async re-derivation (daemon.triggerRederivation, invoked
// from handleReload) and need to observe its effect.
func waitForRederivation(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

// waitForRederivationDone blocks until daemon.rederiveInFlight clears, and
// then until pollInFlight clears too — i.e. both the async re-derivation
// triggerRederivation spawned (from handleReload, an installation event, or
// the ticker) AND the follow-up reconciliation poll it chains into (AC3:
// "so newly-derived repos are pollable promptly") have fully returned,
// including every logf call either one makes. pollInFlight is only ever
// set (by triggerReconciliationPoll) synchronously before rederiveInFlight's
// own deferred clear runs, so waiting for rederiveInFlight first, then
// pollInFlight, cannot miss the follow-up poll's own in-flight window.
// Tests that trigger an async re-derivation must call this before
// returning (not just before asserting): a still-running goroutine calling
// logf after the test's own deferred closeLog() has already nilled the
// package-level Logf hook is a real data race, not just a flaky assertion —
// see wireLogf's own doc comment on this exact hazard.
func waitForRederivationDone(t *testing.T, d *Daemon) {
	t.Helper()
	waitForRederivation(t, 2*time.Second, func() bool { return !d.rederiveInFlight.Load() })
	waitForRederivation(t, 2*time.Second, func() bool { return !d.pollInFlight.Load() })
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

	args, _, daemon, _, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{{ID: 111, Account: "handarbeit"}})
	defer closeLog()
	snapshot := captureLogf(t)

	before := daemon.config()

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos: [unterminated"), 0600); err != nil {
		t.Fatalf("writing malformed config: %v", err)
	}

	handleReload(context.Background(), args, daemon)

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

// TestHandleReload_WatchedRepoUnderAlreadyInstalledOwner_BecomesReviewable
// covers #1641's success path: since every installation of the operator's
// own App is already minted unconditionally by Derive (not just owners
// named in watched_repos — see the issue's own containment explanation),
// adding a repo under an owner that's already installed (but wasn't
// previously named in watched_repos) needs no new mint at all — it becomes
// part of the derived, reviewed set as soon as the reload's triggered
// re-derivation completes.
func TestHandleReload_WatchedRepoUnderAlreadyInstalledOwner_BecomesReviewable(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n"), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	args, _, daemon, _, fake, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "newowner"},
	})
	defer closeLog()
	captureLogf(t)
	fake.selectedRepos[111] = []string{"handarbeit/fabrik"}
	fake.selectedRepos[222] = []string{"newowner/repo"}

	// newowner's installation already existed and was already minted by the
	// initial Derive call (setupReloadDaemon) — confirm the precondition
	// this test's whole point rests on: no new mint should be needed.
	mintsBefore := fake.mintCountFor(222)
	if mintsBefore == 0 {
		t.Fatal("precondition failed: newowner should already have been minted by the initial derivation")
	}
	if _, ok := daemon.client("newowner"); !ok {
		t.Fatal("precondition failed: newowner should already be servable before watched_repos ever named it")
	}

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n  - newowner/repo\n"), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handleReload(ctx, args, daemon)
	waitForRederivationDone(t, daemon) // must complete (incl. every logf call) before this test returns and closeLog() nils Logf

	found := false
	for _, dr := range daemon.derivedSet().Repos {
		if dr.Repo == "newowner/repo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("derived set = %+v, want newowner/repo included", daemon.derivedSet().Repos)
	}

	if got := fake.mintCountFor(222); got != mintsBefore {
		t.Errorf("mintCountFor(222 = newowner) = %d, want unchanged from %d — no re-mint needed for an already-known installation", got, mintsBefore)
	}
	got := daemon.config()
	if len(got.WatchedRepos) != 2 {
		t.Errorf("daemon.config().WatchedRepos = %v, want 2 entries", got.WatchedRepos)
	}
}

// TestHandleReload_WatchedRepoNamingUninstalledOwner_ReportedNotFailed is
// #1641's replacement for the old all-or-nothing mint-failure test: naming
// an owner with no App installation in watched_repos is no longer a reload
// failure at all (there is nothing to mint or roll back) — it's reported via
// the derived set's FilteredOut (R3/AC4), while every other repo (including
// one under an owner that *does* have an installation) is unaffected.
func TestHandleReload_WatchedRepoNamingUninstalledOwner_ReportedNotFailed(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n"), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	// "goodowner" has an installation; "badowner" does not.
	args, _, daemon, _, fake, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "goodowner"},
	})
	defer closeLog()
	captureLogf(t)
	fake.selectedRepos[111] = []string{"handarbeit/fabrik"}
	fake.selectedRepos[222] = []string{"goodowner/repo"}

	newYAML := "watched_repos:\n  - handarbeit/fabrik\n  - goodowner/repo\n  - badowner/repo\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(newYAML), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}

	handleReload(context.Background(), args, daemon)
	waitForRederivationDone(t, daemon) // must complete (incl. every logf call) before this test returns and closeLog() nils Logf

	found := false
	for _, dr := range daemon.derivedSet().Repos {
		if dr.Repo == "goodowner/repo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("derived set = %+v, want goodowner/repo included", daemon.derivedSet().Repos)
	}

	if _, ok := daemon.client("goodowner"); !ok {
		t.Error("expected goodowner to be servable — its own installation exists regardless of badowner's absence")
	}
	foundBad := false
	for _, f := range daemon.derivedSet().FilteredOut {
		if f == "badowner/repo" {
			foundBad = true
		}
	}
	if !foundBad {
		t.Errorf("expected badowner/repo in FilteredOut, got %v", daemon.derivedSet().FilteredOut)
	}
	got := daemon.config()
	if len(got.WatchedRepos) != 3 {
		t.Errorf("daemon.config().WatchedRepos = %v, want all 3 entries applied — naming an uninstalled owner is not a reload failure", got.WatchedRepos)
	}
}

// TestHandleReload_LogsDiffSummary covers the reload summary: it names
// every added/removed repo and every changed field.
func TestHandleReload_LogsDiffSummary(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\nmodel: sonnet\n"), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	args, _, daemon, _, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{{ID: 111, Account: "handarbeit"}})
	defer closeLog()
	snapshot := captureLogf(t)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos: []\nmodel: opus\n"), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}
	handleReload(context.Background(), args, daemon)
	waitForRederivationDone(t, daemon) // must complete (incl. every logf call) before this test returns and closeLog() nils Logf

	joined := strings.Join(snapshot(), "\n")
	if !strings.Contains(joined, "handarbeit/fabrik") {
		t.Errorf("log output missing removed repo name: %s", joined)
	}
	if !strings.Contains(joined, "Model") || !strings.Contains(joined, "sonnet") || !strings.Contains(joined, "opus") {
		t.Errorf("log output missing Model change (sonnet -> opus): %s", joined)
	}
}

// TestHandleReload_RemovedWatchedRepo_NoLongerReviewed_ButInstallationStaysAuthorized
// is #1641's replacement for the old owner-pruning test: dropping verveguy's
// only watched repo no longer drops verveguy's installation Auth at all —
// R3 is a review-scope narrowing filter, not the containment boundary
// (that's the operator's own App identity, #1253) — so verveguy stays
// servable through the daemon, just excluded from the reviewed set. This is
// a deliberate behavior change from the pre-#1641 model this test used to
// assert the opposite of.
func TestHandleReload_RemovedWatchedRepo_NoLongerReviewed_ButInstallationStaysAuthorized(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n  - verveguy/otherrepo\n"), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	args, reconciler, daemon, _, fake, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "verveguy"},
	})
	defer closeLog()
	captureLogf(t)
	fake.selectedRepos[111] = []string{"handarbeit/fabrik"}
	fake.selectedRepos[222] = []string{"verveguy/otherrepo"}
	// Re-derive once now that selectedRepos is populated (setupReloadDaemon's
	// own initial derivation ran before these fakes were configured).
	daemon.rederiveRepos(context.Background())

	if _, ok := daemon.client("verveguy"); !ok {
		t.Fatal("expected verveguy to be servable before the reload")
	}
	beforeInstallationCount := reconciler.InstallationCount()

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n"), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}
	handleReload(context.Background(), args, daemon)
	waitForRederivationDone(t, daemon) // must complete (incl. every logf call) before this test returns and closeLog() nils Logf

	for _, dr := range daemon.derivedSet().Repos {
		if dr.Repo == "verveguy/otherrepo" {
			t.Errorf("expected verveguy/otherrepo to no longer be in the reviewed set after being dropped from watched_repos")
		}
	}

	if _, ok := daemon.client("verveguy"); !ok {
		t.Error("expected verveguy to remain servable — its installation is untouched by a watched_repos edit (R3 is a review-scope filter, not containment)")
	}
	if _, err := reconciler.ClientForRepo(context.Background(), "verveguy", ""); err != nil {
		t.Errorf("expected reconciler to still have a client for verveguy: %v", err)
	}
	if got := reconciler.InstallationCount(); got != beforeInstallationCount {
		t.Errorf("InstallationCount() = %d, want unchanged %d — a watched_repos edit must never detach an installation", got, beforeInstallationCount)
	}
	if _, ok := daemon.client("handarbeit"); !ok {
		t.Error("expected handarbeit (still watched) to remain servable")
	}
}

// TestDaemon_DrainThenStopAuth_WaitsForInFlightReviewThenStops is the
// non-vacuous core of the deferred owner-removal fix: drainThenStopAuth
// must not call Auth.Stop() while a review is still in flight for the
// owner being removed, and must call it promptly once that review
// completes. Uses a real *githubauth.Auth minted against a fake server
// (rather than poking its unexported cancel field, which this package
// cannot reach), started via Reconciler.CommitOwnerAuth exactly as
// production does. Shrinks ownerDrainPollInterval so the test doesn't wait
// out the production 2s poll.
func TestDaemon_DrainThenStopAuth_WaitsForInFlightReviewThenStops(t *testing.T) {
	orig := ownerDrainPollInterval
	ownerDrainPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { ownerDrainPollInterval = orig })

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n"), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}
	_, reconciler, daemon, _, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "acme"},
	})
	defer closeLog()
	captureLogf(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, fresh, err := reconciler.MintOwnerAuth("acme", []string{"acme/widgets"}, nil)
	if err != nil {
		t.Fatalf("MintOwnerAuth: %v", err)
	}
	reconciler.CommitOwnerAuth(ctx, "acme", a, fresh, nil)

	_, release := daemon.acquirePRGate("acme", "widgets", 1)

	drainDone := make(chan struct{})
	go func() {
		daemon.drainThenStopAuth(ctx, "acme", a)
		close(drainDone)
	}()

	select {
	case <-drainDone:
		t.Fatal("drainThenStopAuth returned while a review was still in flight for its owner")
	case <-time.After(100 * time.Millisecond):
		// still waiting, as expected
	}

	release()

	select {
	case <-drainDone:
		// drainThenStopAuth observed the drain and called a.Stop().
	case <-time.After(time.Second):
		t.Fatal("drainThenStopAuth did not return within 1s of the in-flight review completing")
	}
}
