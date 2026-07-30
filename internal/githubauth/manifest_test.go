package githubauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		"issues":        "read",
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

	mc, err := exchangeManifestCode(srv.URL, "the-code")
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

	if _, err := exchangeManifestCode(srv.URL, "expired-code"); err == nil {
		t.Fatal("expected an error for a 404 response (e.g. expired/already-used code)")
	}
}

func TestExchangeManifestCode_MissingIDOrPEM(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/app-manifests/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"slug": "no-id-or-pem"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := exchangeManifestCode(srv.URL, "the-code"); err == nil {
		t.Fatal("expected an error when the response is missing id/pem")
	}
}
