package cmd

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/githubauth"
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

type fakeSetupServerOption func(*fakeGitHubAppSetupServer)

func newFakeGitHubAppSetupServer(t *testing.T, installations []gh.AppInstallation, ownerType map[string]string, opts ...fakeSetupServerOption) *httptest.Server {
	t.Helper()
	f := &fakeGitHubAppSetupServer{installations: installations, ownerType: ownerType}
	for _, opt := range opts {
		opt(f)
	}
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
// engine.RequiredGitHubAppPermissionsForGit(false, true) in full — the
// default HTTPS-git requirement (#1846) — the "everything granted" baseline
// the shortfall tests then narrow.
func fullPermissions() map[string]string {
	return map[string]string{
		"metadata":              "read",
		"organization_projects": "write",
		"issues":                "write",
		"pull_requests":         "write",
		"checks":                "read",
		"statuses":              "read",
		"contents":              "write",
		"actions":               "write",
	}
}

// isolateCmdGitConfig points git's global config at a test-local file
// (content verbatim) and skips the system config, so engine.AppGitUsesHTTPS
// never sees the host's own url.*.insteadOf rewrite.
func isolateCmdGitConfig(t *testing.T, content string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", p)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

const cmdSSHRewriteGitConfig = "[url \"git@github.com:\"]\n\tinsteadOf = https://github.com/\n"

func TestGitHubAppSetupPermissions(t *testing.T) {
	tests := []struct {
		name         string
		opts         githubAppSetupOptions
		rewrite      string
		wantVerify   string // contents level verified
		wantManifest string // contents level a new App requests
	}{
		{name: "create, https", opts: githubAppSetupOptions{}, wantVerify: "write", wantManifest: "write"},
		{name: "create, git_ssh still requests write", opts: githubAppSetupOptions{GitSSH: true}, wantVerify: "read", wantManifest: "write"},
		{name: "create, ssh rewrite still requests write", opts: githubAppSetupOptions{}, rewrite: cmdSSHRewriteGitConfig, wantVerify: "read", wantManifest: "write"},
		{name: "adopt, https", opts: githubAppSetupOptions{AppID: 42}, wantVerify: "write", wantManifest: "write"},
		{name: "adopt, git_ssh", opts: githubAppSetupOptions{AppID: 42, GitSSH: true}, wantVerify: "read", wantManifest: "read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateCmdGitConfig(t, tt.rewrite)
			verify, manifest, _ := githubAppSetupPermissions(tt.opts)
			if got := verify["contents"]; got != tt.wantVerify {
				t.Errorf("verify contents = %q, want %q", got, tt.wantVerify)
			}
			if got := manifest["contents"]; got != tt.wantManifest {
				t.Errorf("manifest contents = %q, want %q", got, tt.wantManifest)
			}
			// Everything else matches the engine's base set in both.
			for k, v := range engine.RequiredGitHubAppPermissions(false) {
				if k == "contents" {
					continue
				}
				if verify[k] != v || manifest[k] != v {
					t.Errorf("%s: verify=%q manifest=%q, want %q", k, verify[k], manifest[k], v)
				}
			}
		})
	}
}

// TestRunGitHubAppSetup_HTTPSGit_ContentsReadRefusedWithFixHint: adopting an
// installation that grants only contents:read under the default HTTPS git
// fails init (the engine would refuse it at startup) and names both fixes.
func TestRunGitHubAppSetup_HTTPSGit_ContentsReadRefusedWithFixHint(t *testing.T) {
	isolateCmdGitConfig(t, "")
	dir := t.TempDir()
	chdirTest(t, dir)
	keyPath := writeCmdTestAppKey(t, dir)
	readOnly := fullPermissions()
	readOnly["contents"] = "read"
	srv := newFakeGitHubAppSetupServer(t,
		[]gh.AppInstallation{{ID: 555, Account: "handarbeit", Permissions: readOnly}},
		map[string]string{"handarbeit": "organization"},
	)

	_, err := runGitHubAppSetup(context.Background(), githubAppSetupOptions{
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, InstallationID: 555, BaseURL: srv.URL, NoBrowser: true,
	})
	if err == nil {
		t.Fatal("expected a contents shortfall under HTTPS git")
	}
	for _, want := range []string{"contents", "settings/apps/", "/permissions", "git_ssh"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}

	// The same installation is fine once git runs over SSH.
	if _, err := runGitHubAppSetup(context.Background(), githubAppSetupOptions{
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, InstallationID: 555, GitSSH: true, BaseURL: srv.URL, NoBrowser: true,
	}); err != nil {
		t.Fatalf("git_ssh with contents:read: %v", err)
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
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, InstallationID: 555, BaseURL: srv.URL, NoBrowser: true,
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
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, BaseURL: srv.URL, NoBrowser: true,
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
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, BaseURL: srv.URL, NoBrowser: true,
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
		Owner: "someuser", AppID: 42, PrivateKeyPath: keyPath, InstallationID: 555, BaseURL: srv.URL, NoBrowser: true,
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
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, InstallationID: 555, BaseURL: srv.URL, NoBrowser: true,
	})
	if err == nil {
		t.Fatal("expected a permission-shortfall error")
	}
	msg := err.Error()
	for _, want := range []string{"issues", "organization_projects", "https://github.com/organizations/handarbeit/settings/installations/555", "https://github.com/organizations/handarbeit/settings/apps/fabrik/permissions"} {
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
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, InstallationID: 555, BaseURL: srv.URL, NoBrowser: true,
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
	granted := fullPermissions() // no "repository_hooks" entry
	srv := newFakeGitHubAppSetupServer(t,
		[]gh.AppInstallation{{ID: 555, Account: "handarbeit", Permissions: granted}},
		map[string]string{"handarbeit": "organization"},
	)

	_, err := runGitHubAppSetup(context.Background(), githubAppSetupOptions{
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, InstallationID: 555, Webhooks: true, BaseURL: srv.URL, NoBrowser: true,
	})
	if err == nil {
		t.Fatal("expected a shortfall for the missing repository_hooks permission when --webhooks is set")
	}
	if !strings.Contains(err.Error(), "repository_hooks") {
		t.Errorf("error %q should name the missing repository_hooks permission", err.Error())
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

// TestRunInit_GitHubApp_RefusesWebhooks is the regression test for a bot
// review finding on this PR (#1752): the engine refuses --webhooks +
// GitHub-App-auth unconditionally at startup (engine.RefuseWebhooksWithGitHubApp),
// because gh webhook forward is feature-gated to user tokens and refuses an
// installation token outright. Without this setup-time check, `--github-app
// --webhooks` would register/adopt a real App, verify repository_hooks
// permission, and persist github_app_* + FABRIK_WEBHOOKS=true, only for the
// engine to refuse to start on every subsequent run. No network call should
// happen — mirrors TestRunInit_GitHubApp_RefusesGHESHost exactly.
func TestRunInit_GitHubApp_RefusesWebhooks(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	err := runInit([]string{"--github-app", "--owner", "myorg", "--webhooks"})
	if err == nil {
		t.Fatal("expected an error when --github-app is combined with --webhooks")
	}
	if !strings.Contains(err.Error(), "--webhooks") || !strings.Contains(err.Error(), "gh webhook forward") {
		t.Errorf("error %q should name --webhooks and explain the refusal", err.Error())
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

// TestRunGitHubAppSetup_Discovery_NoInstallationFound_NeverOpensBrowser is the
// regression guard for #1781. The no-matching-installation path is the one
// branch of runGitHubAppSetup that reaches guideMissingInstallations, and before
// this fix it shelled out through exec.Command("open", …) to a real browser —
// on every `go test ./cmd/...`, which every Fabrik stage worker runs. The
// operator saw a Chrome window pointed at
// https://github.com/apps/fabrik/installations/new (a 404, since "fabrik" is the
// App's creation-time name rather than a registered slug).
//
// Asserting on NoBrowser's effect requires a positive signal, not the absence of
// a window: a test that merely sets NoBrowser: true and passes tells you nothing
// about whether suppression actually worked, because a test that forgot it
// passes identically. So this stubs OpenBrowser and fails if it is ever reached.
// The stub is what the assertion rests on — with NoBrowser: true the gate should
// short-circuit before the opener is consulted at all.
func TestRunGitHubAppSetup_Discovery_NoInstallationFound_NeverOpensBrowser(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	keyPath := writeCmdTestAppKey(t, dir)
	srv := newFakeGitHubAppSetupServer(t,
		[]gh.AppInstallation{{ID: 111, Account: "someone-else", Permissions: fullPermissions()}},
		map[string]string{"handarbeit": "organization"},
	)

	var opened []string
	_, err := runGitHubAppSetup(context.Background(), githubAppSetupOptions{
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, BaseURL: srv.URL,
		NoBrowser: true,
		OpenBrowser: func(url string) error {
			opened = append(opened, url)
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected an error when no installation matches --owner")
	}
	if len(opened) != 0 {
		t.Errorf("browser opener called with %v, want no calls — NoBrowser: true must suppress the guided-install open", opened)
	}
}

func TestAppRepoAccessError(t *testing.T) {
	covered := []string{"shadoworg/fantasy"}
	if err := appRepoAccessError("shadoworg", "fantasy", covered, false, 165275277); err != nil {
		t.Errorf("covered repo: %v", err)
	}
	if err := appRepoAccessError("ShadowOrg", "Fantasy", covered, false, 165275277); err != nil {
		t.Errorf("case differs only: %v", err)
	}
	if err := appRepoAccessError("shadoworg", "dummy-repo", covered, true, 165275277); err != nil {
		t.Errorf("truncated list cannot prove absence, want nil: %v", err)
	}
	err := appRepoAccessError("shadoworg", "dummy-repo", covered, false, 165275277)
	if err == nil {
		t.Fatal("repo outside the grant: want an error")
	}
	for _, want := range []string{"cannot access shadoworg/dummy-repo", "covers: shadoworg/fantasy",
		"https://github.com/organizations/shadoworg/settings/installations/165275277", "omit --repo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	if err := appRepoAccessError("shadoworg", "x", nil, false, 1); err == nil || !strings.Contains(err.Error(), "covers: no repositories") {
		t.Errorf("empty grant: got %v", err)
	}
}

// installationsAfterServer serves /app/installations empty for the first
// hidden calls, then lists installs; calls counts every list request.
func installationsAfterServer(t *testing.T, hidden int32, installs []gh.AppInstallation) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw := []map[string]interface{}{}
		if calls.Add(1) > hidden {
			for _, inst := range installs {
				raw = append(raw, map[string]interface{}{"id": inst.ID, "account": map[string]string{"login": inst.Account}})
			}
		}
		json.NewEncoder(w).Encode(raw)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestWaitForOwnerInstallation(t *testing.T) {
	keyPath := writeCmdTestAppKey(t, t.TempDir())
	quiet := func(string, ...any) {}
	opts := func(url string, wait time.Duration) githubAppSetupOptions {
		return githubAppSetupOptions{Owner: "shadoworg", BaseURL: url, InstallWait: wait, InstallPollInterval: 5 * time.Millisecond}
	}

	t.Run("returns the installation once it appears", func(t *testing.T) {
		srv, calls := installationsAfterServer(t, 3, []gh.AppInstallation{{ID: 11, Account: "someone-else"}, {ID: 42, Account: "ShadowOrg"}})
		id, err := waitForOwnerInstallation(context.Background(), opts(srv.URL, 5*time.Second), 1, keyPath, "shadoworg-fabrik", quiet)
		if err != nil || id != 42 {
			t.Fatalf("got (%d, %v), want (42, nil)", id, err)
		}
		if calls.Load() < 4 {
			t.Errorf("returned after %d polls, before the installation appeared", calls.Load())
		}
	})

	t.Run("gives up at the deadline with no error", func(t *testing.T) {
		srv, _ := installationsAfterServer(t, 1<<30, nil)
		start := time.Now()
		id, err := waitForOwnerInstallation(context.Background(), opts(srv.URL, 60*time.Millisecond), 1, keyPath, "s", quiet)
		if err != nil || id != 0 {
			t.Fatalf("got (%d, %v), want (0, nil)", id, err)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("wait overran its deadline: %s", time.Since(start))
		}
	})

	t.Run("an installation on another account never counts", func(t *testing.T) {
		srv, _ := installationsAfterServer(t, 0, []gh.AppInstallation{{ID: 11, Account: "verveguy"}})
		id, err := waitForOwnerInstallation(context.Background(), opts(srv.URL, 40*time.Millisecond), 1, keyPath, "s", quiet)
		if err != nil || id != 0 {
			t.Fatalf("got (%d, %v), want (0, nil)", id, err)
		}
	})

	t.Run("stops on cancellation", func(t *testing.T) {
		srv, _ := installationsAfterServer(t, 1<<30, nil)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(30*time.Millisecond, cancel)
		_, err := waitForOwnerInstallation(ctx, opts(srv.URL, 5*time.Second), 1, keyPath, "s", quiet)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

// TestRunGitHubAppSetup_WaitsForInstallBeforeFailing: with InstallWait set,
// a run that finds no installation for --owner waits before giving up
// (with the same install-then-re-run error) rather than failing at once.
func TestRunGitHubAppSetup_WaitsForInstallBeforeFailing(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	keyPath := writeCmdTestAppKey(t, dir)
	srv := newFakeGitHubAppSetupServer(t,
		[]gh.AppInstallation{{ID: 111, Account: "someone-else", Permissions: fullPermissions()}},
		map[string]string{"handarbeit": "organization"},
	)
	const wait, interval = 300 * time.Millisecond, 20 * time.Millisecond
	start := time.Now()
	_, err := runGitHubAppSetup(context.Background(), githubAppSetupOptions{
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, BaseURL: srv.URL, NoBrowser: true,
		InstallWait: wait, InstallPollInterval: interval,
	})
	if err == nil || !strings.Contains(err.Error(), "installations/new") {
		t.Fatalf("err = %v, want the install-then-re-run error", err)
	}
	// The loop stops polling once the next poll would land past the
	// deadline, so it may return up to one interval early. Without the
	// wait wired in, it returns in well under a millisecond.
	if elapsed := time.Since(start); elapsed < wait-interval {
		t.Errorf("failed after %s, before the %s install wait (less one %s poll interval)", elapsed, wait, interval)
	}
}

func TestInteractiveInstallWait(t *testing.T) {
	if got := interactiveInstallWait(true); got != defaultInstallWait {
		t.Errorf("interactive: %s, want %s", got, defaultInstallWait)
	}
	if got := interactiveInstallWait(false); got != 0 {
		t.Errorf("non-interactive: %s, want 0 (fail fast)", got)
	}
}

// TestGitHubAppSetupPermissions_ActionsIsRequired (#2105): a new App's manifest
// asks for `actions: write` and the verify set requires it. Asserted on
// githubAppSetupPermissions' manifest map because that is exactly what
// githubauth.buildManifest (unexported) receives as default_permissions.
func TestGitHubAppSetupPermissions_ActionsIsRequired(t *testing.T) {
	isolateCmdGitConfig(t, "")
	for _, adopt := range []bool{false, true} {
		opts := githubAppSetupOptions{}
		if adopt {
			opts.AppID = 42
		}
		verify, manifest, _ := githubAppSetupPermissions(opts)
		if verify["actions"] != "write" {
			t.Errorf("adopt=%v: verify actions = %q, want write", adopt, verify["actions"])
		}
		if manifest["actions"] != "write" {
			t.Errorf("adopt=%v: manifest actions = %q, want write", adopt, manifest["actions"])
		}
	}
}

// captureSetupStdout runs runGitHubAppSetup with stdout captured.
func captureSetupStdout(t *testing.T, opts githubAppSetupOptions) (string, error) {
	t.Helper()
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	_, setupErr := runGitHubAppSetup(context.Background(), opts)
	w.Close()
	os.Stdout = origStdout
	var buf strings.Builder
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String(), setupErr
}

func adoptOptionsFor(srvURL, keyPath string) githubAppSetupOptions {
	return githubAppSetupOptions{
		Owner: "handarbeit", AppID: 42, PrivateKeyPath: keyPath, InstallationID: 555, BaseURL: srvURL, NoBrowser: true,
	}
}

// #2105: adopting an installation without actions:write is refused with the
// same remedy text the engine's startup refusal carries (both URLs).
func TestRunGitHubAppSetup_MissingActionsRefused(t *testing.T) {
	isolateCmdGitConfig(t, "")
	for _, level := range []string{"", "read"} {
		dir := t.TempDir()
		chdirTest(t, dir)
		keyPath := writeCmdTestAppKey(t, dir)
		perms := fullPermissions()
		if level == "" {
			delete(perms, "actions")
		} else {
			perms["actions"] = level
		}
		srv := newFakeGitHubAppSetupServer(t,
			[]gh.AppInstallation{{ID: 555, Account: "handarbeit", Permissions: perms}},
			map[string]string{"handarbeit": "organization"},
		)

		_, err := captureSetupStdout(t, adoptOptionsFor(srv.URL, keyPath))
		if err == nil {
			t.Fatalf("actions=%q: expected a refusal", level)
		}
		want := engine.PermissionShortfallRemedy(
			[]githubauth.RequiredPermissionShortfall{{Permission: "actions", Required: "write"}}, "handarbeit", "fabrik", 555)
		if !strings.Contains(err.Error(), want) {
			t.Errorf("actions=%q: error does not carry the shared remedy text:\n%v\nwant substring:\n%s", level, err, want)
		}
		if !strings.Contains(err.Error(), "actions") {
			t.Errorf("actions=%q: error does not name actions: %v", level, err)
		}
	}
}

// TestRunGitHubAppSetup_ActionsGranted_Succeeds: an installation granting
// actions:write is adopted, prints the authenticated line and mentions no
// permission shortfall (#2105).
func TestRunGitHubAppSetup_ActionsGranted_Succeeds(t *testing.T) {
	isolateCmdGitConfig(t, "")
	dir := t.TempDir()
	chdirTest(t, dir)
	keyPath := writeCmdTestAppKey(t, dir)
	srv := newFakeGitHubAppSetupServer(t,
		[]gh.AppInstallation{{ID: 555, Account: "handarbeit", Permissions: fullPermissions()}},
		map[string]string{"handarbeit": "organization"},
	)

	out, err := captureSetupStdout(t, adoptOptionsFor(srv.URL, keyPath))
	if err != nil {
		t.Fatalf("runGitHubAppSetup: %v", err)
	}
	if !strings.Contains(out, "authenticated as") {
		t.Errorf("adoption did not report success:\n%s", out)
	}
	if strings.Contains(out, "missing required permissions") {
		t.Errorf("unexpected permission shortfall output:\n%s", out)
	}
}
