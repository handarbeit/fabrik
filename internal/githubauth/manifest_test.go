package githubauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBuildManifest_NoActiveWebhook(t *testing.T) {
	m := buildManifest("http://127.0.0.1:12345/callback")
	hook, ok := m["hook_attributes"].(map[string]interface{})
	if !ok {
		t.Fatal("expected hook_attributes to be present")
	}
	if active, _ := hook["active"].(bool); active {
		t.Error("expected hook_attributes.active to be false — Pruefer V1 is polling-only")
	}
}

func TestBuildManifest_ScopedPermissions(t *testing.T) {
	m := buildManifest("http://127.0.0.1:12345/callback")
	perms, ok := m["default_permissions"].(map[string]string)
	if !ok {
		t.Fatal("expected default_permissions to be present")
	}
	want := map[string]string{
		"metadata":      "read",
		"pull_requests": "write",
		"contents":      "read",
		"issues":        "write",
	}
	if len(perms) != len(want) {
		t.Fatalf("default_permissions = %+v, want exactly %+v", perms, want)
	}
	for k, v := range want {
		if perms[k] != v {
			t.Errorf("default_permissions[%q] = %q, want %q", k, perms[k], v)
		}
	}
}

func TestBuildManifest_RedirectURLPropagated(t *testing.T) {
	m := buildManifest("http://127.0.0.1:9999/callback")
	if m["redirect_url"] != "http://127.0.0.1:9999/callback" {
		t.Errorf("redirect_url = %v, want the passed-in redirectURL", m["redirect_url"])
	}
}

// TestBuildManifest_HomepageURLIsStableNotEphemeralCallback is the
// regression test for the bug an external review found: the manifest's
// "url" field (the App's public homepage link on its GitHub settings page)
// must not reuse the ephemeral loopback redirectURL, which stops existing
// the moment the local manifest-flow server shuts down — that would leave
// the created App's homepage link permanently dead.
func TestBuildManifest_HomepageURLIsStableNotEphemeralCallback(t *testing.T) {
	redirectURL := "http://127.0.0.1:54321/callback"
	m := buildManifest(redirectURL)
	if m["url"] == redirectURL {
		t.Error("manifest \"url\" must not be the ephemeral loopback redirectURL — it would be a dead link once local setup finishes")
	}
	if m["url"] != defaultAppHomepageURL {
		t.Errorf("manifest \"url\" = %v, want %v", m["url"], defaultAppHomepageURL)
	}
}

func TestExchangeManifestCode_Success(t *testing.T) {
	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/app-manifests/", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": 999, "slug": "my-pruefer", "pem": "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----",
			"webhook_secret": "whsec", "client_id": "cid", "client_secret": "csecret",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mc, err := exchangeManifestCode(context.Background(), srv.URL, "the-code")
	if err != nil {
		t.Fatalf("exchangeManifestCode: %v", err)
	}
	if mc.AppID != 999 || mc.Slug != "my-pruefer" || mc.WebhookSecret != "whsec" || mc.ClientID != "cid" || mc.ClientSecret != "csecret" {
		t.Errorf("ManifestCredentials = %+v, unexpected", mc)
	}
	if !strings.Contains(gotPath, "the-code") {
		t.Errorf("request path %q did not include the code", gotPath)
	}
	if !strings.HasSuffix(gotPath, "/conversions") {
		t.Errorf("request path %q did not end in /conversions", gotPath)
	}
}

func TestExchangeManifestCode_ErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/app-manifests/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := exchangeManifestCode(context.Background(), srv.URL, "expired-code"); err == nil {
		t.Fatal("expected an error for a 404 response (e.g. expired/already-used code)")
	}
}

// TestExchangeManifestCode_EscapesSpecialCharacters is the regression test
// for a review finding: code comes from an untrusted /callback query
// parameter and was previously interpolated unescaped into the request URL
// via fmt.Sprintf, so a value containing "/", "?", or "#" could redirect
// the request to a different path or inject query parameters instead of
// just failing to exchange. It must be escaped as an opaque path segment.
func TestExchangeManifestCode_EscapesSpecialCharacters(t *testing.T) {
	var gotPath, gotRawQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/app-manifests/", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": 1, "slug": "s", "pem": "pem",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	maliciousCode := "abc?evil=1#frag/xyz"
	if _, err := exchangeManifestCode(context.Background(), srv.URL, maliciousCode); err != nil {
		t.Fatalf("exchangeManifestCode: %v", err)
	}
	if gotRawQuery != "" {
		t.Errorf("gotRawQuery = %q, want empty — an unescaped code must not be able to inject query parameters", gotRawQuery)
	}
	if !strings.HasSuffix(gotPath, "/conversions") {
		t.Errorf("gotPath = %q, want it to still end in /conversions (not redirected by the code's own path-like characters)", gotPath)
	}
	if !strings.Contains(gotPath, "evil=1") {
		t.Errorf("gotPath = %q, want the code's literal characters present as an escaped path segment", gotPath)
	}
}

func TestExchangeManifestCode_MissingIDOrPEM(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/app-manifests/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"slug": "no-id-or-pem"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := exchangeManifestCode(context.Background(), srv.URL, "the-code"); err == nil {
		t.Fatal("expected an error when the response is missing id/pem")
	}
}

// TestExchangeManifestCode_HonorsContextCancellation is the regression test
// for a review finding: exchangeManifestCode previously built its request
// with http.NewRequest (no context), so a caller-driven cancellation (e.g.
// shutdown mid-exchange) couldn't abort it — the call would ride out
// manifestHTTPClient's full 30s timeout regardless. A canceled context must
// now fail the request immediately.
func TestExchangeManifestCode_HonorsContextCancellation(t *testing.T) {
	blockUntilCanceled := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/app-manifests/", func(w http.ResponseWriter, r *http.Request) {
		<-blockUntilCanceled // never responds — only ctx cancellation can end the request
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer close(blockUntilCanceled)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled — the request must fail without ever completing

	done := make(chan error, 1)
	go func() {
		_, err := exchangeManifestCode(ctx, srv.URL, "the-code")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a canceled context")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exchangeManifestCode did not honor context cancellation within 5s")
	}
}
