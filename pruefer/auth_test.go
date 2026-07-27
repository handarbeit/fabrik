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

// fakeAppServer serves the three GitHub App endpoints Bootstrap/refresh use.
// mintCount is incremented on every access_tokens call so tests can assert
// refresh behavior.
type fakeAppServer struct {
	installations []gh.AppInstallation
	slug          string
	tokenExpiry   func() time.Time
	mintCount     atomic.Int32

	mu          sync.Mutex
	lastMintedAt time.Time
}

func newFakeAppServer(slug string, installations []gh.AppInstallation, tokenExpiry func() time.Time) (*httptest.Server, *fakeAppServer) {
	f := &fakeAppServer{slug: slug, installations: installations, tokenExpiry: tokenExpiry}
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"slug": f.slug, "id": 1})
	})
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		raw := make([]map[string]interface{}, len(f.installations))
		for i, inst := range f.installations {
			raw[i] = map[string]interface{}{"id": inst.ID, "account": map[string]string{"login": inst.Account}}
		}
		json.NewEncoder(w).Encode(raw)
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		f.mintCount.Add(1)
		f.mu.Lock()
		f.lastMintedAt = time.Now()
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"token":      fmt.Sprintf("ghs_token_%d", f.mintCount.Load()),
			"expires_at": f.tokenExpiry().UTC().Format(time.RFC3339),
		})
	})
	return httptest.NewServer(mux), f
}

func TestBootstrap_SingleInstallationAutoDiscovered(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{{ID: 111, Account: "handarbeit"}}, func() time.Time {
		return time.Now().Add(time.Hour)
	})
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath}
	client := gh.NewClientWithBaseURL("", srv.URL)
	auth, err := Bootstrap(cfg, client, srv.URL)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if auth.InstallationID != 111 {
		t.Errorf("InstallationID = %d, want 111", auth.InstallationID)
	}
	if auth.BotLogin != "pruefer-bot[bot]" {
		t.Errorf("BotLogin = %q, want pruefer-bot[bot]", auth.BotLogin)
	}
	if client.Token() == "" {
		t.Error("expected client token to be set after Bootstrap")
	}
	if auth.ExpiresAt().IsZero() {
		t.Error("expected ExpiresAt to be set after Bootstrap")
	}
}

func TestBootstrap_ExplicitInstallationIDSkipsDiscovery(t *testing.T) {
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

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath, AppInstallationID: 999}
	client := gh.NewClientWithBaseURL("", srv.URL)
	auth, err := Bootstrap(cfg, client, srv.URL)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if auth.InstallationID != 999 {
		t.Errorf("InstallationID = %d, want 999 (explicit config value)", auth.InstallationID)
	}
	if discoveryCalled {
		t.Error("expected /app/installations to be skipped when AppInstallationID is explicitly configured")
	}
}

func TestBootstrap_MultipleInstallationsRequiresExplicitID(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "someorg"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath}
	client := gh.NewClientWithBaseURL("", srv.URL)
	if _, err := Bootstrap(cfg, client, srv.URL); err == nil {
		t.Fatal("expected an error when multiple installations exist and none is configured")
	}
}

func TestBootstrap_NoInstallations(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	srv, _ := newFakeAppServer("pruefer-bot", nil, func() time.Time { return time.Now().Add(time.Hour) })
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath}
	client := gh.NewClientWithBaseURL("", srv.URL)
	if _, err := Bootstrap(cfg, client, srv.URL); err == nil {
		t.Fatal("expected an error when the app has no installations")
	}
}

func TestBootstrap_MissingPrivateKeyFile(t *testing.T) {
	cfg := Config{AppID: 42, AppPrivateKeyPath: "/nonexistent/key.pem"}
	client := gh.NewClient("")
	if _, err := Bootstrap(cfg, client, ""); err == nil {
		t.Fatal("expected an error when the private key file is missing")
	}
}

func TestRunRefreshLoop_RefreshesBeforeExpiry(t *testing.T) {
	oldMargin, oldRetry := tokenRefreshMargin, tokenRefreshRetryDelay
	tokenRefreshMargin = 50 * time.Millisecond
	tokenRefreshRetryDelay = 20 * time.Millisecond
	defer func() { tokenRefreshMargin, tokenRefreshRetryDelay = oldMargin, oldRetry }()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	// Token expires 100ms after each mint — with a 50ms margin, the refresh
	// loop must mint again ~50ms after the previous mint.
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{{ID: 111, Account: "handarbeit"}}, func() time.Time {
		return time.Now().Add(100 * time.Millisecond)
	})
	defer srv.Close()

	cfg := Config{AppID: 42, AppPrivateKeyPath: keyPath}
	client := gh.NewClientWithBaseURL("", srv.URL)
	auth, err := Bootstrap(cfg, client, srv.URL)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// Bootstrap itself mints once.
	if got := fake.mintCount.Load(); got != 1 {
		t.Fatalf("mintCount after Bootstrap = %d, want 1", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	var logLines []string
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		auth.RunRefreshLoop(ctx, func(format string, args ...any) {
			mu.Lock()
			logLines = append(logLines, fmt.Sprintf(format, args...))
			mu.Unlock()
		})
		close(done)
	}()
	<-done

	// The refresh loop must have minted at least once more within the
	// context window — proving refresh-before-expiry actually fires, not
	// just the initial Bootstrap mint.
	if got := fake.mintCount.Load(); got < 2 {
		t.Errorf("mintCount after refresh loop = %d, want >= 2 (refresh loop must fire before token expiry)", got)
	}
	if got := client.Token(); got == "" {
		t.Fatal("expected a token to be set")
	}
}

func TestAuth_ExpiresAt_ZeroBeforeBootstrap(t *testing.T) {
	a := &Auth{}
	if !a.ExpiresAt().IsZero() {
		t.Error("expected ExpiresAt to be zero before any refresh")
	}
}
