package github

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRestPost_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Errorf("Authorization = %q", auth)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if accept := r.Header.Get("Accept"); accept != "application/vnd.github+json" {
			t.Errorf("Accept = %q", accept)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)
	err := c.restPost(srv.URL+"/test", map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("restPost: %v", err)
	}
}

func TestRestPost_4xxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"message":"Validation Failed"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	err := c.restPost(srv.URL+"/test", map[string]string{})
	if err == nil {
		t.Fatal("expected error for 422 response")
	}
}

func TestRestPost_5xxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`Internal Server Error`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	err := c.restPost(srv.URL+"/test", nil)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestRestPost_SuccessNon200(t *testing.T) {
	// 201 Created is a success
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	err := c.restPost(srv.URL+"/test", map[string]string{})
	if err != nil {
		t.Fatalf("restPost should succeed for 201: %v", err)
	}
}

func TestRestPost_ConnectionError(t *testing.T) {
	c := NewClientWithBaseURL("token", "http://127.0.0.1:1")
	c.httpClient.Timeout = 2 * time.Second // avoid 30s hang on connection refused
	err := c.restPost("http://127.0.0.1:1/test", map[string]string{})
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
}

func TestRestPost_InvalidURL(t *testing.T) {
	c := NewClientWithBaseURL("token", "http://example.com")
	err := c.restPost("://invalid-url", map[string]string{"key": "val"})
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestRestPost_CapturesRateLimitHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4800")
		w.Header().Set("X-RateLimit-Used", "200")
		w.Header().Set("X-RateLimit-Reset", "1700000000")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.restPost(srv.URL+"/test", map[string]string{}); err != nil {
		t.Fatalf("restPost: %v", err)
	}

	rest, _ := c.RateLimitStats()
	if rest.Limit != 5000 {
		t.Errorf("rest.Limit = %d, want 5000", rest.Limit)
	}
	if rest.Remaining != 4800 {
		t.Errorf("rest.Remaining = %d, want 4800", rest.Remaining)
	}
}

func TestRestPost_NoRateLimitHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No X-RateLimit-* headers
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.restPost(srv.URL+"/test", map[string]string{}); err != nil {
		t.Fatalf("restPost: %v", err)
	}

	rest, _ := c.RateLimitStats()
	if rest.Limit != 0 {
		t.Errorf("rest.Limit = %d, want 0 when headers absent", rest.Limit)
	}
}

func TestAuthErrorHint_401ContainsHint(t *testing.T) {
	hint := authErrorHint(401)
	if hint == "" {
		t.Fatal("expected non-empty hint for 401")
	}
	if !containsStr(hint, "classic personal access token") {
		t.Errorf("hint for 401 missing expected text, got: %q", hint)
	}
}

func TestAuthErrorHint_403ContainsHint(t *testing.T) {
	hint := authErrorHint(403)
	if hint == "" {
		t.Fatal("expected non-empty hint for 403")
	}
	if !containsStr(hint, "classic personal access token") {
		t.Errorf("hint for 403 missing expected text, got: %q", hint)
	}
}

func TestAuthErrorHint_500NoHint(t *testing.T) {
	if hint := authErrorHint(500); hint != "" {
		t.Errorf("expected empty hint for 500, got: %q", hint)
	}
}

func TestAuthErrorHint_404NoHint(t *testing.T) {
	if hint := authErrorHint(404); hint != "" {
		t.Errorf("expected empty hint for 404, got: %q", hint)
	}
}

func TestRestPost_401ContainsHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"message":"Requires authentication"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	err := c.restPost(srv.URL+"/test", map[string]string{})
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !containsStr(err.Error(), "classic personal access token") {
		t.Errorf("401 error missing auth hint, got: %q", err.Error())
	}
}

func TestRestPost_403ContainsHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"Forbidden"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	err := c.restPost(srv.URL+"/test", map[string]string{})
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	if !containsStr(err.Error(), "classic personal access token") {
		t.Errorf("403 error missing auth hint, got: %q", err.Error())
	}
}

func TestRestPost_500NoHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`Internal Server Error`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	err := c.restPost(srv.URL+"/test", map[string]string{})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if containsStr(err.Error(), "classic personal access token") {
		t.Errorf("500 error should not contain auth hint, got: %q", err.Error())
	}
}

func TestDo_SuccessBodyPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)
	resp, body, err := c.do("GET", srv.URL+"/test", nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want %q", string(body), `{"ok":true}`)
	}
}

func TestDo_ContentTypeOnlyWhenBodyNonNil(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, _, err := c.do("GET", srv.URL+"/test", nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if gotCT != "" {
		t.Errorf("Content-Type = %q, want empty for nil body", gotCT)
	}

	if _, _, err := c.do("POST", srv.URL+"/test", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json for non-nil body", gotCT)
	}
}

func TestDo_404MapsToErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, _, err := c.do("GET", srv.URL+"/test", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("do: expected ErrNotFound, got %v", err)
	}
}

func TestDo_405MapsToErrMethodNotAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(405)
		w.Write([]byte(`{"message":"Not Allowed"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, _, err := c.do("PUT", srv.URL+"/test", map[string]string{"k": "v"})
	if !errors.Is(err, ErrMethodNotAllowed) {
		t.Fatalf("do: expected ErrMethodNotAllowed, got %v", err)
	}
}

func TestDo_422MapsToErrUnprocessableEntity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"message":"Validation Failed"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, _, err := c.do("POST", srv.URL+"/test", map[string]string{"k": "v"})
	if !errors.Is(err, ErrUnprocessableEntity) {
		t.Fatalf("do: expected ErrUnprocessableEntity, got %v", err)
	}
}

func TestDo_Generic5xxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`Internal Server Error`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, _, err := c.do("GET", srv.URL+"/test", nil)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrMethodNotAllowed) || errors.Is(err, ErrUnprocessableEntity) {
		t.Errorf("500 should not map to a sentinel, got %v", err)
	}
}

func TestDo_AuthHintOn401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, _, err := c.do("GET", srv.URL+"/test", nil)
	if err == nil || !containsStr(err.Error(), "classic personal access token") {
		t.Fatalf("expected auth hint in error, got %v", err)
	}
}

func TestDo_RateLimitHeaderCapture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, _, err := c.do("GET", srv.URL+"/test", nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	rest, _ := c.RateLimitStats()
	if rest.Limit != 5000 {
		t.Errorf("rest.Limit = %d, want 5000", rest.Limit)
	}
}

func TestDo_MarshalError(t *testing.T) {
	c := NewClientWithBaseURL("token", "http://example.com")
	_, _, err := c.do("POST", "http://example.com/test", make(chan int))
	if err == nil {
		t.Fatal("expected marshal error for unsupported body type")
	}
}

func TestDo_RequestCreationError(t *testing.T) {
	c := NewClientWithBaseURL("token", "http://example.com")
	_, _, err := c.do("GET", "://invalid-url", nil)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

// Sentinel-mapping coverage for helpers that did not check these codes before
// the do() core unified the mapping (restGet, restPostWithResponse never
// mapped 404/405/422; restGetJSON/restDelete never mapped 405/422).

func TestRestGet_404MapsToErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.restGet(srv.URL + "/test")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("restGet: expected ErrNotFound, got %v", err)
	}
}

func TestRestPostWithResponse_404MapsToErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	var target struct{}
	err := c.restPostWithResponse(srv.URL+"/test", map[string]string{}, &target)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("restPostWithResponse: expected ErrNotFound, got %v", err)
	}
}

func TestRestGetJSON_405MapsToErrMethodNotAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(405)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	var target struct{}
	err := c.restGetJSON(srv.URL+"/test", &target)
	if !errors.Is(err, ErrMethodNotAllowed) {
		t.Fatalf("restGetJSON: expected ErrMethodNotAllowed, got %v", err)
	}
}

func TestRestDelete_422MapsToErrUnprocessableEntity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	err := c.restDelete(srv.URL + "/test")
	if !errors.Is(err, ErrUnprocessableEntity) {
		t.Fatalf("restDelete: expected ErrUnprocessableEntity, got %v", err)
	}
}

// containsStr reports whether substr is found in s.
func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
