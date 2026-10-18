package hookdeck

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/events"
)

func TestCreateSession_Success(t *testing.T) {
	var gotUser, gotPass string
	var gotOK bool
	var gotBody createSessionRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/cli-sessions" {
			t.Errorf("path = %s, want /cli-sessions", r.URL.Path)
		}
		gotUser, gotPass, gotOK = r.BasicAuth()
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(createSessionResponse{ID: "sess-123"})
	}))
	defer srv.Close()

	id, err := createSession(context.Background(), http.DefaultClient, srv.URL, "test-api-key")
	if err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if id != "sess-123" {
		t.Errorf("id = %q, want 'sess-123'", id)
	}
	if !gotOK {
		t.Fatal("expected HTTP Basic auth to be present")
	}
	if gotUser != "test-api-key" {
		t.Errorf("basic auth user = %q, want 'test-api-key'", gotUser)
	}
	if gotPass != "" {
		t.Errorf("basic auth password = %q, want empty", gotPass)
	}
	if len(gotBody.WebhookIDs) != 0 {
		t.Errorf("webhook_ids = %v, want an empty list", gotBody.WebhookIDs)
	}
}

func TestCreateSession_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"invalid api key"}`))
	}))
	defer srv.Close()

	_, err := createSession(context.Background(), http.DefaultClient, srv.URL, "bad-key")
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
}

func TestCreateSession_MissingID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()

	_, err := createSession(context.Background(), http.DefaultClient, srv.URL, "test-api-key")
	if err == nil {
		t.Fatal("expected an error when the response is missing an id")
	}
}

func TestCreateSession_MalformedResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := createSession(context.Background(), http.DefaultClient, srv.URL, "test-api-key")
	if err == nil {
		t.Fatal("expected an error for a malformed response body")
	}
}

// #2384 / SC-006: the text a session-creation failure hands to
// HealthEvent.Err (and so to the logs) must not carry the API key, and the
// logged form of a long multi-line server body must be bounded.
func TestCreateSession_ErrorTextCarriesNoCredentialAndLogsBounded(t *testing.T) {
	const key = "sentinel-api-key-do-not-log"
	body := strings.Repeat("line of server reply\n", 300)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	_, err := createSession(context.Background(), http.DefaultClient, srv.URL, key)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error text contains the API key: %q", err.Error())
	}
	// A transport failure (URL error) must not carry it either.
	srv.Close()
	_, err = createSession(context.Background(), http.DefaultClient, srv.URL, key)
	if err == nil || strings.Contains(err.Error(), key) {
		t.Errorf("transport error text = %v, must be non-nil and key-free", err)
	}

	// Reproduce the HTTP-status shape again for the sanitiser check.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(body))
	}))
	defer srv2.Close()
	_, err = createSession(context.Background(), http.DefaultClient, srv2.URL, key)
	logged := events.SanitizeErrText(err.Error())
	if strings.ContainsAny(logged, "\r\n") || len([]rune(logged)) > 301 {
		t.Errorf("logged form not single-line/bounded (%d runes)", len([]rune(logged)))
	}
}
