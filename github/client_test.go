package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGraphqlRequest_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/graphql" {
			t.Errorf("path = %s, want /graphql", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Errorf("Authorization = %q", auth)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}

		// Verify request body contains query and variables
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if body["query"] != "{ viewer { login } }" {
			t.Errorf("query = %v", body["query"])
		}

		w.WriteHeader(200)
		w.Write([]byte(`{"data":{"viewer":{"login":"testuser"}}}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)

	var result struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
		} `json:"data"`
	}

	err := c.graphqlRequest("{ viewer { login } }", nil, &result)
	if err != nil {
		t.Fatalf("graphqlRequest: %v", err)
	}
	if result.Data.Viewer.Login != "testuser" {
		t.Errorf("login = %q, want testuser", result.Data.Viewer.Login)
	}
}

func TestGraphqlRequest_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`Internal Server Error`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	var result struct{}
	err := c.graphqlRequest("{ test }", nil, &result)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestGraphqlRequest_GraphQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"errors":[{"message":"Field 'foo' not found"}]}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	var result struct{}
	err := c.graphqlRequest("{ test }", nil, &result)
	if err == nil {
		t.Fatal("expected error for GraphQL error response")
	}
}

func TestGraphqlRequest_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	var result struct{}
	err := c.graphqlRequest("{ test }", nil, &result)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestNewClient(t *testing.T) {
	c := NewClient("my-token")
	if c.token != "my-token" {
		t.Errorf("token = %q", c.token)
	}
	if c.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, defaultBaseURL)
	}
}

func TestNewClientWithBaseURL(t *testing.T) {
	c := NewClientWithBaseURL("tok", "http://localhost:1234")
	if c.baseURL != "http://localhost:1234" {
		t.Errorf("baseURL = %q", c.baseURL)
	}
}

func TestGraphqlRequest_WithVariables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		vars := body["variables"].(map[string]interface{})
		if vars["owner"] != "test-owner" {
			t.Errorf("owner = %v", vars["owner"])
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	var result struct{}
	err := c.graphqlRequest("query($owner: String!) { }", map[string]interface{}{"owner": "test-owner"}, &result)
	if err != nil {
		t.Fatalf("graphqlRequest: %v", err)
	}
}

func TestGraphqlRequest_ConnectionRefused(t *testing.T) {
	c := NewClientWithBaseURL("token", "http://127.0.0.1:1")
	c.httpClient.Timeout = 2 * time.Second // avoid 30s hang on connection refused
	var result struct{}
	err := c.graphqlRequest("{ test }", nil, &result)
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
}
