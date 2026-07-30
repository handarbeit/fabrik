package githubauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// failingRunManifestFlow is installed in place of the package's
// runManifestFlow var by tests that assert the manifest flow must never be
// invoked (the backward-compatibility requirement: valid existing creds
// skip it entirely).
func failingRunManifestFlow(t *testing.T) func(ctx context.Context, opts ManifestFlowOptions) (Credentials, error) {
	return func(ctx context.Context, opts ManifestFlowOptions) (Credentials, error) {
		t.Fatal("runManifestFlow must not be invoked when valid local credentials already exist")
		return Credentials{}, nil
	}
}

// newLogCollector returns a Logf-compatible function and an accessor for
// every line it has recorded so far, safe for concurrent use.
func newLogCollector() (logf func(format string, args ...any), lines func() []string) {
	var mu sync.Mutex
	var collected []string
	logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		collected = append(collected, fmt.Sprintf(format, args...))
	}
	lines = func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(collected))
		copy(out, collected)
		return out
	}
	return logf, lines
}

// TestReconcile_BackwardCompat_SingleOwnerAutoDiscovered mirrors the old
// pruefer/auth.go TestBootstrapMulti_SingleOwnerAutoDiscovered: an operator
// with valid existing creds must get an identical client set with zero
// manifest-flow invocations.
func TestReconcile_BackwardCompat_SingleOwnerAutoDiscovered(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{{ID: 111, Account: "handarbeit"}}, func() time.Time {
		return time.Now().Add(time.Hour)
	})
	defer srv.Close()

	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"handarbeit/fabrik"}, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if r.BotLogin() != "pruefer-bot[bot]" {
		t.Errorf("BotLogin = %q, want pruefer-bot[bot]", r.BotLogin())
	}
	client, err := r.ClientForRepo(context.Background(), "handarbeit", "fabrik")
	if err != nil {
		t.Fatalf("ClientForRepo: %v", err)
	}
	if client.Token() == "" {
		t.Error("expected client token to be set")
	}
	if r.InstallationCount() != 1 {
		t.Errorf("InstallationCount() = %d, want 1", r.InstallationCount())
	}
}

func TestReconcile_BackwardCompat_PinnedInstallationSkipsDiscovery(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

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

	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppInstallationID: 999,
		AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"handarbeit/fabrik", "verveguy/otherrepo"}, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if discoveryCalled {
		t.Error("expected /app/installations to be skipped when AppInstallationID is explicitly configured")
	}
	c1, err := r.ClientForRepo(context.Background(), "handarbeit", "fabrik")
	if err != nil {
		t.Fatalf("ClientForRepo(handarbeit): %v", err)
	}
	c2, err := r.ClientForRepo(context.Background(), "verveguy", "otherrepo")
	if err != nil {
		t.Fatalf("ClientForRepo(verveguy): %v", err)
	}
	if c1 != c2 {
		t.Error("expected both owners to share the single pinned client")
	}
	if r.InstallationCount() != 1 {
		t.Errorf("InstallationCount() = %d, want 1 (one goroutine for the shared pin)", r.InstallationCount())
	}
}

// TestReconcile_PinnedInstallation_PopulatesInstallationRepoCache is the
// regression test for a review finding: the pinned-installation compat path
// (opts.AppInstallationID != 0) returned before ever calling
// saveInstallationRepoCache, so Credentials.InstallationRepoCache stayed
// unpopulated for every operator on the legacy pinned-installation escape
// hatch — even though the owners it should cache are already known at that
// point in Reconcile, with no extra GitHub call required.
func TestReconcile_PinnedInstallation_PopulatesInstallationRepoCache(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	statePath := filepath.Join(dir, "app-state.json")
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"slug": "pruefer-bot"})
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"token": "ghs_x", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppInstallationID: 999,
		AppStatePath: statePath,
		WatchedRepos: []string{"handarbeit/fabrik", "verveguy/otherrepo"}, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	saved, err := loadCredentials(statePath)
	if err != nil {
		t.Fatalf("loadCredentials after Reconcile: %v", err)
	}
	if len(saved.InstallationRepoCache["handarbeit"]) != 1 || saved.InstallationRepoCache["handarbeit"][0] != "handarbeit/fabrik" {
		t.Errorf("InstallationRepoCache[handarbeit] = %v, want [handarbeit/fabrik]", saved.InstallationRepoCache["handarbeit"])
	}
	if len(saved.InstallationRepoCache["verveguy"]) != 1 || saved.InstallationRepoCache["verveguy"][0] != "verveguy/otherrepo" {
		t.Errorf("InstallationRepoCache[verveguy] = %v, want [verveguy/otherrepo]", saved.InstallationRepoCache["verveguy"])
	}
}

func TestReconcile_BackwardCompat_MultiOwnerNeverTouchesStranger(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "verveguy"},
		{ID: 333, Account: "stranger"}, // installed on the App, never watched
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"handarbeit/fabrik", "verveguy/otherrepo"}, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	handarbeitClient, err := r.ClientForRepo(context.Background(), "handarbeit", "fabrik")
	if err != nil || handarbeitClient.Token() == "" {
		t.Fatalf("expected a minted client for owner handarbeit, err=%v", err)
	}
	verveguyClient, err := r.ClientForRepo(context.Background(), "verveguy", "otherrepo")
	if err != nil || verveguyClient.Token() == "" {
		t.Fatalf("expected a minted client for owner verveguy, err=%v", err)
	}
	if handarbeitClient.Token() == verveguyClient.Token() {
		t.Error("expected distinct tokens for distinct owners")
	}
	if _, err := r.ClientForRepo(context.Background(), "stranger", "repo"); err == nil {
		t.Fatal("expected no client for the unwatched stranger installation")
	}
	for _, instID := range fake.hitInstallations() {
		if instID == 333 {
			t.Fatal("expected installation 333 (stranger, unwatched) to never be minted a token")
		}
	}
	if r.InstallationCount() != 2 {
		t.Errorf("InstallationCount() = %d, want 2", r.InstallationCount())
	}
}

func TestReconcile_MissingOwnerInstallation_GuidesInstallInsteadOfHardFailing(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	oldBrowser := openBrowser
	var openedURL string
	openBrowser = func(url string) error {
		openedURL = url
		return nil
	}
	defer func() { openBrowser = oldBrowser }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	logf, lines := newLogCollector()
	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"handarbeit/fabrik", "notinstalled/otherrepo"}, BaseURL: srv.URL, Logf: logf,
	})
	if err != nil {
		t.Fatalf("Reconcile should not hard-fail on a missing owner installation, got: %v", err)
	}
	if _, err := r.ClientForRepo(context.Background(), "handarbeit", "fabrik"); err != nil {
		t.Errorf("expected handarbeit to remain authorized: %v", err)
	}
	if _, err := r.ClientForRepo(context.Background(), "notinstalled", "otherrepo"); err == nil {
		t.Error("expected notinstalled to have no client")
	}
	found := false
	for _, l := range lines() {
		if strings.Contains(l, "notinstalled") && strings.Contains(l, "installations/new") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a guided-install log line naming notinstalled and a grant URL, got: %v", lines())
	}
	wantURL := "https://github.com/apps/pruefer-bot/installations/new"
	if openedURL != wantURL {
		t.Errorf("expected guided installation to open the browser to %q, got %q", wantURL, openedURL)
	}
}

// TestReconcile_OwnerLookupIsCaseInsensitive is the regression test for a
// review finding: byAccount was keyed by GitHub's exact-case canonical
// Account login, but owner comes verbatim from the user's watched_repos
// config string. GitHub org/user logins are case-insensitive, so a config
// entry like "HandArbeit/fabrik" against a canonical "handarbeit"
// installation must still resolve to that installation, not be treated as
// uninstalled.
func TestReconcile_OwnerLookupIsCaseInsensitive(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	logf, lines := newLogCollector()
	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"HandArbeit/fabrik"}, BaseURL: srv.URL, Logf: logf,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := r.ClientForRepo(context.Background(), "HandArbeit", "fabrik"); err != nil {
		t.Errorf("expected a case-insensitive owner match to authorize HandArbeit: %v", err)
	}
	for _, l := range lines() {
		if strings.Contains(l, "installations/new") {
			t.Errorf("expected no guided-install guidance for an already-installed owner differing only in case, got: %v", lines())
		}
	}
}

func TestReconcile_MissingOwnerInstallation_NoBrowserSkipsOpen(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	oldBrowser := openBrowser
	openBrowser = func(url string) error {
		t.Error("openBrowser should not be called when NoBrowser is set")
		return nil
	}
	defer func() { openBrowser = oldBrowser }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	logf, _ := newLogCollector()
	_, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"notinstalled/otherrepo"}, BaseURL: srv.URL, NoBrowser: true, Logf: logf,
	})
	if err != nil {
		t.Fatalf("Reconcile should not hard-fail on a missing owner installation, got: %v", err)
	}
}

// TestReconcile_MultipleMissingOwnerInstallations_OpensBrowserOnlyOnce is
// the regression test for a review finding: WatchedRepos spanning several
// owners with no installation (a plausible first run watching repos under
// multiple orgs) must not pop open a separate browser tab/window per
// missing owner — only the first gets an actual browser-open attempt, every
// missing owner still gets its install URL logged.
func TestReconcile_MultipleMissingOwnerInstallations_OpensBrowserOnlyOnce(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	var openCount int
	oldBrowser := openBrowser
	defer func() { openBrowser = oldBrowser }()
	openBrowser = func(url string) error {
		openCount++
		return nil
	}

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	logf, lines := newLogCollector()
	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"handarbeit/fabrik", "orgone/repo", "orgtwo/repo"}, BaseURL: srv.URL, Logf: logf,
	})
	if err != nil {
		t.Fatalf("Reconcile should not hard-fail with multiple missing installations, got: %v", err)
	}
	if _, err := r.ClientForRepo(context.Background(), "handarbeit", "fabrik"); err != nil {
		t.Errorf("expected handarbeit to remain authorized: %v", err)
	}
	if openCount != 1 {
		t.Errorf("openBrowser called %d times, want exactly 1 (avoid popping a browser tab per missing owner)", openCount)
	}
	for _, owner := range []string{"orgone", "orgtwo"} {
		found := false
		for _, l := range lines() {
			if strings.Contains(l, owner) && strings.Contains(l, "installations/new") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a guided-install log line naming %s, got: %v", owner, lines())
		}
	}
}

func TestReconcile_SelectedModeExclusion_SoftStatusNotHardError(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "someorg", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{111: {"someorg/repo-one"}}
	defer srv.Close()

	logf, lines := newLogCollector()
	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"someorg/repo-one", "someorg/repo-excluded"}, BaseURL: srv.URL, Logf: logf,
	})
	if err != nil {
		t.Fatalf("Reconcile should not hard-fail on a selected-mode exclusion, got: %v", err)
	}
	if _, err := r.ClientForRepo(context.Background(), "someorg", "repo-one"); err != nil {
		t.Errorf("expected someorg to remain authorized: %v", err)
	}
	found := false
	for _, l := range lines() {
		if strings.Contains(l, "someorg/repo-excluded") && strings.Contains(l, "settings/installations") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a soft-status log line naming the excluded repo and a grant URL, got: %v", lines())
	}
}

// TestReconcile_PopulatesInstallationRepoCache is the regression test for
// the bug an external review found: Credentials.InstallationRepoCache was
// defined and round-trip tested but never actually populated by Reconcile,
// making it dead weight despite the issue's own credential-model
// requirement to persist "installation→repo mapping (cacheable metadata)".
// Covers both installation modes: "all" (every watched repo under that
// owner is cached as accessible, no live per-repo check) and "selected"
// (only the repos verifyRepoAccess actually confirms are cached).
func TestReconcile_PopulatesInstallationRepoCache(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	statePath := filepath.Join(dir, "app-state.json")
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit", RepositorySelection: "all"},
		{ID: 222, Account: "someorg", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{222: {"someorg/repo-one"}}
	defer srv.Close()

	_, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: []string{"handarbeit/fabrik", "someorg/repo-one", "someorg/repo-excluded"},
		BaseURL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	saved, err := loadCredentials(statePath)
	if err != nil {
		t.Fatalf("loadCredentials after Reconcile: %v", err)
	}
	if saved.Slug != "pruefer-bot" {
		t.Errorf("saved Slug = %q, want pruefer-bot", saved.Slug)
	}
	if len(saved.InstallationRepoCache["handarbeit"]) != 1 || saved.InstallationRepoCache["handarbeit"][0] != "handarbeit/fabrik" {
		t.Errorf("InstallationRepoCache[handarbeit] = %v, want [handarbeit/fabrik] (an \"all\" installation authorizes every watched repo)", saved.InstallationRepoCache["handarbeit"])
	}
	if len(saved.InstallationRepoCache["someorg"]) != 1 || saved.InstallationRepoCache["someorg"][0] != "someorg/repo-one" {
		t.Errorf("InstallationRepoCache[someorg] = %v, want [someorg/repo-one] only (repo-excluded is not granted by the selected-mode installation)", saved.InstallationRepoCache["someorg"])
	}
}

func TestReconcile_MissingPrivateKey_RepairErrorNeverBootstraps(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	_, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: filepath.Join(dir, "nonexistent.pem"),
		AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"handarbeit/fabrik"},
	})
	if err == nil {
		t.Fatal("expected a repair error when AppID is known but the private key is missing")
	}
	if !strings.Contains(err.Error(), "repair") {
		t.Errorf("error = %v, want it to describe a repair state, not a fresh bootstrap", err)
	}
}

func TestReconcile_CorruptAppStateFile_ReturnsErrorNeverBootstraps(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "app-state.json")
	if err := os.WriteFile(statePath, []byte("not json"), 0600); err != nil {
		t.Fatalf("writing corrupt state file: %v", err)
	}

	_, err := Reconcile(context.Background(), Options{
		AppPrivateKeyPath: filepath.Join(dir, "app-private-key.pem"),
		AppStatePath:      statePath,
		WatchedRepos:      []string{"handarbeit/fabrik"},
	})
	if err == nil {
		t.Fatal("expected an error for a corrupt app-state file")
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("error = %v, want it to mention the corrupt state file", err)
	}
}

func TestReconcile_NoCredentialsAtAll_RunsManifestFlow(t *testing.T) {
	dir := t.TempDir()
	pemPath := filepath.Join(dir, "app-private-key.pem")
	statePath := filepath.Join(dir, "app-state.json")

	oldFlow := runManifestFlow
	invoked := false
	runManifestFlow = func(ctx context.Context, opts ManifestFlowOptions) (Credentials, error) {
		invoked = true
		if err := savePrivateKey(opts.PrivateKeyPath, writeTestPrivateKeyPEM(t)); err != nil {
			t.Fatalf("savePrivateKey in stub: %v", err)
		}
		creds := Credentials{AppID: 314, Slug: "fresh-app"}
		if err := saveCredentials(opts.AppStatePath, creds); err != nil {
			t.Fatalf("saveCredentials in stub: %v", err)
		}
		return creds, nil
	}
	defer func() { runManifestFlow = oldFlow }()

	srv, _ := newFakeAppServer("fresh-app", []gh.AppInstallation{{ID: 555, Account: "handarbeit"}}, func() time.Time {
		return time.Now().Add(time.Hour)
	})
	defer srv.Close()

	r, err := Reconcile(context.Background(), Options{
		AppPrivateKeyPath: pemPath, AppStatePath: statePath,
		WatchedRepos: []string{"handarbeit/fabrik"}, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !invoked {
		t.Fatal("expected runManifestFlow to be invoked when no credentials exist at all")
	}
	if r.BotLogin() != "fresh-app[bot]" {
		t.Errorf("BotLogin = %q, want fresh-app[bot]", r.BotLogin())
	}
}

// TestReconcile_AppDeletedExternally_StateFileAppID_ReentersManifestFlow
// covers the safe self-heal case: the AppID came from a prior manifest run
// (AppStatePath), not from pinned config. Recreating here is safe because
// the next restart resolves the freshly-created App's ID from AppStatePath
// with opts.AppID == 0 — no stale config value can shadow it.
func TestReconcile_AppDeletedExternally_StateFileAppID_ReentersManifestFlow(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app-private-key.pem")
	if err := savePrivateKey(keyPath, writeTestPrivateKeyPEM(t)); err != nil {
		t.Fatalf("savePrivateKey: %v", err)
	}
	statePath := filepath.Join(dir, "app-state.json")
	if err := saveCredentials(statePath, Credentials{AppID: 42, Slug: "old-app"}); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	var appCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		appCalls++
		if appCalls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"app not found"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"slug": "recreated-app"})
	})
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldFlow := runManifestFlow
	invoked := false
	runManifestFlow = func(ctx context.Context, opts ManifestFlowOptions) (Credentials, error) {
		invoked = true
		if err := savePrivateKey(opts.PrivateKeyPath, writeTestPrivateKeyPEM(t)); err != nil {
			t.Fatalf("savePrivateKey in stub: %v", err)
		}
		creds := Credentials{AppID: 999, Slug: "recreated-app"}
		if err := saveCredentials(opts.AppStatePath, creds); err != nil {
			t.Fatalf("saveCredentials in stub: %v", err)
		}
		return creds, nil
	}
	defer func() { runManifestFlow = oldFlow }()

	logf, lines := newLogCollector()
	r, err := Reconcile(context.Background(), Options{
		AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: nil, BaseURL: srv.URL, Logf: logf,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !invoked {
		t.Fatal("expected runManifestFlow to be invoked after identity validation failed")
	}
	if r.BotLogin() != "recreated-app[bot]" {
		t.Errorf("BotLogin = %q, want recreated-app[bot]", r.BotLogin())
	}
	found := false
	for _, l := range lines() {
		if strings.Contains(l, "deleted") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a log line explaining why the App creation flow restarted, got: %v", lines())
	}

	// The regression this guards against: a second Reconcile call (as would
	// happen on the next process restart) must resolve the *new* AppID from
	// AppStatePath, not loop into creating yet another App.
	invoked = false
	r2, err := Reconcile(context.Background(), Options{
		AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: nil, BaseURL: srv.URL, Logf: logf,
	})
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if invoked {
		t.Fatal("second Reconcile should find the recreated App's valid credentials and skip the manifest flow entirely")
	}
	if r2.BotLogin() != "recreated-app[bot]" {
		t.Errorf("second Reconcile BotLogin = %q, want recreated-app[bot]", r2.BotLogin())
	}
}

// TestReconcile_AppDeletedExternally_PinnedAppID_ReturnsRepairErrorNeverLoops
// is the regression test for the bug an external review found: when AppID
// is pinned via explicit config (opts.AppID != 0, e.g. github_app_id in
// config.yaml), auto-recreating on identity-validation failure is unsafe —
// config.yaml is never written back to, so the next restart would resolve
// the same stale, now-deleted AppID again and create yet another orphan App,
// forever. Reconcile must instead return an explicit repair error and never
// invoke the manifest flow.
func TestReconcile_AppDeletedExternally_PinnedAppID_ReturnsRepairErrorNeverLoops(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app-private-key.pem")
	if err := savePrivateKey(keyPath, writeTestPrivateKeyPEM(t)); err != nil {
		t.Fatalf("savePrivateKey: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"app not found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	_, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: nil, BaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected an error when a pinned AppID fails identity validation")
	}
	if !strings.Contains(err.Error(), "repair") || !strings.Contains(err.Error(), "github_app_id") {
		t.Errorf("error = %v, want it to describe a github_app_id repair action, not a silent recreation", err)
	}
}

// TestReconcile_TransientAppIdentityFailure_ReturnsErrorNeverBootstraps is
// the regression test for the bug an external review found: appRequest
// treats a transport failure, timeout, or 5xx identically to a real
// 401/403/404 rejection, and a naive "any FetchAppSlug error means the App
// was deleted" check would kick off a full re-bootstrap (or, on the pinned
// path, a "repair required" error) on an ordinary transient hiccup — the
// steady-state restart path for every self-hosted operator, not just
// first-run. A 500 must be reported as a plain, retryable error and must
// never invoke runManifestFlow.
func TestReconcile_TransientAppIdentityFailure_ReturnsErrorNeverBootstraps(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app-private-key.pem")
	if err := savePrivateKey(keyPath, writeTestPrivateKeyPEM(t)); err != nil {
		t.Fatalf("savePrivateKey: %v", err)
	}
	statePath := filepath.Join(dir, "app-state.json")
	if err := saveCredentials(statePath, Credentials{AppID: 42, Slug: "my-app"}); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"internal server error"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	_, err := Reconcile(context.Background(), Options{
		AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: nil, BaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected an error when app identity validation fails with a 500")
	}
	if strings.Contains(err.Error(), "deleted") || strings.Contains(err.Error(), "repair") {
		t.Errorf("error = %v, want a plain transient-failure error, not deletion/repair language for a 500", err)
	}
	if !strings.Contains(err.Error(), "transient") {
		t.Errorf("error = %v, want it to describe the failure as transient", err)
	}
}

// TestReconcile_TransientAppIdentityFailure_PinnedAppID_ReturnsErrorNeverBootstraps
// is the pinned-AppID variant of the above: a 500 on the compat path must
// also be reported as a plain retryable error, not the "github_app_id ...
// repair required" message reserved for an actual 401/403/404 rejection.
func TestReconcile_TransientAppIdentityFailure_PinnedAppID_ReturnsErrorNeverBootstraps(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app-private-key.pem")
	if err := savePrivateKey(keyPath, writeTestPrivateKeyPEM(t)); err != nil {
		t.Fatalf("savePrivateKey: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"internal server error"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	_, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: nil, BaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected an error when a pinned AppID's identity validation fails with a 500")
	}
	if strings.Contains(err.Error(), "repair") {
		t.Errorf("error = %v, want a plain transient-failure error, not a repair-required message for a 500", err)
	}
	if !strings.Contains(err.Error(), "transient") {
		t.Errorf("error = %v, want it to describe the failure as transient", err)
	}
}

func TestReconcile_NoWatchedRepos_MintsNothing(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if r.InstallationCount() != 0 {
		t.Errorf("InstallationCount() = %d, want 0 with no watched repos", r.InstallationCount())
	}
	if fake.mintCount.Load() != 0 {
		t.Errorf("mintCount = %d, want 0 (no installation calls with zero watched repos)", fake.mintCount.Load())
	}
}

// TestReconcile_MalformedWatchedRepoEntry_LogsAndSkips is the regression
// test for a review finding: distinctOwners/splitOwnerRepo's doc comment
// claimed "Reconcile logs and skips" malformed watched_repos entries, but
// Reconcile never actually logged anything about them — a typo'd entry
// (e.g. missing slash) was silently dropped with zero operator-visible
// feedback.
func TestReconcile_MalformedWatchedRepoEntry_LogsAndSkips(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	logf, lines := newLogCollector()
	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"handarbeit/fabrik", "not-a-valid-entry"}, BaseURL: srv.URL, Logf: logf,
	})
	if err != nil {
		t.Fatalf("Reconcile should not hard-fail on a malformed watched-repo entry, got: %v", err)
	}
	if _, err := r.ClientForRepo(context.Background(), "handarbeit", "fabrik"); err != nil {
		t.Errorf("expected handarbeit to remain authorized: %v", err)
	}
	found := false
	for _, l := range lines() {
		if strings.Contains(l, "not-a-valid-entry") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a log line naming the malformed watched-repo entry, got: %v", lines())
	}
}
