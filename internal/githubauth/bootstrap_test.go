package githubauth

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var manifestValueRe = regexp.MustCompile(`name="manifest" value="([^"]*)"`)

// fetchStartAndExtractManifest fetches the /start page and extracts the
// generated manifest JSON from its hidden form field, HTML-unescaping it
// first (renderManifestForm uses html/template, which auto-escapes the
// value attribute).
func fetchStartAndExtractManifest(t *testing.T, startURL string) map[string]interface{} {
	t.Helper()
	resp, err := http.Get(startURL)
	if err != nil {
		t.Fatalf("GET %s: %v", startURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading start page body: %v", err)
	}
	m := manifestValueRe.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("could not find manifest field in start page: %s", body)
	}
	var manifest map[string]interface{}
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &manifest); err != nil {
		t.Fatalf("unmarshaling manifest JSON: %v", err)
	}
	return manifest
}

type manifestFlowResult struct {
	creds Credentials
	err   error
}

func newManifestExchangeServer(t *testing.T, appID int64, slug string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/app-manifests/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": appID, "slug": slug, "pem": string(writeTestPrivateKeyPEM(t)),
			"webhook_secret": "whsec", "client_id": "cid", "client_secret": "csecret",
		})
	})
	return httptest.NewServer(mux)
}

func TestRunManifestFlow_HappyPath(t *testing.T) {
	dir := t.TempDir()
	pemPath := filepath.Join(dir, "app-private-key.pem")
	statePath := filepath.Join(dir, "app-state.json")

	srv := newManifestExchangeServer(t, 555, "fresh-app")
	defer srv.Close()

	oldBrowser := openBrowser
	defer func() { openBrowser = oldBrowser }()
	browserOpened := make(chan string, 1)
	openBrowser = func(url string) error {
		browserOpened <- url
		return nil
	}

	resultCh := make(chan manifestFlowResult, 1)
	go func() {
		creds, err := RunManifestFlow(context.Background(), ManifestFlowOptions{
			BaseURL: srv.URL, PrivateKeyPath: pemPath, AppStatePath: statePath,
		})
		resultCh <- manifestFlowResult{creds, err}
	}()

	var startURL string
	select {
	case startURL = <-browserOpened:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for browser open")
	}

	state := fetchStartAndExtractState(t, startURL)
	callbackURL := strings.TrimSuffix(startURL, "/start") + "/callback?state=" + state + "&code=abc123"
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	resp.Body.Close()

	var res manifestFlowResult
	select {
	case res = <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RunManifestFlow to return")
	}
	if res.err != nil {
		t.Fatalf("RunManifestFlow: %v", res.err)
	}
	if res.creds.AppID != 555 || res.creds.Slug != "fresh-app" {
		t.Errorf("creds = %+v, want AppID=555 Slug=fresh-app", res.creds)
	}

	if _, err := os.Stat(pemPath); err != nil {
		t.Errorf("expected private key to be persisted at %s: %v", pemPath, err)
	}
	stateOnDisk, err := loadCredentials(statePath)
	if err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}
	if stateOnDisk.AppID != 555 || stateOnDisk.WebhookSecret != "whsec" {
		t.Errorf("persisted state = %+v, want AppID=555 WebhookSecret=whsec", stateOnDisk)
	}
	pemOnDisk, err := os.ReadFile(pemPath)
	if err != nil {
		t.Fatalf("reading persisted PEM: %v", err)
	}
	if want := privateKeyFingerprint(pemOnDisk); stateOnDisk.PrivateKeyFingerprint != want {
		t.Errorf("persisted PrivateKeyFingerprint = %q, want %q (fingerprint of the persisted PEM)", stateOnDisk.PrivateKeyFingerprint, want)
	}
}

// TestRunManifestFlow_NonDefaultOptionsReachManifest is the AC1 regression
// test for #1712 at the RunManifestFlow layer (manifest_test.go covers
// buildManifest directly): a caller (e.g. the engine) supplying non-default
// AppName, AppHomepageURL and RequiredPermissions on ManifestFlowOptions
// must see the generated manifest — the one actually served to the browser
// at /start — reflect those values, not Pruefer's defaults.
func TestRunManifestFlow_NonDefaultOptionsReachManifest(t *testing.T) {
	dir := t.TempDir()
	pemPath := filepath.Join(dir, "app-private-key.pem")
	statePath := filepath.Join(dir, "app-state.json")

	srv := newManifestExchangeServer(t, 4242, "fabrik-engine-app")
	defer srv.Close()

	oldBrowser := openBrowser
	defer func() { openBrowser = oldBrowser }()
	browserOpened := make(chan string, 1)
	openBrowser = func(url string) error {
		browserOpened <- url
		return nil
	}

	wantPerms := map[string]string{
		"metadata":              "read",
		"contents":              "write",
		"organization_projects": "write",
	}

	resultCh := make(chan manifestFlowResult, 1)
	go func() {
		creds, err := RunManifestFlow(context.Background(), ManifestFlowOptions{
			BaseURL: srv.URL, PrivateKeyPath: pemPath, AppStatePath: statePath,
			AppName: "fabrik-engine", AppHomepageURL: "https://example.com/fabrik",
			RequiredPermissions: wantPerms,
		})
		resultCh <- manifestFlowResult{creds, err}
	}()

	var startURL string
	select {
	case startURL = <-browserOpened:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for browser open")
	}

	manifest := fetchStartAndExtractManifest(t, startURL)
	if manifest["name"] != "fabrik-engine" {
		t.Errorf(`manifest["name"] = %v, want "fabrik-engine"`, manifest["name"])
	}
	if manifest["url"] != "https://example.com/fabrik" {
		t.Errorf(`manifest["url"] = %v, want "https://example.com/fabrik"`, manifest["url"])
	}
	perms, ok := manifest["default_permissions"].(map[string]interface{})
	if !ok {
		t.Fatal("expected default_permissions to be present")
	}
	if len(perms) != len(wantPerms) {
		t.Fatalf("default_permissions = %+v, want %+v", perms, wantPerms)
	}
	for k, v := range wantPerms {
		if perms[k] != v {
			t.Errorf("default_permissions[%q] = %v, want %q", k, perms[k], v)
		}
	}

	state := fetchStartAndExtractState(t, startURL)
	callbackURL := strings.TrimSuffix(startURL, "/start") + "/callback?state=" + state + "&code=abc123"
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	resp.Body.Close()

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("RunManifestFlow: %v", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RunManifestFlow to return")
	}
}

func TestRunManifestFlow_NoBrowserSkipsOpen(t *testing.T) {
	dir := t.TempDir()
	pemPath := filepath.Join(dir, "app-private-key.pem")
	statePath := filepath.Join(dir, "app-state.json")

	srv := newManifestExchangeServer(t, 777, "no-browser-app")
	defer srv.Close()

	oldBrowser := openBrowser
	defer func() { openBrowser = oldBrowser }()
	openBrowser = func(url string) error {
		t.Error("openBrowser should not be called when NoBrowser is set")
		return nil
	}

	var mu sync.Mutex
	var logLines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		logLines = append(logLines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	resultCh := make(chan manifestFlowResult, 1)
	go func() {
		creds, err := RunManifestFlow(context.Background(), ManifestFlowOptions{
			BaseURL: srv.URL, NoBrowser: true, PrivateKeyPath: pemPath, AppStatePath: statePath, Logf: logf,
		})
		resultCh <- manifestFlowResult{creds, err}
	}()

	// Poll logLines for the printed URL since NoBrowser means openBrowser
	// (and its channel-based signaling in the happy-path test) never fires.
	var startURL string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, line := range logLines {
			if idx := strings.Index(line, "http://127.0.0.1"); idx >= 0 {
				startURL = strings.TrimSpace(line[idx:])
			}
		}
		mu.Unlock()
		if startURL != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if startURL == "" {
		t.Fatal("timed out waiting for the printed setup URL")
	}

	state := fetchStartAndExtractState(t, startURL)
	callbackURL := strings.TrimSuffix(startURL, "/start") + "/callback?state=" + state + "&code=abc123"
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	resp.Body.Close()

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("RunManifestFlow: %v", res.err)
		}
		if res.creds.AppID != 777 {
			t.Errorf("AppID = %d, want 777", res.creds.AppID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RunManifestFlow to return")
	}
}

// TestRunManifestFlow_PrivateKeyWriteFailureLeavesAppStatePersisted is the
// regression test for a review finding: app-state must be persisted before
// the PEM, not after. If app-state were written second and the PEM write
// (which happens first in the old ordering) succeeded while app-state
// failed, the next Reconcile would see AppID == 0 (no state file) and
// silently start the manifest flow again, minting a second, orphaned App
// and overwriting the first one's now-untracked PEM. With app-state written
// first, a subsequent PEM-write failure instead leaves AppID already
// persisted, which loadOrBootstrapCredentials treats as an explicit "repair
// needed" error — never a silent duplicate App.
func TestRunManifestFlow_PrivateKeyWriteFailureLeavesAppStatePersisted(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "app-state.json")
	// ENAMETOOLONG: an implausibly long directory component deterministically
	// fails the PEM's os.MkdirAll, simulating a private-key write failure
	// without depending on real disk-full/permission conditions (see
	// .claude/rules/golang.md).
	pemPath := filepath.Join(dir, strings.Repeat("a", 10000), "app-private-key.pem")

	srv := newManifestExchangeServer(t, 888, "orphan-guard-app")
	defer srv.Close()

	oldBrowser := openBrowser
	defer func() { openBrowser = oldBrowser }()
	browserOpened := make(chan string, 1)
	openBrowser = func(url string) error {
		browserOpened <- url
		return nil
	}

	resultCh := make(chan manifestFlowResult, 1)
	go func() {
		creds, err := RunManifestFlow(context.Background(), ManifestFlowOptions{
			BaseURL: srv.URL, PrivateKeyPath: pemPath, AppStatePath: statePath,
		})
		resultCh <- manifestFlowResult{creds, err}
	}()

	var startURL string
	select {
	case startURL = <-browserOpened:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for browser open")
	}

	state := fetchStartAndExtractState(t, startURL)
	callbackURL := strings.TrimSuffix(startURL, "/start") + "/callback?state=" + state + "&code=abc123"
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	resp.Body.Close()

	select {
	case res := <-resultCh:
		if res.err == nil {
			t.Fatal("expected RunManifestFlow to fail when the private key write fails")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RunManifestFlow to return")
	}

	stateOnDisk, err := loadCredentials(statePath)
	if err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}
	if stateOnDisk.AppID != 888 {
		t.Errorf("persisted state AppID = %d, want 888 (app-state must persist before the failing PEM write)", stateOnDisk.AppID)
	}
}

func TestRunManifestFlow_TimeoutLeavesNoFilesBehind(t *testing.T) {
	oldTimeout := manifestCallbackTimeout
	manifestCallbackTimeout = 30 * time.Millisecond
	defer func() { manifestCallbackTimeout = oldTimeout }()

	dir := t.TempDir()
	pemPath := filepath.Join(dir, "app-private-key.pem")
	statePath := filepath.Join(dir, "app-state.json")

	oldBrowser := openBrowser
	defer func() { openBrowser = oldBrowser }()
	openBrowser = func(string) error { return nil } // never actually completes the flow

	_, err := RunManifestFlow(context.Background(), ManifestFlowOptions{
		PrivateKeyPath: pemPath, AppStatePath: statePath,
	})
	if err == nil {
		t.Fatal("expected an error when the callback never arrives before timeout")
	}
	if _, statErr := os.Stat(pemPath); !os.IsNotExist(statErr) {
		t.Error("expected no private key file to be written on a timed-out flow")
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Error("expected no app-state file to be written on a timed-out flow")
	}
}
