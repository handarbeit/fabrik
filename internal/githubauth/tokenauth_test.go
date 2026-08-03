package githubauth

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

// fakeAppServer serves the GitHub App endpoints this package's reconciler
// uses: /app, /app/installations, /app/installations/{id}/access_tokens,
// and (for repository_selection=="selected" installations)
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
	// failRepoList, if set, is consulted before every /installation/repositories
	// response for a given installation ID; returning true fails that
	// request (simulating a transient listing failure) without affecting
	// installation-token minting.
	failRepoList func(installationID int64) bool

	mintCount atomic.Int32

	mu               sync.Mutex
	mintCountByInst  map[int64]int
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
		if f.failRepoList != nil && f.failRepoList(instID) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"simulated repo-list failure"}`))
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

func TestAuth_ExpiresAt_ZeroBeforeAnyRefresh(t *testing.T) {
	a := &Auth{}
	if !a.ExpiresAt().IsZero() {
		t.Error("expected ExpiresAt to be zero before any refresh")
	}
}

func TestMintAuth_MintsAndSetsToken(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	privateKey, err := loadPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	srv, _ := newFakeAppServer("pruefer-bot", []gh.AppInstallation{{ID: 111, Account: "handarbeit"}}, func() time.Time {
		return time.Now().Add(time.Hour)
	})
	defer srv.Close()

	a, err := mintAuth(42, 111, "pruefer-bot[bot]", privateKey, srv.URL)
	if err != nil {
		t.Fatalf("mintAuth: %v", err)
	}
	if a.client.Token() == "" {
		t.Error("expected client token to be set after mintAuth")
	}
	if a.ExpiresAt().IsZero() {
		t.Error("expected ExpiresAt to be set after mintAuth")
	}
}

func TestAuth_RunRefreshLoop_IndependentPerInstallation(t *testing.T) {
	oldMargin, oldRetry := tokenRefreshMargin, tokenRefreshRetryDelay
	tokenRefreshMargin = 50 * time.Millisecond
	tokenRefreshRetryDelay = 20 * time.Millisecond

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	privateKey, err := loadPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	// Token expires 100ms after each mint — with a 50ms margin, a healthy
	// refresh loop must mint again ~50ms after the previous mint.
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "handarbeit"},
		{ID: 222, Account: "verveguy"},
	}, func() time.Time { return time.Now().Add(100 * time.Millisecond) })
	// Installation 222 mints fine once (its initial token) but fails every
	// subsequent refresh — must not stop 111's loop.
	fake.failMint = func(instID int64) bool { return instID == 222 && fake.mintCountFor(222) >= 1 }
	defer srv.Close()

	a111, err := mintAuth(42, 111, "pruefer-bot[bot]", privateKey, srv.URL)
	if err != nil {
		t.Fatalf("mintAuth(111): %v", err)
	}
	a222, err := mintAuth(42, 222, "pruefer-bot[bot]", privateKey, srv.URL)
	if err != nil {
		t.Fatalf("mintAuth(222): %v", err)
	}

	var mu sync.Mutex
	var logLines []string
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	for _, a := range []*Auth{a111, a222} {
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
