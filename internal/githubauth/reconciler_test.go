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

// TestReconcile_PinnedInstallation_CacheWriteFailureIsNonFatal is the
// regression test for a review finding: saveInstallationRepoCache's call
// site in the pinned-AppInstallationID branch (unlike the discovery path,
// which already had coverage) had no test confirming a failing write there
// is merely logged, not surfaced as a Reconcile error — even though
// saveInstallationRepoCache has no error return at all, so this is true by
// construction today. Forces that structural guarantee into an explicit
// test so a future refactor threading an error return through this call
// can't silently make the pinned path fail reconciliation over a
// diagnostics-only cache write.
func TestReconcile_PinnedInstallation_CacheWriteFailureIsNonFatal(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)

	// A regular file in place of AppStatePath's parent directory makes
	// saveCredentials's os.MkdirAll (and therefore the whole write) fail
	// deterministically, without relying on OS-specific permission
	// semantics.
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatalf("writing blocker file: %v", err)
	}
	statePath := filepath.Join(blocker, "app-state.json")

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

	logf, lines := newLogCollector()
	_, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppInstallationID: 999,
		AppStatePath: statePath,
		WatchedRepos: []string{"handarbeit/fabrik"}, BaseURL: srv.URL, Logf: logf,
	})
	if err != nil {
		t.Fatalf("Reconcile must not fail when the diagnostics-only installation-repo cache write fails: %v", err)
	}
	found := false
	for _, l := range lines() {
		if strings.Contains(l, "could not update installation-repo cache") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a log line about the failed cache write, got: %v", lines())
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

// TestReconcile_CaseVariantOwnersDedupeToOneInstallation is the regression
// test for a review finding: distinctOwnersLogging deduped owners by exact
// case, so watched_repos entries naming the same GitHub account with
// different casing (e.g. "MyOrg/repo1" and "myorg/repo2") survived as two
// "distinct" owners. Both then independently resolved to the same
// installation via the already-case-insensitive byAccount lookup, so
// Reconcile minted a redundant token and ran verifyRepoAccess twice for one
// installation, and would have spawned two redundant refresh goroutines.
// Confirms exactly one installation is minted and ClientForRepo resolves
// both casings to it.
func TestReconcile_CaseVariantOwnersDedupeToOneInstallation(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "myorg", RepositorySelection: "all"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	logf, _ := newLogCollector()
	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"MyOrg/repo1", "myorg/repo2"}, BaseURL: srv.URL, Logf: logf,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if r.InstallationCount() != 1 {
		t.Errorf("InstallationCount() = %d, want 1 (case-variant owners must dedupe to one installation)", r.InstallationCount())
	}
	if _, err := r.ClientForRepo(context.Background(), "MyOrg", "repo1"); err != nil {
		t.Errorf("expected ClientForRepo(MyOrg) to resolve: %v", err)
	}
	if _, err := r.ClientForRepo(context.Background(), "myorg", "repo2"); err != nil {
		t.Errorf("expected ClientForRepo(myorg) to resolve to the same installation: %v", err)
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

// TestReconcile_RepoVerifyFailure_LogsSkippedNotAuthorized is the regression
// test for a review finding: when verifyRepoAccess errors (e.g. a transient
// failure listing /installation/repositories), the minted client is still
// usable, but the final "✓ owner authorized" log line previously implied
// repo access had actually been confirmed even though verification was
// skipped. The log line must say so.
func TestReconcile_RepoVerifyFailure_LogsSkippedNotAuthorized(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "someorg", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.failRepoList = func(installationID int64) bool { return installationID == 111 }
	defer srv.Close()

	logf, lines := newLogCollector()
	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"someorg/repo-one"}, BaseURL: srv.URL, Logf: logf,
	})
	if err != nil {
		t.Fatalf("Reconcile should not hard-fail on a repo-verify error, got: %v", err)
	}
	if _, err := r.ClientForRepo(context.Background(), "someorg", "repo-one"); err != nil {
		t.Errorf("expected the minted client to still be usable despite the verify failure: %v", err)
	}
	found := false
	for _, l := range lines() {
		if strings.Contains(l, "someorg") && strings.Contains(l, "authorized") {
			if !strings.Contains(l, "skipped") {
				t.Errorf("expected the authorized log line to note verification was skipped, got: %q", l)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("expected an authorized log line for someorg, got: %v", lines())
	}
}

// TestReconcile_TruncatedInstallationsListYieldsAmbiguousNotFoundMessage is
// the regression test for a review finding: FetchAppInstallations caps at
// 100 results and only logs truncation at the github package's own log tag,
// with no signal Reconcile could previously observe. An owner whose
// installation exists beyond the 100th result would be reported "has no
// installation" with the same confidence as a genuinely missing one — a
// false negative that triggers an unnecessary guided-install browser-open
// — unlike verifyRepoAccess's already-covered analogous case for
// FetchInstallationRepositories. This constructs exactly 100 fake
// installations (none matching the watched owner) and asserts the log line
// flags the ambiguity instead of confidently claiming no installation.
func TestReconcile_TruncatedInstallationsListYieldsAmbiguousNotFoundMessage(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	installations := make([]gh.AppInstallation, 100)
	for i := range installations {
		installations[i] = gh.AppInstallation{ID: int64(i + 1), Account: fmt.Sprintf("someorg-%03d", i)}
	}
	srv, _ := newFakeAppServer("pruefer-bot", installations, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)

	logf, lines := newLogCollector()
	_, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"missingowner/repo"}, BaseURL: srv.URL, NoBrowser: true, Logf: logf,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var found string
	for _, l := range lines() {
		if strings.Contains(l, "missingowner") {
			found = l
		}
	}
	if found == "" {
		t.Fatalf("expected a log line about missingowner, got: %v", lines())
	}
	if !strings.Contains(found, "100") || !strings.Contains(found, "pagination") {
		t.Errorf("log line = %q, expected it to flag the 100-result truncation as a possible pagination gap", found)
	}
	if strings.Contains(found, "has no installation") {
		t.Errorf("log line = %q, should not confidently claim no installation when the result set was truncated", found)
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

// TestReconcile_InstallationRepoCache_KeysAreLowerCased is the regression
// test for a review finding: the discovery-path branches (both "all" and
// "selected" installation modes) keyed InstallationRepoCache by the raw,
// first-seen-case owner from distinctOwnersLogging, while the pinned-
// installation path already lower-cased its keys — an inconsistency that
// would produce differently-capitalized cache entries for the same owner
// depending on which mode Reconcile ran under, and depending on which
// casing watched_repos happened to spell an owner with first. Uses a
// mixed-case owner in watched_repos to catch a regression to the old,
// inconsistent keying.
func TestReconcile_InstallationRepoCache_KeysAreLowerCased(t *testing.T) {
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
	fake.selectedRepos = map[int64][]string{222: {"SomeOrg/repo-one"}}
	defer srv.Close()

	_, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: []string{"HandArbeit/fabrik", "SomeOrg/repo-one"},
		BaseURL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	saved, err := loadCredentials(statePath)
	if err != nil {
		t.Fatalf("loadCredentials after Reconcile: %v", err)
	}
	if len(saved.InstallationRepoCache["handarbeit"]) != 1 {
		t.Errorf("InstallationRepoCache[handarbeit] (lower-cased key) = %v, want a single entry — got keys %v", saved.InstallationRepoCache["handarbeit"], saved.InstallationRepoCache)
	}
	if len(saved.InstallationRepoCache["someorg"]) != 1 {
		t.Errorf("InstallationRepoCache[someorg] (lower-cased key) = %v, want a single entry — got keys %v", saved.InstallationRepoCache["someorg"], saved.InstallationRepoCache)
	}
	if _, ok := saved.InstallationRepoCache["HandArbeit"]; ok {
		t.Errorf("InstallationRepoCache has a raw-case %q key — cache keys must always be lower-cased", "HandArbeit")
	}
	if _, ok := saved.InstallationRepoCache["SomeOrg"]; ok {
		t.Errorf("InstallationRepoCache has a raw-case %q key — cache keys must always be lower-cased", "SomeOrg")
	}
}

// TestReconcile_RepoVerifyFailure_PreservesPriorCacheEntry is the regression
// test for a review finding: saveInstallationRepoCache used to replace
// Credentials.InstallationRepoCache wholesale on every call. Since repoCache
// only contains entries for owners verifyRepoAccess got a definitive answer
// for this round, a single transient failure listing
// /installation/repositories wiped that owner's last-known-good cache entry
// to nothing, even though nothing about actual access changed. A second
// Reconcile call that hits the same transient failure must leave the first
// call's cached entry for that owner intact.
func TestReconcile_RepoVerifyFailure_PreservesPriorCacheEntry(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	statePath := filepath.Join(dir, "app-state.json")
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "someorg", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{111: {"someorg/repo-one"}}
	defer srv.Close()

	opts := Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: []string{"someorg/repo-one"}, BaseURL: srv.URL,
	}

	if _, err := Reconcile(context.Background(), opts); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	saved, err := loadCredentials(statePath)
	if err != nil {
		t.Fatalf("loadCredentials after first Reconcile: %v", err)
	}
	if len(saved.InstallationRepoCache["someorg"]) != 1 || saved.InstallationRepoCache["someorg"][0] != "someorg/repo-one" {
		t.Fatalf("InstallationRepoCache[someorg] after first Reconcile = %v, want [someorg/repo-one]", saved.InstallationRepoCache["someorg"])
	}

	fake.failRepoList = func(installationID int64) bool { return installationID == 111 }
	if _, err := Reconcile(context.Background(), opts); err != nil {
		t.Fatalf("second Reconcile (transient repo-list failure): %v", err)
	}

	saved, err = loadCredentials(statePath)
	if err != nil {
		t.Fatalf("loadCredentials after second Reconcile: %v", err)
	}
	if len(saved.InstallationRepoCache["someorg"]) != 1 || saved.InstallationRepoCache["someorg"][0] != "someorg/repo-one" {
		t.Errorf("InstallationRepoCache[someorg] after a transient verify failure = %v, want the prior entry [someorg/repo-one] preserved, not wiped", saved.InstallationRepoCache["someorg"])
	}
}

// TestReconcile_RepoVerifyDefiniteZeroAccess_OverwritesStaleCacheEntry is
// the regression test for a review finding: unlike the transient-failure
// case above, a *successful* verifyRepoAccess call that definitively finds
// zero authorized repos (e.g. a repo was removed from a selected-mode
// installation) is not a "we didn't check this round" outcome and must
// overwrite — not preserve — a stale prior "authorized" cache entry. Before
// the fix, repoCache only gained a key when at least one repo came back
// authorized, so a definitive zero-access answer was indistinguishable from
// the repoVerifyFailed case and silently left the old entry in place.
func TestReconcile_RepoVerifyDefiniteZeroAccess_OverwritesStaleCacheEntry(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	statePath := filepath.Join(dir, "app-state.json")
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "someorg", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{111: {"someorg/repo-one"}}
	defer srv.Close()

	opts := Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: []string{"someorg/repo-one"}, BaseURL: srv.URL,
	}

	if _, err := Reconcile(context.Background(), opts); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	saved, err := loadCredentials(statePath)
	if err != nil {
		t.Fatalf("loadCredentials after first Reconcile: %v", err)
	}
	if len(saved.InstallationRepoCache["someorg"]) != 1 || saved.InstallationRepoCache["someorg"][0] != "someorg/repo-one" {
		t.Fatalf("InstallationRepoCache[someorg] after first Reconcile = %v, want [someorg/repo-one]", saved.InstallationRepoCache["someorg"])
	}

	// The repo is removed from the installation's selected-repos grant —
	// a definitive, successful check that now finds zero access.
	fake.selectedRepos = map[int64][]string{111: {}}
	if _, err := Reconcile(context.Background(), opts); err != nil {
		t.Fatalf("second Reconcile (definitive zero access): %v", err)
	}

	saved, err = loadCredentials(statePath)
	if err != nil {
		t.Fatalf("loadCredentials after second Reconcile: %v", err)
	}
	if entries, ok := saved.InstallationRepoCache["someorg"]; !ok || len(entries) != 0 {
		t.Errorf("InstallationRepoCache[someorg] after a definitive zero-access verify = %v (present=%v), want an empty entry overwriting the stale [someorg/repo-one]", entries, ok)
	}
}

// TestReconcile_AppIDChange_ClearsStaleSecretsAndCache is the regression
// test for a review finding: if AppStatePath is reused across a switch to a
// different App (e.g. an operator repoints github_app_id at a new App,
// or a different App entirely gets pinned via config), saveInstallationRepoCache
// used to overwrite only AppID/Slug/InstallationRepoCache, leaving the
// previous App's WebhookSecret/ClientID/ClientSecret on disk under the new
// AppID/Slug pair — a latent mismatch a future webhook-transport consumer
// keyed on AppID would read as if it belonged to the new App.
func TestReconcile_AppIDChange_ClearsStaleSecretsAndCache(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	statePath := filepath.Join(dir, "app-state.json")

	// Seed app-state.json as if a prior App (ID 42) had completed a manifest
	// flow and left behind webhook/client secrets and a populated cache.
	if err := saveCredentials(statePath, Credentials{
		AppID: 42, Slug: "old-app", WebhookSecret: "old-hook-secret",
		ClientID: "old-client-id", ClientSecret: "old-client-secret",
		InstallationRepoCache: map[string][]string{"someorg": {"someorg/repo-one"}},
	}); err != nil {
		t.Fatalf("seeding app-state.json: %v", err)
	}

	// Reconcile now runs against a *different* App ID (99), e.g. because the
	// operator pointed github_app_id at a new App while reusing the same
	// AppStatePath.
	srv, _ := newFakeAppServer("new-app", []gh.AppInstallation{{ID: 222, Account: "someorg"}}, func() time.Time {
		return time.Now().Add(time.Hour)
	})
	defer srv.Close()

	if _, err := Reconcile(context.Background(), Options{
		AppID: 99, AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: []string{"someorg/repo-one"}, BaseURL: srv.URL,
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	saved, err := loadCredentials(statePath)
	if err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}
	if saved.AppID != 99 || saved.Slug != "new-app" {
		t.Errorf("saved AppID/Slug = %d/%q, want 99/new-app", saved.AppID, saved.Slug)
	}
	if saved.WebhookSecret != "" || saved.ClientID != "" || saved.ClientSecret != "" {
		t.Errorf("expected the previous App's webhook/client secrets to be cleared on an AppID change, got WebhookSecret=%q ClientID=%q ClientSecret=%q", saved.WebhookSecret, saved.ClientID, saved.ClientSecret)
	}
	if len(saved.InstallationRepoCache["someorg"]) != 1 || saved.InstallationRepoCache["someorg"][0] != "someorg/repo-one" {
		t.Errorf("InstallationRepoCache[someorg] = %v, want the newly-reconciled [someorg/repo-one], not stale data from the previous App", saved.InstallationRepoCache["someorg"])
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

// TestReconcile_MissingPrivateKey_AppIDFromStateFile_DoesNotClaimConfigured
// covers the case where AppID is resolved from AppStatePath (a prior
// manifest-flow run), not pinned via opts.AppID/github_app_id — the repair
// error must not claim "github_app_id N is configured" (found by bot
// review), since a fresh manifest bootstrap never sets that config value at
// all; an operator following that message would search for a config entry
// that was never there.
func TestReconcile_MissingPrivateKey_AppIDFromStateFile_DoesNotClaimConfigured(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "app-state.json")
	if err := saveCredentials(statePath, Credentials{AppID: 42, Slug: "old-app"}); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	_, err := Reconcile(context.Background(), Options{
		AppPrivateKeyPath: filepath.Join(dir, "nonexistent.pem"),
		AppStatePath:      statePath,
		WatchedRepos:      []string{"handarbeit/fabrik"},
	})
	if err == nil {
		t.Fatal("expected a repair error when AppID (from state) is known but the private key is missing")
	}
	if strings.Contains(err.Error(), "github_app_id 42 is configured") {
		t.Errorf("error = %v, falsely claims github_app_id is configured when AppID was resolved from the state file", err)
	}
	if !strings.Contains(err.Error(), "resolved from") {
		t.Errorf("error = %v, want it to name the state file as the source of App ID 42", err)
	}
	if !strings.Contains(err.Error(), "42") {
		t.Errorf("error = %v, want it to mention App ID 42", err)
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

// TestReconcile_StaleKeyAfterInterruptedSelfHeal_ReturnsRepairErrorNeverLoops
// is the regression test for a review finding: RunManifestFlow persists
// app-state.json (carrying the new AppID and PrivateKeyFingerprint) before
// the PEM. If self-heal recreates an App and the process is killed between
// those two writes, AppStatePath ends up naming the new App while
// AppPrivateKeyPath still holds the *previous* (presumed-deleted) App's key
// — both files exist and parse fine, so nothing before the fingerprint
// check would ever notice. Without it, every subsequent Reconcile would
// build a JWT with the mismatched key, fail identity validation again, and
// silently self-heal a *second* time — an unbounded orphan-App-creation
// loop with no error ever naming the real cause. This test sets up exactly
// that post-crash on-disk state directly (state file already updated to the
// new AppID/fingerprint, PEM still the old App's) and asserts Reconcile
// returns a clear repair-needed error instead of ever invoking
// runManifestFlow again.
func TestReconcile_StaleKeyAfterInterruptedSelfHeal_ReturnsRepairErrorNeverLoops(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app-private-key.pem")
	statePath := filepath.Join(dir, "app-state.json")

	oldPEM := writeTestPrivateKeyPEM(t)
	newPEM := writeTestPrivateKeyPEM(t)
	if err := savePrivateKey(keyPath, oldPEM); err != nil {
		t.Fatalf("savePrivateKey: %v", err)
	}
	// Simulates the state half of self-heal's persistence having
	// succeeded — naming a new App and the fingerprint of the PEM it
	// *should* have — while the PEM write that was supposed to follow
	// never happened (the crash window this guards).
	if err := saveCredentials(statePath, Credentials{
		AppID: 999, Slug: "recreated-app",
		PrivateKeyFingerprint: privateKeyFingerprint(newPEM),
	}); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	oldFlow := runManifestFlow
	runManifestFlow = func(ctx context.Context, opts ManifestFlowOptions) (Credentials, error) {
		t.Fatal("runManifestFlow must not be invoked — a fingerprint mismatch is a repair-needed error, not grounds for another self-heal attempt")
		return Credentials{}, nil
	}
	defer func() { runManifestFlow = oldFlow }()

	_, err := Reconcile(context.Background(), Options{
		AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: nil, BaseURL: "http://unused.invalid",
	})
	if err == nil {
		t.Fatal("expected an error for the fingerprint mismatch, got nil")
	}
	for _, want := range []string{"999", "fingerprint", "repair required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing expected substring %q", err.Error(), want)
		}
	}
}

// TestReconcile_SelfHealRecreatedApp_TransientIdentityFailure_RetriesInsteadOfFailing
// is the regression test for a review finding: after the self-heal branch
// recreates the App via runManifestFlow, the immediately-following identity
// check was not retried like the justBootstrapped branch's is — so a
// transient 401/403/404 right after creation (propagation lag) returned a
// hard error even though AppStatePath/PEM had already been persisted for the
// new App. The next Reconcile call would then load that new AppID from
// state, fail identity validation again, and self-heal a *second* time,
// producing an orphan App. Reconcile must retry the post-recreation identity
// check the same way before giving up.
func TestReconcile_SelfHealRecreatedApp_TransientIdentityFailure_RetriesInsteadOfFailing(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app-private-key.pem")
	if err := savePrivateKey(keyPath, writeTestPrivateKeyPEM(t)); err != nil {
		t.Fatalf("savePrivateKey: %v", err)
	}
	statePath := filepath.Join(dir, "app-state.json")
	if err := saveCredentials(statePath, Credentials{AppID: 42, Slug: "old-app"}); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	oldSleep := identityValidationSleep
	identityValidationSleep = func(context.Context, time.Duration) error { return nil }
	defer func() { identityValidationSleep = oldSleep }()

	var appCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		appCalls++
		switch appCalls {
		case 1:
			// Original App ID 42's identity check: deleted, triggers self-heal.
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"app not found"}`))
		case 2:
			// First identity check on the freshly-recreated App: transient
			// propagation lag, not a second deletion.
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"app not found"}`))
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{"slug": "recreated-app"})
		}
	})
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldFlow := runManifestFlow
	var bootstrapCalls int
	runManifestFlow = func(ctx context.Context, opts ManifestFlowOptions) (Credentials, error) {
		bootstrapCalls++
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

	r, err := Reconcile(context.Background(), Options{
		AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: nil, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if bootstrapCalls != 1 {
		t.Fatalf("runManifestFlow called %d times, want exactly 1 (retry must not trigger a second self-heal)", bootstrapCalls)
	}
	if r.BotLogin() != "recreated-app[bot]" {
		t.Errorf("BotLogin = %q, want recreated-app[bot]", r.BotLogin())
	}
	if appCalls < 3 {
		t.Errorf("expected at least 3 calls to /app (original failure + recreated-App failure + retry), got %d", appCalls)
	}
}

// TestReconcile_SelfHealRecreatedApp_PersistentIdentityFailure_ReturnsErrorNeverDoubleHeals
// covers the case where the recreated App's identity check never resolves:
// Reconcile must give up with a distinct, non-"deleted" error rather than
// falling through to self-heal a second time within the same call.
func TestReconcile_SelfHealRecreatedApp_PersistentIdentityFailure_ReturnsErrorNeverDoubleHeals(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app-private-key.pem")
	if err := savePrivateKey(keyPath, writeTestPrivateKeyPEM(t)); err != nil {
		t.Fatalf("savePrivateKey: %v", err)
	}
	statePath := filepath.Join(dir, "app-state.json")
	if err := saveCredentials(statePath, Credentials{AppID: 42, Slug: "old-app"}); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	oldSleep := identityValidationSleep
	identityValidationSleep = func(context.Context, time.Duration) error { return nil }
	defer func() { identityValidationSleep = oldSleep }()

	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"app not found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldFlow := runManifestFlow
	var bootstrapCalls int
	runManifestFlow = func(ctx context.Context, opts ManifestFlowOptions) (Credentials, error) {
		bootstrapCalls++
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

	_, err := Reconcile(context.Background(), Options{
		AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: nil, BaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected an error when the recreated App's identity never resolves")
	}
	if bootstrapCalls != 1 {
		t.Fatalf("runManifestFlow called %d times, want exactly 1 (must never self-heal twice within one Reconcile call)", bootstrapCalls)
	}
	if strings.Contains(err.Error(), "may have been deleted") {
		t.Errorf("error = %v, want it to NOT claim a second deletion — the App was just recreated in this same call", err)
	}
	if !strings.Contains(err.Error(), "just created") {
		t.Errorf("error = %v, want it to explain the App was just created in this run", err)
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

// TestReconcile_AppDeletedExternally_PinnedInstallationID_ReturnsRepairErrorNeverMintsStaleInstall
// is the regression test for a review finding: when opts.AppID is *not*
// pinned (resolved from the state file) but opts.AppInstallationID *is*
// pinned, and the state-file App fails identity validation, the old code
// fell into the "safe to self-heal" branch — creating a brand-new App with
// a new AppID, then falling through to mint an installation token using
// that new AppID against the *old*, now-stale pinned installation ID
// (installation IDs are App-specific). That mint would fail with an opaque
// GitHub error instead of a clear diagnosis, and runManifestFlow would have
// been invoked (creating an orphan App) even though the pin made recovery
// unsafe. Reconcile must instead return an explicit repair error and never
// invoke the manifest flow.
func TestReconcile_AppDeletedExternally_PinnedInstallationID_ReturnsRepairErrorNeverMintsStaleInstall(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app-private-key.pem")
	if err := savePrivateKey(keyPath, writeTestPrivateKeyPEM(t)); err != nil {
		t.Fatalf("savePrivateKey: %v", err)
	}
	statePath := filepath.Join(dir, "app-state.json")
	if err := saveCredentials(statePath, Credentials{AppID: 42, Slug: "old-app"}); err != nil {
		t.Fatalf("saveCredentials: %v", err)
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
		AppPrivateKeyPath: keyPath, AppStatePath: statePath, AppInstallationID: 999,
		WatchedRepos: []string{"handarbeit/fabrik"}, BaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected an error when a pinned AppInstallationID's App fails identity validation")
	}
	if !strings.Contains(err.Error(), "repair") || !strings.Contains(err.Error(), "github_app_installation_id") {
		t.Errorf("error = %v, want it to describe a github_app_installation_id repair action, not a silent recreation", err)
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

// TestReconcile_JustBootstrapped_TransientIdentityFailure_RetriesInsteadOfSelfHealing
// is the regression test for the bug an external review found: when
// loadOrBootstrapCredentials runs the manifest flow because no local
// credentials existed at all (opts.AppID == 0, no app-state.json), the very
// next call — FetchAppSlug, a few lines below — could transiently 401/403/404
// (e.g. brief propagation lag on a brand-new App). Before this fix, that was
// indistinguishable from "the App was deleted externally" and immediately
// re-invoked runManifestFlow, silently walking the user through creating a
// second App. Reconcile must instead retry the identity check a few times
// before ever considering self-heal, and must never call runManifestFlow a
// second time within the same call.
func TestReconcile_JustBootstrapped_TransientIdentityFailure_RetriesInsteadOfSelfHealing(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app-private-key.pem")
	statePath := filepath.Join(dir, "app-state.json")

	oldSleep := identityValidationSleep
	identityValidationSleep = func(context.Context, time.Duration) error { return nil }
	defer func() { identityValidationSleep = oldSleep }()

	var appCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		appCalls++
		if appCalls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"app not found"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"slug": "brand-new-app"})
	})
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldFlow := runManifestFlow
	var bootstrapCalls int
	runManifestFlow = func(ctx context.Context, opts ManifestFlowOptions) (Credentials, error) {
		bootstrapCalls++
		if err := savePrivateKey(opts.PrivateKeyPath, writeTestPrivateKeyPEM(t)); err != nil {
			t.Fatalf("savePrivateKey in stub: %v", err)
		}
		creds := Credentials{AppID: 999, Slug: "brand-new-app"}
		if err := saveCredentials(opts.AppStatePath, creds); err != nil {
			t.Fatalf("saveCredentials in stub: %v", err)
		}
		return creds, nil
	}
	defer func() { runManifestFlow = oldFlow }()

	r, err := Reconcile(context.Background(), Options{
		AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: nil, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if bootstrapCalls != 1 {
		t.Fatalf("runManifestFlow called %d times, want exactly 1 (retry must not trigger a second App creation)", bootstrapCalls)
	}
	if r.BotLogin() != "brand-new-app[bot]" {
		t.Errorf("BotLogin = %q, want brand-new-app[bot]", r.BotLogin())
	}
	if appCalls < 2 {
		t.Errorf("expected at least 2 calls to /app (initial failure + retry), got %d", appCalls)
	}
}

// TestReconcile_JustBootstrapped_PersistentIdentityFailure_ReturnsErrorNeverDoubleBootstraps
// covers the case where the just-created App's identity check keeps failing
// through every retry: Reconcile must give up with a distinct error rather
// than falling through to the general "may have been deleted" self-heal path
// (which would create a second App).
func TestReconcile_JustBootstrapped_PersistentIdentityFailure_ReturnsErrorNeverDoubleBootstraps(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app-private-key.pem")
	statePath := filepath.Join(dir, "app-state.json")

	oldSleep := identityValidationSleep
	identityValidationSleep = func(context.Context, time.Duration) error { return nil }
	defer func() { identityValidationSleep = oldSleep }()

	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"app not found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldFlow := runManifestFlow
	var bootstrapCalls int
	runManifestFlow = func(ctx context.Context, opts ManifestFlowOptions) (Credentials, error) {
		bootstrapCalls++
		if err := savePrivateKey(opts.PrivateKeyPath, writeTestPrivateKeyPEM(t)); err != nil {
			t.Fatalf("savePrivateKey in stub: %v", err)
		}
		creds := Credentials{AppID: 999, Slug: "brand-new-app"}
		if err := saveCredentials(opts.AppStatePath, creds); err != nil {
			t.Fatalf("saveCredentials in stub: %v", err)
		}
		return creds, nil
	}
	defer func() { runManifestFlow = oldFlow }()

	_, err := Reconcile(context.Background(), Options{
		AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: nil, BaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected an error when the just-bootstrapped App's identity never resolves")
	}
	if bootstrapCalls != 1 {
		t.Fatalf("runManifestFlow called %d times, want exactly 1 (must never double-bootstrap within one Reconcile call)", bootstrapCalls)
	}
	if strings.Contains(err.Error(), "deleted") {
		t.Errorf("error = %v, want it to NOT claim deletion — the App was just created in this same call", err)
	}
	if !strings.Contains(err.Error(), "just created") {
		t.Errorf("error = %v, want it to explain the App was just created in this run", err)
	}
}

// TestReconcile_JustBootstrapped_PersistentIdentityFailure_WithPinnedInstallationID_MentionsInstallationID
// is the regression test for a review finding: when a just-manifest-created
// App's identity check never resolves AND opts.AppInstallationID is also
// pinned (a valid, if narrow, config combination — an operator can set
// github_app_installation_id without github_app_id), the returned error must
// name the installation-ID mismatch that will also break the next mint
// attempt, exactly as the sibling opts.AppInstallationID != 0 branch already
// does for the non-just-bootstrapped case.
func TestReconcile_JustBootstrapped_PersistentIdentityFailure_WithPinnedInstallationID_MentionsInstallationID(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app-private-key.pem")
	statePath := filepath.Join(dir, "app-state.json")

	oldSleep := identityValidationSleep
	identityValidationSleep = func(context.Context, time.Duration) error { return nil }
	defer func() { identityValidationSleep = oldSleep }()

	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"app not found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldFlow := runManifestFlow
	var bootstrapCalls int
	runManifestFlow = func(ctx context.Context, opts ManifestFlowOptions) (Credentials, error) {
		bootstrapCalls++
		if err := savePrivateKey(opts.PrivateKeyPath, writeTestPrivateKeyPEM(t)); err != nil {
			t.Fatalf("savePrivateKey in stub: %v", err)
		}
		creds := Credentials{AppID: 999, Slug: "brand-new-app"}
		if err := saveCredentials(opts.AppStatePath, creds); err != nil {
			t.Fatalf("saveCredentials in stub: %v", err)
		}
		return creds, nil
	}
	defer func() { runManifestFlow = oldFlow }()

	_, err := Reconcile(context.Background(), Options{
		AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		AppInstallationID: 555,
		WatchedRepos:      nil, BaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected an error when the just-bootstrapped App's identity never resolves")
	}
	if bootstrapCalls != 1 {
		t.Fatalf("runManifestFlow called %d times, want exactly 1 (must never double-bootstrap within one Reconcile call)", bootstrapCalls)
	}
	if !strings.Contains(err.Error(), "just created") {
		t.Errorf("error = %v, want it to explain the App was just created in this run", err)
	}
	if !strings.Contains(err.Error(), "555") {
		t.Errorf("error = %v, want it to mention the pinned github_app_installation_id (555) that also needs updating", err)
	}
	// A pinned installation ID necessarily predates the App this call just
	// created — it cannot be an installation of a brand-new App, so the
	// message must not claim otherwise (found by bot review: the original
	// wording said it "still refers to this new App's installation," which
	// is structurally impossible).
	if strings.Contains(err.Error(), "still refers to this new App") {
		t.Errorf("error = %v, claims the stale pinned installation ID refers to the new App, which is impossible", err)
	}
	if !strings.Contains(err.Error(), "leftover pin") {
		t.Errorf("error = %v, want it to describe the pinned installation ID as a leftover from before this run", err)
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

// TestIdentityValidationSleep_HonorsContextCancellation is the regression
// test for a review finding: retryIdentityCheckAfterCreation's backoff
// previously slept unconditionally (time.Sleep(delay)) between attempts, so
// a canceled context (e.g. shutdown mid-retry) couldn't shorten the wait —
// bounded at ~1s total across identityValidationRetryDelays, but still
// inconsistent with the cancellation discipline used everywhere else in
// this package. The default identityValidationSleep must now return
// promptly, with ctx.Err(), when ctx is canceled before its timer fires.
func TestIdentityValidationSleep_HonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- identityValidationSleep(ctx, time.Hour) // would hang for an hour if cancellation weren't honored
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a canceled context")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("identityValidationSleep did not honor context cancellation within 5s")
	}
}

// TestRetryIdentityCheckAfterCreation_ContextCanceledDuringBackoff_ReturnsPromptly
// confirms retryIdentityCheckAfterCreation itself propagates a canceled
// context out of its backoff loop rather than always sleeping out every
// configured delay first.
func TestRetryIdentityCheckAfterCreation_ContextCanceledDuringBackoff_ReturnsPromptly(t *testing.T) {
	oldDelays := identityValidationRetryDelays
	identityValidationRetryDelays = []time.Duration{time.Hour, time.Hour} // would hang if cancellation weren't honored
	defer func() { identityValidationRetryDelays = oldDelays }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := retryIdentityCheckAfterCreation(ctx, "", "fake-jwt")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a canceled context")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retryIdentityCheckAfterCreation did not honor context cancellation within 5s")
	}
}

// TestReconcile_JustBootstrapped_ContextCanceledDuringRetryBackoff_DoesNotClaimRetriesExhausted
// is the regression test for a review finding: the justBootstrapped
// identity-failure error unconditionally said "its identity still doesn't
// resolve on GitHub after N retries" — misleading when the actual cause of
// retryIdentityCheckAfterCreation giving up was a canceled context (e.g.
// shutdown mid-wait), not GitHub genuinely rejecting the identity check N
// times over. The message must describe cancellation distinctly.
func TestReconcile_JustBootstrapped_ContextCanceledDuringRetryBackoff_DoesNotClaimRetriesExhausted(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app-private-key.pem")
	statePath := filepath.Join(dir, "app-state.json")

	oldSleep := identityValidationSleep
	identityValidationSleep = func(ctx context.Context, d time.Duration) error {
		return context.Canceled // simulates a cancellation interrupting the backoff wait
	}
	defer func() { identityValidationSleep = oldSleep }()

	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"app not found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldFlow := runManifestFlow
	runManifestFlow = func(ctx context.Context, opts ManifestFlowOptions) (Credentials, error) {
		if err := savePrivateKey(opts.PrivateKeyPath, writeTestPrivateKeyPEM(t)); err != nil {
			t.Fatalf("savePrivateKey in stub: %v", err)
		}
		creds := Credentials{AppID: 999, Slug: "brand-new-app"}
		if err := saveCredentials(opts.AppStatePath, creds); err != nil {
			t.Fatalf("saveCredentials in stub: %v", err)
		}
		return creds, nil
	}
	defer func() { runManifestFlow = oldFlow }()

	_, err := Reconcile(context.Background(), Options{
		AppPrivateKeyPath: keyPath, AppStatePath: statePath,
		WatchedRepos: nil, BaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected an error when the retry backoff is canceled")
	}
	if strings.Contains(err.Error(), "still doesn't resolve on GitHub after") {
		t.Errorf("error = %v, want it to NOT claim retries were exhausted against GitHub — the wait was canceled, not GitHub-rejected", err)
	}
	if !strings.Contains(err.Error(), "canceled") {
		t.Errorf("error = %v, want it to describe the cancellation", err)
	}
	if !strings.Contains(err.Error(), "just created") {
		t.Errorf("error = %v, want it to still explain the App was just created in this run", err)
	}
	// Regression check for a review finding: identityRetryFailureClause's
	// canceled-context branch previously restated "(context canceled)"
	// itself, on top of the wrapped err's own "%w" already surfacing that
	// same text — a mildly redundant "canceled ... canceled" in one
	// sentence. The word must now appear exactly once.
	if n := strings.Count(err.Error(), "canceled"); n != 1 {
		t.Errorf("error = %v, contains %q %d times, want exactly once (not redundantly restated)", err, "canceled", n)
	}
}
