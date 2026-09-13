package cmd

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
	"testing"
	"time"

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
