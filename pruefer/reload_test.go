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

	d := &Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   &mockClaudeInvoker{},
		Clone:    mustFakeClone(t),
		Config:   Config{WatchedRepos: []string{"owner/repo1"}, ConcurrencyCap: 3},
		BotLogin: "pruefer-bot[bot]",
	}

	d.poll(context.Background())
	if hist := client.historySnapshot(); len(hist) != 1 || hist[0] != "owner/repo1" {
		t.Fatalf("first cycle history = %v, want [owner/repo1] only", hist)
	}

	merged, _ := applyConfigReload(d.config(), Config{WatchedRepos: []string{"owner/repo1", "owner/repo2"}, ConcurrencyCap: 3})
	d.ApplyReload(merged, nil, nil) // same owner already has a client — nothing new to add

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

	d := &Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   &mockClaudeInvoker{},
		Clone:    mustFakeClone(t),
		Config:   Config{WatchedRepos: []string{"owner/repo"}, ConcurrencyCap: 4},
		BotLogin: "pruefer-bot[bot]",
	}

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

// fakeReloadAppServer is a minimal httptest server covering exactly the
// three GitHub App endpoints Reconcile/MintOwnerAuth need: app identity
// (/app), installation discovery (/app/installations), and token minting
// (/app/installations/{id}/access_tokens). Unlike internal/githubauth's own
// fakeAppServer, this never needs to serve the manifest-exchange endpoint —
// every test here starts from an already-bootstrapped App (an explicit
// AppID + on-disk PEM), never the first-run flow.
type fakeReloadAppServer struct {
	mu        sync.Mutex
	mintCalls map[int64]int
}

func newFakeReloadAppServer(t *testing.T, slug string, installations []gh.AppInstallation) (*httptest.Server, *fakeReloadAppServer) {
	t.Helper()
	fake := &fakeReloadAppServer{mintCalls: map[int64]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id": 42, "slug": %q}`, slug)
	})
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
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
		fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token": "tok-%d", "expires_at": %q}`, instID, time.Now().Add(time.Hour).Format(time.RFC3339))
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
// NewDaemon production path, and returns the pieces handleReload needs.
// args is the flag slice a later call to handleReload should reuse,
// mirroring Execute()'s own single `args` capture.
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
		BaseURL:           srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	cfg.AppStatePath = filepath.Join(dir, "app-state.json")
	clients := map[string]GitHubLister{}
	for _, owner := range distinctOwners(cfg.WatchedRepos) {
		c, err := reconciler.ClientForRepo(context.Background(), owner, "")
		if err != nil {
			continue
		}
		clients[strings.ToLower(owner)] = c
	}
	daemon, closeLog = NewDaemon(cfg, clients, &RealClaudeInvoker{}, nil, reconciler.BotLogin())
	return args, reconciler, daemon, srv, fake, closeLog
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

	args, reconciler, daemon, _, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{{ID: 111, Account: "handarbeit"}})
	defer closeLog()
	snapshot := captureLogf(t)

	before := daemon.config()

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos: [unterminated"), 0600); err != nil {
		t.Fatalf("writing malformed config: %v", err)
	}

	handleReload(context.Background(), args, reconciler, daemon)

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

// TestHandleReload_NewOwnerMintsInstallationTokenAndServable covers the
// success path: adding a repo under a previously-unwatched owner mints that
// owner's installation token as part of the reload, and the new owner's
// repo is servable (has a client) immediately after.
func TestHandleReload_NewOwnerMintsInstallationTokenAndServable(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n"), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	args, reconciler, daemon, _, fake, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "newowner"},
	})
	defer closeLog()
	captureLogf(t)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n  - newowner/repo\n"), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handleReload(ctx, args, reconciler, daemon)

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
// covers the failure path and the all-or-nothing guarantee across a
// multi-owner batch: when one of two newly-watched owners has no App
// installation, neither new owner is registered and the running config is
// untouched.
func TestHandleReload_NewOwnerNoInstallation_FailsWholeReloadAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n"), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	// "goodowner" has an installation; "badowner" does not.
	args, reconciler, daemon, _, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "goodowner"},
	})
	defer closeLog()
	captureLogf(t)

	before := daemon.config()
	beforeInstallationCount := reconciler.InstallationCount()

	newYAML := "watched_repos:\n  - handarbeit/fabrik\n  - goodowner/repo\n  - badowner/repo\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(newYAML), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}

	handleReload(context.Background(), args, reconciler, daemon)

	if got := daemon.config(); !reflect.DeepEqual(got, before) {
		t.Errorf("daemon.config() changed despite a failed reload: got %+v, want unchanged %+v", got, before)
	}
	if _, ok := daemon.client("goodowner"); ok {
		t.Error("goodowner must not be registered — the whole reload failed because badowner had no installation (all-or-nothing)")
	}
	if _, err := reconciler.ClientForRepo(context.Background(), "goodowner", ""); err == nil {
		t.Error("reconciler must not have a client for goodowner — CommitOwnerAuth must never run for a batch that ultimately fails")
	}
	if reconciler.InstallationCount() != beforeInstallationCount {
		t.Errorf("InstallationCount() = %d, want unchanged %d — a minted-but-uncommitted Auth must never be registered", reconciler.InstallationCount(), beforeInstallationCount)
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

	args, reconciler, daemon, _, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{{ID: 111, Account: "handarbeit"}})
	defer closeLog()
	snapshot := captureLogf(t)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos: []\nmodel: opus\n"), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}
	handleReload(context.Background(), args, reconciler, daemon)

	joined := strings.Join(snapshot(), "\n")
	if !strings.Contains(joined, "handarbeit/fabrik") {
		t.Errorf("log output missing removed repo name: %s", joined)
	}
	if !strings.Contains(joined, "Model") || !strings.Contains(joined, "sonnet") || !strings.Contains(joined, "opus") {
		t.Errorf("log output missing Model change (sonnet -> opus): %s", joined)
	}
}

// TestHandleReload_RemovedOwnerPrunesClient is the handleReload-level proof
// that dropping an owner's last watched repo via reload leaves that owner
// unresolvable through the daemon (not just absent from watched_repos) and
// drops its Auth from the reconciler.
func TestHandleReload_RemovedOwnerPrunesClient(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n  - verveguy/otherrepo\n"), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	args, reconciler, daemon, _, _, closeLog := setupReloadDaemon(t, dir, keyPath, []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "verveguy"},
	})
	defer closeLog()
	captureLogf(t)

	if _, ok := daemon.client("verveguy"); !ok {
		t.Fatal("expected verveguy to be servable before the reload")
	}
	beforeInstallationCount := reconciler.InstallationCount()

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("watched_repos:\n  - handarbeit/fabrik\n"), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}
	handleReload(context.Background(), args, reconciler, daemon)

	if _, ok := daemon.client("verveguy"); ok {
		t.Error("expected verveguy to no longer be servable through the daemon after its last watched repo was removed")
	}
	if _, err := reconciler.ClientForRepo(context.Background(), "verveguy", ""); err == nil {
		t.Error("expected reconciler to no longer have a client for verveguy after the reload")
	}
	if got := reconciler.InstallationCount(); got != beforeInstallationCount-1 {
		t.Errorf("InstallationCount() = %d, want %d (verveguy's dedicated installation dropped)", got, beforeInstallationCount-1)
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
