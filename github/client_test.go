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

func TestGraphqlRequest_InvalidURL(t *testing.T) {
	c := NewClientWithBaseURL("token", "://bad-base")
	var result struct{}
	err := c.graphqlRequest("{ test }", nil, &result)
	if err == nil {
		t.Fatal("expected error for invalid base URL")
	}
}

func TestParseRateLimitHeaders_AllPresent(t *testing.T) {
	h := http.Header{}
	h.Set("X-RateLimit-Limit", "5000")
	h.Set("X-RateLimit-Remaining", "4998")
	h.Set("X-RateLimit-Used", "2")
	h.Set("X-RateLimit-Reset", "1700000000")

	stats := parseRateLimitHeaders(h)

	if stats.Limit != 5000 {
		t.Errorf("Limit = %d, want 5000", stats.Limit)
	}
	if stats.Remaining != 4998 {
		t.Errorf("Remaining = %d, want 4998", stats.Remaining)
	}
	if stats.Used != 2 {
		t.Errorf("Used = %d, want 2", stats.Used)
	}
	if stats.Reset.IsZero() {
		t.Error("Reset should be set")
	}
	if stats.Reset.Unix() != 1700000000 {
		t.Errorf("Reset.Unix() = %d, want 1700000000", stats.Reset.Unix())
	}
	if stats.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be set")
	}
}

func TestParseRateLimitHeaders_ResetAbsent(t *testing.T) {
	h := http.Header{}
	h.Set("X-RateLimit-Limit", "60")
	h.Set("X-RateLimit-Remaining", "0")
	// X-RateLimit-Reset intentionally omitted

	stats := parseRateLimitHeaders(h)

	if stats.Limit != 60 {
		t.Errorf("Limit = %d, want 60", stats.Limit)
	}
	if !stats.Reset.IsZero() {
		t.Errorf("Reset should be zero when header is absent, got %v", stats.Reset)
	}
}

func TestParseRateLimitHeaders_NoHeaders(t *testing.T) {
	stats := parseRateLimitHeaders(http.Header{})

	if stats.Limit != 0 {
		t.Errorf("Limit = %d, want 0 for empty headers", stats.Limit)
	}
}

func TestParseRateLimitHeaders_ZeroLimit(t *testing.T) {
	h := http.Header{}
	h.Set("X-RateLimit-Limit", "0")
	h.Set("X-RateLimit-Remaining", "0")

	stats := parseRateLimitHeaders(h)

	if stats.Limit != 0 {
		t.Errorf("Limit = %d, want 0", stats.Limit)
	}
}

func TestRateLimitStats_StoresOnRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4950")
		w.Header().Set("X-RateLimit-Used", "50")
		w.Header().Set("X-RateLimit-Reset", "1700000000")
		w.WriteHeader(200)
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	var result struct{}
	if err := c.graphqlRequest("{ test }", nil, &result); err != nil {
		t.Fatalf("graphqlRequest: %v", err)
	}

	_, graphql := c.RateLimitStats()
	if graphql.Limit != 5000 {
		t.Errorf("graphql.Limit = %d, want 5000", graphql.Limit)
	}
	if graphql.Remaining != 4950 {
		t.Errorf("graphql.Remaining = %d, want 4950", graphql.Remaining)
	}
}

func TestRateLimitStats_Concurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "1700000000")
		w.WriteHeader(200)
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)

	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			var result struct{}
			c.graphqlRequest("{ test }", nil, &result) //nolint:errcheck
		}()
	}
	for i := 0; i < 5; i++ {
		<-done
	}
	// Verify stats were written without data races (race detector catches any issues).
	_, graphql := c.RateLimitStats()
	if graphql.Limit != 5000 {
		t.Errorf("graphql.Limit = %d, want 5000", graphql.Limit)
	}
}
