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
)

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
