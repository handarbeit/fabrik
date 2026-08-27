package githubauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// TestDerive_FutureRepoPickup is the regression test for AC2/AC3's "future
// repos become pollable with no restart" requirement: Derive always
// re-fetches live from GitHub rather than trusting any cache, so a repo
// added to an installation's grant between two Derive calls must appear in
// the second call's result with no restart and no re-mint of the owner's
// token.
func TestDerive_FutureRepoPickup(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{111: {"handarbeit/fabrik"}}
	defer srv.Close()

	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	first := r.LastDerived()
	if len(first.Repos) != 1 || first.Repos[0].Repo != "handarbeit/fabrik" {
		t.Fatalf("first derivation = %+v, want just handarbeit/fabrik", first.Repos)
	}
	mintCountAfterFirst := fake.mintCountFor(111)

	// The installation's grant gains a new repo — e.g. the operator created
	// it, or an "all"-mode installation simply has a new repo now visible.
	fake.selectedRepos = map[int64][]string{111: {"handarbeit/fabrik", "handarbeit/new-repo"}}

	set, detached, err := r.Derive(context.Background(), nil, 0, func(string, ...any) {})
	if err != nil {
		t.Fatalf("Derive (second call): %v", err)
	}
	if len(detached) != 0 {
		t.Errorf("expected no detached owners, got %+v", detached)
	}
	if len(set.Repos) != 2 {
		t.Fatalf("second derivation = %+v, want both handarbeit repos", set.Repos)
	}
	foundNew := false
	for _, dr := range set.Repos {
		if dr.Repo == "handarbeit/new-repo" {
			foundNew = true
		}
	}
	if !foundNew {
		t.Errorf("expected handarbeit/new-repo to appear in the second derivation, got %+v", set.Repos)
	}
	// The owner's installation was already known — Derive must not have
	// re-minted a fresh token for it just to pick up the new repo.
	if got := fake.mintCountFor(111); got != mintCountAfterFirst {
		t.Errorf("mint count for installation 111 = %d after second Derive, want unchanged from %d (no re-mint for an already-known installation)", got, mintCountAfterFirst)
	}
}

// TestDerive_MaxDerivedReposCapsDeterministically is R5/AC6's regression
// test: a synthetic installation granting far more repos than
// max_derived_repos must be capped, with Capped/CapApplied reported, and the
// same repos dropped every time (sorted owner/repo ascending, not an
// arbitrary API-order tail).
func TestDerive_MaxDerivedReposCapsDeterministically(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	const totalRepos = 250
	const wantCap = 200
	repos := make([]string, totalRepos)
	for i := range repos {
		repos[i] = fmt.Sprintf("bigorg/repo-%03d", i)
	}

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "bigorg", RepositorySelection: "all"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{111: repos}
	defer srv.Close()

	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		MaxDerivedRepos: wantCap, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	set := r.LastDerived()
	if !set.Capped {
		t.Fatal("expected Capped = true")
	}
	if set.CapApplied != wantCap {
		t.Errorf("CapApplied = %d, want %d", set.CapApplied, wantCap)
	}
	if len(set.Repos) != wantCap {
		t.Fatalf("len(Repos) = %d, want %d", len(set.Repos), wantCap)
	}
	if set.TotalGranted() != totalRepos {
		t.Errorf("TotalGranted() = %d, want %d (the full grant, before capping)", set.TotalGranted(), totalRepos)
	}
	// Deterministic: sorted owner/repo ascending, so repo-199 (0-indexed)
	// survives and repo-249 does not.
	if set.Repos[len(set.Repos)-1].Repo != "bigorg/repo-199" {
		t.Errorf("last surviving repo = %q, want bigorg/repo-199 (sorted, first 200 kept)", set.Repos[len(set.Repos)-1].Repo)
	}
	for _, dr := range set.Repos {
		if dr.Repo == "bigorg/repo-249" {
			t.Error("expected bigorg/repo-249 to be dropped by the cap, found it in the surviving set")
		}
	}

	// Calling Derive again must drop exactly the same repos, not an
	// arbitrary subset — determinism across repeated calls.
	set2, _, err := r.Derive(context.Background(), nil, wantCap, func(string, ...any) {})
	if err != nil {
		t.Fatalf("Derive (second call): %v", err)
	}
	if len(set2.Repos) != len(set.Repos) {
		t.Fatalf("second Derive returned %d repos, want %d", len(set2.Repos), len(set.Repos))
	}
	for i := range set.Repos {
		if set.Repos[i].Repo != set2.Repos[i].Repo {
			t.Errorf("repo[%d] = %q on first call, %q on second — capping must be deterministic", i, set.Repos[i].Repo, set2.Repos[i].Repo)
		}
	}
}

// TestDerive_PreCapCount_ReflectsPostFilterNotTotalGrant is the regression
// test for a review finding (handarbeit-pruefer): the "capped from N"
// log/TUI message used to report TotalGranted() — the installation grant's
// raw, pre-watched_repos-filter total — as the pre-cap count. That's wrong
// whenever a narrowing watched_repos filter and max_derived_repos are both
// active: the cap actually slices from the post-filter count, not the raw
// grant, so reporting TotalGranted() misattributes repos the filter already
// excluded to the cap instead. PreCapCount must reflect the post-filter,
// pre-cap count that was actually sliced.
func TestDerive_PreCapCount_ReflectsPostFilterNotTotalGrant(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	const totalGrant = 10
	const wantCap = 3
	repos := make([]string, totalGrant)
	for i := range repos {
		repos[i] = fmt.Sprintf("bigorg/repo-%03d", i)
	}
	// filter narrows the grant to 5 of the 10 repos.
	filter := repos[:5]

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "bigorg", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{111: repos}
	defer srv.Close()

	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: filter, MaxDerivedRepos: wantCap, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	set := r.LastDerived()
	if !set.Capped {
		t.Fatal("expected Capped = true")
	}
	if len(set.Repos) != wantCap {
		t.Fatalf("len(Repos) = %d, want %d", len(set.Repos), wantCap)
	}
	if set.TotalGranted() != totalGrant {
		t.Errorf("TotalGranted() = %d, want %d (the raw installation grant, before the filter)", set.TotalGranted(), totalGrant)
	}
	if set.PreCapCount != len(filter) {
		t.Errorf("PreCapCount = %d, want %d (the post-filter count the cap actually sliced from, not TotalGranted()=%d)", set.PreCapCount, len(filter), set.TotalGranted())
	}
}

// --- AC7 containment regression: two distinct App identities ---

// jwtIssuer decodes the "iss" claim (App ID) from an unverified JWT — good
// enough for this test's fake server, which only needs to distinguish which
// App a request is authenticating as, not verify the signature.
func jwtIssuer(t *testing.T, jwt string) int64 {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed JWT: %q", jwt)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding JWT payload: %v", err)
	}
	var claims struct {
		Iss int64 `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshaling JWT claims: %v", err)
	}
	return claims.Iss
}

// multiAppFakeServer serves /app, /app/installations, .../access_tokens, and
// /installation/repositories for TWO distinct GitHub App identities on one
// httptest server, keyed by the JWT's own "iss" claim (App ID) — simulating
// two operators, each with their own dedicated App (post-#1253's model),
// sharing nothing except this test's HTTP endpoint. This is what makes AC7's
// containment property structural rather than allowlist-based: a request
// authenticated as App A's JWT can only ever resolve App A's installations,
// because GitHub's own /app/installations is scoped to the calling App's
// identity by construction — there is no owner-allowlist code path to bypass
// or forget.
type multiAppFakeServer struct {
	t *testing.T

	mu               sync.Mutex
	installTokenToID map[string]int64 // token -> installation ID, across both apps

	appAID    int64
	appASlug  string
	appAInsts []gh.AppInstallation
	appARepos map[int64][]string

	appBID    int64
	appBSlug  string
	appBInsts []gh.AppInstallation
	appBRepos map[int64][]string

	// contactedAppB is set if any request ever authenticates as App B's
	// JWT/installation token — the assertion this whole test exists to make.
	contactedAppB bool
}

func newMultiAppFakeServer(t *testing.T) (*httptest.Server, *multiAppFakeServer) {
	f := &multiAppFakeServer{
		t:                t,
		installTokenToID: map[string]int64{},
		appAID:           42, appASlug: "operator-a-bot",
		appAInsts: []gh.AppInstallation{{ID: 1001, Account: "operator-a-org"}},
		appARepos: map[int64][]string{1001: {"operator-a-org/repo"}},
		appBID:    99, appBSlug: "operator-b-bot",
		appBInsts: []gh.AppInstallation{{ID: 2001, Account: "operator-b-org"}},
		appBRepos: map[int64][]string{2001: {"operator-b-org/repo"}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		iss := jwtIssuer(f.t, jwt)
		f.recordIfAppB(iss)
		slug := f.appASlug
		if iss == f.appBID {
			slug = f.appBSlug
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"slug": slug, "id": iss})
	})
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		iss := jwtIssuer(f.t, jwt)
		f.recordIfAppB(iss)
		if r.URL.Query().Get("page") != "" && r.URL.Query().Get("page") != "1" {
			json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}
		insts := f.appAInsts
		if iss == f.appBID {
			insts = f.appBInsts
		}
		raw := make([]map[string]interface{}, len(insts))
		for i, inst := range insts {
			raw[i] = map[string]interface{}{"id": inst.ID, "account": map[string]string{"login": inst.Account}}
		}
		json.NewEncoder(w).Encode(raw)
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		var instID int64
		fmt.Sscanf(r.URL.Path, "/app/installations/%d/access_tokens", &instID)
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		iss := jwtIssuer(f.t, jwt)
		f.recordIfAppB(iss)

		token := fmt.Sprintf("ghs_token_app%d_inst%d", iss, instID)
		f.mu.Lock()
		f.installTokenToID[token] = instID
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"token": token, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		instID := f.installTokenToID[auth]
		f.mu.Unlock()
		if strings.Contains(auth, fmt.Sprintf("app%d", f.appBID)) {
			f.mu.Lock()
			f.contactedAppB = true
			f.mu.Unlock()
		}
		if r.URL.Query().Get("page") != "" && r.URL.Query().Get("page") != "1" {
			json.NewEncoder(w).Encode(map[string]interface{}{"repositories": []map[string]interface{}{}})
			return
		}
		repos := f.appARepos[instID]
		if _, ok := f.appBRepos[instID]; ok {
			repos = f.appBRepos[instID]
		}
		out := make([]map[string]interface{}, len(repos))
		for i, name := range repos {
			out[i] = map[string]interface{}{"full_name": name}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"repositories": out})
	})
	return httptest.NewServer(mux), f
}

func (f *multiAppFakeServer) recordIfAppB(iss int64) {
	if iss != f.appBID {
		return
	}
	f.mu.Lock()
	f.contactedAppB = true
	f.mu.Unlock()
}

func (f *multiAppFakeServer) everContactedAppB() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contactedAppB
}

// TestDerive_Containment_NeverContactsOtherAppsInstallations is AC7's
// regression test: a Reconciler built from operator A's own App key must
// never see or contact operator B's App's installations, even though both
// Apps' endpoints live behind the same GitHub host in this test. This is
// what makes containment structural post-#1253 rather than an
// allowlist-derived property (ADR-1233's superseded model): FetchAppInstallations
// is JWT-scoped to the calling App's own identity by GitHub's own API
// contract — there is no owner list to check against, and so no owner list
// to get wrong.
func TestDerive_Containment_NeverContactsOtherAppsInstallations(t *testing.T) {
	oldFlow := runManifestFlow
	runManifestFlow = failingRunManifestFlow(t)
	defer func() { runManifestFlow = oldFlow }()

	srv, fake := newMultiAppFakeServer(t)
	defer srv.Close()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)

	// Reconcile as operator A's own App (ID 42) — deliberately never given
	// any credential or identifier for App B (ID 99).
	r, err := Reconcile(context.Background(), Options{
		AppID: 42, AppPrivateKeyPath: keyPath, AppStatePath: filepath.Join(dir, "app-state.json"),
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := r.ClientForRepo(context.Background(), "operator-a-org", "repo"); err != nil {
		t.Errorf("expected a client for operator-a-org (App A's own installation): %v", err)
	}
	if _, err := r.ClientForRepo(context.Background(), "operator-b-org", "repo"); err == nil {
		t.Error("expected no client for operator-b-org — it belongs to a different App entirely")
	}
	if fake.everContactedAppB() {
		t.Error("App B's installations/tokens were contacted from a Reconciler built with App A's own key — containment violated")
	}
	derived := r.LastDerived()
	for _, dr := range derived.Repos {
		if dr.Owner == "operator-b-org" {
			t.Errorf("derived set includes operator-b-org's repo %q — containment violated", dr.Repo)
		}
	}
}
