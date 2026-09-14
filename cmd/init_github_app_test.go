package cmd

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
)

// writeCmdTestAppKey generates a small (test-only) RSA key and writes it as
// a PEM file under dir — mirroring internal/githubauth/tokenauth_test.go's
// writeTestPrivateKey and engine/github_app_auth_test.go's
// writeEngineTestAppKey (duplicated rather than exported cross-package for
// one helper).
func writeCmdTestAppKey(t *testing.T, dir string) string {
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

// fakeGitHubAppSetupServer serves just enough of the GitHub App + GraphQL
// surface for runGitHubAppSetup to run end-to-end against an httptest
// server, covering both the pinned-installation path (adopt with an
// explicit --github-app-installation-id) and the non-pinned discovery path
// (adopt/create without one, which additionally exercises Derive's
// installation-enumeration and per-installation repo-listing calls):
// /app (identity), /app/installations (list, JWT-authenticated discovery),
// /app/installations/{id} (single-installation granted-permissions read),
// /app/installations/{id}/access_tokens (token mint),
// /installation/repositories (installation-token-authenticated repo list,
// called unconditionally by Derive for every installation regardless of
// repository_selection — see gh.FetchInstallationRepositories's doc
// comment), and /graphql (ResolveOwner's repositoryOwner query, answered
// from ownerType).
type fakeGitHubAppSetupServer struct {
	installations []gh.AppInstallation
	ownerType     map[string]string // login (lower-cased) -> "organization"/"user"
}

func newFakeGitHubAppSetupServer(t *testing.T, installations []gh.AppInstallation, ownerType map[string]string) *httptest.Server {
	t.Helper()
	f := &fakeGitHubAppSetupServer{installations: installations, ownerType: ownerType}
	mux := http.NewServeMux()

	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"slug": "fabrik", "id": 1})
	})

	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		raw := make([]map[string]interface{}, len(f.installations))
		for i, inst := range f.installations {
			raw[i] = map[string]interface{}{
				"id": inst.ID, "account": map[string]string{"login": inst.Account},
				"repository_selection": "all",
				"permissions":          inst.Permissions,
			}
		}
		json.NewEncoder(w).Encode(raw)
	})

	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "ghs_test_token",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
			return
		}
		var instID int64
		fmt.Sscanf(r.URL.Path, "/app/installations/%d", &instID)
		for _, inst := range f.installations {
			if inst.ID == instID {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"id": inst.ID, "account": map[string]string{"login": inst.Account},
					"repository_selection": "all",
					"permissions":          inst.Permissions,
				})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"installation not found"}`))
	})

	mux.HandleFunc("/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"repositories": []map[string]string{}})
	})

	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables struct {
				Login string `json:"login"`
			} `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		typ := f.ownerType[strings.ToLower(body.Variables.Login)]
		var typename string
		switch typ {
		case "organization":
			typename = "Organization"
		case "user":
			typename = "User"
		default:
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"data":{"repositoryOwner":{"__typename":%q,"id":"node-id-123"}}}`, typename)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fullPermissions returns a granted-permissions map satisfying
// engine.RequiredGitHubAppPermissions(false) in full — the "everything
// granted" baseline the shortfall test then narrows.
func fullPermissions() map[string]string {
	return map[string]string{
		"metadata":              "read",
		"organization_projects": "write",
		"issues":                "write",
		"pull_requests":         "write",
		"checks":                "read",
		"statuses":              "read",
	}
}

func TestRunGitHubAppSetup_AdoptPinnedInstallation_Success(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir) // runGitHubAppSetup resolves AppStatePath relative to cwd — never write into the source tree
	keyPath := writeCmdTestAppKey(t, dir)
	srv := newFakeGitHubAppSetupServer(t,
		[]gh.AppInstallation{{ID: 555, Account: "handarbeit", Permissions: fullPermissions()}},
		map[string]string{"handarbeit": "organization"},
	)

	res, err := runGitHubAppSetup(context.Background(), githubAppSetupOptions{
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, InstallationID: 555, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("runGitHubAppSetup: %v", err)
	}
	if res.AppID != 42 {
		t.Errorf("AppID = %d, want 42 (the pinned adopt AppID)", res.AppID)
	}
	if res.InstallationID != 555 {
		t.Errorf("InstallationID = %d, want 555", res.InstallationID)
	}
	if res.PrivateKeyPath != keyPath {
		t.Errorf("PrivateKeyPath = %q, want %q", res.PrivateKeyPath, keyPath)
	}
	if res.Client == nil {
		t.Error("expected a non-nil *gh.Client")
	}
}

func TestRunGitHubAppSetup_Discovery_FindsInstallation(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir) // runGitHubAppSetup resolves AppStatePath relative to cwd — never write into the source tree
	keyPath := writeCmdTestAppKey(t, dir)
	srv := newFakeGitHubAppSetupServer(t,
		[]gh.AppInstallation{
			{ID: 111, Account: "someone-else", Permissions: fullPermissions()},
			{ID: 222, Account: "handarbeit", Permissions: fullPermissions()},
		},
		map[string]string{"handarbeit": "organization"},
	)

	res, err := runGitHubAppSetup(context.Background(), githubAppSetupOptions{
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("runGitHubAppSetup: %v", err)
	}
	if res.InstallationID != 222 {
		t.Errorf("InstallationID = %d, want 222 (discovered by matching --owner against the installation list)", res.InstallationID)
	}
}

func TestRunGitHubAppSetup_Discovery_NoInstallationFound(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir) // runGitHubAppSetup resolves AppStatePath relative to cwd — never write into the source tree
	keyPath := writeCmdTestAppKey(t, dir)
	srv := newFakeGitHubAppSetupServer(t,
		[]gh.AppInstallation{{ID: 111, Account: "someone-else", Permissions: fullPermissions()}},
		map[string]string{"handarbeit": "organization"},
	)

	_, err := runGitHubAppSetup(context.Background(), githubAppSetupOptions{
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, BaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected an error when no installation matches --owner")
	}
	if !strings.Contains(err.Error(), "handarbeit") || !strings.Contains(err.Error(), "installations/new") {
		t.Errorf("error %q should name the owner and the guided-install URL", err.Error())
	}
}

func TestRunGitHubAppSetup_UserOwnedBoard_Refused(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir) // runGitHubAppSetup resolves AppStatePath relative to cwd — never write into the source tree
	keyPath := writeCmdTestAppKey(t, dir)
	srv := newFakeGitHubAppSetupServer(t,
		[]gh.AppInstallation{{ID: 555, Account: "someuser", Permissions: fullPermissions()}},
		map[string]string{"someuser": "user"},
	)

	_, err := runGitHubAppSetup(context.Background(), githubAppSetupOptions{
		Owner: "someuser", AppID: 42, PrivateKeyPath: keyPath, InstallationID: 555, BaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected a refusal for a user-owned target")
	}
	if !strings.Contains(err.Error(), "organization-owned") {
		t.Errorf("error %q should explain the org-only requirement (R4)", err.Error())
	}
}

func TestRunGitHubAppSetup_PermissionShortfall_ReportsApprovalURL(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir) // runGitHubAppSetup resolves AppStatePath relative to cwd — never write into the source tree
	keyPath := writeCmdTestAppKey(t, dir)
	partial := fullPermissions()
	delete(partial, "issues")
	partial["organization_projects"] = "read" // required "write"
	srv := newFakeGitHubAppSetupServer(t,
		[]gh.AppInstallation{{ID: 555, Account: "handarbeit", Permissions: partial}},
		map[string]string{"handarbeit": "organization"},
	)

	_, err := runGitHubAppSetup(context.Background(), githubAppSetupOptions{
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, InstallationID: 555, BaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected a permission-shortfall error")
	}
	msg := err.Error()
	for _, want := range []string{"issues", "organization_projects", "https://github.com/settings/installations/555"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// TestRunGitHubAppSetup_SetsRequiredPermissionsOnReconcileOptions is a bot
// review finding (PR #1731): baseOpts never set githubauth.Options.
// RequiredPermissions, so on the create-new-App path (opts.AppID == 0),
// buildManifest would fall back to PrueferRequiredPermissions() instead of
// engine.RequiredGitHubAppPermissions — a freshly created App would always
// be missing organization_projects:write and fail its own R3 check
// immediately after creation. That code path (a real manifest/browser
// exchange) isn't reachable from a cmd-level test (see the ADR's disclosed
// limitation) — but Options.RequiredPermissions is the same field
// Reconcile's own verifyPinnedGrants (a soft, log-only check that only
// fires when the field is non-nil and the call is pinned) consults, so its
// effect IS observable here: this test captures stdout during a pinned
// adopt-path call with a known permission shortfall and asserts the
// resulting "[github-app] !" warning line appears — proof the field
// reached Reconcile, not just the explicit VerifyGrants call afterward
// (which uses a separately-computed value and would still report the
// shortfall correctly even if baseOpts.RequiredPermissions regressed to
// nil, making the returned-error assertions in the sibling
// PermissionShortfall test above unable to catch this specific regression
// by themselves).
func TestRunGitHubAppSetup_SetsRequiredPermissionsOnReconcileOptions(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	keyPath := writeCmdTestAppKey(t, dir)
	partial := fullPermissions()
	delete(partial, "issues")
	srv := newFakeGitHubAppSetupServer(t,
		[]gh.AppInstallation{{ID: 555, Account: "handarbeit", Permissions: partial}},
		map[string]string{"handarbeit": "organization"},
	)

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	_, setupErr := runGitHubAppSetup(context.Background(), githubAppSetupOptions{
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, InstallationID: 555, BaseURL: srv.URL,
	})

	w.Close()
	os.Stdout = origStdout
	var buf strings.Builder
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	captured := buf.String()

	if setupErr == nil {
		t.Fatal("expected a permission-shortfall error")
	}
	if !strings.Contains(captured, `permission "issues" is granted "none"`) {
		t.Errorf("expected Reconcile's own soft verifyPinnedGrants check to log the missing \"issues\" permission — "+
			"its absence means baseOpts.RequiredPermissions was nil, the exact regression this test guards against. "+
			"captured output:\n%s", captured)
	}
}

func TestRunGitHubAppSetup_Webhooks_ExpandsRequiredPermissions(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir) // runGitHubAppSetup resolves AppStatePath relative to cwd — never write into the source tree
	keyPath := writeCmdTestAppKey(t, dir)
	granted := fullPermissions() // no "webhooks" entry
	srv := newFakeGitHubAppSetupServer(t,
		[]gh.AppInstallation{{ID: 555, Account: "handarbeit", Permissions: granted}},
		map[string]string{"handarbeit": "organization"},
	)

	_, err := runGitHubAppSetup(context.Background(), githubAppSetupOptions{
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, InstallationID: 555, Webhooks: true, BaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected a shortfall for the missing webhooks permission when --webhooks is set")
	}
	if !strings.Contains(err.Error(), "webhooks") {
		t.Errorf("error %q should name the missing webhooks permission", err.Error())
	}
}

// ── flag validation (runInit) — no network involved ─────────────────────────

func TestRunInit_GitHubApp_RequiresOwner(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	err := runInit([]string{"--github-app"})
	if err == nil {
		t.Fatal("expected an error when --github-app is given without --owner")
	}
	if !strings.Contains(err.Error(), "--owner") {
		t.Errorf("error %q should mention --owner", err.Error())
	}
}

func TestRunInit_GitHubApp_AdoptPairMismatch(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	err := runInit([]string{"--github-app", "--owner", "acme", "--github-app-id", "42"})
	if err == nil {
		t.Fatal("expected an error when --github-app-id is given without --github-app-private-key-path")
	}
	if !strings.Contains(err.Error(), "--github-app-private-key-path") {
		t.Errorf("error %q should mention the missing pair member", err.Error())
	}

	dir2 := t.TempDir()
	chdirTest(t, dir2)
	err = runInit([]string{"--github-app", "--owner", "acme", "--github-app-private-key-path", filepath.Join(dir2, "key.pem")})
	if err == nil {
		t.Fatal("expected an error when --github-app-private-key-path is given without --github-app-id")
	}
}

func TestRunInit_GitHubApp_RejectsProjectURL(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	// Review finding (PR #1731): --github-app sets owner from --owner, same
	// as --create-board does — combining it with a project-URL positional
	// argument (which names its own, possibly different, owner) risked
	// silently writing a .fabrik/config.yaml whose owner and project belong
	// to different accounts. No network call should happen — this must be
	// rejected by flag validation before runGitHubAppSetup ever runs.
	err := runInit([]string{"--github-app", "--owner", "myorg", "https://github.com/orgs/otherorg/projects/5"})
	if err == nil {
		t.Fatal("expected an error when --github-app is combined with a <project-url> argument")
	}
	if !strings.Contains(err.Error(), "--github-app") || !strings.Contains(err.Error(), "project-url") {
		t.Errorf("error %q should name both --github-app and the project-url argument", err.Error())
	}
}

func TestRunInit_GitHubAppFlags_RequireGitHubApp(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	err := runInit([]string{"--github-app-installation-id", "99"})
	if err == nil {
		t.Fatal("expected an error when --github-app-installation-id is given without --github-app")
	}
	if !strings.Contains(err.Error(), "--github-app") {
		t.Errorf("error %q should mention --github-app", err.Error())
	}
}

// ── config-file output (buildConfigWithValues) ──────────────────────────────

func TestBuildConfigWithValues_GitHubAppFields(t *testing.T) {
	result := buildConfigWithValues(configValues{
		Owner: "acme", GitHubAppID: 42, GitHubAppPrivateKeyPath: ".fabrik/github-app-key.pem", GitHubAppInstallationID: 555,
	})
	for _, want := range []string{
		"github_app_id: 42",
		"github_app_private_key_path: .fabrik/github-app-key.pem",
		"github_app_installation_id: 555",
	} {
		if !strings.Contains(result, want) {
			t.Errorf("result missing %q:\n%s", want, result)
		}
	}
}

func TestBuildConfigWithValues_GitHubAppFields_UnsetStayCommented(t *testing.T) {
	result := buildConfigWithValues(configValues{Owner: "acme"})
	for _, want := range []string{"# github_app_id:", "# github_app_private_key_path:", "# github_app_installation_id:"} {
		if !strings.Contains(result, want) {
			t.Errorf("expected %q to remain commented when unset:\n%s", want, result)
		}
	}
}

// ── review fixes (PR #1731) ──────────────────────────────────────────────

// TestWriteGitExclude_CoversGitHubAppSecrets is a bot review finding: a
// fresh `--github-app` manifest run writes the App's private key and state
// file under .fabrik/, but writeGitExclude's entry list didn't cover either
// — a routine `git add .` in the operator's own repo could commit the
// private key. Confirms both paths land in .git/info/exclude.
func TestWriteGitExclude_CoversGitHubAppSecrets(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	if err := os.Mkdir(".git", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(".git", "info"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := writeGitExclude(); err != nil {
		t.Fatalf("writeGitExclude: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{defaultGitHubAppPrivateKeyPath, engine.GitHubAppStatePath(".")} {
		if !strings.Contains(string(content), want) {
			t.Errorf(".git/info/exclude missing %q:\n%s", want, content)
		}
	}
}

// TestRunInit_GitHubApp_RefusesExistingConfigWithoutForce is a review
// finding: without this guard, runGitHubAppSetup would run to completion —
// registering/adopting a real App, minting an installation token, writing
// the private key to disk — and then writeConfigTemplate's own
// no-op-when-file-exists gate would silently discard the resolved
// github_app_* fields instead of persisting them. No network call should
// happen — this must be rejected by flag validation before
// runGitHubAppSetup ever runs.
func TestRunInit_GitHubApp_RefusesExistingConfigWithoutForce(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	if err := os.MkdirAll(".fabrik", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".fabrik", "config.yaml"), []byte("owner: acme\n"), 0644); err != nil {
		t.Fatal(err)
	}

	err := runInit([]string{"--github-app", "--owner", "myorg"})
	if err == nil {
		t.Fatal("expected an error when --github-app runs against an existing .fabrik/config.yaml without --force")
	}
	if !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), "config.yaml") {
		t.Errorf("error %q should mention --force and .fabrik/config.yaml", err.Error())
	}
}

// TestRunInit_GitHubApp_RefusesGHESHost is a review finding: the engine
// refuses GHES host + GitHub-App-auth unconditionally at startup
// (engine.RefuseGHESWithGitHubApp), because internal/githubauth's client
// construction doesn't yet derive correct GHES endpoints. Without this
// setup-time check, --github-app would register/adopt an App against
// production github.com regardless of --ghes-host, then write a
// ghes_host + github_app_* combination the engine refuses on its very next
// startup. No network call should happen.
func TestRunInit_GitHubApp_RefusesGHESHost(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	err := runInit([]string{"--github-app", "--owner", "myorg", "--ghes-host", "ghes.example.com"})
	if err == nil {
		t.Fatal("expected an error when --github-app is combined with --ghes-host")
	}
	if !strings.Contains(err.Error(), "ghes.example.com") || !strings.Contains(err.Error(), "Enterprise Server") {
		t.Errorf("error %q should name the GHES host and explain the refusal", err.Error())
	}
}

// Note: the composed "env vars satisfy the adopt-pair check inside runInit"
// behavior is deliberately not tested end-to-end through runInit — doing so
// would require a real network call, since --github-app has no BaseURL
// test-injection point in runInit (see cmd/init_github_app.go's BaseURL doc
// comment; mirrors --create-board's identical gap). Depending on external
// network for a test is against this repo's own testing convention
// (.claude/rules/golang.md). The dedicated resolveGitHubAppInitFlagsFromEnv
// unit tests below, combined with TestRunInit_GitHubApp_AdoptPairMismatch's
// existing coverage of the mismatch check itself (both consult the exact
// same *githubAppIDFlag/*githubAppKeyPathFlag values), together prove the
// composed behavior without needing a live round trip.

// TestResolveGitHubAppInitFlagsFromEnv_FlagWins confirms flag values take
// precedence over env vars when both are given (flag > env, no config.yaml
// layer — mirrors resolveGitHubAppConfig's precedence minus its third tier,
// which doesn't exist yet at init time).
func TestResolveGitHubAppInitFlagsFromEnv_FlagWins(t *testing.T) {
	t.Setenv("FABRIK_GITHUB_APP_ID", "999")
	t.Setenv("FABRIK_GITHUB_APP_PRIVATE_KEY_PATH", "/env/path.pem")
	t.Setenv("FABRIK_GITHUB_APP_INSTALLATION_ID", "888")

	id, keyPath, instID := int64(42), "/flag/path.pem", int64(555)
	if err := resolveGitHubAppInitFlagsFromEnv(&id, &keyPath, &instID); err != nil {
		t.Fatalf("resolveGitHubAppInitFlagsFromEnv: %v", err)
	}
	if id != 42 || keyPath != "/flag/path.pem" || instID != 555 {
		t.Errorf("flag values overridden by env: id=%d keyPath=%q instID=%d", id, keyPath, instID)
	}
}

// TestResolveGitHubAppInitFlagsFromEnv_EnvFillsZeroValues confirms env vars
// are used only when the corresponding flag is left at its zero value.
func TestResolveGitHubAppInitFlagsFromEnv_EnvFillsZeroValues(t *testing.T) {
	t.Setenv("FABRIK_GITHUB_APP_ID", "999")
	t.Setenv("FABRIK_GITHUB_APP_PRIVATE_KEY_PATH", "/env/path.pem")
	t.Setenv("FABRIK_GITHUB_APP_INSTALLATION_ID", "888")

	var id, instID int64
	var keyPath string
	if err := resolveGitHubAppInitFlagsFromEnv(&id, &keyPath, &instID); err != nil {
		t.Fatalf("resolveGitHubAppInitFlagsFromEnv: %v", err)
	}
	if id != 999 || keyPath != "/env/path.pem" || instID != 888 {
		t.Errorf("env vars not applied: id=%d keyPath=%q instID=%d", id, keyPath, instID)
	}
}

// TestResolveGitHubAppInitFlagsFromEnv_InvalidIntEnv confirms a malformed
// integer env var is surfaced as an error, not silently ignored or panicking.
func TestResolveGitHubAppInitFlagsFromEnv_InvalidIntEnv(t *testing.T) {
	t.Setenv("FABRIK_GITHUB_APP_ID", "not-a-number")

	var id, instID int64
	var keyPath string
	if err := resolveGitHubAppInitFlagsFromEnv(&id, &keyPath, &instID); err == nil {
		t.Fatal("expected an error for a malformed FABRIK_GITHUB_APP_ID")
	}
}

// TestRunInit_PlainInvocation_IgnoresGitHubAppEnvVars is the regression test
// for the bot review finding on PR #1731 (second pass): an operator who
// exports FABRIK_GITHUB_APP_ID/FABRIK_GITHUB_APP_PRIVATE_KEY_PATH/
// FABRIK_GITHUB_APP_INSTALLATION_ID in their shell (exactly what the
// top-level `fabrik` command's own flag help text encourages) must still be
// able to run a plain `fabrik init` with no GitHub-App-related flags at
// all. Before the fix, resolveGitHubAppInitFlagsFromEnv ran unconditionally
// and pulled these into the flag variables regardless of --github-app,
// tripping the "requires --github-app" validation and failing outright.
func TestRunInit_PlainInvocation_IgnoresGitHubAppEnvVars(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	t.Setenv("FABRIK_GITHUB_APP_ID", "999")
	t.Setenv("FABRIK_GITHUB_APP_PRIVATE_KEY_PATH", "/env/path.pem")
	t.Setenv("FABRIK_GITHUB_APP_INSTALLATION_ID", "888")

	if err := runInit([]string{}); err != nil {
		t.Fatalf("runInit with no GitHub-App flags should ignore FABRIK_GITHUB_APP_* env vars, got error: %v", err)
	}
}

// TestRunInit_PlainInvocation_IgnoresMalformedGitHubAppEnvVar is the same
// regression, for the harder case: a malformed FABRIK_GITHUB_APP_ID must
// not fail a plain `fabrik init` either, since resolveGitHubAppInitFlagsFromEnv
// (and its parse error) should never run without --github-app.
func TestRunInit_PlainInvocation_IgnoresMalformedGitHubAppEnvVar(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	t.Setenv("FABRIK_GITHUB_APP_ID", "not-a-number")

	if err := runInit([]string{}); err != nil {
		t.Fatalf("runInit with no GitHub-App flags should ignore a malformed FABRIK_GITHUB_APP_ID, got error: %v", err)
	}
}
