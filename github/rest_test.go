// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package github

import (
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
