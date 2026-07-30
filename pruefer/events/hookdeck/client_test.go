package hookdeck

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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

	id, err := createSession(http.DefaultClient, srv.URL, "test-api-key")
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

	_, err := createSession(http.DefaultClient, srv.URL, "bad-key")
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

	_, err := createSession(http.DefaultClient, srv.URL, "test-api-key")
	if err == nil {
		t.Fatal("expected an error when the response is missing an id")
	}
}

func TestCreateSession_MalformedResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := createSession(http.DefaultClient, srv.URL, "test-api-key")
	if err == nil {
		t.Fatal("expected an error for a malformed response body")
	}
}
