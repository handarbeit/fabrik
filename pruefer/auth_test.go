package pruefer

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
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

// fakeAppServer serves the GitHub App endpoints BootstrapMulti/refresh use:
// /app, /app/installations, /app/installations/{id}/access_tokens, and
// (for repository_selection=="selected" installations)
// /installation/repositories. mintCount is incremented on every
// access_tokens call so tests can assert refresh behavior; mintCountByInst
// breaks that down per installation ID for independent-refresh assertions.
// installTokenToID lets the /installation/repositories handler figure out
// which installation a request is authenticated as, from the Bearer token
// minted for it.
type fakeAppServer struct {
	installations []gh.AppInstallation
	slug          string
	tokenExpiry   func() time.Time
	// failMint, if set, is consulted before every access_tokens mint for a
	// given installation ID; returning true fails that mint (simulating a
	// down/erroring installation) without affecting any other installation.
	failMint func(installationID int64) bool
	// selectedRepos maps an installation ID to the repos it actually grants
	// access to, for installations with RepositorySelection == "selected".
	selectedRepos map[int64][]string

	mintCount atomic.Int32

	mu               sync.Mutex
	mintCountByInst  map[int64]int
	lastMintedAt     time.Time
	installTokenToID map[string]int64
	installationHits []int64 // access_tokens calls, in order, for assertion
}

func newFakeAppServer(slug string, installations []gh.AppInstallation, tokenExpiry func() time.Time) (*httptest.Server, *fakeAppServer) {
	f := &fakeAppServer{
		slug: slug, installations: installations, tokenExpiry: tokenExpiry,
		mintCountByInst:  map[int64]int{},
		installTokenToID: map[string]int64{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"slug": f.slug, "id": 1})
	})
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		raw := make([]map[string]interface{}, len(f.installations))
		for i, inst := range f.installations {
			raw[i] = map[string]interface{}{
				"id": inst.ID, "account": map[string]string{"login": inst.Account},
				"repository_selection": inst.RepositorySelection,
			}
		}
		json.NewEncoder(w).Encode(raw)
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		var instID int64
		fmt.Sscanf(r.URL.Path, "/app/installations/%d/access_tokens", &instID)

		f.mu.Lock()
		f.installationHits = append(f.installationHits, instID)
		f.mu.Unlock()

		if f.failMint != nil && f.failMint(instID) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"simulated mint failure"}`))
			return
		}

		f.mintCount.Add(1)
		f.mu.Lock()
		f.mintCountByInst[instID]++
		f.lastMintedAt = time.Now()
		token := fmt.Sprintf("ghs_token_inst%d_%d", instID, f.mintCountByInst[instID])
		f.installTokenToID[token] = instID
		f.mu.Unlock()

		json.NewEncoder(w).Encode(map[string]interface{}{
			"token":      token,
			"expires_at": f.tokenExpiry().UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		instID, ok := f.installTokenToID[auth]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		repos := f.selectedRepos[instID]
		out := make([]map[string]interface{}, len(repos))
		for i, r := range repos {
			out[i] = map[string]interface{}{"full_name": r}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"repositories": out})
	})
	return httptest.NewServer(mux), f
}

func (f *fakeAppServer) mintCountFor(instID int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mintCountByInst[instID]
}

func (f *fakeAppServer) hitInstallations() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int64, len(f.installationHits))
	copy(out, f.installationHits)
	return out
}

func TestBootstrapMulti_SingleOwnerAutoDiscovered(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{{ID: 111, Account: "handarbeit"}}, func() time.Time {
		return time.Now().Add(time.Hour)
	})
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath, WatchedRepos: []string{"handarbeit/fabrik"}}
	as, err := BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}
	if as.BotLogin != "pruefer-bot[bot]" {
		t.Errorf("BotLogin = %q, want pruefer-bot[bot]", as.BotLogin)
	}
	client, ok := as.Clients["handarbeit"]
	if !ok {
		t.Fatal("expected a client for owner handarbeit")
	}
	if client.Token() == "" {
		t.Error("expected client token to be set after BootstrapMulti")
	}
	if as.InstallationCount() != 1 {
		t.Errorf("InstallationCount() = %d, want 1", as.InstallationCount())
	}
}

func TestBootstrapMulti_ExplicitInstallationIDSkipsDiscovery(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	discoveryCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"slug": "pruefer-bot"})
	})
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		discoveryCalled = true
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"token": "ghs_x", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// A pin combined with a multi-owner watched_repos: every owner must map
	// to the one pinned client, and discovery must never be called.
	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath, AppInstallationID: 999,
		WatchedRepos: []string{"handarbeit/fabrik", "verveguy/otherrepo"}}
	as, err := BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}
	if discoveryCalled {
		t.Error("expected /app/installations to be skipped when AppInstallationID is explicitly configured")
	}
	if len(as.Clients) != 2 {
		t.Fatalf("expected 2 owners mapped, got %d", len(as.Clients))
	}
	if as.Clients["handarbeit"] != as.Clients["verveguy"] {
		t.Error("expected both owners to share the single pinned client")
	}
	if as.InstallationCount() != 1 {
		t.Errorf("InstallationCount() = %d, want 1 (one goroutine for the shared pin)", as.InstallationCount())
	}
}

func TestBootstrapMulti_MultiOwnerMintsPerOwnerAndNeverTouchesStranger(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "verveguy"},
		{ID: 333, Account: "stranger"}, // installed on the App, never watched
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath,
		WatchedRepos: []string{"handarbeit/fabrik", "verveguy/otherrepo"}}
	as, err := BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}
	if len(as.Clients) != 2 {
		t.Fatalf("expected exactly 2 owners mapped, got %d: %v", len(as.Clients), as.Clients)
	}
	handarbeitClient, ok := as.Clients["handarbeit"]
	if !ok || handarbeitClient.Token() == "" {
		t.Fatal("expected a minted client for owner handarbeit")
	}
	verveguyClient, ok := as.Clients["verveguy"]
	if !ok || verveguyClient.Token() == "" {
		t.Fatal("expected a minted client for owner verveguy")
	}
	if handarbeitClient.Token() == verveguyClient.Token() {
		t.Error("expected distinct tokens for distinct owners")
	}
	if _, ok := as.Clients["stranger"]; ok {
		t.Fatal("expected no client for the unwatched stranger installation")
	}

	// The core public-app safety assertion: installation 333 (stranger) must
	// never have had a token minted for it.
	for _, instID := range fake.hitInstallations() {
		if instID == 333 {
			t.Fatal("expected installation 333 (stranger, unwatched) to never be minted a token")
		}
	}
	if fake.mintCountFor(333) != 0 {
		t.Errorf("mintCountFor(333) = %d, want 0 — stranger installation must never be tokenized", fake.mintCountFor(333))
	}
	if as.InstallationCount() != 2 {
		t.Errorf("InstallationCount() = %d, want 2", as.InstallationCount())
	}
}

func TestBootstrapMulti_MissingOwnerInstallationNamesOwner(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath,
		WatchedRepos: []string{"handarbeit/fabrik", "notinstalled/otherrepo"}}
	_, err := BootstrapMulti(cfg, srv.URL)
	if err == nil {
		t.Fatal("expected an error when an owner has no matching installation")
	}
	if !strings.Contains(err.Error(), "notinstalled") {
		t.Errorf("error %q does not name the missing owner %q", err.Error(), "notinstalled")
	}
}

func TestBootstrapMulti_SelectedModeExclusionNamesRepo(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "someorg", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{111: {"someorg/repo-one"}}
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath,
		WatchedRepos: []string{"someorg/repo-one", "someorg/repo-excluded"}}
	_, err := BootstrapMulti(cfg, srv.URL)
	if err == nil {
		t.Fatal("expected an error when a selected-mode installation excludes a watched repo")
	}
	if !strings.Contains(err.Error(), "someorg/repo-excluded") {
		t.Errorf("error %q does not name the excluded repo %q", err.Error(), "someorg/repo-excluded")
	}
}

func TestBootstrapMulti_SelectedModeSuccessWhenRepoIncluded(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "someorg", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{111: {"someorg/repo-one", "someorg/repo-two"}}
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath,
		WatchedRepos: []string{"someorg/repo-one"}}
	as, err := BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}
	if _, ok := as.Clients["someorg"]; !ok {
		t.Fatal("expected a client for owner someorg")
	}
}

func TestBootstrapMulti_NoWatchedReposMintsNothing(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath}
	as, err := BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}
	if len(as.Clients) != 0 {
		t.Errorf("expected no clients when no repos are watched, got %d", len(as.Clients))
	}
	if fake.mintCount.Load() != 0 {
		t.Errorf("mintCount = %d, want 0 (no installation calls with zero watched repos)", fake.mintCount.Load())
	}
}

func TestBootstrapMulti_NoInstallations(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", nil, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath, WatchedRepos: []string{"handarbeit/fabrik"}}
	if _, err := BootstrapMulti(cfg, srv.URL); err == nil {
		t.Fatal("expected an error when the app has no installations and a repo is watched")
	}
}

func TestBootstrapMulti_MissingPrivateKeyFile(t *testing.T) {
	cfg := Config{AppID: 42, AppPrivateKeyPath: "/nonexistent/key.pem", WatchedRepos: []string{"handarbeit/fabrik"}}
	if _, err := BootstrapMulti(cfg, ""); err == nil {
		t.Fatal("expected an error when the private key file is missing")
	}
}

func TestAuthSet_RunRefreshLoops_IndependentPerInstallation(t *testing.T) {
	oldMargin, oldRetry := tokenRefreshMargin, tokenRefreshRetryDelay
	tokenRefreshMargin = 50 * time.Millisecond
	tokenRefreshRetryDelay = 20 * time.Millisecond

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	// Token expires 100ms after each mint — with a 50ms margin, a healthy
	// refresh loop must mint again ~50ms after the previous mint.
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "verveguy"},
	}, func() time.Time { return time.Now().Add(100 * time.Millisecond) })
	// Installation 222 mints fine once (for BootstrapMulti's initial token)
	// but fails every subsequent refresh — must not stop 111's loop.
	fake.failMint = func(instID int64) bool { return instID == 222 && fake.mintCountFor(222) >= 1 }
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath, WatchedRepos: []string{"handarbeit/fabrik", "verveguy/otherrepo"}}
	as, err := BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}

	var mu sync.Mutex
	var logLines []string
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	// RunRefreshLoops's own logic is just `go a.RunRefreshLoop(...)` fanned
	// out per distinct installation in as.auths (same package, so visible
	// here) — it gives no completion signal. Wrap each Auth's loop in a
	// WaitGroup we own so the test can deterministically wait for every
	// loop goroutine to actually return (not just for ctx to be Done, which
	// only means they *may* be about to return) before restoring the
	// shared tokenRefreshMargin/tokenRefreshRetryDelay package vars below;
	// racing that restore against a still-running loop (which reads them)
	// is exactly the kind of unsynchronized access `-race` exists to catch.
	var wg sync.WaitGroup
	for _, a := range as.auths {
		a := a
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.RunRefreshLoop(ctx, func(format string, args ...any) {
				mu.Lock()
				logLines = append(logLines, fmt.Sprintf(format, args...))
				mu.Unlock()
			})
		}()
	}
	wg.Wait()
	tokenRefreshMargin, tokenRefreshRetryDelay = oldMargin, oldRetry

	if got := fake.mintCountFor(111); got < 2 {
		t.Errorf("mintCountFor(111) = %d, want >= 2 (healthy installation's loop must keep refreshing)", got)
	}
	mu.Lock()
	sawFailure := false
	for _, line := range logLines {
		if strings.Contains(line, "refresh failed") {
			sawFailure = true
		}
	}
	mu.Unlock()
	if !sawFailure {
		t.Error("expected at least one logged refresh failure for the always-failing installation")
	}
}

// TestAuthSet_RunRefreshLoops_DispatchesOneGoroutinePerAuth is a lightweight
// smoke test for RunRefreshLoops itself (the independent-refresh mechanics
// are covered in depth by TestAuthSet_RunRefreshLoops_IndependentPerInstallation
// above): it uses the default (multi-minute) refresh margin so no actual
// refresh fires during the test, avoiding any need to shrink the shared
// tokenRefreshMargin/tokenRefreshRetryDelay package vars — the logf-builder
// callback RunRefreshLoops invokes once per Auth, synchronously, before
// spawning that Auth's goroutine gives a race-free dispatch signal.
func TestAuthSet_RunRefreshLoops_DispatchesOneGoroutinePerAuth(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "verveguy"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath, WatchedRepos: []string{"handarbeit/fabrik", "verveguy/otherrepo"}}
	as, err := BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}

	var mu sync.Mutex
	seen := map[int64]bool{}
	ctx, cancel := context.WithCancel(context.Background())
	as.RunRefreshLoops(ctx, func(installationID int64) func(format string, args ...any) {
		mu.Lock()
		seen[installationID] = true
		mu.Unlock()
		return func(format string, args ...any) {}
	})
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Errorf("RunRefreshLoops built a logf for %d installation(s), want 2 (one per distinct Auth)", len(seen))
	}
	if !seen[111] || !seen[222] {
		t.Errorf("seen = %v, want both installation 111 and 222", seen)
	}
}

func TestAuth_ExpiresAt_ZeroBeforeAnyRefresh(t *testing.T) {
	a := &Auth{}
	if !a.ExpiresAt().IsZero() {
		t.Error("expected ExpiresAt to be zero before any refresh")
	}
}

func TestNewlyWatchedOwners(t *testing.T) {
	old := Config{WatchedRepos: []string{"handarbeit/fabrik", "verveguy/otherrepo"}}
	cand := Config{WatchedRepos: []string{"handarbeit/fabrik", "verveguy/otherrepo", "newowner/repo", "newowner/repo2"}}
	got := newlyWatchedOwners(old, cand)
	if len(got) != 1 || got[0] != "newowner" {
		t.Errorf("newlyWatchedOwners = %v, want [newowner] (deduped, and an already-watched owner adding a repo is not new)", got)
	}
}

func TestNewlyWatchedOwners_NoChangeIsEmpty(t *testing.T) {
	cfg := Config{WatchedRepos: []string{"handarbeit/fabrik"}}
	if got := newlyWatchedOwners(cfg, cfg); len(got) != 0 {
		t.Errorf("newlyWatchedOwners(cfg, cfg) = %v, want empty", got)
	}
}

func TestAuthSet_MintOwnerAuth_NewOwner(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "newowner"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath, WatchedRepos: []string{"handarbeit/fabrik"}}
	as, err := BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}
	if fake.mintCountFor(222) != 0 {
		t.Fatalf("installation 222 (newowner) minted before it was ever watched — mintCount = %d", fake.mintCountFor(222))
	}

	newCfg := Config{AppID: 42, AppPrivateKeyPath: keyPath, WatchedRepos: []string{"handarbeit/fabrik", "newowner/repo"}}
	a, fresh, err := as.mintOwnerAuth(newCfg, "newowner")
	if err != nil {
		t.Fatalf("mintOwnerAuth: %v", err)
	}
	if !fresh {
		t.Error("expected mintedFresh=true for a non-pinned new owner")
	}
	if a.InstallationID != 222 {
		t.Errorf("InstallationID = %d, want 222", a.InstallationID)
	}
	if fake.mintCountFor(222) != 1 {
		t.Errorf("mintCountFor(222) = %d, want 1", fake.mintCountFor(222))
	}
	// mintOwnerAuth must not itself register anything — that's CommitOwner's job.
	if _, ok := as.Clients["newowner"]; ok {
		t.Error("mintOwnerAuth must not register a client itself — CommitOwner does")
	}
	if as.InstallationCount() != 1 {
		t.Errorf("InstallationCount() = %d, want 1 (mintOwnerAuth alone must not append to auths)", as.InstallationCount())
	}

	as.CommitOwner("newowner", a, fresh)
	if client, ok := as.Clients["newowner"]; !ok || client.Token() == "" {
		t.Fatal("expected CommitOwner to register a token-bearing client for newowner")
	}
	if as.InstallationCount() != 2 {
		t.Errorf("InstallationCount() = %d, want 2 after CommitOwner", as.InstallationCount())
	}
}

func TestAuthSet_MintOwnerAuth_NoInstallation_Errors(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath, WatchedRepos: []string{"handarbeit/fabrik"}}
	as, err := BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}

	_, _, err = as.mintOwnerAuth(cfg, "notinstalled")
	if err == nil {
		t.Fatal("expected an error minting a token for an owner with no App installation")
	}
	if !strings.Contains(err.Error(), "notinstalled") {
		t.Errorf("error %q does not name the owner %q", err.Error(), "notinstalled")
	}
}

func TestAuthSet_MintOwnerAuth_PinnedInstallation(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	discoveryCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"slug": "pruefer-bot"})
	})
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		discoveryCalled = true
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"token": "ghs_x", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath, AppInstallationID: 999, WatchedRepos: []string{"handarbeit/fabrik"}}
	as, err := BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}

	a, fresh, err := as.mintOwnerAuth(cfg, "newowner")
	if err != nil {
		t.Fatalf("mintOwnerAuth: %v", err)
	}
	if fresh {
		t.Error("expected mintedFresh=false when sharing the pinned installation")
	}
	if a != as.auths[0] {
		t.Error("expected mintOwnerAuth to return the App's existing pinned Auth, not a new one")
	}
	if discoveryCalled {
		t.Error("expected /app/installations to never be called in pinned-installation mode")
	}

	as.CommitOwner("newowner", a, fresh)
	if as.InstallationCount() != 1 {
		t.Errorf("InstallationCount() = %d, want 1 — CommitOwner must not append a duplicate Auth for a shared pinned installation", as.InstallationCount())
	}
	if client, ok := as.Clients["newowner"]; !ok || client != as.Clients["handarbeit"] {
		t.Error("expected newowner to share the same pinned client as handarbeit")
	}
}

func TestAuthSet_CommitOwner_NoDuplicateAuthOnPinnedOwner(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", nil, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath, AppInstallationID: 999, WatchedRepos: []string{"handarbeit/fabrik"}}
	as, err := BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}

	// Two owners newly discovered in the same reload batch, both sharing
	// the pinned installation: neither CommitOwner call may append to
	// auths, or InstallationCount would over-report and RunRefreshLoops
	// would start a second, redundant refresh goroutine for the same
	// installation.
	for _, owner := range []string{"ownerA", "ownerB"} {
		a, fresh, err := as.mintOwnerAuth(cfg, owner)
		if err != nil {
			t.Fatalf("mintOwnerAuth(%s): %v", owner, err)
		}
		as.CommitOwner(owner, a, fresh)
	}
	if as.InstallationCount() != 1 {
		t.Errorf("InstallationCount() = %d, want 1 after committing two owners sharing one pinned installation", as.InstallationCount())
	}
	if len(as.Clients) != 3 { // handarbeit (from BootstrapMulti) + ownerA + ownerB
		t.Errorf("len(Clients) = %d, want 3", len(as.Clients))
	}
}

func TestNoLongerWatchedOwners(t *testing.T) {
	old := Config{WatchedRepos: []string{"handarbeit/fabrik", "verveguy/otherrepo", "verveguy/second"}}
	cand := Config{WatchedRepos: []string{"handarbeit/fabrik"}}
	got := noLongerWatchedOwners(old, cand)
	if len(got) != 1 || got[0] != "verveguy" {
		t.Errorf("noLongerWatchedOwners = %v, want [verveguy] (deduped, and an owner keeping another watched repo is not removed)", got)
	}
}

func TestNoLongerWatchedOwners_NoChangeIsEmpty(t *testing.T) {
	cfg := Config{WatchedRepos: []string{"handarbeit/fabrik"}}
	if got := noLongerWatchedOwners(cfg, cfg); len(got) != 0 {
		t.Errorf("noLongerWatchedOwners(cfg, cfg) = %v, want empty", got)
	}
}

// TestAuthSet_PruneOwners_NonPinnedCancelsRefreshLoopAndDropsOwner is the
// non-vacuous proof for the #1640 review finding: pruning a non-pinned
// owner must not just delete its Clients map entry — it must actually stop
// that owner's RunRefreshLoop goroutine, not merely leave it running
// forever against a Clients entry nobody references anymore.
//
// Deliberately does not touch the shared tokenRefreshMargin/
// tokenRefreshRetryDelay package vars (unlike
// TestAuthSet_RunRefreshLoops_IndependentPerInstallation, which must
// because it wants a refresh to actually fire): with the default,
// multi-minute margin and an hour-long ExpiresAt, RunRefreshLoop's first
// select blocks on ctx.Done() for the test's whole duration, so there is
// nothing to race against those vars. Mirrors startRefreshLoop's own logic
// (deriving a cancellable child context, recording it on the Auth) so the
// assertion exercises exactly the mechanism pruneOwners uses in production,
// with a completion channel the production helper doesn't expose.
func TestAuthSet_PruneOwners_NonPinnedCancelsRefreshLoopAndDropsOwner(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "verveguy"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath, WatchedRepos: []string{"handarbeit/fabrik", "verveguy/otherrepo"}}
	as, err := BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}
	if as.InstallationCount() != 2 {
		t.Fatalf("InstallationCount() = %d, want 2 before pruning", as.InstallationCount())
	}

	var verveguyAuth *Auth
	for _, a := range as.auths {
		if as.Clients["verveguy"] == a.client {
			verveguyAuth = a
		}
	}
	if verveguyAuth == nil {
		t.Fatal("could not find verveguy's Auth in as.auths")
	}

	// Mirrors startRefreshLoop: derive a cancellable child context, record
	// it on the Auth (the field pruneOwners reads), and run RunRefreshLoop
	// in a goroutine we can observe the completion of.
	loopCtx, loopCancel := context.WithCancel(context.Background())
	verveguyAuth.mu.Lock()
	verveguyAuth.cancel = loopCancel
	verveguyAuth.mu.Unlock()
	done := make(chan struct{})
	go func() {
		verveguyAuth.RunRefreshLoop(loopCtx, func(format string, args ...any) {})
		close(done)
	}()

	as.pruneOwners([]string{"verveguy"})

	if _, ok := as.Clients["verveguy"]; ok {
		t.Error("expected verveguy's Clients entry to be removed by pruneOwners")
	}
	if as.InstallationCount() != 1 {
		t.Errorf("InstallationCount() = %d, want 1 after pruning verveguy's (non-pinned, unshared) installation", as.InstallationCount())
	}
	select {
	case <-done:
		// pruneOwners called verveguyAuth.cancel, which cancelled loopCtx,
		// which made RunRefreshLoop return — the actual mechanism under
		// test, not a proxy for it.
	case <-time.After(time.Second):
		t.Fatal("RunRefreshLoop did not return within 1s of pruneOwners — its cancel func was not invoked")
	}
}

// TestAuthSet_PruneOwners_PinnedModeKeepsSharedAuth confirms pruning an
// owner under a pinned installation only removes that owner's Clients
// entry — it never cancels or drops the single shared Auth, since another
// owner (or a future reload-added one) still depends on it.
func TestAuthSet_PruneOwners_PinnedModeKeepsSharedAuth(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", nil, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath, AppInstallationID: 999, WatchedRepos: []string{"handarbeit/fabrik"}}
	as, err := BootstrapMulti(cfg, srv.URL)
	if err != nil {
		t.Fatalf("BootstrapMulti: %v", err)
	}
	a, fresh, err := as.mintOwnerAuth(cfg, "ownerB")
	if err != nil {
		t.Fatalf("mintOwnerAuth: %v", err)
	}
	as.CommitOwner("ownerB", a, fresh)

	as.pruneOwners([]string{"ownerB"})

	if _, ok := as.Clients["ownerB"]; ok {
		t.Error("expected ownerB's Clients entry to be removed by pruneOwners")
	}
	if as.InstallationCount() != 1 {
		t.Errorf("InstallationCount() = %d, want 1 — the pinned installation's Auth must survive a pinned owner's removal", as.InstallationCount())
	}
	if _, ok := as.Clients["handarbeit"]; !ok {
		t.Error("expected handarbeit (still watched, sharing the same pinned installation) to remain unaffected")
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
